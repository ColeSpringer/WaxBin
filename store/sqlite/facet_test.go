package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

func bucketByDisplay(r *read.FacetResult, display string) (read.Bucket, bool) {
	for _, b := range r.Buckets {
		if b.Display == display {
			return b, true
		}
	}
	return read.Bucket{}, false
}

func TestFacetByGenre(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", genre: "Rock; Pop"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "C", artist: "Z", album: "Cl", genre: ""})

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupGenre, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	rock, ok := bucketByDisplay(res, "Rock")
	if !ok || rock.Count != 2 {
		t.Errorf("Rock bucket = %+v, want count 2", rock)
	}
	if rock.EntityPID == "" {
		t.Error("genre bucket should carry the entity pid for drilldown")
	}
	pop, ok := bucketByDisplay(res, "Pop")
	if !ok || pop.Count != 1 {
		t.Errorf("Pop bucket = %+v, want count 1", pop)
	}
	noGenre, ok := bucketByDisplay(res, read.NoGenre)
	if !ok || noGenre.Count != 1 || !noGenre.IsUnknown {
		t.Errorf("No-Genre bucket = %+v, want count 1 + unknown", noGenre)
	}
}

func TestFacetByArtistUnknownBucket(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "Radiohead", album: "OK"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "", album: ""})

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	if b, ok := bucketByDisplay(res, "Radiohead"); !ok || b.Count != 1 {
		t.Errorf("Radiohead bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, read.UnknownArtist); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("Unknown-Artist bucket = %+v, want count 1 + unknown", b)
	}
}

func TestFacetByAlbum(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "First"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "X", album: "First"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "C", artist: "Y", album: "Second"})
	// A book is track-only-absent (no t.album_id), so it lands in [Non-Album].
	putBook(t, st, lib.ID, bookSpec{path: "/lib/book.m4b", essence: "be1", content: "bc1", title: "Memoir", author: "Author", durationMS: 100})

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupAlbum, "", 0, "")
	if err != nil {
		t.Fatalf("facet album: %v", err)
	}
	first, ok := bucketByDisplay(res, "First")
	if !ok || first.Count != 2 {
		t.Errorf("First bucket = %+v, want count 2", first)
	}
	if first.EntityPID == "" {
		t.Error("album bucket should carry the entity pid for drilldown")
	}
	if b, ok := bucketByDisplay(res, "Second"); !ok || b.Count != 1 {
		t.Errorf("Second bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, read.NonAlbum); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("Non-Album bucket = %+v, want count 1 + unknown (the book)", b)
	}

	// Drilldown: the bucket's EntityPID pairs with the album_pid query field and
	// returns exactly that album's tracks, whose item-view AlbumPID matches.
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("album_pid", query.OpIs, string(first.EntityPID)).Build(), "")
	if err != nil {
		t.Fatalf("album drilldown: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("album drilldown returned %d items, want the 2 First tracks", len(items))
	}
	for _, it := range items {
		if it.Album != "First" {
			t.Errorf("drilldown item album = %q, want First", it.Album)
		}
		if it.AlbumPID != first.EntityPID {
			t.Errorf("drilldown item AlbumPID = %s, want the bucket pid %s", it.AlbumPID, first.EntityPID)
		}
	}
}

