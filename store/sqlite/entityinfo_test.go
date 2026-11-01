package sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// allEntityPIDs lists every pid of a kind's table. The EntityKind value is the
// table name (artist/release_group/album/genre/series), the same whitelist basis
// the batch and merge paths interpolate a table name on.
func allEntityPIDs(t *testing.T, st *Store, kind read.EntityKind) []model.PID {
	t.Helper()
	rows, err := st.rdb().QueryContext(context.Background(), "SELECT pid FROM "+string(kind))
	if err != nil {
		t.Fatalf("list %s pids: %v", kind, err)
	}
	defer rows.Close()
	var out []model.PID
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("scan %s pid: %v", kind, err)
		}
		out = append(out, model.PID(pid))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating %s pids: %v", kind, err)
	}
	return out
}

// entityPIDByName resolves an entity table row's pid by its display column, for
// asserting against the pids EntityByPID is looked up with.
func entityPIDByName(t *testing.T, st *Store, table, nameCol, name string) model.PID {
	t.Helper()
	var pid string
	if err := st.rdb().QueryRowContext(context.Background(),
		"SELECT pid FROM "+table+" WHERE "+nameCol+" = ?", name).Scan(&pid); err != nil {
		t.Fatalf("pid of %s %q: %v", table, name, err)
	}
	return model.PID(pid)
}

// entityInfoFixture catalogs two Radiohead tracks (one artist, one release
// group, one album, one genre) plus a two-part series book by a different
// author, the members every kind's lookup is asserted against.
func entityInfoFixture(t *testing.T) (*Store, *model.Library) {
	st, lib := entityFixture(t)
	// The release identifiers ride on the fixture album so the batch/single parity
	// check below compares them non-empty.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/01.flac", essence: "e1", content: "c1",
		title: "Airbag", artist: "Radiohead", album: "OK Computer", genre: "Rock",
		year: 1997, durationMS: 100,
		barcode: "724385522925", label: "Parlophone", catNo: "CDNODATA 02",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Radiohead/OK Computer/02.flac", essence: "e2", content: "c2",
		title: "Paranoid Android", artist: "Radiohead", album: "OK Computer", genre: "Rock",
		year: 1997, durationMS: 250,
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/Hobbit/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", series: "Middle-earth", seq: "0",
		asin: "B0000000A1", durationMS: 3000,
	})
	return st, lib
}

