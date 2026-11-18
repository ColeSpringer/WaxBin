package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// artWalkCatalog holds one entity per way a front cover is found, and the hash each
// reference should answer with ("" for none).
type artWalkCatalog struct {
	st   *Store
	want map[model.EntityRef]string
}

func (c *artWalkCatalog) refs() []model.EntityRef {
	refs := make([]model.EntityRef, 0, len(c.want))
	for ref := range c.want {
		refs = append(refs, ref)
	}
	return refs
}

// seedArtWalk builds the catalog. Every album has its own artist, so a picture set on
// one artist reaches that album's entities alone.
func seedArtWalk(t *testing.T) *artWalkCatalog {
	t.Helper()
	st, lib := entityFixture(t)
	ctx := context.Background()
	c := &artWalkCatalog{st: st, want: map[model.EntityRef]string{}}
	user := model.Attribution{Source: model.SourceUser}

	seed := 0
	pic := func() *model.ArtImage {
		seed++
		return testPNG(t, 30+seed, 30+seed)
	}
	set := func(typ model.ArtEntity, pid model.PID) string {
		t.Helper()
		img := pic()
		if _, err := st.SetEntityArt(ctx, typ, pid, model.ArtRoleFront, img.Data, "", user, model.LockOf(false), false); err != nil {
			t.Fatalf("set %s art: %v", typ, err)
		}
		return img.Hash
	}
	n := 0
	track := func(album, artist, genre string, cover *model.ArtImage) model.PID {
		t.Helper()
		n++
		in := trackSpecInput(lib.ID, trackSpec{
			path: fmt.Sprintf("/lib/%d.flac", n), essence: fmt.Sprintf("e%d", n), content: fmt.Sprintf("c%d", n),
			title: fmt.Sprintf("T%d", n), artist: artist, albumArt: artist, album: album, genre: genre,
		})
		in.CoverArt = cover
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatalf("put track %d: %v", n, err)
		}
		return res.ItemPID
	}
	pidOf := func(table, col, val string) model.PID {
		t.Helper()
		return model.PID(scalarStr(t, st, "SELECT pid FROM "+table+" WHERE "+col+" = ?", val))
	}
	groupOf := func(album string) model.PID {
		t.Helper()
		return model.PID(scalarStr(t, st,
			"SELECT rg.pid FROM album al JOIN release_group rg ON rg.id = al.release_group_id WHERE al.title = ?", album))
	}
	// expect records one album's worth of answers: its tracks, the album, its release
	// group and its artist.
	expect := func(album, artist string, tracks []model.PID, trackHash, albumHash, groupHash, artistHash string) {
		t.Helper()
		for _, pid := range tracks {
			c.want[model.EntityRef{Type: model.ArtTrack, PID: pid}] = trackHash
		}
		c.want[model.EntityRef{Type: model.ArtAlbum, PID: pidOf("album", "title", album)}] = albumHash
		c.want[model.EntityRef{Type: model.ArtReleaseGroup, PID: groupOf(album)}] = groupHash
		c.want[model.EntityRef{Type: model.ArtArtist, PID: pidOf("artist", "name", artist)}] = artistHash
	}

	// A cover on one track answers for it, for its album, and for the sibling without one.
	own := pic()
	covered := track("Own", "Ar Own", "", own)
	sibling := track("Own", "Ar Own", "", nil)
	expect("Own", "Ar Own", []model.PID{covered, sibling}, own.Hash, own.Hash, "", "")

	// An album row of its own wins over a member's cover, for the album and for a
	// coverless member, while the member with a cover keeps its own.
	member := pic()
	rowed := track("Rowed", "Ar Rowed", "", member)
	bare := track("Rowed", "Ar Rowed", "", nil)
	rowHash := set(model.ArtAlbum, pidOf("album", "title", "Rowed"))
	expect("Rowed", "Ar Rowed", []model.PID{bare}, rowHash, rowHash, "", "")
	c.want[model.EntityRef{Type: model.ArtTrack, PID: rowed}] = member.Hash

	grouped := track("Grouped", "Ar Grouped", "", nil)
	groupHash := set(model.ArtReleaseGroup, groupOf("Grouped"))
	expect("Grouped", "Ar Grouped", []model.PID{grouped}, groupHash, groupHash, groupHash, "")

	byArtist := track("By Artist", "Ar Pictured", "", nil)
	artistHash := set(model.ArtArtist, pidOf("artist", "name", "Ar Pictured"))
	expect("By Artist", "Ar Pictured", []model.PID{byArtist}, artistHash, artistHash, artistHash, artistHash)

	// A genre answers for a track and for nothing above it.
	byGenre := track("By Genre", "Ar Genre", "Zydeco", nil)
	genreHash := set(model.ArtGenre, pidOf("genre", "name", "Zydeco"))
	expect("By Genre", "Ar Genre", []model.PID{byGenre}, genreHash, "", "", "")
	c.want[model.EntityRef{Type: model.ArtGenre, PID: pidOf("genre", "name", "Zydeco")}] = genreHash

	plain := track("Plain", "Ar Plain", "Polka", nil)
	expect("Plain", "Ar Plain", []model.PID{plain}, "", "", "", "")
	c.want[model.EntityRef{Type: model.ArtGenre, PID: pidOf("genre", "name", "Polka")}] = ""

	// A track in no album reaches its own artist.
	loose := track("", "Ar Loose", "", nil)
	looseArtist := pidOf("artist", "name", "Ar Loose")
	looseHash := set(model.ArtArtist, looseArtist)
	c.want[model.EntityRef{Type: model.ArtTrack, PID: loose}] = looseHash
	c.want[model.EntityRef{Type: model.ArtArtist, PID: looseArtist}] = looseHash

	// A book stops at its own cover, whatever its author holds.
	jacket := pic()
	in := bookSpecInput(lib.ID, bookSpec{path: "/lib/b1.m4b", essence: "eb1", content: "cb1", title: "Jacketed", author: "Au Thor"})
	in.CoverArt = jacket
	res, err := st.PutScannedBook(ctx, in)
	if err != nil {
		t.Fatalf("put book: %v", err)
	}
	c.want[model.EntityRef{Type: model.ArtTrack, PID: res.ItemPID}] = jacket.Hash
	coverless := putBook(t, st, lib.ID, bookSpec{path: "/lib/b2.m4b", essence: "eb2", content: "cb2", title: "Bare", author: "Au Thor"})
	author := pidOf("artist", "name", "Au Thor")
	c.want[model.EntityRef{Type: model.ArtArtist, PID: author}] = set(model.ArtArtist, author)
	c.want[model.EntityRef{Type: model.ArtTrack, PID: coverless.ItemPID}] = ""

	// An episode answers from its own cover, else its show's.
	shown := putFeed(t, st, "http://cast.example/shown", "Alpha", "Beta")
	showHash := set(model.ArtPodcast, shown.PodcastPID)
	alpha := episodeByTitle(t, st, shown.PodcastPID, "Alpha").PID
	alphaHash := set(model.ArtEpisode, alpha)
	c.want[model.EntityRef{Type: model.ArtPodcast, PID: shown.PodcastPID}] = showHash
	c.want[model.EntityRef{Type: model.ArtEpisode, PID: alpha}] = alphaHash
	c.want[model.EntityRef{Type: model.ArtEpisode, PID: episodeByTitle(t, st, shown.PodcastPID, "Beta").PID}] = showHash
	blank := putFeed(t, st, "http://cast.example/blank", "Gamma")
	c.want[model.EntityRef{Type: model.ArtPodcast, PID: blank.PodcastPID}] = ""
	c.want[model.EntityRef{Type: model.ArtEpisode, PID: episodeByTitle(t, st, blank.PodcastPID, "Gamma").PID}] = ""

	pictured, empty := newPlaylist(t, st, "Pictured"), newPlaylist(t, st, "Empty")
	c.want[model.EntityRef{Type: model.ArtPlaylist, PID: pictured}] = set(model.ArtPlaylist, pictured)
	c.want[model.EntityRef{Type: model.ArtPlaylist, PID: empty}] = ""
	return c
}