// detachReleaseGroup clears one album's release group, the state a scan never
// produces on its own (resolveAlbumChain always chains an album to a group) but the
// schema explicitly allows: album.release_group_id is nullable with
// ON DELETE SET NULL, so a merged or GC'd group leaves exactly this behind. It is
// what separates [No Release Group] from [Non-Album].
func detachReleaseGroup(t *testing.T, st *Store, albumTitle string) {
	t.Helper()
	res, err := st.write.ExecContext(context.Background(),
		"UPDATE album SET release_group_id = NULL WHERE title = ?", albumTitle)
	if err != nil {
		t.Fatalf("detach release group from %q: %v", albumTitle, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("no album titled %q to detach", albumTitle)
	}
}

// TestFacetByReleaseGroup covers the dimension above album: its buckets, the two
// distinct unknown states it has to keep apart, and the drilldown through
// release_group_pid that a bucket's EntityPID is for.
func TestFacetByReleaseGroup(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two editions of one record: same album artist and album title, different
	// folders, so they are two albums under one release group.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/cd/1.flac", essence: "e1", content: "c1",
		title: "A", artist: "X", albumArt: "X", album: "Kid A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/vinyl/2.flac", essence: "e2", content: "c2",
		title: "B", artist: "X", albumArt: "X", album: "Kid A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/other/3.flac", essence: "e3", content: "c3",
		title: "C", artist: "Y", albumArt: "Y", album: "Insides"})
	// A track on an album that has no release group, which is not the same state as
	// having no album.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/orphan/4.flac", essence: "e4", content: "c4",
		title: "D", artist: "Z", albumArt: "Z", album: "Orphan"})
	detachReleaseGroup(t, st, "Orphan")
	// A book has no track row at all, so it lands in the same unknown bucket.
	putBook(t, st, lib.ID, bookSpec{path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "Memoir", author: "Author", durationMS: 100})
	// An episode is excluded from the dimension entirely (kindWhere), like every
	// other music dimension.
	putFeed(t, st, "http://cast.example/f", "Ep1")

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupReleaseGroup, "", 0, "")
	if err != nil {
		t.Fatalf("facet releaseGroup: %v", err)
	}
	kidA, ok := bucketByDisplay(res, "Kid A")
	if !ok || kidA.Count != 2 {
		t.Fatalf("Kid A bucket = %+v, want count 2 (both editions under one group)", kidA)
	}
	if kidA.EntityPID == "" {
		t.Error("release group bucket should carry the entity pid for drilldown")
	}
	if b, ok := bucketByDisplay(res, "Insides"); !ok || b.Count != 1 {
		t.Errorf("Insides bucket = %+v, want count 1", b)
	}
	// The group-less track and the book share the unknown bucket; the episode is not
	// in the dimension at all.
	if b, ok := bucketByDisplay(res, read.NoReleaseGroup); !ok || b.Count != 2 || !b.IsUnknown {
		t.Errorf("%s bucket = %+v, want count 2 + unknown (the group-less track and the book)",
			read.NoReleaseGroup, b)
	}
	if _, ok := bucketByDisplay(res, "Cast"); ok {
		t.Error("the episode's feed leaked into the release-group dimension")
	}

	// Drilldown: the bucket's EntityPID pairs with release_group_pid and returns
	// exactly the bucket's count, whose item views carry the same handle.
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("release_group_pid", query.OpIs, string(kidA.EntityPID)).Build(), "")
	if err != nil {
		t.Fatalf("release group drilldown: %v", err)
	}
	if len(items) != kidA.Count {
		t.Fatalf("drilldown returned %d items, want the bucket's %d", len(items), kidA.Count)
	}
	for _, it := range items {
		if it.ReleaseGroupPID != kidA.EntityPID {
			t.Errorf("drilldown item ReleaseGroupPID = %s, want the bucket pid %s",
				it.ReleaseGroupPID, kidA.EntityPID)
		}
	}
	// isMissing is the complement over the whole dimension plus the episode, since a
	// NULL release_group_id covers no album, no group, and not a track.
	if n := countWhere(t, st, "release_group_pid", query.OpIsMissing, nil); n != 3 {
		t.Errorf("release_group_pid isMissing = %d, want 3 (the group-less track, the book, the episode)", n)
	}
}

func TestFacetByYearAndKind(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", year: 1997})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", year: 1997})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3", title: "C", artist: "Z", album: "Cl"}) // no year

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupYear, "", 0, "")
	if err != nil {
		t.Fatalf("facet year: %v", err)
	}
	if b, ok := bucketByDisplay(res, "1997"); !ok || b.Count != 2 {
		t.Errorf("1997 bucket = %+v, want count 2", b)
	}
	if b, ok := bucketByDisplay(res, read.UnknownYear); !ok || b.Count != 1 {
		t.Errorf("Unknown-Year bucket = %+v, want count 1", b)
	}

	kindRes, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupKind, "", 0, "")
	if err != nil {
		t.Fatalf("facet kind: %v", err)
	}
	if b, ok := bucketByDisplay(kindRes, string(model.KindTrack)); !ok || b.Count != 3 {
		t.Errorf("track kind bucket = %+v, want count 3", b)
	}
}