func TestEntityByPIDAllKinds(t *testing.T) {
	t.Parallel()
	st, lib := entityInfoFixture(t)
	ctx := context.Background()

	artistPID := entityPIDByName(t, st, "artist", "name", "Radiohead")
	rgPID := entityPIDByName(t, st, "release_group", "title", "OK Computer")
	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")
	genrePID := entityPIDByName(t, st, "genre", "name", "Rock")
	seriesPID := entityPIDByName(t, st, "series", "name", "Middle-earth")

	artist, err := st.EntityByPID(ctx, read.EntityArtist, artistPID)
	if err != nil {
		t.Fatalf("artist: %v", err)
	}
	if artist.Name != "Radiohead" || artist.ItemCount != 2 || artist.ReleaseGroupCount != 1 ||
		artist.TotalDurationMS != 350 {
		t.Errorf("artist = %+v, want 2 items, 1 release group, 350ms", artist)
	}
	if artist.SortKey == "" {
		t.Error("artist sort key missing")
	}

	rg, err := st.EntityByPID(ctx, read.EntityReleaseGroup, rgPID)
	if err != nil {
		t.Fatalf("release group: %v", err)
	}
	if rg.Name != "OK Computer" || rg.ItemCount != 2 || rg.TotalDurationMS != 350 {
		t.Errorf("release group = %+v, want 2 items, 350ms", rg)
	}
	if rg.ArtistPID != artistPID {
		t.Errorf("release group artist link = %s, want %s", rg.ArtistPID, artistPID)
	}
	if rg.Type != "album" {
		t.Errorf("release group type = %q, want album", rg.Type)
	}

	album, err := st.EntityByPID(ctx, read.EntityAlbum, albumPID)
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Name != "OK Computer" || album.Year != 1997 {
		t.Errorf("album = %+v, want OK Computer 1997", album)
	}
	if album.ReleaseGroupPID != rgPID {
		t.Errorf("album release-group link = %s, want %s", album.ReleaseGroupPID, rgPID)
	}
	if album.ItemCount != 2 || album.TotalDurationMS != 350 {
		t.Errorf("album live counts = %d items %dms, want 2 items 350ms", album.ItemCount, album.TotalDurationMS)
	}

	genre, err := st.EntityByPID(ctx, read.EntityGenre, genrePID)
	if err != nil {
		t.Fatalf("genre: %v", err)
	}
	if genre.Name != "Rock" || genre.ItemCount != 2 || genre.TotalDurationMS != 350 {
		t.Errorf("genre = %+v, want 2 items, 350ms", genre)
	}
	if genre.MBID != "" {
		t.Errorf("genre mbid = %q, want empty (genres carry no external id)", genre.MBID)
	}

	series, err := st.EntityByPID(ctx, read.EntitySeries, seriesPID)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if series.Name != "Middle-earth" || series.ItemCount != 1 || series.TotalDurationMS != 3000 {
		t.Errorf("series = %+v, want 1 book, 3000ms (the maintained parts sum)", series)
	}

	// Every kind resolved members through the fixture library.
	for _, info := range []*read.EntityInfo{artist, rg, album, genre, series} {
		if len(info.LibraryPIDs) != 1 || info.LibraryPIDs[0] != lib.PID {
			t.Errorf("%s libraries = %v, want [%s]", info.Kind, info.LibraryPIDs, lib.PID)
		}
	}
}

// TestSeriesEntityCarriesNoMBID guards the dropped series.mbid column against a
// reflex reintroduction. A series has no external id to hold: enrichment resolves
// a book release from a tagged release id and never reaches the series above it,
// so the column had no writer in any catalog. Both read paths are checked, since
// the batch one is a separate statement.
func TestSeriesEntityCarriesNoMBID(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()
	pid := entityPIDByName(t, st, "series", "name", "Middle-earth")

	single, err := st.EntityByPID(ctx, read.EntitySeries, pid)
	if err != nil {
		t.Fatalf("EntityByPID: %v", err)
	}
	if single.MBID != "" {
		t.Errorf("series mbid = %q, want empty (a series carries no external id)", single.MBID)
	}

	batch, err := st.EntityByPIDs(ctx, read.EntitySeries, []model.PID{pid})
	if err != nil {
		t.Fatalf("EntityByPIDs: %v", err)
	}
	if got := batch[pid]; got == nil || got.MBID != "" {
		t.Errorf("batched series = %+v, want a hit with an empty mbid", got)
	}
}