// TestArtHashesWalksEveryRung reads the whole catalog in one call and checks each answer
// against the rung it should come from.
func TestArtHashesWalksEveryRung(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	got, err := c.st.ArtHashes(context.Background(), c.refs())
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	for ref, want := range c.want {
		if got[ref] != want {
			t.Errorf("%s %s = %q, want %q", ref.Type, ref.PID, got[ref], want)
		}
		if _, present := got[ref]; present && want == "" {
			t.Errorf("%s %s has an entry and resolves no cover", ref.Type, ref.PID)
		}
	}
	if len(got) > len(c.want) {
		t.Errorf("%d answers for %d references", len(got), len(c.want))
	}
}

// TestArtHashesMatchesResolveArt holds the batch to the single resolve: the hash it
// reports is the one the picture is served under, and it is silent exactly where the
// resolve finds nothing.
func TestArtHashesMatchesResolveArt(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	ctx := context.Background()
	got, err := c.st.ArtHashes(ctx, c.refs())
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	for ref := range c.want {
		blob, err := c.st.ResolveArt(ctx, ref, model.ArtRoleFront, 0)
		switch {
		case waxerr.Is(err, waxerr.CodeNotFound):
			if hash, present := got[ref]; present {
				t.Errorf("%s %s = %q, and ResolveArt finds nothing", ref.Type, ref.PID, hash)
			}
		case err != nil:
			t.Errorf("ResolveArt %s %s: %v", ref.Type, ref.PID, err)
		case got[ref] != blob.SourceHash:
			t.Errorf("%s %s = %q, and ResolveArt serves %q", ref.Type, ref.PID, got[ref], blob.SourceHash)
		}
	}
}

