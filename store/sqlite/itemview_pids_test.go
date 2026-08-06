package sqlite

import (
	"context"
	"sort"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

// TestItemViewEntityPIDs pins the entity-handle columns the item view projects: a
// track carries its artist, album-artist, and album entity pids; a book resolves
// its author for the two artist pids and has no album; an episode, which has no
// track or book row, carries none of those three and carries its show's instead.
func TestItemViewEntityPIDs(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A track projects all three handles, each resolving the right entity: the
	// track artist, the (distinct) album artist, and the album.
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Track Artist", albumArt: "Album Artist", album: "OK Computer",
		year: 1997, durationMS: 100,
	})
	trackView, err := st.ItemByPID(ctx, tr.ItemPID)
	if err != nil {
		t.Fatalf("track view: %v", err)
	}
	trackArtistPID := entityPIDByName(t, st, "artist", "name", "Track Artist")
	albumArtistPID := entityPIDByName(t, st, "artist", "name", "Album Artist")
	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")
	if trackView.ArtistPID != trackArtistPID {
		t.Errorf("track ArtistPID = %s, want %s", trackView.ArtistPID, trackArtistPID)
	}
	if trackView.AlbumArtistPID != albumArtistPID {
		t.Errorf("track AlbumArtistPID = %s, want %s", trackView.AlbumArtistPID, albumArtistPID)
	}
	if trackView.AlbumPID != albumPID {
		t.Errorf("track AlbumPID = %s, want %s", trackView.AlbumPID, albumPID)
	}

	// A book resolves its author for both artist handles (the facet membership
	// rule) and has no album pid: a book is not an album member.
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 3000,
	})
	bookView, err := st.ItemByPID(ctx, bk.ItemPID)
	if err != nil {
		t.Fatalf("book view: %v", err)
	}
	authorPID := entityPIDByName(t, st, "artist", "name", "J.R.R. Tolkien")
	if bookView.ArtistPID != authorPID {
		t.Errorf("book ArtistPID = %s, want the author %s", bookView.ArtistPID, authorPID)
	}
	if bookView.AlbumArtistPID != authorPID {
		t.Errorf("book AlbumArtistPID = %s, want the author %s", bookView.AlbumArtistPID, authorPID)
	}
	if bookView.AlbumPID != "" {
		t.Errorf("book AlbumPID = %q, want empty (a book is not an album member)", bookView.AlbumPID)
	}

	// An episode has neither a track nor a book row, so all three handles are empty.
	feedURL := "http://feed.example/f"
	feed := model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: identity.PodcastKey("", feedURL),
		Feed: model.Feed{Title: "My Show", Author: "Host", Episodes: []model.FeedEpisode{{
			GUID: "g1", Title: "Ep1", EnclosureURL: feedURL + "/e.mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: 1_000_000_000,
		}}},
		FetchedAtNS: 1,
	}
	if _, err := st.UpsertFeed(ctx, feed); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	var epPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'").Scan(&epPID); err != nil {
		t.Fatalf("episode pid: %v", err)
	}
	epView, err := st.ItemByPID(ctx, epPID)
	if err != nil {
		t.Fatalf("episode view: %v", err)
	}
	if epView.ArtistPID != "" || epView.AlbumArtistPID != "" || epView.AlbumPID != "" {
		t.Errorf("episode pids = %q/%q/%q, want all empty",
			epView.ArtistPID, epView.AlbumArtistPID, epView.AlbumPID)
	}
	// The one handle an episode does carry, and the one the other kinds do not.
	podcastPID := entityPIDByName(t, st, "podcast", "title", "My Show")
	if epView.PodcastPID != podcastPID {
		t.Errorf("episode PodcastPID = %s, want %s", epView.PodcastPID, podcastPID)
	}
	if trackView.PodcastPID != "" || bookView.PodcastPID != "" {
		t.Errorf("track/book PodcastPID = %q/%q, want empty",
			trackView.PodcastPID, bookView.PodcastPID)
	}
}