// TestEntityByPIDMatchesFacet: an entity's count is its live members, so it agrees with
// the facet that leaves the trash out, a trashed member gone from both.
func TestEntityByPIDMatchesFacet(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()
	trashItemFile(t, st, &model.ScanItemResult{FilePID: model.PID(scalarStr(t, st,
		"SELECT pid FROM file WHERE display_path = ?", "/lib/Radiohead/OK Computer/01.flac"))})

	live := query.New(query.EntityItems).WhereValues("state", query.OpNotIn, string(model.StateArchived)).Build()
	res, err := st.Facet(ctx, live, read.GroupArtist, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	var checked int
	for _, b := range res.Buckets {
		if b.EntityPID == "" || b.Display != "Radiohead" {
			continue
		}
		info, err := st.EntityByPID(ctx, read.EntityArtist, b.EntityPID)
		if err != nil {
			t.Fatalf("lookup of facet pid %s: %v", b.EntityPID, err)
		}
		if info.ItemCount != b.Count || b.Count != 1 {
			t.Errorf("artist %s: facet count %d, entity count %d; want both 1, the live member", b.Display, b.Count, info.ItemCount)
		}
		checked++
	}
	if checked != 1 {
		t.Fatalf("checked %d artist buckets, want the Radiohead one", checked)
	}
}

// TestEntityLibraryPIDsSpanLibraries verifies membership resolves libraries per
// member: an artist backing items in two libraries reports both, and a book
// counts under its author (the facet membership rule).
func TestEntityLibraryPIDsSpanLibraries(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	lib2, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/other"), DisplayRoot: "/other", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("second library: %v", err)
	}
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "e1", content: "c1",
		title: "One", artist: "Spread", album: "Alp"})
	putTrack(t, st, lib2.ID, trackSpec{path: "/other/b.flac", essence: "e2", content: "c2",
		title: "Two", artist: "Spread", album: "Bet"})
	putBook(t, st, lib2.ID, bookSpec{path: "/other/book.m4b", essence: "be1", content: "bc1",
		title: "Memoir", author: "Lone Author", durationMS: 100})

	spread, err := st.EntityByPID(ctx, read.EntityArtist, entityPIDByName(t, st, "artist", "name", "Spread"))
	if err != nil {
		t.Fatalf("artist: %v", err)
	}
	if len(spread.LibraryPIDs) != 2 || spread.LibraryPIDs[0] != lib.PID || spread.LibraryPIDs[1] != lib2.PID {
		t.Errorf("spread libraries = %v, want [%s %s] in library order", spread.LibraryPIDs, lib.PID, lib2.PID)
	}

	// The author artist has no tracks, so the rollup count is zero, but the
	// authored book still places the artist in its library (facet membership).
	author, err := st.EntityByPID(ctx, read.EntityArtist, entityPIDByName(t, st, "artist", "name", "Lone Author"))
	if err != nil {
		t.Fatalf("author: %v", err)
	}
	if len(author.LibraryPIDs) != 1 || author.LibraryPIDs[0] != lib2.PID {
		t.Errorf("author libraries = %v, want [%s] via the authored book", author.LibraryPIDs, lib2.PID)
	}
}

// TestEntityByPIDs pins the batch lookup: a map keyed by pid that is independent
// of input order, omits unknown pids, collapses a repeat, and matches EntityByPID
// field for field across every kind (the divergence guard the plan asks for).
func TestEntityByPIDs(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()

	radiohead := entityPIDByName(t, st, "artist", "name", "Radiohead")
	tolkien := entityPIDByName(t, st, "artist", "name", "J.R.R. Tolkien")

	// Order-independent + omit-missing + duplicate-collapse: request reversed with a
	// bogus pid and a repeat mixed in.
	got, err := st.EntityByPIDs(ctx, read.EntityArtist, []model.PID{"missing", tolkien, radiohead, tolkien})
	if err != nil {
		t.Fatalf("EntityByPIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entities, want 2 (unknown omitted, duplicate collapsed)", len(got))
	}
	if _, ok := got["missing"]; ok {
		t.Error("unknown pid must be omitted from the map")
	}
	if got[radiohead] == nil || got[radiohead].Name != "Radiohead" {
		t.Errorf("radiohead entry = %+v, want the Radiohead info", got[radiohead])
	}

	// Field-by-field parity with EntityByPID for every kind's entities.
	for _, kind := range read.EntityKinds() {
		pids := allEntityPIDs(t, st, kind)
		if len(pids) == 0 {
			t.Fatalf("fixture has no %s entities to check parity against", kind)
		}
		batch, err := st.EntityByPIDs(ctx, kind, pids)
		if err != nil {
			t.Fatalf("%s batch: %v", kind, err)
		}
		if len(batch) != len(pids) {
			t.Errorf("%s batch returned %d, want %d", kind, len(batch), len(pids))
		}
		for _, pid := range pids {
			single, err := st.EntityByPID(ctx, kind, pid)
			if err != nil {
				t.Fatalf("%s single %s: %v", kind, pid, err)
			}
			b := batch[pid]
			if b == nil {
				t.Fatalf("%s %s absent from batch", kind, pid)
			}
			if !reflect.DeepEqual(b, single) {
				t.Errorf("%s %s parity: batch %+v != single %+v", kind, pid, b, single)
			}
		}
	}
}