// TestItemsMissingArtIsWhatTheWalkLeavesBare holds the audit to the resolver: it reports
// the present tracks and books that resolve no cover on any rung, the artist's and the
// genre's included, in title order, and counts them all past the sample.
func TestItemsMissingArtIsWhatTheWalkLeavesBare(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	ctx := context.Background()
	var want []model.PID
	for _, v := range mustItems(t, c.st) {
		if v.Kind != model.KindEpisode && c.want[model.EntityRef{Type: model.ArtTrack, PID: v.PID}] == "" {
			want = append(want, v.PID)
		}
	}
	if len(want) != 2 {
		t.Fatalf("the catalog holds %d coverless tracks and books, want the plain track and the bare book", len(want))
	}
	items, total, err := c.st.ItemsMissingArt(ctx, 10)
	if err != nil {
		t.Fatalf("ItemsMissingArt: %v", err)
	}
	if total != len(want) || len(items) != len(want) {
		t.Fatalf("total %d, sample %d, want %d of each: %+v", total, len(items), len(want), items)
	}
	for i, pid := range want {
		if items[i].PID != pid {
			t.Errorf("sample[%d] = %s %q, want %s", i, items[i].PID, items[i].Title, pid)
		}
	}
	if items, total, err := c.st.ItemsMissingArt(ctx, 1); err != nil || total != 2 || len(items) != 1 || items[0].PID != want[0] {
		t.Errorf("a sample of one = %+v of %d (err %v), want the first of 2", items, total, err)
	}
	if items, total, err := c.st.ItemsMissingArt(ctx, 0); err != nil || total != 2 || len(items) != 0 {
		t.Errorf("a count alone = %+v of %d (err %v), want no sample of 2", items, total, err)
	}
}