// TestItemViewReleaseGroupPID pins the fourth entity handle and its three empty
// states, which are not one state: a book and an episode have no track row at all,
// while a track whose album carries no release group has an album and still no group
// above it. AlbumPID is re-asserted here because it is now produced by a joined
// column rather than a correlated seek, and the value must not have moved.
func TestItemViewReleaseGroupPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	grouped := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Everything In Its Right Place", artist: "Radiohead", albumArt: "Radiohead",
		album: "Kid A", year: 2000,
	})
	orphan := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/orphan/01.flac", essence: "e2", content: "c2",
		title: "Loose", artist: "Nobody", albumArt: "Nobody", album: "Orphan",
	})
	detachReleaseGroup(t, st, "Orphan")

	groupedView, err := st.ItemByPID(ctx, grouped.ItemPID)
	if err != nil {
		t.Fatalf("grouped track view: %v", err)
	}
	rgPID := entityPIDByName(t, st, "release_group", "title", "Kid A")
	albumPID := entityPIDByName(t, st, "album", "title", "Kid A")
	if groupedView.ReleaseGroupPID != rgPID {
		t.Errorf("track ReleaseGroupPID = %s, want %s", groupedView.ReleaseGroupPID, rgPID)
	}
	if groupedView.AlbumPID != albumPID {
		t.Errorf("track AlbumPID = %s, want %s (the joined column must read what the seek did)",
			groupedView.AlbumPID, albumPID)
	}

	orphanView, err := st.ItemByPID(ctx, orphan.ItemPID)
	if err != nil {
		t.Fatalf("orphan track view: %v", err)
	}
	if orphanView.AlbumPID == "" {
		t.Error("orphan track AlbumPID is empty; it does have an album, only no release group")
	}
	if orphanView.ReleaseGroupPID != "" {
		t.Errorf("orphan track ReleaseGroupPID = %q, want empty", orphanView.ReleaseGroupPID)
	}

	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", durationMS: 3000,
	})
	bookView, err := st.ItemByPID(ctx, bk.ItemPID)
	if err != nil {
		t.Fatalf("book view: %v", err)
	}
	if bookView.ReleaseGroupPID != "" {
		t.Errorf("book ReleaseGroupPID = %q, want empty", bookView.ReleaseGroupPID)
	}

	putFeed(t, st, "http://cast.example/f", "Ep1")
	var epPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'").Scan(&epPID); err != nil {
		t.Fatalf("episode pid: %v", err)
	}
	epView, err := st.ItemByPID(ctx, epPID)
	if err != nil {
		t.Fatalf("episode view: %v", err)
	}
	if epView.ReleaseGroupPID != "" {
		t.Errorf("episode ReleaseGroupPID = %q, want empty", epView.ReleaseGroupPID)
	}
}

// TestItemViewExplicit pins the projected advisory flags against the fields that
// filter on them: a row's Explicit and an `explicit is 1` match must agree for every
// kind, which is the contract the field map carries. A track and a book read false on
// both because WaxBin stores no music advisory flag. An episode of a channel-marked
// show reads Explicit false and PodcastExplicit true, which is the inheritance
// question AdvisoryFlagged answers in one read.
func TestItemViewExplicit(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "Track", artist: "X", album: "Al"})
	bk := putBook(t, st, lib.ID, bookSpec{path: "/lib/b.m4b", essence: "be1", content: "bc1",
		title: "Book", author: "A. Author"})
	// A channel-explicit show with one item-marked episode and one unmarked.
	putFeedAdvisory(t, st, "http://cast.example/f", true, map[string]bool{"Marked": true}, "Marked", "Unmarked")

	// The set of items the explicit field matches, by pid.
	matched, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("explicit", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatalf("explicit query: %v", err)
	}
	byField := make(map[model.PID]bool, len(matched))
	for _, v := range matched {
		byField[v.PID] = true
	}

	pids := []model.PID{tr.ItemPID, bk.ItemPID}
	rows, err := st.read.QueryContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'")
	if err != nil {
		t.Fatalf("episode pids: %v", err)
	}
	for rows.Next() {
		var pid model.PID
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("scan episode pid: %v", err)
		}
		pids = append(pids, pid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("episode pid rows: %v", err)
	}

	var explicitTitles, showExplicitTitles, flaggedTitles []string
	for _, pid := range pids {
		v, err := st.ItemByPID(ctx, pid)
		if err != nil {
			t.Fatalf("item %s: %v", pid, err)
		}
		if v.Explicit != byField[pid] {
			t.Errorf("%s (%s) Explicit = %v, but the explicit field match = %v",
				v.Title, v.Kind, v.Explicit, byField[pid])
		}
		if v.Explicit {
			explicitTitles = append(explicitTitles, v.Title)
		}
		if v.PodcastExplicit {
			showExplicitTitles = append(showExplicitTitles, v.Title)
		}
		if v.AdvisoryFlagged() {
			flaggedTitles = append(flaggedTitles, v.Title)
		}
	}
	if !equalStrings(explicitTitles, []string{"Marked"}) {
		t.Errorf("projected Explicit = %v, want just [Marked]", explicitTitles)
	}
	// The show flag rides on both of its episodes, including the one carrying no item
	// flag of its own, which is exactly what an Explicit-only check would miss. The
	// episode pids are collected unordered, so compare sorted.
	sort.Strings(showExplicitTitles)
	sort.Strings(flaggedTitles)
	if !equalStrings(showExplicitTitles, []string{"Marked", "Unmarked"}) {
		t.Errorf("projected PodcastExplicit = %v, want both episodes", showExplicitTitles)
	}
	if !equalStrings(flaggedTitles, []string{"Marked", "Unmarked"}) {
		t.Errorf("AdvisoryFlagged = %v, want both episodes", flaggedTitles)
	}
}