// TestEntityNames: the names the catalog keeps for artists, and an album's title, by pid,
// an unknown pid left out; other kinds are refused.
func TestEntityNames(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()
	radiohead := entityPIDByName(t, st, "artist", "name", "Radiohead")
	tolkien := entityPIDByName(t, st, "artist", "name", "J.R.R. Tolkien")
	album := entityPIDByName(t, st, "album", "title", "OK Computer")
	got, err := st.EntityNames(ctx, read.EntityArtist, []model.PID{radiohead, "missing", tolkien, radiohead})
	if err != nil || len(got) != 2 || got[radiohead] != "Radiohead" || got[tolkien] != "J.R.R. Tolkien" {
		t.Errorf("artist names = %v (err %v), want Radiohead and J.R.R. Tolkien", got, err)
	}
	if got, err := st.EntityNames(ctx, read.EntityAlbum, []model.PID{album}); err != nil || got[album] != "OK Computer" {
		t.Errorf("album names = %v (err %v), want OK Computer", got, err)
	}
	if _, err := st.EntityNames(ctx, read.EntityGenre, []model.PID{"x"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("genre names = %v, want CodeInvalid", err)
	}
	if got, err := st.ArtistNames(ctx, []string{identity.MatchKey("RADIOHEAD"), "nobody"}); err != nil || len(got) != 1 || got["radiohead"] != "Radiohead" {
		t.Errorf("artist names by match key = %v (err %v), want Radiohead alone", got, err)
	}
	var key string
	if err := st.rdb().QueryRowContext(ctx, "SELECT match_key FROM album WHERE pid = ?", string(album)).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if got, err := st.AlbumTitles(ctx, []string{key, "al:nothing"}); err != nil || len(got) != 1 || got[key] != "OK Computer" {
		t.Errorf("album titles by key = %v (err %v), want OK Computer alone", got, err)
	}
}

func TestEntityByPIDsEmptyAndBadKind(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()
	if got, err := st.EntityByPIDs(ctx, read.EntityArtist, nil); err != nil || got != nil {
		t.Errorf("empty pids = (%v, %v), want (nil, nil)", got, err)
	}
	if _, err := st.EntityByPIDs(ctx, "podcast", []model.PID{"x"}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown kind = %v, want CodeInvalid", err)
	}
}

func TestEntityByPIDUnknown(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	if _, err := st.EntityByPID(ctx, "podcast", "x"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown kind = %v, want CodeInvalid", err)
	}
	if _, err := st.EntityByPID(ctx, read.EntityArtist, "missing"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown pid = %v, want CodeNotFound", err)
	}
}

// drainEntityPages walks every page of one kind at a given page size and returns the
// entities in the order they came back, asserting each page respects the limit and
// that HasMore and Next agree. It is the entity twin of drainBrowse.
func drainEntityPages(t *testing.T, st *Store, kind read.EntityKind, limit int) []*read.EntityInfo {
	t.Helper()
	ctx := context.Background()
	var out []*read.EntityInfo
	var cursor read.Cursor
	for pages := 0; ; pages++ {
		if pages > 100 {
			t.Fatalf("%s pagination did not terminate at limit %d", kind, limit)
		}
		page, err := st.EntityPage(ctx, kind, cursor, limit)
		if err != nil {
			t.Fatalf("%s page: %v", kind, err)
		}
		if limit > 0 && len(page.Entities) > limit {
			t.Fatalf("%s page returned %d entities, over the limit %d", kind, len(page.Entities), limit)
		}
		if page.HasMore != (page.Next != "") {
			t.Fatalf("%s page HasMore=%v but Next=%q", kind, page.HasMore, page.Next)
		}
		out = append(out, page.Entities...)
		if !page.HasMore {
			return out
		}
		cursor = page.Next
	}
}

// TestEntityPageCoversAllOnce drains every kind at several page sizes and checks the
// three things keyset pagination has to get right: every entity appears exactly once,
// the sequence is (sort_key, pid) order, and each row is field-for-field what
// EntityByPIDs hydrates for the same pid. The last is what makes a page row and a
// looked-up row the same thing by construction rather than by coincidence.
func TestEntityPageCoversAllOnce(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()

	for _, kind := range read.EntityKinds() {
		want := allEntityPIDs(t, st, kind)
		hydrated, err := st.EntityByPIDs(ctx, kind, want)
		if err != nil {
			t.Fatalf("%s hydrate: %v", kind, err)
		}
		for _, limit := range []int{1, 2, 0} {
			got := drainEntityPages(t, st, kind, limit)
			if len(got) != len(want) {
				t.Fatalf("%s at limit %d returned %d entities, want %d", kind, limit, len(got), len(want))
			}
			seen := make(map[model.PID]bool, len(got))
			for i, e := range got {
				if seen[e.PID] {
					t.Errorf("%s at limit %d repeated %s", kind, limit, e.PID)
				}
				seen[e.PID] = true
				if i > 0 {
					prev := got[i-1]
					if prev.SortKey > e.SortKey || (prev.SortKey == e.SortKey && prev.PID >= e.PID) {
						t.Errorf("%s at limit %d out of order: (%q,%s) then (%q,%s)",
							kind, limit, prev.SortKey, prev.PID, e.SortKey, e.PID)
					}
				}
				if !reflect.DeepEqual(e, hydrated[e.PID]) {
					t.Errorf("%s %s page row != hydrated row:\npage %+v\nhydr %+v", kind, e.PID, e, hydrated[e.PID])
				}
			}
		}
	}
}

// TestEntityPageSharedSortKey pins the pid tiebreak. sort_key carries only a
// non-unique index (match_key is the UNIQUE one), so two entities can collide there:
// "The Wall" and "Wall" both generate the sort key "wall". Without pid in the keyset
// comparison a page boundary landing between them would drop one or repeat both.
func TestEntityPageSharedSortKey(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/1.flac", essence: "e1", content: "c1",
		title: "A", artist: "X", album: "The Wall"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/2.flac", essence: "e2", content: "c2",
		title: "B", artist: "Y", album: "Wall"})

	albums := allEntityPIDs(t, st, read.EntityAlbum)
	if len(albums) != 2 {
		t.Fatalf("fixture albums = %d, want 2", len(albums))
	}
	keys := map[string]bool{}
	for _, e := range drainEntityPages(t, st, read.EntityAlbum, 0) {
		keys[e.SortKey] = true
	}
	if len(keys) != 1 {
		t.Fatalf("albums do not share a sort key (%v); the tiebreak is not being exercised", keys)
	}
	// One entity per page is the boundary that lands between the colliding pair.
	got := drainEntityPages(t, st, read.EntityAlbum, 1)
	if len(got) != 2 || got[0].PID == got[1].PID {
		t.Fatalf("paging a shared sort key returned %d entities (%+v), want both exactly once", len(got), got)
	}
}