// mustItems lists every item in the order the audit samples in.
func mustItems(t *testing.T, st *Store) []*model.ItemView {
	t.Helper()
	items, err := st.QueryItems(context.Background(), query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	return items
}

// TestListedShapesBecomeArtReferences takes the three shapes a host lists (item views,
// facet buckets, entity cards) to references the way their helpers say, and checks each
// names an entity of that art type and answers what the catalog expects of it.
func TestListedShapesBecomeArtReferences(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	ctx := context.Background()
	check := func(what string, refs []model.EntityRef) {
		t.Helper()
		if len(refs) == 0 {
			return
		}
		pids := make([]model.PID, len(refs))
		for i, ref := range refs {
			pids[i] = ref.PID
		}
		fronts, err := c.st.artWalk(ctx, refs[0].Type, model.ArtRoleFront, pids, "test")
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, ref := range refs {
			front, known := fronts[ref.PID]
			if !known {
				t.Errorf("%s: %s is no %s", what, ref.PID, ref.Type)
			}
			if want, listed := c.want[ref]; listed && front.hit.hash != want {
				t.Errorf("%s: %s %s = %q, want %q", what, ref.Type, ref.PID, front.hit.hash, want)
			}
		}
	}

	byType := map[model.ArtEntity][]model.EntityRef{}
	items := mustItems(t, c.st)
	for _, v := range items {
		ref := v.ArtRef()
		if _, listed := c.want[ref]; !listed {
			t.Errorf("item %q (%s) is asked for under %s, which the catalog does not expect", v.Title, v.Kind, ref.Type)
		}
		byType[ref.Type] = append(byType[ref.Type], ref)
	}
	if len(byType[model.ArtEpisode]) == 0 || len(byType[model.ArtTrack]) == 0 {
		t.Fatalf("items = %d, want tracks, books and episodes among them", len(items))
	}
	for typ, refs := range byType {
		check("items as "+string(typ), refs)
	}

	all := query.New(query.EntityItems).Build()
	for _, g := range read.GroupBys() {
		typ, ok := g.ArtEntity()
		if !ok {
			continue
		}
		res, err := c.st.Facet(ctx, all, g, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", g, err)
		}
		var refs []model.EntityRef
		for _, b := range res.Buckets {
			if b.EntityPID != "" {
				refs = append(refs, model.EntityRef{Type: typ, PID: b.EntityPID})
			}
		}
		if len(refs) == 0 && g != read.GroupPlaylist && g != read.GroupNarrator {
			t.Errorf("facet %s has no entity buckets to check", g)
		}
		check("facet "+string(g), refs)
	}
	for _, k := range read.EntityKinds() {
		typ, ok := k.ArtEntity()
		if !ok {
			continue
		}
		page, err := c.st.EntityPage(ctx, k, "", 200)
		if err != nil {
			t.Fatalf("entity page %s: %v", k, err)
		}
		var refs []model.EntityRef
		for _, e := range page.Entities {
			refs = append(refs, model.EntityRef{Type: typ, PID: e.PID})
		}
		if len(refs) == 0 {
			t.Errorf("no %s cards to check", k)
		}
		check("cards "+string(k), refs)
	}
}

// TestCardsAndBucketsCarryTheCoverHash reads every card shape and every facet over the
// catalog and holds the ArtHash each carries to what the catalog expects of that
// entity: the walk's answer where it resolves a cover, nothing where it does not, and
// nothing on a dimension or a kind that holds no art.
func TestCardsAndBucketsCarryTheCoverHash(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	ctx := context.Background()
	expect := func(what string, typ model.ArtEntity, pid model.PID, got string) {
		t.Helper()
		want, listed := c.want[model.EntityRef{Type: typ, PID: pid}]
		if listed && got != want {
			t.Errorf("%s: %s %s carries %q, want %q", what, typ, pid, got, want)
		}
	}

	all := query.New(query.EntityItems).Build()
	for _, g := range append(read.GroupBys(), "tag.MOOD") {
		res, err := c.st.Facet(ctx, all, g, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", g, err)
		}
		typ, holdsArt := g.ArtEntity()
		carried := 0
		for _, b := range res.Buckets {
			if b.ArtHash != "" {
				carried++
			}
			switch {
			case !holdsArt || b.IsUnknown:
				if b.ArtHash != "" {
					t.Errorf("facet %s: bucket %q carries %q, want none", g, b.Display, b.ArtHash)
				}
			default:
				expect("facet "+string(g), typ, b.EntityPID, b.ArtHash)
			}
		}
		// The dimensions the catalog has covers under must show at least one.
		switch g {
		case read.GroupAlbum, read.GroupReleaseGroup, read.GroupArtist, read.GroupAlbumArtist,
			read.GroupCreditArtist, read.GroupGenre, read.GroupPodcast, read.GroupAuthor:
			if carried == 0 {
				t.Errorf("facet %s: no bucket carries a cover", g)
			}
		}
		// A facet that only counts carries none.
		bare, err := c.st.facet(ctx, all, g, "", 0, "", "test")
		if err != nil {
			t.Fatalf("bare facet %s: %v", g, err)
		}
		for _, b := range bare.Buckets {
			if b.ArtHash != "" {
				t.Errorf("the counting facet %s carries a cover on %q", g, b.Display)
			}
		}
	}
	// A limit bounds the buckets returned and each still carries its own cover.
	top, err := c.st.Facet(ctx, all, read.GroupAlbum, read.FacetOrderCount, 2, "")
	if err != nil || len(top.Buckets) != 2 {
		t.Fatalf("top albums = %+v (err %v), want two buckets", top, err)
	}
	for _, b := range top.Buckets {
		expect("top albums", model.ArtAlbum, b.EntityPID, b.ArtHash)
	}

	for _, k := range read.EntityKinds() {
		page, err := c.st.EntityPage(ctx, k, "", 200)
		if err != nil {
			t.Fatalf("entity page %s: %v", k, err)
		}
		typ, holdsArt := k.ArtEntity()
		carried := 0
		for _, e := range page.Entities {
			if e.ArtHash != "" {
				carried++
			}
			if !holdsArt {
				if e.ArtHash != "" {
					t.Errorf("%s %q carries %q, want none", k, e.Name, e.ArtHash)
				}
				continue
			}
			expect("page of "+string(k), typ, e.PID, e.ArtHash)
			one, err := c.st.EntityByPID(ctx, k, e.PID)
			if err != nil || one.ArtHash != e.ArtHash {
				t.Errorf("%s %q read alone carries %q (err %v), and %q on the page", k, e.Name, one.ArtHash, err, e.ArtHash)
			}
		}
		if holdsArt && carried == 0 {
			t.Errorf("no %s card carries a cover", k)
		}
	}

	shows, err := c.st.Podcasts(ctx)
	if err != nil || len(shows) != 2 {
		t.Fatalf("podcasts = %d (err %v), want the two shows", len(shows), err)
	}
	for _, p := range shows {
		expect("podcasts", model.ArtPodcast, p.PID, p.ArtHash)
		one, err := c.st.PodcastByPID(ctx, p.PID)
		if err != nil || one.ArtHash != p.ArtHash {
			t.Errorf("show %q read alone carries %q (err %v), and %q in the list", p.Title, one.ArtHash, err, p.ArtHash)
		}
	}
	lists, err := c.st.ListPlaylists(ctx, "")
	if err != nil || len(lists) != 2 {
		t.Fatalf("playlists = %d (err %v), want the two", len(lists), err)
	}
	for _, p := range lists {
		expect("playlists", model.ArtPlaylist, p.PID, p.ArtHash)
	}
}

// TestSearchHitsCarryTheCoverHash searches the catalog for something in every group and
// holds each hit's ArtHash to what the catalog expects of the entity it names.
func TestSearchHitsCarryTheCoverHash(t *testing.T) {
	t.Parallel()
	c := seedArtWalk(t)
	ctx := context.Background()
	seen := map[string]int{}
	carried := map[string]int{}
	for _, q := range []string{"Ar", "Own", "Rowed", "Grouped", "Plain", "Jacketed", "Bare", "Alpha", "Beta", "Gamma"} {
		res, err := c.st.Search(ctx, q, read.SearchOptions{})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		for _, hits := range [][]read.SearchHit{res.Artists, res.Albums, res.Tracks, res.Books, res.Episodes} {
			for _, h := range hits {
				want, listed := c.want[h.ArtRef()]
				if !listed {
					t.Errorf("search %q: the %s hit %q is asked for under %s, which the catalog does not expect", q, h.Kind, h.Title, h.ArtRef().Type)
					continue
				}
				if h.ArtHash != want {
					t.Errorf("search %q: the %s hit %q carries %q, want %q", q, h.Kind, h.Title, h.ArtHash, want)
				}
				seen[h.Kind]++
				if h.ArtHash != "" {
					carried[h.Kind]++
				}
			}
		}
	}
	for _, kind := range []string{"artist", "album", "track", "book", "episode"} {
		if carried[kind] == 0 || carried[kind] == seen[kind] {
			t.Errorf("%s hits: %d of %d carry a cover, want some with and some without", kind, carried[kind], seen[kind])
		}
	}
}

// TestArtHashesFollowsACoverChange is the reason the read exists: the hash moves when
// the picture an entity draws does, including for the entities that only inherit it.
func TestArtHashesFollowsACoverChange(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	first, second := testPNG(t, 40, 40), testPNG(t, 41, 41)
	covered := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", first)
	sibling := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)
	refs := []model.EntityRef{
		{Type: model.ArtTrack, PID: covered}, {Type: model.ArtTrack, PID: sibling},
		{Type: model.ArtAlbum, PID: albumPID(t, st)},
	}
	check := func(want string) {
		t.Helper()
		got, err := st.ArtHashes(ctx, refs)
		if err != nil {
			t.Fatalf("ArtHashes: %v", err)
		}
		for _, ref := range refs {
			if got[ref] != want {
				t.Errorf("%s %s = %q, want %q", ref.Type, ref.PID, got[ref], want)
			}
		}
	}
	check(first.Hash)
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", second)
	check(second.Hash)
}