// TestItemViewIdentifiers pins the external-identifier columns across the three paths
// that splice the shared column list differently. The book arm is the one that is not
// a straight copy: with no album entity, its AlbumMBID reads its own book.mbid rather
// than staying empty the way AlbumPID does.
func TestItemViewIdentifiers(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	const (
		recordingMBID   = "rec-1111"
		releaseMBID     = "rel-2222"
		rgMBID          = "rg-3333"
		artistMBID      = "art-4444"
		albumArtistMBID = "art-5555"
		trackISRC       = "USRC17607839"
		bookMBID        = "rel-6666"
	)
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Track Artist", albumArt: "Album Artist", album: "OK Computer",
		year: 1997, durationMS: 100,
		mbRecording: recordingMBID, mbRelease: releaseMBID, mbReleaseGroup: rgMBID,
		mbArtists: []string{artistMBID}, mbAlbumArtists: []string{albumArtistMBID}, isrc: trackISRC,
	})
	bk := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/book.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", mbid: bookMBID, durationMS: 3000,
	})

	wantTrack := model.ItemView{
		MBID: recordingMBID, ISRC: trackISRC, AlbumMBID: releaseMBID,
		ReleaseGroupMBID: rgMBID, ArtistMBID: artistMBID, AlbumArtistMBID: albumArtistMBID,
	}
	wantBook := model.ItemView{
		MBID: bookMBID, AlbumMBID: bookMBID,
	}
	check := func(path string, got *model.ItemView, want model.ItemView) {
		t.Helper()
		if got.MBID != want.MBID || got.ISRC != want.ISRC || got.AlbumMBID != want.AlbumMBID ||
			got.ReleaseGroupMBID != want.ReleaseGroupMBID || got.ArtistMBID != want.ArtistMBID ||
			got.AlbumArtistMBID != want.AlbumArtistMBID {
			t.Errorf("%s %s identifiers = mbid:%q isrc:%q album:%q rg:%q artist:%q albumArtist:%q, "+
				"want mbid:%q isrc:%q album:%q rg:%q artist:%q albumArtist:%q",
				path, got.Title, got.MBID, got.ISRC, got.AlbumMBID, got.ReleaseGroupMBID,
				got.ArtistMBID, got.AlbumArtistMBID,
				want.MBID, want.ISRC, want.AlbumMBID, want.ReleaseGroupMBID,
				want.ArtistMBID, want.AlbumArtistMBID)
		}
	}

	trackView, err := st.ItemByPID(ctx, tr.ItemPID)
	if err != nil {
		t.Fatalf("track view: %v", err)
	}
	check("ItemByPID", trackView, wantTrack)
	bookView, err := st.ItemByPID(ctx, bk.ItemPID)
	if err != nil {
		t.Fatalf("book view: %v", err)
	}
	check("ItemByPID", bookView, wantBook)

	if trackView.ArtistMBID == trackView.AlbumArtistMBID {
		t.Error("track artist and album-artist MBIDs are equal; the fixture credits distinct artists")
	}
	if bookView.ArtistMBID != bookView.AlbumArtistMBID {
		t.Errorf("book artist/album-artist MBIDs = %q/%q, want both to resolve the one author",
			bookView.ArtistMBID, bookView.AlbumArtistMBID)
	}

	byPID := func(items []*model.ItemView) map[model.PID]*model.ItemView {
		m := make(map[model.PID]*model.ItemView, len(items))
		for _, v := range items {
			m[v.PID] = v
		}
		return m
	}

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	q := byPID(items)
	if len(q) != 2 {
		t.Fatalf("query returned %d items, want 2", len(q))
	}
	check("QueryItems", q[tr.ItemPID], wantTrack)
	check("QueryItems", q[bk.ItemPID], wantBook)

	page, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Limit: 10})
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	b := byPID(page.Items)
	if len(b) != 2 {
		t.Fatalf("browse returned %d items, want 2", len(b))
	}
	check("BrowsePage", b[tr.ItemPID], wantTrack)
	check("BrowsePage", b[bk.ItemPID], wantBook)
}