func TestEntityPageBadInput(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()
	if _, err := st.EntityPage(ctx, "podcast", "", 0); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unknown kind = %v, want CodeInvalid", err)
	}
	if _, err := st.EntityPage(ctx, read.EntityArtist, "not-a-cursor!!", 0); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("malformed cursor = %v, want CodeInvalid", err)
	}
}

// TestEntityInfoAlbumIdentifiers reads back the album's other release identifiers,
// which scan fills from tags and entity edit can write but nothing could read.
func TestEntityInfoAlbumIdentifiers(t *testing.T) {
	t.Parallel()
	st, _ := entityInfoFixture(t)
	ctx := context.Background()

	albumPID := entityPIDByName(t, st, "album", "title", "OK Computer")
	album, err := st.EntityByPID(ctx, read.EntityAlbum, albumPID)
	if err != nil {
		t.Fatalf("album: %v", err)
	}
	if album.Barcode != "724385522925" || album.Label != "Parlophone" || album.CatalogNumber != "CDNODATA 02" {
		t.Errorf("album identifiers = %q/%q/%q, want the scanned values",
			album.Barcode, album.Label, album.CatalogNumber)
	}

	// Album only, as Type and Year already are.
	rg, err := st.EntityByPID(ctx, read.EntityReleaseGroup,
		entityPIDByName(t, st, "release_group", "title", "OK Computer"))
	if err != nil {
		t.Fatalf("release group: %v", err)
	}
	if rg.Barcode != "" || rg.Label != "" || rg.CatalogNumber != "" {
		t.Errorf("release group carries album identifiers %q/%q/%q, want all empty",
			rg.Barcode, rg.Label, rg.CatalogNumber)
	}
}