func TestFacetHonorsFilter(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al", genre: "Rock", year: 2000})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "Y", album: "Bl", genre: "Rock", year: 1990})

	q := query.New(query.EntityItems).Where("year", query.OpGte, 2000).Build()
	res, err := st.Facet(ctx, q, read.GroupGenre, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	rock, ok := bucketByDisplay(res, "Rock")
	if !ok || rock.Count != 1 {
		t.Errorf("filtered Rock bucket = %+v, want count 1 (year>=2000)", rock)
	}
}

// playlistFacetFixture seeds the shape the owner-scoped facet has to get right: the
// default user's private "Mine" holds A twice plus B, bob owns a shared "BobShared"
// (B) and a private "BobPrivate" (C), and D sits in no playlist at all.
func playlistFacetFixture(t *testing.T) (st *Store, bob *model.User, mine, bobShared, bobPrivate model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID
	c := putTrack(t, st, lib.ID, trackSpec{path: "/lib/c.flac", essence: "ec", content: "cc", title: "C", artist: "X", album: "Al"}).ItemPID
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/d.flac", essence: "ed", content: "cd", title: "D", artist: "X", album: "Al"})

	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	mkList := func(name string, owner model.PID, vis model.PlaylistVisibility, items ...model.PID) model.PID {
		t.Helper()
		pid, err := st.CreatePlaylist(ctx, name, owner, model.PlaylistStatic, vis, nil)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := st.AddPlaylistItems(ctx, pid, items); err != nil {
			t.Fatalf("add to %s: %v", name, err)
		}
		return pid
	}
	// A held twice, so the COUNT(DISTINCT pi.id) contract has something to bite on.
	mine = mkList("Mine", "", model.VisibilityPrivate, a, a, b)
	bobShared = mkList("BobShared", bob.PID, model.VisibilityShared, b)
	bobPrivate = mkList("BobPrivate", bob.PID, model.VisibilityPrivate, c)
	return st, bob, mine, bobShared, bobPrivate
}

func TestFacetByPlaylist(t *testing.T) {
	st, _, mine, bobShared, _ := playlistFacetFixture(t)
	ctx := context.Background()

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupPlaylist, "", 0, "")
	if err != nil {
		t.Fatalf("facet playlist: %v", err)
	}
	// A sits at two positions and counts once: COUNT(DISTINCT pi.id), not entries.
	if b, ok := bucketByDisplay(res, "Mine"); !ok || b.Count != 2 || b.EntityPID != mine {
		t.Errorf("Mine bucket = %+v, want count 2 keyed by its pid (A is held twice)", b)
	}
	if b, ok := bucketByDisplay(res, "BobShared"); !ok || b.Count != 1 || b.EntityPID != bobShared {
		t.Errorf("BobShared bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, "BobPrivate"); ok {
		t.Errorf("BobPrivate bucket = %+v, want none (another user's private list)", b)
	}
	if len(res.Buckets) != 2 {
		t.Errorf("buckets = %+v, want just Mine and BobShared", res.Buckets)
	}
	for _, b := range res.Buckets {
		if b.IsUnknown {
			t.Errorf("unexpected unknown bucket %+v: the dimension has none, so D is absent "+
				"rather than bucketed", b)
		}
		if b.EntityPID == "" {
			t.Errorf("bucket %+v carries no EntityPID", b)
		}
	}
}

// TestFacetByPlaylistBucketSetVariesByUser is the property no other dimension has:
// the same query faceted by the same dimension returns a different bucket set per
// caller.
func TestFacetByPlaylistBucketSetVariesByUser(t *testing.T) {
	st, bob, _, bobShared, bobPrivate := playlistFacetFixture(t)
	ctx := context.Background()
	all := query.New(query.EntityItems).Build()

	res, err := st.Facet(ctx, all, read.GroupPlaylist, "", 0, bob.PID)
	if err != nil {
		t.Fatalf("facet as bob: %v", err)
	}
	if b, ok := bucketByDisplay(res, "BobPrivate"); !ok || b.Count != 1 || b.EntityPID != bobPrivate {
		t.Errorf("BobPrivate bucket as bob = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, "BobShared"); !ok || b.EntityPID != bobShared {
		t.Errorf("BobShared bucket as bob = %+v, want its own list", b)
	}
	if _, ok := bucketByDisplay(res, "Mine"); ok {
		t.Error("bob sees the default user's private Mine bucket")
	}

	if _, err := st.Facet(ctx, all, read.GroupPlaylist, "", 0, model.PID("no-such-user")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("facet for an unknown user = %v, want CodeNotFound", err)
	}
}

func TestFacetByPlaylistExcludesSmart(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"})
	rule := query.New(query.EntityItems).Where("artist", query.OpIs, "X").Build()
	if _, err := st.CreatePlaylist(ctx, "Smart", "", model.PlaylistSmart, "", &rule); err != nil {
		t.Fatalf("create smart: %v", err)
	}

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupPlaylist, "", 0, "")
	if err != nil {
		t.Fatalf("facet playlist: %v", err)
	}
	if len(res.Buckets) != 0 {
		t.Errorf("buckets = %+v, want none: a smart playlist stores no membership rows", res.Buckets)
	}
}

// TestFacetByPlaylistWithUserFilter is the arg-ordering assertion for the second
// dimension with joinArgs: the user id binds twice, once for the play_state join and
// once for the visibility clause, and the WHERE args follow. Any swap makes the
// visibility clause test a non-id and the facet comes back empty or wrong.
func TestFacetByPlaylistWithUserFilter(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	a := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "X", album: "Al"}).ItemPID
	b := putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "X", album: "Al"}).ItemPID

	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "Bobs", bob.PID, model.PlaylistStatic, model.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{a, b}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := st.SetStar(ctx, bob.PID, a, true, nil); err != nil {
		t.Fatalf("star: %v", err)
	}

	q := query.New(query.EntityItems).
		Where("starred", query.OpIs, 1).
		Where("kind", query.OpIs, "track").Build()
	res, err := st.Facet(ctx, q, read.GroupPlaylist, "", 0, bob.PID)
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	if bk, ok := bucketByDisplay(res, "Bobs"); !ok || bk.Count != 1 {
		t.Fatalf("Bobs bucket = %+v, want count 1 (only A is starred); arg order likely wrong", bk)
	}
	if len(res.Buckets) != 1 {
		t.Errorf("buckets = %+v, want just Bobs:1", res.Buckets)
	}
}