// TestItemViewIdentifiersUnderMegabytesBudget covers the one reader that extends the
// column list and the dest list at two independent sites, so an off-by-one between
// them shifts the identifiers rather than failing the scan. The values are asserted,
// not just the query: an all-text shift still scans cleanly.
func TestItemViewIdentifiersUnderMegabytesBudget(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	tr := putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/al/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Track Artist", albumArt: "Album Artist", album: "OK Computer",
		durationMS:  100,
		mbRecording: "rec-1111", mbRelease: "rel-2222", mbReleaseGroup: "rg-3333",
		mbArtists: []string{"art-4444"}, mbAlbumArtists: []string{"art-5555"}, isrc: "USRC17607839",
	})
	setFileSize(t, st, "/lib/al/01.flac", 100)

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(1).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil {
		t.Fatalf("megabytes query: %v", err)
	}
	if len(items) != 1 || items[0].PID != tr.ItemPID {
		t.Fatalf("megabytes budget returned %d items, want the one track", len(items))
	}
	v := items[0]
	if v.MBID != "rec-1111" || v.ISRC != "USRC17607839" || v.AlbumMBID != "rel-2222" ||
		v.ReleaseGroupMBID != "rg-3333" || v.ArtistMBID != "art-4444" || v.AlbumArtistMBID != "art-5555" {
		t.Errorf("identifiers under the megabytes budget = %q/%q/%q/%q/%q/%q; a shifted "+
			"column list would land the wrong value in each",
			v.MBID, v.ISRC, v.AlbumMBID, v.ReleaseGroupMBID, v.ArtistMBID, v.AlbumArtistMBID)
	}
	if v.DurationMS == 0 {
		t.Error("budget row lost its duration; the widened SELECT and its dests are out of step")
	}
}

// TestItemViewLibraryPID reads the projected library pid back through all four shapes
// that splice itemViewCols, because the megabytes budget extends the column list and
// the dest list at two independent sites and a mis-ordered column shows up only there.
func TestItemViewLibraryPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib2: %v", err)
	}
	one := putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "A", artist: "X", album: "Al", durationMS: 100})
	two := putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/2.flac", essence: "e2", content: "c2",
		title: "B", artist: "X", album: "Al", durationMS: 100})
	setFileSize(t, st, "/lib/1.flac", 100)
	setFileSize(t, st, "/lib2/2.flac", 100)
	putFeed(t, st, "http://cast.example/f", "Ep1")
	var epPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE kind='episode'").Scan(&epPID); err != nil {
		t.Fatalf("episode pid: %v", err)
	}

	want := map[model.PID]model.PID{one.ItemPID: lib.PID, two.ItemPID: lib2.PID, epPID: ""}
	if lib.PID == lib2.PID {
		t.Fatal("the two roots share a pid; the fixture cannot distinguish them")
	}
	check := func(shape string, got map[model.PID]*model.ItemView) {
		t.Helper()
		for pid, wantLib := range want {
			v, ok := got[pid]
			if !ok {
				t.Errorf("%s did not return item %s", shape, pid)
				continue
			}
			if v.LibraryPID != wantLib {
				t.Errorf("%s %s LibraryPID = %q, want %q", shape, v.Title, v.LibraryPID, wantLib)
			}
		}
	}
	byPID := func(items []*model.ItemView) map[model.PID]*model.ItemView {
		m := make(map[model.PID]*model.ItemView, len(items))
		for _, v := range items {
			m[v.PID] = v
		}
		return m
	}

	single := map[model.PID]*model.ItemView{}
	for pid := range want {
		v, err := st.ItemByPID(ctx, pid)
		if err != nil {
			t.Fatalf("item %s: %v", pid, err)
		}
		single[pid] = v
	}
	check("ItemByPID", single)

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	check("QueryItems", byPID(items))

	page, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Limit: 10})
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	check("BrowsePage", byPID(page.Items))

	budgeted, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Limit(1).LimitBy(query.LimitMegabytes).Build(), "")
	if err != nil {
		t.Fatalf("megabytes query: %v", err)
	}
	if len(budgeted) == 0 {
		t.Fatal("megabytes budget returned nothing")
	}
	for _, v := range budgeted {
		if v.LibraryPID != want[v.PID] {
			t.Errorf("megabytes budget %s LibraryPID = %q, want %q; a shifted column list "+
				"lands the wrong value here alone", v.Title, v.LibraryPID, want[v.PID])
		}
		if v.DurationMS == 0 {
			t.Error("budget row lost its duration; the widened SELECT and its dests are out of step")
		}
	}
}