func TestArtHashesSkipsWhatItCannotName(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	track := model.EntityRef{Type: model.ArtTrack, PID: pid}

	got, err := st.ArtHashes(ctx, []model.EntityRef{
		track, track,
		{Type: model.ArtTrack, PID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"},
		{Type: model.ArtAlbum, PID: pid}, // a track's pid is no album
		{Type: model.ArtEpisode, PID: pid},
	})
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	if len(got) != 1 || got[track] != cover.Hash {
		t.Errorf("got %v, want the one track's cover", got)
	}
	if got, err := st.ArtHashes(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("no references = %v (err %v), want an empty answer", got, err)
	}
	if _, err := st.ArtHashes(ctx, []model.EntityRef{track, {Type: "series", PID: pid}}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("an unknown type = %v, want CodeInvalid", err)
	}
}

// TestArtHashesReadsPastOneChunk puts real references on both sides of a chunk boundary.
func TestArtHashesReadsPastOneChunk(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	head := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	tail := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)

	refs := []model.EntityRef{{Type: model.ArtTrack, PID: head}}
	for i := range idBatchSize + 20 {
		refs = append(refs, model.EntityRef{Type: model.ArtTrack, PID: model.PID(fmt.Sprintf("NOSUCHPID%017d", i))})
	}
	refs = append(refs, model.EntityRef{Type: model.ArtTrack, PID: tail})
	got, err := st.ArtHashes(ctx, refs)
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	if len(got) != 2 || got[refs[0]] != cover.Hash || got[refs[len(refs)-1]] != cover.Hash {
		t.Errorf("got %d answers, want the cover for both real tracks", len(got))
	}
}

