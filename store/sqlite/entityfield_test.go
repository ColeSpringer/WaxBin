package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// scalarStr reads one string cell, for white-box entity-pid lookups.
func scalarStr(t *testing.T, st *Store, q string, args ...any) string {
	t.Helper()
	var s string
	if err := st.read.QueryRowContext(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

// putFeed upserts a podcast feed with remote (undownloaded) episodes.
func putFeed(t *testing.T, st *Store, feedURL string, titles ...string) *model.UpsertFeedResult {
	t.Helper()
	eps := make([]model.FeedEpisode, len(titles))
	for i, tt := range titles {
		eps[i] = model.FeedEpisode{
			GUID: "guid-" + tt, Title: tt, EnclosureURL: feedURL + "/" + tt + ".mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: int64(i+1) * 1_000_000_000,
		}
	}
	res, err := st.UpsertFeed(context.Background(), model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: identity.PodcastKey("", feedURL),
		Feed:        model.Feed{Title: "Cast", Author: "Host", Episodes: eps},
		FetchedAtNS: 1,
	})
	if err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	return res
}

// countWhere counts items matching one condition over the items entity.
func countWhere(t *testing.T, st *Store, field string, op query.Op, value any) int {
	t.Helper()
	b := query.New(query.EntityItems)
	if op == query.OpIsPresent || op == query.OpIsMissing {
		b.WherePresence(field, op)
	} else {
		b.Where(field, op, value)
	}
	n, err := st.CountItems(context.Background(), b.Build(), "")
	if err != nil {
		t.Fatalf("count %s %s: %v", field, op, err)
	}
	return n
}

func TestEntityPIDFieldsMirrorFacets(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/r1.flac", essence: "e1", content: "c1",
		title: "One", artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer", genre: "Rock; Electronic"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/r2.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Radiohead", albumArt: "Radiohead", album: "OK Computer", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/o1.flac", essence: "e3", content: "c3",
		title: "Three", artist: "Orbital", albumArt: "Orbital", album: "Insides", genre: "Electronic"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/earthsea.m4b", essence: "be1", content: "bc1",
		title: "A Wizard of Earthsea", author: "Ursula K. Le Guin", genres: []string{"Fantasy"}})

	// The mirror guarantee: for every entity-keyed facet bucket, filtering by the
	// matching pid field returns exactly the bucket's count. The facet specs and
	// the pid fields share their entity expressions, which is what keeps the two
	// sides in agreement.
	for _, mirror := range []struct {
		group read.GroupBy
		field string
	}{
		{read.GroupArtist, "artist_pid"},
		{read.GroupAlbumArtist, "album_artist_pid"},
		{read.GroupAlbum, "album_pid"},
		{read.GroupGenre, "genre_pid"},
	} {
		res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), mirror.group, "")
		if err != nil {
			t.Fatalf("facet %s: %v", mirror.group, err)
		}
		for _, b := range res.Buckets {
			if b.EntityPID == "" {
				continue
			}
			if n := countWhere(t, st, mirror.field, query.OpIs, string(b.EntityPID)); n != b.Count {
				t.Errorf("%s bucket %q count %d != %s filter count %d",
					mirror.group, b.Display, b.Count, mirror.field, n)
			}
		}
	}

	// A book matches by its author, the same entity the artist facet groups it
	// under.
	authorPID := scalarStr(t, st, "SELECT pid FROM artist WHERE name = ?", "Ursula K. Le Guin")
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("artist_pid", query.OpIs, authorPID).Build(), "")
	if err != nil {
		t.Fatalf("query by author pid: %v", err)
	}
	if len(items) != 1 || items[0].Title != "A Wizard of Earthsea" {
		t.Errorf("artist_pid over a book author = %v, want the book", titlesOf(items))
	}

	// album_pid selects one album's tracks and reads NULL for other kinds, so
	// isMissing works there.
	albumPID := scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "OK Computer")
	if n := countWhere(t, st, "album_pid", query.OpIs, albumPID); n != 2 {
		t.Errorf("album_pid filter = %d, want 2", n)
	}
	// The book and nothing else lacks an album here (books never carry album_pid).
	if n := countWhere(t, st, "album_pid", query.OpIsMissing, nil); n != 1 {
		t.Errorf("album_pid isMissing = %d, want 1 (the book)", n)
	}
}