// TestFacetByPlaylistDrivesFromPlaylistItem pins the plan the dimension is cheap
// because of: the drive is playlist_item, not playable_item. The invariant holds
// without statistics, which is what production and every fixture run with, and it is
// what a flip to LEFT JOIN would break. SCAN fpl is deliberately not asserted: it
// appears only after ANALYZE.
func TestFacetByPlaylistDrivesFromPlaylistItem(t *testing.T) {
	st, _, _, _, _ := playlistFacetFixture(t)
	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	c, err := query.Compile(query.New(query.EntityItems).Build(), fm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	spec, ok := facetSpecFor(read.GroupPlaylist, 1)
	if !ok {
		t.Fatal("no playlist facet spec")
	}
	stmt := "SELECT " + spec.keyExpr + ", " + spec.display + ", COUNT(DISTINCT pi.id)" +
		itemJoins + spec.join + " WHERE 1=1 GROUP BY " + spec.groupBy
	args := append(append([]any{}, spec.joinArgs...), c.Args...)
	plan := explainPlan(t, st, stmt, args...)
	if !strings.Contains(plan, "SCAN fpli") {
		t.Errorf("the playlist facet no longer drives off playlist_item:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN pi") {
		t.Errorf("the playlist facet scans playable_item, which is the cost this dimension "+
			"exists to avoid (a LEFT JOIN would do this):\n%s", plan)
	}
}

// TestFacetSpecUserScopedMatchesFlag keeps read.GroupBy.UserScoped honest against the
// specs it describes. Without it a future user-scoped dimension that forgets the flag
// would silently bind userID = 0 and return an empty bucket set instead of an error,
// and the read package's own test cannot see the specs.
func TestFacetSpecUserScopedMatchesFlag(t *testing.T) {
	for _, g := range read.GroupBys() {
		zero, ok := facetSpecFor(g, 0)
		if !ok {
			t.Fatalf("no spec for %s", g)
		}
		one, _ := facetSpecFor(g, 1)
		differs := !reflect.DeepEqual(zero, one)
		if differs != g.UserScoped() {
			t.Errorf("%s: spec varies with the user id = %v, but UserScoped() = %v",
				g, differs, g.UserScoped())
		}
	}
}

func TestFacetRejectsBadGroupBy(t *testing.T) {
	st, _ := entityFixture(t)
	if _, err := st.Facet(context.Background(), query.New(query.EntityItems).Build(), read.GroupBy("bogus"), "", 0, ""); err == nil {
		t.Fatal("expected an error for an unsupported group-by")
	}
	if _, err := st.Facet(context.Background(), query.New(query.EntityItems).Build(), read.GroupGenre, "bogus", 0, ""); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("unsupported facet order = %v, want CodeInvalid", err)
	}
}

// orderLimitFixture catalogs genres of deliberately unequal size with two of them
// tied, so both facet orders have something to distinguish: counts 3, 2, 2, 1, with
// the tied pair adjacent in collation order.
func orderLimitFixture(t *testing.T) (*Store, *model.Library) {
	st, lib := entityFixture(t)
	genres := []string{"Rock", "Rock", "Rock", "Ambient", "Ambient", "Blues", "Blues", "Country"}
	for i, g := range genres {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: "X", album: "Al", genre: g,
		})
	}
	return st, lib
}