// TestArtWalkStatementCounts pins what the walk costs: one statement for entities that
// hold their own cover, and one more per rung only for the ones still open, however
// many share it.
func TestArtWalkStatementCounts(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var covered, bare []model.EntityRef
	for i := range 6 {
		pid := putWithCover(t, st, lib.ID, fmt.Sprintf("/lib/own/%d.flac", i), fmt.Sprintf("o%d", i), testPNG(t, 40, 40))
		covered = append(covered, model.EntityRef{Type: model.ArtTrack, PID: pid})
	}
	for i := range 6 {
		in := trackSpecInput(lib.ID, trackSpec{path: fmt.Sprintf("/lib/bare/%d.flac", i), essence: fmt.Sprintf("b%d", i),
			content: fmt.Sprintf("b%d", i), title: fmt.Sprintf("B%d", i), artist: "Bare", album: "Bare", genre: "Polka"})
		res, err := st.PutScannedTrack(ctx, in)
		if err != nil {
			t.Fatalf("put: %v", err)
		}
		bare = append(bare, model.EntityRef{Type: model.ArtTrack, PID: res.ItemPID})
	}
	ran := func(refs []model.EntityRef) int {
		t.Helper()
		before := st.rstmts.stats()
		if _, err := st.ArtHashes(ctx, refs); err != nil {
			t.Fatalf("ArtHashes: %v", err)
		}
		after := st.rstmts.stats()
		return after.prepares + after.hits - before.prepares - before.hits
	}
	if n := ran(covered); n != 1 {
		t.Errorf("six tracks with their own covers ran %d statements, want 1", n)
	}
	// The tracks, their album, its members' covers, its release group, the artist, the genre.
	if n := ran(bare); n != 6 {
		t.Errorf("six coverless tracks of one album ran %d statements, want 6", n)
	}
	if n := ran(covered[:1]); n != 1 {
		t.Errorf("one covered track ran %d statements, want 1", n)
	}
}