// TestItemViewLibraryPIDMatchesTheLibraryFilter pins projection against filter. Chained
// with TestLibraryFieldAndFacet's mirror loop this gives projection equals filter equals
// facet, which is the guarantee a consumer holding an item view relies on.
func TestItemViewLibraryPIDMatchesTheLibraryFilter(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib2"), DisplayRoot: "/lib2", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib2: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1", title: "A", artist: "X", album: "Al"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2", title: "B", artist: "X", album: "Al"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/lib2/3.flac", essence: "e3", content: "c3", title: "C", artist: "X", album: "Al"})
	putFeed(t, st, "http://cast.example/f", "Ep1")

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	projected := map[model.PID]int{}
	for _, v := range items {
		projected[v.LibraryPID]++
	}
	for _, l := range []*model.Library{lib, lib2} {
		if n := countWhere(t, st, "library", query.OpIs, string(l.PID)); n != projected[l.PID] {
			t.Errorf("library %s: filter selects %d, projection reports %d", l.DisplayRoot, n, projected[l.PID])
		}
	}
	if n := countWhere(t, st, "library", query.OpIsMissing, nil); n != projected[""] {
		t.Errorf("library isMissing selects %d, but %d items project an empty LibraryPID", n, projected[""])
	}
	if projected[""] == 0 {
		t.Error("no item projects an empty LibraryPID; the fileless case is untested")
	}
}

// TestIdentifierQueryFields covers the filters behind a "which items have no
// MusicBrainz id" sweep. isMissing is the whole point: the columns are nullable and
// the fields COALESCE to ”, so it has to match both an absent row and an empty
// string. mbid spans track and book, which is why an untagged book counts.
func TestIdentifierQueryFields(t *testing.T) {
	st, lib := entityFixture(t)

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/1.flac", essence: "e1", content: "c1", title: "Tagged",
		artist: "A", albumArt: "A", album: "Al",
		mbRecording: "rec-1", mbRelease: "rel-1", isrc: "USRC17607839",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/2.flac", essence: "e2", content: "c2", title: "Bare",
		artist: "B", albumArt: "B", album: "Bl",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b1.m4b", essence: "be1", content: "bc1",
		title: "Tagged Book", author: "Auth One", mbid: "rel-book",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b2.m4b", essence: "be2", content: "bc2",
		title: "Bare Book", author: "Auth Two",
	})

	for _, tc := range []struct {
		field string
		op    query.Op
		value any
		want  int
		why   string
	}{
		{"mbid", query.OpIs, "rec-1", 1, "the tagged track by its recording id"},
		{"mbid", query.OpIs, "rel-book", 1, "the tagged book by its release id"},
		{"mbid", query.OpIsPresent, nil, 2, "one tagged track and one tagged book"},
		{"mbid", query.OpIsMissing, nil, 2, "the bare track and the bare book"},
		{"isrc", query.OpIs, "USRC17607839", 1, "the one track carrying an ISRC"},
		{"isrc", query.OpIsMissing, nil, 3, "everything else, books included"},
		{"album_mbid", query.OpIs, "rel-1", 1, "the track whose album carries the release id"},
		{"album_mbid", query.OpIs, "rel-book", 1, "the book, which reports its own id here"},
		{"album_mbid", query.OpIsMissing, nil, 2, "the bare track and the bare book"},
	} {
		if n := countWhere(t, st, tc.field, tc.op, tc.value); n != tc.want {
			t.Errorf("%s %s %v = %d, want %d (%s)", tc.field, tc.op, tc.value, n, tc.want, tc.why)
		}
	}
}