func TestGenrePIDSetSemantics(t *testing.T) {
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "A", artist: "X", album: "Al", genre: "Rock; Pop"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "B", artist: "X", album: "Al", genre: "Rock"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/3.flac", essence: "e3", content: "c3",
		title: "C", artist: "X", album: "Al"})

	rockPID := scalarStr(t, st, "SELECT pid FROM genre WHERE name = ?", "Rock")
	if n := countWhere(t, st, "genre_pid", query.OpIs, rockPID); n != 2 {
		t.Errorf("genre_pid is Rock = %d, want 2", n)
	}
	// isNot is the deny-list: an item with no genre at all matches, while an item
	// carrying Rock (even among other genres) does not.
	if n := countWhere(t, st, "genre_pid", query.OpIsNot, rockPID); n != 1 {
		t.Errorf("genre_pid isNot Rock = %d, want 1 (the genreless track)", n)
	}
	if n := countWhere(t, st, "genre_pid", query.OpIsPresent, nil); n != 2 {
		t.Errorf("genre_pid isPresent = %d, want 2", n)
	}
	if n := countWhere(t, st, "genre_pid", query.OpIsMissing, nil); n != 1 {
		t.Errorf("genre_pid isMissing = %d, want 1", n)
	}
	// Ordered operators are rejected on a set field.
	_, err := st.QueryItems(context.Background(), query.New(query.EntityItems).
		Where("genre_pid", query.OpGt, rockPID).Build(), "")
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("genre_pid gt: want CodeInvalid, got %v", err)
	}
}

func TestLibraryFieldAndFacet(t *testing.T) {
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
	// One remote (fileless) episode: no primary file, so no library.
	putFeed(t, st, "http://cast.example/f", "Ep1")

	if n := countWhere(t, st, "library", query.OpIs, string(lib.PID)); n != 2 {
		t.Errorf("library is lib1 = %d, want 2", n)
	}
	if n := countWhere(t, st, "library", query.OpIs, string(lib2.PID)); n != 1 {
		t.Errorf("library is lib2 = %d, want 1", n)
	}
	// A fileless item has a NULL library, so isMissing matches the undownloaded.
	if n := countWhere(t, st, "library", query.OpIsMissing, nil); n != 1 {
		t.Errorf("library isMissing = %d, want 1 (the remote episode)", n)
	}

	res, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupLibrary, "")
	if err != nil {
		t.Fatalf("facet library: %v", err)
	}
	if b, ok := bucketByDisplay(res, "/lib"); !ok || b.Count != 2 || b.EntityPID != lib.PID {
		t.Errorf("/lib bucket = %+v, want count 2 keyed by the library pid", b)
	}
	if b, ok := bucketByDisplay(res, "/lib2"); !ok || b.Count != 1 || b.EntityPID != lib2.PID {
		t.Errorf("/lib2 bucket = %+v, want count 1", b)
	}
	if b, ok := bucketByDisplay(res, read.NoFile); !ok || b.Count != 1 || !b.IsUnknown {
		t.Errorf("no-file bucket = %+v, want count 1 + unknown (the episode)", b)
	}
	// Mirror guarantee for the library dimension.
	for _, b := range res.Buckets {
		if b.EntityPID == "" {
			continue
		}
		if n := countWhere(t, st, "library", query.OpIs, string(b.EntityPID)); n != b.Count {
			t.Errorf("library bucket %q count %d != filter count %d", b.Display, b.Count, n)
		}
	}
}

func TestHasArtAcrossKinds(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Track with its own cover; track without.
	covered := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 32, 32))
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/al/2.flac", essence: "e2", content: "c2",
		title: "Bare", artist: "X", album: "Al"})

	// Book with a cover (item art lives under the 'track' slot for books too).
	if _, err := st.PutScannedBook(ctx, model.PutScannedBookInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/b.m4b"), DisplayPath: "/lib/b.m4b", RelPath: []byte("b.m4b"),
			Kind: model.FileAudio, Size: 1, MTimeNS: 1, ContentHash: "bc", EssenceHash: "be",
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: "Covered Book",
			SortKey: model.SortKey("Covered Book"), IdentityKey: "essence:be",
		},
		Book:     model.Book{Author: "A. Author", AuthorSort: model.SortKey("A. Author"), Authors: []string{"A. Author"}},
		CoverArt: testPNG(t, 24, 24),
	}); err != nil {
		t.Fatalf("put covered book: %v", err)
	}

	// Two remote episodes; downloading one attaches its feed cover under the
	// 'episode' slot, which only the kind-switched predicate finds.
	feed := putFeed(t, st, "http://cast.example/f", "Ep1", "Ep2")
	eps, err := st.EpisodesByPodcast(ctx, feed.PodcastPID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %v (err %v), want 2", eps, err)
	}
	// The listing is newest-first; pick Ep1 by title rather than by position.
	ep1 := eps[0]
	if ep1.Title != "Ep1" {
		ep1 = eps[1]
	}
	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("podcast library: %v", err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: ep1.PID, LibraryID: libID,
		File: model.File{
			Path: []byte("/podcasts/ep1.mp3"), DisplayPath: "/podcasts/ep1.mp3", RelPath: []byte("ep1.mp3"),
			Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed,
		},
		Image: testPNG(t, 16, 16),
	}); err != nil {
		t.Fatalf("attach episode file: %v", err)
	}

	withArt, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("has_art", query.OpIs, 1).OrderBy("title", false).Build(), "")
	if err != nil {
		t.Fatalf("has_art query: %v", err)
	}
	got := titlesOf(withArt)
	want := map[string]bool{"Te1": true, "Covered Book": true, "Ep1": true}
	if len(got) != 3 {
		t.Fatalf("has_art is 1 = %v, want the covered track, book, and episode", got)
	}
	for _, title := range got {
		if !want[title] {
			t.Errorf("unexpected has_art item %q", title)
		}
	}
	if n := countWhere(t, st, "has_art", query.OpIs, 0); n != 2 {
		t.Errorf("has_art is 0 = %d, want 2 (the bare track and the remote episode)", n)
	}
	_ = covered

	// Chain-inherited art does not count: album-level art on the bare track's
	// album leaves its own cover absent, which is exactly what has_art exists
	// to find.
	albumPID := scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al")
	if err := st.SetEntityArt(ctx, model.ArtAlbum, model.PID(albumPID), model.ArtRoleFront, tinyPNG(t)); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	if n := countWhere(t, st, "has_art", query.OpIs, 0); n != 2 {
		t.Errorf("has_art is 0 after album art = %d, want still 2 (inherited art is not own art)", n)
	}
}