// liveCountFixture is two tracks of one album, artist and genre, and two books of one
// series, with the inputs that put the first of each, for a trash and a restore put.
func liveCountFixture(t *testing.T) (st *Store, lib *model.Library, one *model.ScanItemResult, oneSpec trackSpec,
	first *model.ScanItemResult, firstSpec bookSpec) {
	t.Helper()
	st, lib = entityFixture(t)
	oneSpec = trackSpec{path: "/lib/A/Al/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", albumArt: "A", album: "Al", genre: "Rock", durationMS: 100}
	one = putTrack(t, st, lib.ID, oneSpec)
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/A/Al/02.flac", essence: "e2", content: "c2", title: "Two",
		artist: "A", albumArt: "A", album: "Al", genre: "Rock", durationMS: 250})
	firstSpec = bookSpec{path: "/lib/B/1.m4b", essence: "be1", content: "bc1", title: "Book One",
		author: "Au", series: "Saga", seq: "1", durationMS: 3000}
	first = putBook(t, st, lib.ID, firstSpec)
	putBook(t, st, lib.ID, bookSpec{path: "/lib/B/2.m4b", essence: "be2", content: "bc2", title: "Book Two",
		author: "Au", series: "Saga", seq: "2", durationMS: 5000})
	return st, lib, one, oneSpec, first, firstSpec
}

// entityCountsOf reads an entity's count and duration through the single read and the
// batch read, failing when the two disagree.
func entityCountsOf(t *testing.T, st *Store, kind read.EntityKind, pid model.PID) (int, int64) {
	t.Helper()
	ctx := context.Background()
	info, err := st.EntityByPID(ctx, kind, pid)
	if err != nil {
		t.Fatalf("%s %s: %v", kind, pid, err)
	}
	batch, err := st.EntityByPIDs(ctx, kind, []model.PID{pid})
	if err != nil || batch[pid] == nil {
		t.Fatalf("batch %s %s: %v", kind, pid, err)
	}
	if b := batch[pid]; b.ItemCount != info.ItemCount || b.TotalDurationMS != info.TotalDurationMS {
		t.Errorf("%s batch counts %d/%d, single %d/%d", kind, b.ItemCount, b.TotalDurationMS, info.ItemCount, info.TotalDurationMS)
	}
	return info.ItemCount, info.TotalDurationMS
}