// TestFacetCountOrderAndLimit pins the top-N shelf: count order is descending by
// bucket size with ties left in collation order (so the result is deterministic, not
// whatever the grouping produced), and limit truncates that order without reshaping
// it. The tied pair is the load-bearing case; without the collation tiebreak the two
// could swap between runs.
func TestFacetCountOrderAndLimit(t *testing.T) {
	st, _ := orderLimitFixture(t)
	ctx := context.Background()
	all := query.New(query.EntityItems).Build()

	res, err := st.Facet(ctx, all, read.GroupGenre, read.FacetOrderCount, 0, "")
	if err != nil {
		t.Fatalf("count-order facet: %v", err)
	}
	// Rock 3, then the tied Ambient/Blues in collation order, then Country 1.
	want := []struct {
		display string
		count   int
	}{{"Rock", 3}, {"Ambient", 2}, {"Blues", 2}, {"Country", 1}}
	if len(res.Buckets) != len(want) {
		t.Fatalf("count-order buckets = %d, want %d", len(res.Buckets), len(want))
	}
	for i, w := range want {
		if res.Buckets[i].Display != w.display || res.Buckets[i].Count != w.count {
			t.Errorf("count-order bucket[%d] = %+v, want %s/%d", i, res.Buckets[i], w.display, w.count)
		}
	}

	// A limit truncates that same order rather than changing it.
	top, err := st.Facet(ctx, all, read.GroupGenre, read.FacetOrderCount, 2, "")
	if err != nil {
		t.Fatalf("limited count-order facet: %v", err)
	}
	if len(top.Buckets) != 2 {
		t.Fatalf("limit 2 returned %d buckets", len(top.Buckets))
	}
	for i := range top.Buckets {
		if top.Buckets[i] != res.Buckets[i] {
			t.Errorf("limited bucket[%d] = %+v, want the unlimited %+v", i, top.Buckets[i], res.Buckets[i])
		}
	}

	// A limit on the default label order truncates that order instead.
	labeled, err := st.Facet(ctx, all, read.GroupGenre, "", 0, "")
	if err != nil {
		t.Fatalf("label-order facet: %v", err)
	}
	shortLabeled, err := st.Facet(ctx, all, read.GroupGenre, read.FacetOrderLabel, 2, "")
	if err != nil {
		t.Fatalf("limited label-order facet: %v", err)
	}
	if len(shortLabeled.Buckets) != 2 {
		t.Fatalf("label-order limit 2 returned %d buckets", len(shortLabeled.Buckets))
	}
	for i := range shortLabeled.Buckets {
		if shortLabeled.Buckets[i] != labeled.Buckets[i] {
			t.Errorf("limited label bucket[%d] = %+v, want %+v", i, shortLabeled.Buckets[i], labeled.Buckets[i])
		}
	}
}