func TestHasLyricsField(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	if _, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File: model.File{
			Path: []byte("/lib/l.flac"), DisplayPath: "/lib/l.flac", RelPath: []byte("l.flac"),
			Kind: model.FileAudio, ContentHash: "cl", EssenceHash: "el", ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "Sung",
			SortKey: model.SortKey("Sung"), IdentityKey: "essence:el",
		},
		Track:  model.Track{Artist: "X", Album: "Al"},
		Lyrics: &model.Lyrics{Source: "embedded", Unsynced: "la la la"},
	}); err != nil {
		t.Fatalf("put track with lyrics: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/i.flac", essence: "ei", content: "ci",
		title: "Instrumental", artist: "X", album: "Al"})

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("has_lyrics", query.OpIs, 1).Build(), "")
	if err != nil {
		t.Fatalf("has_lyrics query: %v", err)
	}
	if got := titlesOf(items); !equalStrings(got, []string{"Sung"}) {
		t.Errorf("has_lyrics is 1 = %v, want [Sung]", got)
	}
	if n := countWhere(t, st, "has_lyrics", query.OpIs, 0); n != 1 {
		t.Errorf("has_lyrics is 0 = %d, want 1", n)
	}
}

// TestPresenceFieldPlansSeekIndexes is the EXPLAIN sanity check: the has_art
// probe must seek art_map's primary key (the CASE'd entity_type still seeks,
// with the key computed per row; the (entity_type, entity_id, role) PK covers
// the whole probe) and has_lyrics must seek the lyrics rowid PK, with neither
// table scanned. Loose string matching, since plan wording varies by SQLite
// version.
func TestPresenceFieldPlansSeekIndexes(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()

	fm, ok := fieldMapFor(query.EntityItems)
	if !ok {
		t.Fatal("no field map for items")
	}
	c, err := query.Compile(query.New(query.EntityItems).
		Where("has_art", query.OpIs, 1).Where("has_lyrics", query.OpIs, 0).Build(), fm)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	rows, err := st.read.QueryContext(ctx, "EXPLAIN QUERY PLAN "+itemSelect+" WHERE "+c.Where, c.Args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	plan := strings.Join(details, "\n")
	// The PK autoindex name is a SQLite implementation detail; assert a SEARCH on
	// art_map keyed by entity_type rather than the index's exact name.
	if !strings.Contains(plan, "SEARCH amq") || !strings.Contains(plan, "entity_type=?") {
		t.Errorf("has_art probe does not seek the art_map primary key:\n%s", plan)
	}
	for _, d := range details {
		if strings.Contains(d, "SCAN") && (strings.Contains(d, "art_map") || strings.Contains(d, "lyrics")) {
			t.Errorf("presence probe full-scans a table: %s", d)
		}
	}
}

func TestSmartRuleRoundTripsNewFields(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	covered := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 32, 32))
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/al/2.flac", essence: "e2", content: "c2",
		title: "Bare", artist: "X", album: "Al"})

	rule := query.New(query.EntityItems).
		Where("library", query.OpIs, string(lib.PID)).
		Where("has_art", query.OpIs, 1).
		Build()
	data, err := query.MarshalRule(rule)
	if err != nil {
		t.Fatalf("marshal rule: %v", err)
	}
	parsed, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse rule: %v", err)
	}
	pl, err := st.CreatePlaylist(ctx, "Covered here", "", model.PlaylistSmart, "", &parsed)
	if err != nil {
		t.Fatalf("create smart playlist: %v", err)
	}
	items, err := st.PlaylistItems(ctx, pl, "")
	if err != nil {
		t.Fatalf("playlist items: %v", err)
	}
	if len(items) != 1 || items[0].PID != covered {
		t.Errorf("smart membership = %v, want just the covered track", titlesOf(items))
	}
}