// TestEntityCountsLeaveTheTrashOut: an album's and a series' counts are their live
// members, so a trashed member leaves the count as it leaves the duration, and the
// restore's put brings both back.
func TestEntityCountsLeaveTheTrashOut(t *testing.T) {
	t.Parallel()
	st, lib, one, oneSpec, first, firstSpec := liveCountFixture(t)
	album := entityPIDByName(t, st, "album", "title", "Al")
	series := entityPIDByName(t, st, "series", "name", "Saga")
	check := func(step string, kind read.EntityKind, pid model.PID, wantN int, wantMS int64) {
		t.Helper()
		if n, ms := entityCountsOf(t, st, kind, pid); n != wantN || ms != wantMS {
			t.Errorf("%s: %s counts %d/%d ms, want %d/%d", step, kind, n, ms, wantN, wantMS)
		}
	}
	check("before", read.EntityAlbum, album, 2, 350)
	check("before", read.EntitySeries, series, 2, 8000)
	trashItemFile(t, st, one)
	trashItemFile(t, st, first)
	check("trashed", read.EntityAlbum, album, 1, 250)
	check("trashed", read.EntitySeries, series, 1, 5000)
	putTrack(t, st, lib.ID, oneSpec)
	putBook(t, st, lib.ID, firstSpec)
	check("restored", read.EntityAlbum, album, 2, 350)
	check("restored", read.EntitySeries, series, 2, 8000)
	assertVerifyClean(t, st)
}

// TestRollupsLeaveTheTrashOut: the artist, release-group and genre rollups count live
// members as the album does, and verify recomputes them by the same rule, so a trash and
// a restore each move the counts and leave no drift.
func TestRollupsLeaveTheTrashOut(t *testing.T) {
	t.Parallel()
	st, lib, one, oneSpec, _, _ := liveCountFixture(t)
	artist := entityPIDByName(t, st, "artist", "name", "A")
	group := entityPIDByName(t, st, "release_group", "title", "Al")
	genre := entityPIDByName(t, st, "genre", "name", "Rock")
	check := func(step string, wantN int, wantMS int64) {
		t.Helper()
		for _, e := range []struct {
			kind read.EntityKind
			pid  model.PID
		}{{read.EntityArtist, artist}, {read.EntityReleaseGroup, group}, {read.EntityGenre, genre}} {
			if n, ms := entityCountsOf(t, st, e.kind, e.pid); n != wantN || ms != wantMS {
				t.Errorf("%s: %s counts %d/%d ms, want %d/%d", step, e.kind, n, ms, wantN, wantMS)
			}
		}
		assertVerifyClean(t, st)
	}
	check("before", 2, 350)
	trashItemFile(t, st, one)
	check("trashed", 1, 250)
	putTrack(t, st, lib.ID, oneSpec)
	check("restored", 2, 350)
}

// TestEntityInfoListsAliases: an artist's info names the other spellings it answers to (a
// merged loser's name), in the single and the batch read alike, and another kind names none.
func TestEntityInfoListsAliases(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a.flac", essence: "ea", content: "ca", title: "A", artist: "The Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b.flac", essence: "eb", content: "cb", title: "B", artist: "Beatles", album: "B"})
	survivor := entityPIDByName(t, st, "artist", "name", "The Beatles")
	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, entityPIDByName(t, st, "artist", "name", "Beatles")); err != nil {
		t.Fatal(err)
	}
	info, err := st.EntityByPID(ctx, read.EntityArtist, survivor)
	if err != nil || !reflect.DeepEqual(info.Aliases, []string{"Beatles"}) {
		t.Fatalf("artist info = %+v (err %v), want the alias Beatles", info, err)
	}
	batch, err := st.EntityByPIDs(ctx, read.EntityArtist, []model.PID{survivor})
	if err != nil || batch[survivor] == nil || !reflect.DeepEqual(batch[survivor].Aliases, []string{"Beatles"}) {
		t.Fatalf("batch artist info = %+v (err %v), want the alias Beatles", batch[survivor], err)
	}
	album, err := st.EntityByPID(ctx, read.EntityAlbum, entityPIDByName(t, st, "album", "title", "A"))
	if err != nil || len(album.Aliases) != 0 {
		t.Errorf("album info = %+v (err %v), want no aliases", album, err)
	}
}