// TestFacetDefaultOrderUnchanged pins the compatibility half of the order/limit
// change on every dimension, custom-tag dimensions included: ("", 0) is collation
// order with the unknown bucket last, which is what every existing call site was
// already getting, and FacetOrderLabel is that same order spelled explicitly.
//
// The unknown-last assertion is what makes this test bite. Comparing the two orders
// alone would be tautological, since both take the same branch by construction; it
// is dropping the (sortExpr IS NULL) term that this has to catch, and that only
// shows up against a dimension with an unknown bucket to misplace.
func TestFacetDefaultOrderUnchanged(t *testing.T) {
	st, lib := orderLimitFixture(t)
	ctx := context.Background()
	all := query.New(query.EntityItems).Build()

	// A track with no artist, album, genre, or year lands in the unknown bucket of
	// every music dimension at once, and a remote episode has no file so it lands in
	// the library dimension's. A tagged item and a feed complete the sweep over the
	// spec shapes: music, kind-agnostic, episode-scoped, and custom-tag.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/bare.flac", essence: "ebare", content: "cbare", title: "Bare"})
	putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")
	items, err := st.QueryItems(ctx, all, "")
	if err != nil || len(items) == 0 {
		t.Fatalf("items = %d (err %v)", len(items), err)
	}
	if _, _, err := st.SetItemTag(ctx, items[0].PID, "MOOD", []string{"calm"}, model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set tag: %v", err)
	}
	// The playlist dimension needs a default-user playlist or its loop iteration
	// asserts nothing.
	pl, err := st.CreatePlaylist(ctx, "Mix", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{items[0].PID}); err != nil {
		t.Fatalf("add to playlist: %v", err)
	}

	withUnknown := 0
	dims := append(read.GroupBys(), read.GroupBy("tag.MOOD"))
	for _, g := range dims {
		base, err := st.Facet(ctx, all, g, "", 0, "")
		if err != nil {
			t.Fatalf("facet %s: %v", g, err)
		}
		explicit, err := st.Facet(ctx, all, g, read.FacetOrderLabel, 0, "")
		if err != nil {
			t.Fatalf("facet %s (explicit label): %v", g, err)
		}
		if !reflect.DeepEqual(base.Buckets, explicit.Buckets) {
			t.Errorf("%s: explicit label order differs from the default\n got %+v\nwant %+v",
				g, explicit.Buckets, base.Buckets)
		}
		for i, b := range base.Buckets {
			if !b.IsUnknown {
				continue
			}
			withUnknown++
			if i != len(base.Buckets)-1 {
				t.Errorf("%s: unknown bucket %q is at %d of %d, want last: %+v",
					g, b.Display, i, len(base.Buckets), base.Buckets)
			}
		}
	}
	// Guard the guard: if the fixture stops producing unknown buckets the loop above
	// asserts nothing and the (sortExpr IS NULL) term goes unpinned again.
	if withUnknown < 5 {
		t.Fatalf("fixture produced %d unknown buckets across %d dimensions, want the "+
			"five music dimensions plus library", withUnknown, len(dims))
	}
}