// TestArtWalkPlans keeps each rung an index seek. The derived rung is the one to watch:
// left to the planner it starts from every track cover in the catalog rather than from
// the album's members, which is what albumMemberFronts pins, for the enrichment queue's
// vacancy check as much as for the walk.
func TestArtWalkPlans(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	for i := range 4 {
		putWithCover(t, st, lib.ID, fmt.Sprintf("/lib/al/%d.flac", i), fmt.Sprintf("e%d", i), testPNG(t, 40+i, 40))
	}
	putFeed(t, st, "http://cast.example/f", "Ep1")
	members := "SEARCH t USING COVERING INDEX track_album_id (album_id=?)"
	for name, tc := range map[string]struct {
		stmt string
		args []any
		want string
	}{
		"items":     {artItemEntry, []any{"front", `["x"]`}, ""},
		"episodes":  {artEpisodeEntry, []any{"front", `["x"]`}, ""},
		"albums":    {artAlbumNodes + "al.pid IN " + inArray, []any{"front", `["x"]`}, ""},
		"album ids": {artAlbumNodes + "al.id IN " + inArray, []any{"front", `[1]`}, ""},
		"groups":    {artGroupEntry, []any{"front", `["x"]`}, ""},
		"own":       {artOwnFronts, []any{"artist", `[1]`}, ""},
		"derived":   {artDerivedFronts, []any{`[1]`}, members},
		"genres":    {artGenreFronts, []any{`[1]`}, ""},
		"vacancy":   {"SELECT al.id FROM album al WHERE al.id = ? AND NOT " + albumResolvesFrontArt, []any{1}, members},
		// The audit's member rung is a set built once, not a probe per item.
		"audit": {"SELECT COUNT(*) " + itemsMissingArtWhere, nil, "LIST SUBQUERY"},
	} {
		plan := explainPlan(t, st, tc.stmt, tc.args...)
		for _, line := range strings.Split(plan, "\n") {
			// A scan of the bound list, or of the genre subquery's own rows, reads no table.
			if strings.Contains(line, "SCAN") && !strings.Contains(line, "VIRTUAL TABLE") && !strings.Contains(line, "SCAN g") {
				t.Errorf("%s scans a table:\n%s", name, plan)
			}
		}
		if !strings.Contains(plan, tc.want) {
			t.Errorf("%s does not start from the album's members:\n%s", name, plan)
		}
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s sorts, so it cannot stop at the first cover:\n%s", name, plan)
		}
		// The audit reads every track cover on purpose, once, into its member set.
		if strings.Contains(plan, "(entity_type=?)") && name != "audit" {
			t.Errorf("%s reads every cover of a type:\n%s", name, plan)
		}
	}
}