// TestAlbumEntityQueryFields covers the album entity's release columns. They compare raw
// text, which is the trap worth pinning: a scan stores the tag and an edit normalizes, so
// "USA" and "US" differ here even though the enrichment matcher folds them together.
func TestAlbumEntityQueryFields(t *testing.T) {
	st, lib := entityFixture(t)

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/1.flac", essence: "e1", content: "c1", title: "Tagged",
		artist: "A", albumArt: "A", album: "Al",
		barcode: "0075992739429", label: "Harvest", catNo: "SHVL 804",
		media: `12" Vinyl`, country: "USA",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/2.flac", essence: "e2", content: "c2", title: "Bare",
		artist: "B", albumArt: "B", album: "Bl",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b1.m4b", essence: "be1", content: "bc1", title: "Book", author: "Auth",
	})

	for _, tc := range []struct {
		field string
		op    query.Op
		value any
		want  int
		why   string
	}{
		{"album_barcode", query.OpIs, "0075992739429", 1, "the tagged track's album"},
		{"album_label", query.OpIs, "Harvest", 1, "the tagged track's album"},
		{"album_catalog_number", query.OpIs, "SHVL 804", 1, "the tagged track's album"},
		{"album_media", query.OpIs, `12" Vinyl`, 1, "the medium as tagged"},
		{"album_country", query.OpIs, "USA", 1, "the country as tagged, unnormalized"},
		{"album_country", query.OpIs, "US", 0, "the field compares raw text; an edit would have folded this"},
		{"album_media", query.OpContains, "Vinyl", 1, "contains is what an untidily-tagged catalog wants"},
		{"album_media", query.OpIsMissing, nil, 2, "the bare track and the book"},
		{"album_country", query.OpIsMissing, nil, 2, "the bare track and the book"},
		{"album_media", query.OpIsPresent, nil, 1, "only the tagged track"},
	} {
		if n := countWhere(t, st, tc.field, tc.op, tc.value); n != tc.want {
			t.Errorf("%s %s %v = %d, want %d (%s)", tc.field, tc.op, tc.value, n, tc.want, tc.why)
		}
	}
}

// TestReleaseGroupMBIDField: the query surface must be able to express what audit's
// missing_mbid walks. An enriched-but-untagged library carries only a release-group
// id, so mbid/album_mbid alone would report every track as missing while the audit
// reports none.
func TestReleaseGroupMBIDField(t *testing.T) {
	st, lib := entityFixture(t)

	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/1.flac", essence: "e1", content: "c1", title: "Grouped",
		artist: "A", albumArt: "A", album: "Al1", mbReleaseGroup: "rg-1"})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/2.flac", essence: "e2", content: "c2", title: "Bare",
		artist: "B", albumArt: "B", album: "Al2"})
	// An episode carries no MusicBrainz identity and never will, so the audit excludes
	// it by kind. Without it in the fixture the equivalence below passes for the wrong
	// reason: the query chain has to spell that exclusion out.
	putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")

	if n := countWhere(t, st, "release_group_mbid", query.OpIs, "rg-1"); n != 1 {
		t.Errorf("release_group_mbid is rg-1 = %d, want 1", n)
	}
	// The bare track plus both episodes: an episode has no release group either, which
	// is why the audit scopes by kind rather than leaning on this field alone.
	if n := countWhere(t, st, "release_group_mbid", query.OpIsMissing, nil); n != 3 {
		t.Errorf("release_group_mbid isMissing = %d, want 3 (the bare track and two episodes)", n)
	}

	// The full chain the audit walks is now expressible, and agrees with it. The kind
	// filter is part of the equivalence, not incidental: itemsMissingMBIDWhere is scoped
	// to tracks and books, so a query without it also counts every episode.
	uncovered, err := st.CountItems(context.Background(), query.New(query.EntityItems).
		WhereValues("kind", query.OpIn, "track", "book").
		WherePresence("mbid", query.OpIsMissing).
		WherePresence("album_mbid", query.OpIsMissing).
		WherePresence("release_group_mbid", query.OpIsMissing).Build(), "")
	if err != nil {
		t.Fatalf("chain query: %v", err)
	}
	_, total, err := st.ItemsMissingMBID(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if uncovered != total {
		t.Errorf("query chain counts %d uncovered, audit counts %d; they must agree", uncovered, total)
	}
}