// TestFacetLabelOrderIsCollation pins the label order as the SORT KEY's order, not
// the display label's. The two are chosen to disagree: sort keys drop a leading
// article, so "The Aardvark" keys to "aardvark" and sorts before "Beta", while by
// title "Beta" comes first. Ordering by display ahead of sort_key, or dropping the
// sort_key term, flips the result.
func TestFacetLabelOrderIsCollation(t *testing.T) {
	st, lib := entityFixture(t)
	for i, album := range []string{"Beta", "The Aardvark"} {
		n := string(rune('a' + i))
		putTrack(t, st, lib.ID, trackSpec{path: "/lib/" + n + ".flac", essence: "e" + n, content: "c" + n,
			title: "T" + n, artist: "X", album: album})
	}
	res, err := st.Facet(context.Background(), query.New(query.EntityItems).Build(), read.GroupAlbum, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	var got []string
	for _, b := range res.Buckets {
		got = append(got, b.Display)
	}
	want := []string{"The Aardvark", "Beta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("album label order = %v, want %v (sort_key order: aardvark, beta)", got, want)
	}
}

func TestQueryPageKeysetCoversAllOnce(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	titles := []string{"Echo", "Alpha", "Delta", "Bravo", "Charlie"}
	for i, title := range titles {
		putTrack(t, st, lib.ID, trackSpec{
			path:    "/lib/" + title + ".flac",
			essence: "e" + title, content: "c" + title, title: title, artist: "X", album: "Al",
		})
		_ = i
	}

	seen := map[model.PID]int{}
	var order []string
	cursor := read.Cursor("")
	pages := 0
	for {
		page, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), cursor, 2, false, "")
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, it := range page.Items {
			seen[it.PID]++
			order = append(order, it.Title)
		}
		pages++
		if !page.HasMore {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != len(titles) {
		t.Fatalf("saw %d distinct items, want %d", len(seen), len(titles))
	}
	for pid, n := range seen {
		if n != 1 {
			t.Errorf("item %s returned %d times, want exactly 1", pid, n)
		}
	}
	want := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} // collation order
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("page order[%d] = %q, want %q (full order %v)", i, order[i], want[i], order)
		}
	}
}

func TestQueryPageDescending(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	var order []string
	cursor := read.Cursor("")
	for {
		page, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), cursor, 2, true, "") // desc
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, it := range page.Items {
			order = append(order, it.Title)
		}
		if !page.HasMore {
			break
		}
		cursor = page.Next
	}
	want := []string{"Charlie", "Bravo", "Alpha"} // reverse collation order, no dups
	if len(order) != len(want) {
		t.Fatalf("desc paging returned %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("desc order[%d] = %q, want %q (full %v)", i, order[i], want[i], order)
		}
	}
}

func TestQueryPageStableUnderConcurrentInsert(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	for _, title := range []string{"Alpha", "Charlie", "Echo"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	page1, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), "", 2, false, "")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Items) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %d items, hasMore=%v; want 2 + more", len(page1.Items), page1.HasMore)
	}

	// Insert rows that sort before (Bravo) and after (Delta) the cursor position.
	for _, title := range []string{"Bravo", "Delta"} {
		putTrack(t, st, lib.ID, trackSpec{
			path: "/lib/" + title + ".flac", essence: "e" + title, content: "c" + title,
			title: title, artist: "X", album: "Al",
		})
	}

	page2, err := st.QueryPage(ctx, query.New(query.EntityItems).Build(), page1.Next, 10, false, "")
	if err != nil {
		t.Fatalf("page2: %v", err)
	}

	seen := map[string]bool{}
	for _, it := range append(append([]*model.ItemView{}, page1.Items...), page2.Items...) {
		if seen[it.Title] {
			t.Errorf("title %q returned on more than one page (keyset must not duplicate)", it.Title)
		}
		seen[it.Title] = true
	}
	// Every item present when pagination started and sorting after the cursor
	// must still be returned (no skips): Alpha, Charlie from page1; Echo after.
	for _, title := range []string{"Alpha", "Charlie", "Echo"} {
		if !seen[title] {
			t.Errorf("item %q present at page-1 time was skipped across pages", title)
		}
	}
}

// TestFacetArgumentErrorsBeatUserLookup pins the error a caller gets when more than one
// argument is wrong. Resolving the user before validating the dimension and the order
// masked both with "no such user", sending the caller after the wrong argument.
func TestFacetArgumentErrorsBeatUserLookup(t *testing.T) {
	st, _, _, _, _ := playlistFacetFixture(t)
	ctx := context.Background()
	all := query.New(query.EntityItems).Build()
	bogus := model.PID("no-such-user")

	if _, err := st.Facet(ctx, all, read.GroupBy("bogus"), "", 0, bogus); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad dimension + bad user = %v, want CodeInvalid naming the dimension", err)
	}
	if _, err := st.Facet(ctx, all, read.GroupPlaylist, "bogus-order", 0, bogus); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad order + bad user = %v, want CodeInvalid naming the order", err)
	}
	// With every other argument sound, the user error is the one that surfaces.
	if _, err := st.Facet(ctx, all, read.GroupPlaylist, "", 0, bogus); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("bad user alone = %v, want CodeNotFound", err)
	}
}
