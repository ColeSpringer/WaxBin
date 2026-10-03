package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
)

type bookSpec struct {
	path, essence, content string
	title, author          string
	narrators              []string
	series, seq            string
	asin, isbn, edition    string
	mbid, publisher        string
	preserveLocks          bool
	year                   int
	genres                 []string
	position               int
	durationMS             int64
	chapters               []model.Chapter
	custom                 map[string][]string
}

func putBook(t *testing.T, st *Store, libID int64, s bookSpec) *model.ScanItemResult {
	t.Helper()
	res, err := st.PutScannedBook(context.Background(), bookSpecInput(libID, s))
	if err != nil {
		t.Fatalf("put book %s: %v", s.path, err)
	}
	return res
}

// bookSpecInput is the scan input putBook writes, for a test that puts it itself.
func bookSpecInput(libID int64, s bookSpec) model.PutScannedBookInput {
	key := identity.BookKey(s.asin, s.isbn, s.author, s.title, s.edition)
	if key == "" {
		key = "essence:" + s.essence
	}
	genre := ""
	if len(s.genres) > 0 {
		genre = s.genres[0]
	}
	return model.PutScannedBookInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(s.path), DisplayPath: s.path, RelPath: []byte(filepath.Base(s.path)),
			Kind: model.FileAudio, Size: int64(len(s.content)), MTimeNS: 1,
			ContentHash: s.content, EssenceHash: s.essence, DurationMS: s.durationMS,
			ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindBook, State: model.StatePresent, Title: s.title,
			SortKey: model.SortKey(s.title), IdentityKey: key,
		},
		Book: model.Book{
			Author: s.author, AuthorSort: model.SortKey(s.author), Authors: []string{s.author},
			Narrators: s.narrators, Series: s.series, SeriesSeq: s.seq,
			ASIN: s.asin, ISBN: s.isbn, Edition: s.edition, MBID: s.mbid, Year: s.year,
			Publisher: s.publisher, Genres: s.genres, Genre: genre,
		},
		Position:      s.position,
		Chapters:      s.chapters,
		PreserveLocks: s.preserveLocks,
		CustomTags:    s.custom,
	}
}

// TestPutScannedBookRelinksOnMove: a moved book file re-links onto its existing row and
// reports the path it moved from. The scanner deletes that path from its preloaded index,
// so without it end-of-walk reconciliation reads the old path as a vanished file and marks
// the book missing. resolveScannedFile serves the virtual-track path too, so this covers
// both.
func TestPutScannedBookRelinksOnMove(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)

	first := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/old.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien",
	})
	if first.RelinkedFrom != "" {
		t.Errorf("RelinkedFrom on the first put = %q, want empty", first.RelinkedFrom)
	}

	// The same essence at a new path, which is a move rather than a duplicate.
	moved := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/new.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien",
	})
	if !moved.Relinked {
		t.Fatalf("expected a re-link, got %+v", moved)
	}
	if moved.FilePID != first.FilePID {
		t.Fatalf("re-link should preserve the file pid: %s -> %s", first.FilePID, moved.FilePID)
	}
	if moved.RelinkedFrom != "/lib/Tolkien/old.m4b" {
		t.Errorf("RelinkedFrom = %q, want the old path", moved.RelinkedFrom)
	}
	assertVerifyClean(t, st)
}

func TestPutScannedBookSingleFile(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Tolkien/The Hobbit/hobbit.m4b", essence: "be1", content: "bc1",
		title: "The Hobbit", author: "J.R.R. Tolkien", narrators: []string{"Rob Inglis"},
		series: "Middle-earth", seq: "0", asin: "B0001", year: 1937, genres: []string{"Fantasy"},
		durationMS: 3000,
		chapters: []model.Chapter{
			{Position: 0, Title: "An Unexpected Party", FileStartMS: 0},
			{Position: 1, Title: "Roast Mutton", FileStartMS: 1000},
			{Position: 2, Title: "A Short Rest", FileStartMS: 2000},
		},
	})
	if !res.ItemCreated {
		t.Fatal("expected a new book item")
	}

	// The shared item view exposes a book with author standing in for artist.
	v, err := st.ItemByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Kind != model.KindBook {
		t.Errorf("kind = %s, want book", v.Kind)
	}
	if v.Artist != "J.R.R. Tolkien" {
		t.Errorf("artist (author) = %q, want Tolkien", v.Artist)
	}
	if v.Narrator != "" { // joined narrator display only set from the denormalized column
		// narrator column is set via Book.Narrator (joined); putBook left it empty, so
		// the view narrator is empty; contributors carry the narrator instead.
	}

	d, err := st.BookByPID(ctx, res.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	if got := d.Authors; len(got) != 1 || got[0] != "J.R.R. Tolkien" {
		t.Errorf("authors = %v, want [J.R.R. Tolkien]", got)
	}
	if got := d.Narrators; len(got) != 1 || got[0] != "Rob Inglis" {
		t.Errorf("narrators = %v, want [Rob Inglis]", got)
	}
	if d.Series != "Middle-earth" {
		t.Errorf("series = %q, want Middle-earth", d.Series)
	}
	if d.ASIN != "B0001" {
		t.Errorf("asin = %q, want B0001", d.ASIN)
	}
	if d.TotalDurationMS != 3000 {
		t.Errorf("total duration = %d, want 3000", d.TotalDurationMS)
	}
	if len(d.Chapters) != 3 {
		t.Fatalf("chapters = %d, want 3", len(d.Chapters))
	}
	// Open-ended file offsets fill into book-timeline spans across the single file.
	if d.Chapters[0].StartMS != 0 || d.Chapters[0].EndMS != 1000 {
		t.Errorf("chapter 0 span = [%d,%d), want [0,1000)", d.Chapters[0].StartMS, d.Chapters[0].EndMS)
	}
	if d.Chapters[2].StartMS != 2000 || d.Chapters[2].EndMS != 3000 {
		t.Errorf("chapter 2 span = [%d,%d), want [2000,3000)", d.Chapters[2].StartMS, d.Chapters[2].EndMS)
	}

	if rep, err := st.VerifyDerived(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !rep.Consistent() {
		t.Errorf("derived data not consistent after book scan: %+v", rep)
	}
}

func TestMultiFileBookGrouping(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Two distinct files sharing one ASIN are the two parts of one book.
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Sanderson/Mistborn/part1.mp3", essence: "mb1", content: "mc1",
		title: "Mistborn", author: "Brandon Sanderson", asin: "B0010", position: 1, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "Part 1"}},
	})
	r2 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Sanderson/Mistborn/part2.mp3", essence: "mb2", content: "mc2",
		title: "Mistborn", author: "Brandon Sanderson", asin: "B0010", position: 2, durationMS: 2000,
		chapters: []model.Chapter{{Position: 0, Title: "Part 2"}},
	})
	if r1.ItemPID != r2.ItemPID {
		t.Fatalf("two parts of one book got different items: %s vs %s", r1.ItemPID, r2.ItemPID)
	}
	if r2.ItemCreated {
		t.Error("second part should attach to the existing book, not create a new one")
	}

	d, err := st.BookByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("parts = %d, want 2", len(d.Files))
	}
	if d.TotalDurationMS != 3000 {
		t.Errorf("total duration = %d, want 3000 (1000+2000)", d.TotalDurationMS)
	}
	// The whole-file chapter of part 2 is offset by part 1's duration on the book timeline.
	if len(d.Chapters) != 2 {
		t.Fatalf("chapters = %d, want 2", len(d.Chapters))
	}
	if d.Chapters[0].StartMS != 0 || d.Chapters[0].EndMS != 1000 {
		t.Errorf("chapter 0 = [%d,%d), want [0,1000)", d.Chapters[0].StartMS, d.Chapters[0].EndMS)
	}
	if d.Chapters[1].StartMS != 1000 || d.Chapters[1].EndMS != 3000 {
		t.Errorf("chapter 1 = [%d,%d), want [1000,3000)", d.Chapters[1].StartMS, d.Chapters[1].EndMS)
	}

	// Chapter-level resume resolves a book-timeline position to its chapter.
	for _, tc := range []struct {
		pos  int64
		want int
	}{{500, 0}, {1500, 1}, {2999, 1}} {
		ch, err := st.CurrentChapter(ctx, r1.ItemPID, tc.pos)
		if err != nil {
			t.Fatalf("CurrentChapter(%d): %v", tc.pos, err)
		}
		if ch == nil || ch.Position != tc.want {
			t.Errorf("CurrentChapter(%d) = %v, want position %d", tc.pos, ch, tc.want)
		}
	}

	if rep, err := st.VerifyDerived(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !rep.Consistent() {
		t.Errorf("derived data not consistent: %+v", rep)
	}
}

func TestMultiFileBookRescanIsStable(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)

	spec := bookSpec{
		path: "/lib/A/B/p1.mp3", essence: "se1", content: "sc1",
		title: "Solo", author: "Auth", asin: "B0030", position: 1, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "One"}},
	}
	putBook(t, st, lib.ID, spec)
	// A byte-identical rescan must neither duplicate the file edge nor the chapter.
	putBook(t, st, lib.ID, spec)

	pid := mustItemPID(t, st, "Solo")
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_file itf
		JOIN playable_item pi ON pi.id = itf.item_id WHERE pi.pid = ?`, string(pid)); n != 1 {
		t.Errorf("item_file edges after rescan = %d, want 1", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM chapter c
		JOIN playable_item pi ON pi.id = c.book_item_id WHERE pi.pid = ?`, string(pid)); n != 1 {
		t.Errorf("chapters after rescan = %d, want 1", n)
	}
}

func TestBooksInSeriesOrdering(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Sequences chosen to expose numeric-aware ordering: 1.5 between 1 and 2, and 10
	// after 2 (a plain string sort would place "10" before "2").
	for _, s := range []struct{ title, seq string }{
		{"Book Ten", "10"}, {"Book Two", "2"}, {"Book One", "1"}, {"Book One-Five", "1.5"},
	} {
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/S/" + s.title + ".m4b", essence: "se" + s.seq, content: "sc" + s.seq,
			title: s.title, author: "Author", series: "Saga", seq: s.seq,
			asin: "ASIN" + s.seq, durationMS: 100,
		})
	}

	var seriesPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM series WHERE name = 'Saga'").Scan(&seriesPID); err != nil {
		t.Fatalf("series pid: %v", err)
	}
	books, err := st.BooksInSeries(ctx, seriesPID)
	if err != nil {
		t.Fatalf("BooksInSeries: %v", err)
	}
	got := make([]string, len(books))
	for i, b := range books {
		got[i] = b.SeriesSeq
	}
	want := []string{"1", "1.5", "2", "10"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("series order = %v, want %v", got, want)
		}
	}
}

func TestBookSearchAndFacet(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Adams/Hitchhiker.m4b", essence: "ad1", content: "adc1",
		title: "The Hitchhiker's Guide", author: "Douglas Adams", asin: "B0050",
		genres: []string{"Science Fiction"}, year: 1979, durationMS: 100,
	})

	res, err := st.Search(ctx, "hitchhiker", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Books) != 1 || res.Books[0].Title != "The Hitchhiker's Guide" {
		t.Fatalf("book search = %+v, want one Hitchhiker hit", res.Books)
	}
	if res.Books[0].Subtitle != "Douglas Adams" {
		t.Errorf("book hit subtitle = %q, want the author", res.Books[0].Subtitle)
	}

	// The author appears in the artist facet via the author COALESCE.
	f, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "", 0, "")
	if err != nil {
		t.Fatalf("facet: %v", err)
	}
	found := false
	for _, b := range f.Buckets {
		if b.Display == "Douglas Adams" && b.Count == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("author not in artist facet: %+v", f.Buckets)
	}
}

func mustItemPID(t *testing.T, st *Store, title string) model.PID {
	t.Helper()
	var pid model.PID
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM playable_item WHERE title = ?", title).Scan(&pid); err != nil {
		t.Fatalf("item pid for %q: %v", title, err)
	}
	return pid
}

func TestMultiFileBookGenreRollupDuration(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	// A two-part book (1000 + 2000 ms) tagged with a genre. The genre rollup must
	// count the WHOLE book duration, not just the primary part.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/G/p1.mp3", essence: "g1", content: "gc1", title: "Tome", author: "Auth",
		asin: "B0100", position: 1, durationMS: 1000, genres: []string{"Fantasy"},
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/G/p2.mp3", essence: "g2", content: "gc2", title: "Tome", author: "Auth",
		asin: "B0100", position: 2, durationMS: 2000, genres: []string{"Fantasy"},
	})

	dur := scalarInt(t, st, `SELECT gr.total_duration_ms FROM genre_rollup gr
		JOIN genre g ON g.id = gr.genre_id WHERE g.name = 'Fantasy'`)
	if dur != 3000 {
		t.Errorf("genre rollup duration = %d, want 3000 (both parts summed)", dur)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !rep.Consistent() {
		t.Errorf("derived data inconsistent: %+v", rep)
	}
}

func TestMultiFileBookEmptyMiddlePart(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()

	// Three parts; the middle one carries no chapters. Its duration must still
	// advance the book timeline, so part 3's chapter starts at 1000+2000 = 3000.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/E/p1.mp3", essence: "e1", content: "ec1", title: "Epic", author: "Auth",
		asin: "B0200", position: 1, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "Front"}},
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/E/p2.mp3", essence: "e2", content: "ec2", title: "Epic", author: "Auth",
		asin: "B0200", position: 2, durationMS: 2000, // no chapters
	})
	r3 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/E/p3.mp3", essence: "e3", content: "ec3", title: "Epic", author: "Auth",
		asin: "B0200", position: 3, durationMS: 3000,
		chapters: []model.Chapter{{Position: 0, Title: "Finale"}},
	})

	chs, err := st.Chapters(ctx, r3.ItemPID)
	if err != nil {
		t.Fatalf("Chapters: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("chapters = %d, want 2 (the empty middle part has none)", len(chs))
	}
	if chs[0].Title != "Front" || chs[0].StartMS != 0 {
		t.Errorf("chapter 0 = %q@%d, want Front@0", chs[0].Title, chs[0].StartMS)
	}
	// The empty middle part's 2000 ms still shifts the finale to 3000 on the timeline.
	if chs[1].Title != "Finale" || chs[1].StartMS != 3000 {
		t.Errorf("chapter 1 = %q@%d, want Finale@3000 (empty part counted)", chs[1].Title, chs[1].StartMS)
	}
	if chs[1].EndMS != 6000 {
		t.Errorf("finale end = %d, want 6000 (total book duration)", chs[1].EndMS)
	}
}

func TestTrackEntityExcludesBooks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/m/a.flac", essence: "te1", content: "tc1", title: "Song", artist: "Band"})
	putBook(t, st, lib.ID, bookSpec{path: "/lib/b/x.m4b", essence: "be9", content: "bc9", title: "Book", author: "Auth", asin: "BX", durationMS: 100})

	// The items entity is kind-agnostic; the tracks entity is music-only.
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (track + book)", len(items))
	}
	tracks, err := st.QueryItems(ctx, query.New(query.EntityTracks).Build(), "")
	if err != nil {
		t.Fatalf("query tracks: %v", err)
	}
	if len(tracks) != 1 || tracks[0].Kind != model.KindTrack {
		t.Fatalf("tracks entity = %v, want exactly the one track", tracks)
	}
	if n, _ := st.CountItems(ctx, query.New(query.EntityTracks).Build(), ""); n != 1 {
		t.Errorf("count tracks = %d, want 1", n)
	}
	if n, _ := st.CountItems(ctx, query.New(query.EntityItems).Build(), ""); n != 2 {
		t.Errorf("count items = %d, want 2", n)
	}
}

func TestBookMatchesItemFilters(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/h.m4b", essence: "he1", content: "hc1", title: "The Hobbit",
		author: "J.R.R. Tolkien", asin: "BH", year: 1937, genres: []string{"Fantasy"}, durationMS: 100,
	})

	// A book matches the shared item filters by the same author/year/genre the row
	// displays (the field map COALESCEs the book columns), via the items entity.
	for _, tc := range []struct {
		field, op string
		val       any
	}{
		{"artist", string(query.OpContains), "Tolkien"},
		{"year", string(query.OpIs), 1937},
		{"genre", string(query.OpContains), "Fantasy"},
	} {
		got, err := st.QueryItems(ctx, query.New(query.EntityItems).Where(tc.field, query.Op(tc.op), tc.val).Build(), "")
		if err != nil {
			t.Fatalf("query %s: %v", tc.field, err)
		}
		if len(got) != 1 {
			t.Errorf("filter %s=%v matched %d, want the book", tc.field, tc.val, len(got))
		}
	}
	// The tracks entity excludes the book even when its author matches.
	got, err := st.QueryItems(ctx, query.New(query.EntityTracks).Where("artist", query.OpContains, "Tolkien").Build(), "")
	if err != nil {
		t.Fatalf("query tracks: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("tracks entity matched a book by author: %v", got)
	}
}

func TestMultiFileBookEmitsItemUpdateOnNewPart(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "iu1", content: "ic1", title: "Tome", author: "Auth",
		asin: "BT", position: 1, durationMS: 1000,
	})
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}

	// Attaching a second part changes the book (parts/duration), so the existing book
	// item must get an update delta, not just the new file's create delta.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "iu2", content: "ic2", title: "Tome", author: "Auth",
		asin: "BT", position: 2, durationMS: 2000,
	})
	changes, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	var itemUpdated bool
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == r1.ItemPID && c.Op == model.OpUpdate {
			itemUpdated = true
		}
	}
	if !itemUpdated {
		t.Errorf("no item update delta after attaching a new book part; changes=%+v", changes)
	}
}

func TestMultiFileBookMetadataOwnedByPrimary(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Part 1 is scanned first, so it becomes the primary and owns the book metadata.
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "po1", content: "pc1", title: "Tome", author: "Auth",
		narrators: []string{"Reader"}, series: "Saga", asin: "BP", position: 1, durationMS: 1000,
	})
	// Part 2 is asymmetrically tagged (no narrator, no series). It must NOT clobber
	// the book's metadata set by the primary part.
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "po2", content: "pc2", title: "Tome", author: "Auth",
		asin: "BP", position: 2, durationMS: 2000,
	})

	d, err := st.BookByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	if len(d.Narrators) != 1 || d.Narrators[0] != "Reader" {
		t.Errorf("narrator clobbered by the untagged second part: %v", d.Narrators)
	}
	if d.Series != "Saga" {
		t.Errorf("series clobbered by the untagged second part: %q", d.Series)
	}
	// Both parts are still attached, and the list-view duration sums them.
	if len(d.Files) != 2 {
		t.Errorf("parts = %d, want 2", len(d.Files))
	}
	v, _ := st.ItemByPID(ctx, r1.ItemPID)
	if v.DurationMS != 3000 {
		t.Errorf("list-view duration = %d, want 3000 (sum of parts)", v.DurationMS)
	}
}

func TestNaturalPartOrdering(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Parts with no track numbers (position 0) and un-zero-padded names. Natural
	// ordering must read them 1,2,10 rather than lexicographic 1,10,2.
	for _, n := range []string{"1", "2", "10"} {
		putBook(t, st, lib.ID, bookSpec{
			path: "/lib/b/" + n + ".mp3", essence: "no" + n, content: "nc" + n,
			title: "Tome", author: "Auth", asin: "BN", position: 0, durationMS: 1000,
			chapters: []model.Chapter{{Position: 0, Title: "Ch" + n}},
		})
	}
	pid := mustItemPID(t, st, "Tome")
	chs, err := st.Chapters(ctx, pid)
	if err != nil {
		t.Fatalf("Chapters: %v", err)
	}
	got := []string{chs[0].Title, chs[1].Title, chs[2].Title}
	want := []string{"Ch1", "Ch2", "Ch10"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chapter order = %v, want %v (natural, not lexicographic)", got, want)
		}
	}
}

func TestPromotePrimaryOnDetach(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "dp1", content: "dc1", title: "Tome", author: "Auth",
		asin: "BD", position: 1, durationMS: 1000,
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "dp2", content: "dc2", title: "Tome", author: "Auth",
		asin: "BD", position: 2, durationMS: 2000,
	})
	bookPID := r1.ItemPID

	// Re-key the book's primary part (p1) as a music track. The book loses its
	// primary but keeps p2, so a part must be promoted to primary.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/p1.mp3", essence: "trk1", content: "tcx1", title: "Song", artist: "Band"})

	v, err := st.ItemByPID(ctx, bookPID)
	if err != nil {
		t.Fatalf("ItemByPID(book): %v", err)
	}
	if v.FilePID == "" {
		t.Fatal("book left headless after its primary part was detached; expected a promoted primary")
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_file itf JOIN playable_item pi ON pi.id=itf.item_id
		WHERE pi.pid = ? AND itf.role = 'primary'`, string(bookPID)); n != 1 {
		t.Errorf("book primary edges = %d, want exactly 1 after promotion", n)
	}
}

// TestArchivedBookShedsItsDuration pins that a book losing its last part sheds its
// denormalized total the way the entity rollups on the same path shed theirs. Left
// stale, the book reads back with a running time it no longer has and `db verify`
// reports drift a rescan cannot clear, since the file it would re-read is gone.
func TestArchivedBookShedsItsDuration(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "ad1", content: "ac1", title: "Tome", author: "Auth",
		asin: "AD", position: 1, durationMS: 1000,
	})
	r2 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "ad2", content: "ac2", title: "Tome", author: "Auth",
		asin: "AD", position: 2, durationMS: 2000,
	})

	// One part gone: the book survives, and its total is the surviving part alone.
	if _, err := st.DetachFile(ctx, r1.FilePID); err != nil {
		t.Fatalf("detach p1: %v", err)
	}
	d, err := st.BookByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	if d.TotalDurationMS != 2000 {
		t.Errorf("total after losing p1 = %d, want 2000", d.TotalDurationMS)
	}

	// Last part gone: the book is archived with no parts, so its total is 0.
	if _, err := st.DetachFile(ctx, r2.FilePID); err != nil {
		t.Fatalf("detach p2: %v", err)
	}
	if s := itemState(t, st, r1.ItemPID); s != string(model.StateArchived) {
		t.Fatalf("book state = %q, want archived", s)
	}
	if d, err = st.BookByPID(ctx, r1.ItemPID); err != nil {
		t.Fatalf("BookByPID after archive: %v", err)
	}
	if d.TotalDurationMS != 0 {
		t.Errorf("archived book total = %d, want 0 (it has no parts left)", d.TotalDurationMS)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("VerifyDerived: %v", err)
	}
	if rep.BookDurationDrift != 0 {
		t.Errorf("book-duration drift = %d, want 0", rep.BookDurationDrift)
	}
}

// TestRefreshRollupsRepairsBookDuration covers the other half: a catalog that already
// drifted, which is every catalog written before the detach path refreshed the total.
// `db verify --fix` runs RefreshRollups, and without this it reported drift it could
// not clear.
func TestRefreshRollupsRepairsBookDuration(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "rr1", content: "rc1", title: "Tome", author: "Auth",
		asin: "RR", position: 1, durationMS: 1000,
	})
	if _, err := st.write.ExecContext(ctx,
		"UPDATE book SET total_duration_ms = 999999 WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)",
		string(r.ItemPID)); err != nil {
		t.Fatalf("stage the drift: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("VerifyDerived: %v", err)
	}
	if rep.BookDurationDrift != 1 {
		t.Fatalf("staged drift = %d, want 1", rep.BookDurationDrift)
	}

	if err := st.RefreshRollups(ctx); err != nil {
		t.Fatalf("RefreshRollups: %v", err)
	}
	if rep, err = st.VerifyDerived(ctx); err != nil {
		t.Fatalf("VerifyDerived after repair: %v", err)
	}
	if rep.BookDurationDrift != 0 {
		t.Errorf("drift after --fix = %d, want 0", rep.BookDurationDrift)
	}
	d, err := st.BookByPID(ctx, r.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	if d.TotalDurationMS != 1000 {
		t.Errorf("repaired total = %d, want 1000", d.TotalDurationMS)
	}
}

func TestZeroDurationPartAdvancesTimeline(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Part 1 has an unknown (0) duration but a chapter that ends at 500ms; part 2's
	// chapter must start at 500, not 0 (the fallback advances the timeline).
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "zd1", content: "zc1", title: "Tome", author: "Auth",
		asin: "BZ", position: 1, durationMS: 0,
		chapters: []model.Chapter{{Position: 0, Title: "A", FileStartMS: 0, FileEndMS: 500}},
	})
	r2 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "zd2", content: "zc2", title: "Tome", author: "Auth",
		asin: "BZ", position: 2, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "B", FileStartMS: 0}},
	})
	chs, err := st.Chapters(ctx, r2.ItemPID)
	if err != nil {
		t.Fatalf("Chapters: %v", err)
	}
	if len(chs) != 2 || chs[1].Title != "B" {
		t.Fatalf("chapters = %v, want [A B]", chs)
	}
	if chs[1].StartMS != 500 {
		t.Errorf("part-2 chapter start = %d, want 500 (zero-duration part 1 advanced via its chapter end)", chs[1].StartMS)
	}
}

func TestStatsAndBrowseIncludeBooks(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/h.m4b", essence: "sb1", content: "sbc1", title: "The Hobbit",
		author: "J.R.R. Tolkien", asin: "BH2", year: 1937, durationMS: 1000,
	})

	// Stats acknowledges the book and a played book shows its author, not a blank.
	if err := st.MarkPlayed(ctx, "", res.ItemPID, true, nil); err != nil {
		t.Fatalf("mark played: %v", err)
	}
	stats, err := st.Stats(ctx, "", 10)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Books != 1 {
		t.Errorf("stats books = %d, want 1", stats.Books)
	}
	if len(stats.Play.MostPlayed) != 1 || stats.Play.MostPlayed[0].Artist != "J.R.R. Tolkien" {
		t.Errorf("most-played artist = %+v, want the book author", stats.Play.MostPlayed)
	}

	// By-year browse includes the book (its year COALESCEs the missing track year).
	page, err := st.BrowsePage(ctx, read.ListByYear, read.BrowseOptions{Year: 1937, Limit: 10})
	if err != nil {
		t.Fatalf("browse by-year: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].PID != res.ItemPID {
		t.Fatalf("by-year browse = %+v, want the 1937 book", page.Items)
	}
}

func TestRekeyNonPrimaryPartLeavesNoDanglingEdge(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "dk1", content: "dkc1", title: "Tome", author: "Auth",
		asin: "BDK", position: 1, durationMS: 1000, genres: []string{"Fantasy"},
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "dk2", content: "dkc2", title: "Tome", author: "Auth",
		asin: "BDK", position: 2, durationMS: 2000, genres: []string{"Fantasy"},
	})

	// Re-key the NON-primary part p2 as a music track. p2 must detach from the book
	// entirely (its 'part' edge gone), not stay attached to both.
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/p2.mp3", essence: "trkp2", content: "tcp2", title: "Song", artist: "Band"})

	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_file itf
		JOIN playable_item pi ON pi.id = itf.item_id WHERE pi.pid = ?`, string(r1.ItemPID)); n != 1 {
		t.Errorf("book item_file edges = %d, want 1 (p2 fully detached)", n)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_file itf JOIN file f ON f.id = itf.file_id
		WHERE f.path = ?`, []byte("/lib/b/p2.mp3")); n != 1 {
		t.Errorf("p2 file edges = %d, want 1 (only the track, no dangling book part)", n)
	}
	// The shrunken book's rollups and denormalized duration were recomputed.
	if rep, err := st.VerifyDerived(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !rep.Consistent() {
		t.Errorf("derived data inconsistent after re-key: %+v", rep)
	}
	v, _ := st.ItemByPID(ctx, r1.ItemPID)
	if v.DurationMS != 1000 {
		t.Errorf("book duration after losing p2 = %d, want 1000", v.DurationMS)
	}
}

func TestBookTotalCoversChapterSpan(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "bt1", content: "btc1", title: "Tome", author: "Auth",
		asin: "BBT", position: 1, durationMS: 0,
		chapters: []model.Chapter{{Position: 0, Title: "A", FileStartMS: 0, FileEndMS: 500}},
	})
	r2 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "bt2", content: "btc2", title: "Tome", author: "Auth",
		asin: "BBT", position: 2, durationMS: 1000,
		chapters: []model.Chapter{{Position: 0, Title: "B", FileStartMS: 0}},
	})

	d, err := st.BookByPID(ctx, r2.ItemPID)
	if err != nil {
		t.Fatalf("BookByPID: %v", err)
	}
	// p1's effective duration is its chapter span (500); plus p2 (1000) = 1500.
	if d.TotalDurationMS != 1500 {
		t.Errorf("total = %d, want 1500 (effective p1 500 + p2 1000)", d.TotalDurationMS)
	}
	last := d.Chapters[len(d.Chapters)-1]
	if last.EndMS > d.TotalDurationMS {
		t.Errorf("last chapter end %d exceeds reported total %d", last.EndMS, d.TotalDurationMS)
	}
	// The denormalized column agrees with the effective sum (verify clean).
	if rep, _ := st.VerifyDerived(ctx); !rep.Consistent() {
		t.Errorf("book-duration drift after zero-duration part: %+v", rep)
	}
}

func TestTrashDetachEmitsItemUpdate(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r1 := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p1.mp3", essence: "td1", content: "tdc1", title: "Tome", author: "Auth",
		asin: "BTD", position: 1, durationMS: 1000,
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/p2.mp3", essence: "td2", content: "tdc2", title: "Tome", author: "Auth",
		asin: "BTD", position: 2, durationMS: 2000,
	})
	var p2pid model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM file WHERE path = ?", []byte("/lib/b/p2.mp3")).Scan(&p2pid); err != nil {
		t.Fatalf("p2 pid: %v", err)
	}
	seq, _ := st.LatestChangeSeq(ctx)

	// Detaching a part of a surviving book must emit an item update (symmetric with attach).
	if _, err := st.DetachFile(ctx, p2pid); err != nil {
		t.Fatalf("DetachFile: %v", err)
	}
	changes, _ := st.ChangesSince(ctx, seq)
	found := false
	for _, c := range changes {
		if c.EntityType == "item" && c.EntityPID == r1.ItemPID && c.Op == model.OpUpdate {
			found = true
		}
	}
	if !found {
		t.Errorf("no item update delta after detaching a book part: %+v", changes)
	}
}

func TestStatsArtistCountMatchesFacet(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/h.m4b", essence: "sa1", content: "sac1", title: "Tome", author: "Author",
		narrators: []string{"Narrator"}, asin: "BSA", durationMS: 100,
	})
	stats, err := st.Stats(ctx, "", 10)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	// Only the author is counted, mirroring the artist facet (the narrator is a
	// separate artist entity but is not surfaced by GroupArtist).
	if stats.Artists != 1 {
		t.Errorf("artists = %d, want 1 (author only)", stats.Artists)
	}
	f, _ := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "", 0, "")
	nonUnknown := 0
	for _, b := range f.Buckets {
		if !b.IsUnknown {
			nonUnknown++
		}
	}
	if stats.Artists != nonUnknown {
		t.Errorf("artist count %d != artist facet bucket count %d", stats.Artists, nonUnknown)
	}
}

// TestEqualStartChaptersCollapse: two scanned chapters at one instant keep only the
// last, the one the read path would have given the span to, so every chapter reads
// back with a real [start, end) span and the list round-trips through
// SetItemChapters, which refuses equal starts.
func TestEqualStartChaptersCollapse(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/marked.wma", essence: "eq1", content: "eqc1", title: "Marked", author: "Auth",
		asin: "BEQ", durationMS: 2000,
		chapters: []model.Chapter{
			{Position: 0, Title: "Preroll"},
			{Position: 1, Title: "Intro"},
			{Position: 2, Title: "Middle", FileStartMS: 750},
		},
	})
	chs, err := st.Chapters(ctx, r.ItemPID)
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	want := []model.Chapter{
		{Title: "Intro", StartMS: 0, EndMS: 750},
		{Title: "Middle", StartMS: 750, EndMS: 2000},
	}
	if len(chs) != len(want) {
		t.Fatalf("chapters = %+v, want %+v", chs, want)
	}
	for i, w := range want {
		if chs[i].Title != w.Title || chs[i].StartMS != w.StartMS || chs[i].EndMS != w.EndMS {
			t.Errorf("chapter %d = %q [%d, %d), want %q [%d, %d)", i, chs[i].Title, chs[i].StartMS, chs[i].EndMS, w.Title, w.StartMS, w.EndMS)
		}
	}
	if err := st.SetItemChapters(ctx, r.ItemPID, chs, model.LockUnchanged, false); err != nil {
		t.Errorf("round trip through SetItemChapters: %v", err)
	}
}

// TestBookPartCopyIsAnAlternate: a byte-identical copy of a book's part attaches to the
// book as an alternate of that part, so the parts, the chapter timeline and the running
// time read as before, and the copy is diagnosed against the part it copies.
func TestBookPartCopyIsAnAlternate(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int, dur int64, chapter string) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: dur,
			chapters: []model.Chapter{{Position: 0, Title: chapter}}}
	}
	p1, p2, pc := filepath.Join(root, "book", "part1.mp3"), filepath.Join(root, "book", "part2.mp3"), filepath.Join(root, "backup", "part2.mp3")
	r1 := putBook(t, st, lib.ID, part(p1, "mb1", "mc1", 1, 1000, "Part 1"))
	r2 := putBook(t, st, lib.ID, part(p2, "mb2", "mc2", 2, 2000, "Part 2"))
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}

	rc := putBook(t, st, lib.ID, part(pc, "mb2", "mc2-copy", 2, 2000, "Part 2"))
	if !rc.AttachedAsCopy || rc.ItemPID != r1.ItemPID || rc.ItemCreated {
		t.Fatalf("copy of part 2 = %+v, want it attached to %s", rc, r1.ItemPID)
	}
	d, err := st.BookByPID(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 || d.Files[1].FilePID != r2.FilePID || d.TotalDurationMS != 3000 || len(d.Chapters) != 2 {
		t.Errorf("book = %d parts (%+v), %d ms, %d chapters; want parts 1 and 2 only, 3000 ms, 2 chapters",
			len(d.Files), d.Files, d.TotalDurationMS, len(d.Chapters))
	}
	if d.Item.DurationMS != 3000 {
		t.Errorf("stored running time = %d ms, want 3000", d.Item.DurationMS)
	}
	refs, err := st.ItemFiles(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[model.PID]model.ItemFileRef{}
	for _, r := range refs {
		roles[r.FilePID] = r
	}
	if len(roles) != 3 || roles[r1.FilePID].Role != "primary" || roles[r2.FilePID].Role != "part" ||
		roles[rc.FilePID].Role != "alternate" || roles[rc.FilePID].Position != 2 {
		t.Errorf("edges = %+v, want parts 1 and 2 and the copy an alternate at position 2", refs)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: rc.FilePID, Code: model.DiagDuplicateCopy})
	if err != nil || len(ds) != 1 || ds[0].Detail != p2 {
		t.Errorf("copy diagnostics = %+v (err %v), want one naming %s", ds, err, p2)
	}
	cs, err := st.ChangesSince(ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	var items int
	for _, c := range cs {
		if c.EntityType == "item" {
			items++
		}
	}
	if items != 1 {
		t.Errorf("deltas = %+v, want one item update", cs)
	}
	if again := putBook(t, st, lib.ID, part(pc, "mb2", "mc2-copy", 2, 2000, "Part 2")); !again.AttachedAsCopy {
		t.Errorf("copy re-put = %+v, want it kept an alternate", again)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("verify = %+v (err %v), want consistent", rep, err)
	}
}

// TestBookPartCopyWaitsForReconciliation: a part's copy re-read while the part's path is
// missing stays an alternate (mid-walk the part may have moved), and reconciling the gone
// part is what puts the copy in its place.
func TestBookPartCopyWaitsForReconciliation(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int, dur int64) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: dur}
	}
	p1, p2, pc := filepath.Join(root, "book", "part1.mp3"), filepath.Join(root, "book", "part2.mp3"), filepath.Join(root, "backup", "part2.mp3")
	r1 := putBook(t, st, lib.ID, part(p1, "mb1", "mc1", 1, 1000))
	r2 := putBook(t, st, lib.ID, part(p2, "mb2", "mc2", 2, 2000))
	putBook(t, st, lib.ID, part(pc, "mb2", "mc2-copy", 2, 2000))
	if err := os.Remove(p2); err != nil {
		t.Fatal(err)
	}
	rc := putBook(t, st, lib.ID, bookSpec{path: pc, essence: "mb2", content: "mc2-copy", title: "Mistborn",
		author: "Brandon Sanderson", asin: "B0010", position: 2, durationMS: 2000})
	if !rc.AttachedAsCopy {
		t.Errorf("re-read copy = %+v, want it kept an alternate", rc)
	}
	if _, err := st.MarkFilesMissing(ctx, []model.PID{r2.FilePID}); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ItemFiles(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.PID]model.ItemFileRef{}
	for _, r := range refs {
		got[r.FilePID] = r
	}
	if len(refs) != 2 || got[rc.FilePID].Role != "part" || got[rc.FilePID].Position != 2 || got[r1.FilePID].Role != "primary" {
		t.Errorf("edges = %+v, want the copy in part 2's place and the gone row dropped", refs)
	}
	d, err := st.BookByPID(ctx, r1.ItemPID)
	if err != nil || len(d.Files) != 2 || d.TotalDurationMS != 3000 {
		t.Errorf("book = %+v (err %v), want two parts over 3000 ms", d, err)
	}
}

// TestBookPartCopyPromotedWhenThePartIsTrashed: trashing a part hands its place to its
// copy, at the part's position and without the copy's diagnostic, and a copy of another
// part never stands in for it.
func TestBookPartCopyPromotedWhenThePartIsTrashed(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int, dur int64) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: dur}
	}
	r1 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part1.mp3"), "mb1", "mc1", 1, 1000))
	r2 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part2.mp3"), "mb2", "mc2", 2, 2000))
	c1 := putBook(t, st, lib.ID, part(filepath.Join(root, "backup", "part1.mp3"), "mb1", "mc1-copy", 1, 1000))
	c2 := putBook(t, st, lib.ID, part(filepath.Join(root, "backup", "part2.mp3"), "mb2", "mc2-copy", 2, 2000))

	res, err := st.DetachFile(ctx, r2.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 1 || res.Promoted[0].FilePID != c2.FilePID {
		t.Fatalf("promoted = %+v, want part 2's copy %s", res.Promoted, c2.FilePID)
	}
	refs, err := st.ItemFiles(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.PID]model.ItemFileRef{}
	for _, r := range refs {
		got[r.FilePID] = r
	}
	if got[c2.FilePID].Role != "part" || got[c2.FilePID].Position != 2 || got[c1.FilePID].Role != "alternate" {
		t.Errorf("edges = %+v, want part 2's copy in its place and part 1's copy still an alternate", refs)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: c2.FilePID, Code: model.DiagDuplicateCopy})
	if err != nil || len(ds) != 0 {
		t.Errorf("promoted copy diagnostics = %+v (err %v), want none", ds, err)
	}
	if d, err := st.BookByPID(ctx, r1.ItemPID); err != nil || len(d.Files) != 2 || d.Item.DurationMS != 3000 {
		t.Errorf("book = %+v (err %v), want two parts over 3000 ms", d, err)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("verify = %+v (err %v), want consistent", rep, err)
	}
}

// TestBookPartCopyDiagnosticsFollowTheirPart: when part 1's copy takes the book's primary
// place, part 2's copy is still the same audio as part 2 and names it, not the new
// primary.
func TestBookPartCopyDiagnosticsFollowTheirPart(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: 1000}
	}
	p2 := filepath.Join(root, "book", "part2.mp3")
	r1 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part1.mp3"), "mb1", "mc1", 1))
	putBook(t, st, lib.ID, part(p2, "mb2", "mc2", 2))
	c1 := putBook(t, st, lib.ID, part(filepath.Join(root, "backup", "part1.mp3"), "mb1", "mc1-copy", 1))
	c2 := putBook(t, st, lib.ID, part(filepath.Join(root, "backup", "part2.mp3"), "mb2", "mc2-copy", 2))

	res, err := st.DetachFile(ctx, r1.FilePID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Promoted) != 1 || res.Promoted[0].FilePID != c1.FilePID {
		t.Fatalf("promoted = %+v, want part 1's copy %s", res.Promoted, c1.FilePID)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: c2.FilePID})
	if err != nil || len(ds) != 1 || ds[0].Code != model.DiagDuplicateCopy || ds[0].Detail != p2 {
		t.Errorf("part 2's copy diagnostics = %+v (err %v), want duplicate_copy naming %s", ds, err, p2)
	}
}

// TestBookPartCopyIsNotAnalyzed: a copy of any part, not only the primary's, holds audio
// the book already measures, so the analyze pass leaves it out.
func TestBookPartCopyIsNotAnalyzed(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: 1000}
	}
	putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part1.mp3"), "mb1", "mc1", 1))
	putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part2.mp3"), "mb2", "mc2", 2))
	c2 := putBook(t, st, lib.ID, part(filepath.Join(root, "backup", "part2.mp3"), "mb2", "mc2-copy", 2))
	files, err := st.FilesNeedingAnalysis(ctx, 1, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || slices.ContainsFunc(files, func(f *model.File) bool { return f.PID == c2.FilePID }) {
		t.Errorf("files needing analysis = %d, want the two parts and not part 2's copy %s", len(files), c2.FilePID)
	}
}

// TestBookDoubledPartFoldsOnRescan: a copy cataloged as a part of its own before copies
// became alternates (the book doubled its running time) folds into an alternate of its
// part when it is read again.
func TestBookDoubledPartFoldsOnRescan(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: 1000}
	}
	p2 := filepath.Join(root, "book", "part2.mp3")
	r1 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part1.mp3"), "mb1", "mc1", 1))
	putBook(t, st, lib.ID, part(p2, "mb2", "mc2", 2))
	spare := part(filepath.Join(root, "spare", "part2.mp3"), "mb2", "mc2-copy", 2)
	c2 := putBook(t, st, lib.ID, spare)
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		var itemID int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = ?", string(r1.ItemPID)).Scan(&itemID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE item_file SET role = 'part', position = 3
			WHERE file_id = (SELECT id FROM file WHERE pid = ?)`, string(c2.FilePID)); err != nil {
			return err
		}
		return refreshBookDuration(ctx, tx, itemID)
	}); err != nil {
		t.Fatal(err)
	}
	if d, err := st.BookByPID(ctx, r1.ItemPID); err != nil || d.Item.DurationMS != 3000 {
		t.Fatalf("doubled book = %+v (err %v), want 3000 ms", d, err)
	}

	res := putBook(t, st, lib.ID, spare)
	if !res.AttachedAsCopy || !res.Joined {
		t.Errorf("re-read = %+v, want the doubled part joined as a copy", res)
	}
	refs, err := st.ItemFiles(ctx, r1.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.FilePID == c2.FilePID && (r.Role != "alternate" || r.Position != 2) {
			t.Errorf("copy edge = %+v, want an alternate at position 2", r)
		}
	}
	if d, err := st.BookByPID(ctx, r1.ItemPID); err != nil || d.Item.DurationMS != 2000 {
		t.Errorf("book = %+v (err %v), want 2000 ms", d, err)
	}
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: c2.FilePID, Code: model.DiagDuplicateCopy})
	if err != nil || len(ds) != 1 || ds[0].Detail != p2 {
		t.Errorf("copy diagnostics = %+v (err %v), want duplicate_copy naming %s", ds, err, p2)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Errorf("verify = %+v (err %v), want consistent", rep, err)
	}
}

// TestBookCopyReadWithoutAnEssence: a book's alternate read again with no essence (so no
// part can be matched to it) stays an alternate of its book, rather than failing on the
// missing twin or becoming a second part of the same audio.
func TestBookCopyReadWithoutAnEssence(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: 1000}
	}
	r1 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "part1.mp3"), "mb1", "mc1", 1))
	spare := part(filepath.Join(root, "spare", "part1.mp3"), "mb1", "mc1-copy", 1)
	if c := putBook(t, st, lib.ID, spare); !c.AttachedAsCopy {
		t.Fatalf("copy = %+v, want an alternate", c)
	}
	spare.essence = ""
	res := putBook(t, st, lib.ID, spare)
	if !res.AttachedAsCopy || res.ItemPID != r1.ItemPID {
		t.Errorf("re-read = %+v, want still an alternate of %s", res, r1.ItemPID)
	}
	if n := scalarInt(t, st, `SELECT COUNT(*) FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		WHERE pi.pid = ? AND itf.role IN ('primary', 'part')`, string(r1.ItemPID)); n != 1 {
		t.Errorf("parts = %d, want the one", n)
	}
}

// TestBookCopyRevivesAMissingBook: a copy of a part arriving for a book reconciliation
// marked missing brings it back. With the part's file gone the copy takes its place; with
// the part back on disk the copy attaches as an alternate. Either way the book is present
// again and emits an update.
func TestBookCopyRevivesAMissingBook(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	newLib := func() (*model.Library, string) {
		root := t.TempDir()
		lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
		if err != nil {
			t.Fatal(err)
		}
		return lib, root
	}
	part := func(path, essence, content, asin string) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: asin, position: 1, durationMS: 1000}
	}
	lib, root := newLib()
	spareLib, spareRoot := newLib()
	updated := func(seq int64, pid model.PID) bool {
		changes, err := st.ChangesSince(ctx, seq)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(changes, func(c model.Change) bool { return c.EntityType == "item" && c.EntityPID == pid })
	}

	p1 := filepath.Join(root, "book", "part1.mp3")
	r1 := putBook(t, st, lib.ID, part(p1, "mb1", "mc1", "B0010"))
	if err := os.Remove(p1); err != nil {
		t.Fatal(err)
	}
	if mr, err := st.MarkFilesMissing(ctx, []model.PID{r1.FilePID}); err != nil || mr.Marked != 1 {
		t.Fatalf("reconcile = %+v (err %v), want the book missing", mr, err)
	}
	seq := scalarInt(t, st, "SELECT COALESCE(MAX(seq), 0) FROM change_log")
	c1 := putBook(t, st, spareLib.ID, part(filepath.Join(spareRoot, "part1.mp3"), "mb1", "mc1-copy", "B0010"))
	if c1.AttachedAsCopy || itemState(t, st, r1.ItemPID) != string(model.StatePresent) {
		t.Errorf("copy = %+v, state %s, want it in the gone part's place and the book present", c1, itemState(t, st, r1.ItemPID))
	}
	if !updated(int64(seq), r1.ItemPID) {
		t.Error("the revived book emitted no update")
	}

	// The part and its attached copy were both reconciled away and are back on disk: the
	// copy, read again first, attaches as before and the book is present again.
	q1 := filepath.Join(root, "other", "part1.mp3")
	s1 := putBook(t, st, lib.ID, part(q1, "mb2", "md1", "B0020"))
	spare := part(filepath.Join(spareRoot, "other.mp3"), "mb2", "md1-copy", "B0020")
	d1 := putBook(t, st, spareLib.ID, spare)
	if _, err := st.MarkFilesMissing(ctx, []model.PID{s1.FilePID, d1.FilePID}); err != nil ||
		itemState(t, st, s1.ItemPID) != string(model.StateMissing) {
		t.Fatalf("reconcile err %v, state %s, want the book missing", err, itemState(t, st, s1.ItemPID))
	}
	seq = scalarInt(t, st, "SELECT COALESCE(MAX(seq), 0) FROM change_log")
	d2 := putBook(t, st, spareLib.ID, spare)
	if !d2.AttachedAsCopy || itemState(t, st, s1.ItemPID) != string(model.StatePresent) {
		t.Errorf("copy beside a restored part = %+v, state %s, want an alternate and the book present", d2, itemState(t, st, s1.ItemPID))
	}
	if !updated(int64(seq), s1.ItemPID) {
		t.Error("the book the copy brought back emitted no update")
	}
}

// TestItemsWithCopiesReasons: a copy of a book part is the same audio, though it is not
// the primary's, and the item lists its parts before its alternates.
func TestItemsWithCopiesReasons(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	lib, err := st.EnsureLibrary(ctx, &model.Library{Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace})
	if err != nil {
		t.Fatal(err)
	}
	part := func(path, essence, content string, pos int) bookSpec {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return bookSpec{path: path, essence: essence, content: content, title: "Mistborn",
			author: "Brandon Sanderson", asin: "B0010", position: pos, durationMS: 1000}
	}
	r1 := putBook(t, st, lib.ID, part(filepath.Join(root, "book", "p1.mp3"), "mb1", "mc1", 1))
	putBook(t, st, lib.ID, part(filepath.Join(root, "book", "p2.mp3"), "mb2", "mc2", 2))
	rc := putBook(t, st, lib.ID, part(filepath.Join(root, "spare", "p2.mp3"), "mb2", "mc2c", 2))
	got, err := st.ItemsWithCopies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ItemPID != r1.ItemPID || len(got[0].Files) != 3 {
		t.Fatalf("items with copies = %+v, want the book with three files", got)
	}
	last := got[0].Files[2]
	if last.FilePID != rc.FilePID || last.Role != "alternate" || last.Reason != model.CopySameAudio ||
		got[0].Files[0].Reason != "" || got[0].Files[1].Reason != "" {
		t.Errorf("files = %+v, want the parts then the copy as the same audio", got[0].Files)
	}
}

// encodedPart is the put of a part of the book "Dune" at position pos in one encoding.
func encodedPart(libID int64, path, essence, codec string, pos int, durationMS int64) model.PutScannedBookInput {
	in := bookSpecInput(libID, bookSpec{path: path, essence: essence, content: essence + "-bytes",
		title: "Dune", author: "Frank Herbert", position: pos, durationMS: durationMS})
	in.File.Codec, in.File.SampleRate = codec, 44100
	if codec == "flac" {
		in.File.BitDepth, in.File.Bitrate = 16, 900
	} else {
		in.File.Bitrate = 320
	}
	return in
}

// TestBookPartEncodingsMakeOnePart: a FLAC and an MP3 of a one-part book, tagged alike, are
// one part and its other encoding whichever is read first: the better encoding is the part,
// the book runs as long as one of them, and the lesser one is diagnosed against it.
func TestBookPartEncodingsMakeOnePart(t *testing.T) {
	t.Parallel()
	for _, flacFirst := range []bool{true, false} {
		st, _ := entityFixture(t)
		ctx := context.Background()
		lib, file := diskLibrary(t, st)
		flac := encodedPart(lib.ID, file("Dune/01.flac", "f"), "de-flac", "flac", 1, 1000)
		mp3 := encodedPart(lib.ID, file("Dune/01.mp3", "m"), "de-mp3", "mp3", 1, 1012)
		order := []model.PutScannedBookInput{flac, mp3}
		if !flacFirst {
			order = []model.PutScannedBookInput{mp3, flac}
		}
		var book model.PID
		for _, in := range order {
			res, err := st.PutScannedBook(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			book = res.ItemPID
		}
		refs, err := st.ItemFiles(ctx, book)
		if err != nil {
			t.Fatal(err)
		}
		roles := map[string]string{}
		for _, r := range refs {
			roles[filepath.Ext(r.DisplayPath)] = r.Role
			if r.Position != 1 {
				t.Errorf("flac first %v: %s at position %d, want 1", flacFirst, r.DisplayPath, r.Position)
			}
		}
		if len(refs) != 2 || roles[".flac"] != "primary" || roles[".mp3"] != "alternate" {
			t.Errorf("flac first %v: edges = %v, want the FLAC the part and the MP3 its alternate", flacFirst, roles)
		}
		if d, err := st.BookByPID(ctx, book); err != nil || len(d.Files) != 1 || d.TotalDurationMS != 1000 {
			t.Errorf("flac first %v: book = %+v (err %v), want one part over 1000 ms", flacFirst, d, err)
		}
		var mp3PID model.PID
		for _, r := range refs {
			if filepath.Ext(r.DisplayPath) == ".mp3" {
				mp3PID = r.FilePID
			}
		}
		ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{FilePID: mp3PID, Code: model.DiagAlternateEncoding})
		if err != nil || len(ds) != 1 || ds[0].Detail != string(flac.File.Path) {
			t.Errorf("flac first %v: mp3 diagnostics = %+v (err %v), want one naming the FLAC", flacFirst, ds, err)
		}
		assertVerifyClean(t, st)
	}
}

// TestBookPartEncodingOfAnotherLengthIsAPart: two files at one position whose running
// times differ are two parts, not one part in two encodings.
func TestBookPartEncodingOfAnotherLengthIsAPart(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	for _, in := range []model.PutScannedBookInput{
		encodedPart(lib.ID, file("Dune/01.flac", "f"), "dl-flac", "flac", 1, 1000),
		encodedPart(lib.ID, file("Dune/01b.mp3", "m"), "dl-mp3", "mp3", 1, 60000),
	} {
		if _, err := st.PutScannedBook(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	books := listBooks(t, st)
	if len(books) != 1 {
		t.Fatalf("books = %d, want one", len(books))
	}
	if d, err := st.BookByPID(ctx, books[0]); err != nil || len(d.Files) != 2 {
		t.Errorf("book = %+v (err %v), want both files parts", d, err)
	}
}

// TestBookPartEncodingTakesTheLostPartsPlace: when a part leaves its book, another
// encoding of it the book holds takes its place rather than leaving a gap.
func TestBookPartEncodingTakesTheLostPartsPlace(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	flac := encodedPart(lib.ID, file("Dune/01.flac", "f"), "dp-flac", "flac", 1, 1000)
	var flacPID, mp3PID model.PID
	var book model.PID
	for _, in := range []model.PutScannedBookInput{
		flac,
		encodedPart(lib.ID, file("Dune/02.flac", "f2"), "dp-flac2", "flac", 2, 2000),
		encodedPart(lib.ID, file("Dune/01.mp3", "m"), "dp-mp3", "mp3", 1, 1000),
	} {
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		book = res.ItemPID
		switch string(in.File.Path) {
		case string(flac.File.Path):
			flacPID = res.FilePID
		case filepath.Join(filepath.Dir(string(flac.File.Path)), "01.mp3"):
			mp3PID = res.FilePID
		}
	}
	before, err := st.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range before {
		if r.FilePID == mp3PID && r.Role != "alternate" {
			t.Fatalf("mp3 = %s before the detach, want part 1's other encoding", r.Role)
		}
	}
	if _, err := st.DetachFile(ctx, flacPID); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	got := map[model.PID]model.ItemFileRef{}
	for _, r := range refs {
		got[r.FilePID] = r
	}
	if r := got[mp3PID]; r.Role == "alternate" || r.Position != 1 {
		t.Errorf("edges = %+v, want the MP3 in part 1's place", refs)
	}
	if d, err := st.BookByPID(ctx, book); err != nil || len(d.Files) != 2 || d.TotalDurationMS != 3000 {
		t.Errorf("book = %+v (err %v), want two parts over 3000 ms", d, err)
	}
}

// TestBookPartYieldsToABetterEncodingOnDisk: a FLAC read while its part's MP3 was away
// waits as an alternate, and the MP3 read again at its new path yields the part to it,
// so the walk order does not decide which encoding is the part.
func TestBookPartYieldsToABetterEncodingOnDisk(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	mp3 := encodedPart(lib.ID, file("Dune/01.mp3", "m"), "dy-mp3", "mp3", 1, 1000)
	res, err := st.PutScannedBook(ctx, mp3)
	if err != nil {
		t.Fatal(err)
	}
	book := res.ItemPID
	moved := file("Moved/01.mp3", "m")
	if err := os.Remove(string(mp3.File.Path)); err != nil {
		t.Fatal(err)
	}
	flac := encodedPart(lib.ID, file("Dune/01.flac", "f"), "dy-flac", "flac", 1, 1000)
	if out, err := st.PutScannedBook(ctx, flac); err != nil || !out.AttachedAsCopy {
		t.Fatalf("flac = %+v (err %v), want it waiting as an alternate while the MP3 is away", out, err)
	}
	mp3.File.Path, mp3.File.DisplayPath = []byte(moved), moved
	if _, err := st.PutScannedBook(ctx, mp3); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ItemFiles(ctx, book)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]string{}
	for _, r := range refs {
		roles[filepath.Ext(r.DisplayPath)] = r.Role
	}
	if len(refs) != 2 || roles[".flac"] != "primary" || roles[".mp3"] != "alternate" {
		t.Errorf("edges = %v, want the FLAC the part once the MP3 was read again", roles)
	}
}

// listBooks returns the pids of the catalog's books.
func listBooks(t *testing.T, st *Store) []model.PID {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(), "SELECT pid FROM playable_item WHERE kind = 'book' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []model.PID
	for rows.Next() {
		var p model.PID
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// TestBookPartsNumberedAlikeStayParts: parts of one rip that share a number and run as
// long are still parts, whether a splitter copied track 1 onto every file or they are
// back matter that all sorts last; only another encoding of a part is its alternate.
func TestBookPartsNumberedAlikeStayParts(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	for _, set := range []struct {
		title string
		pos   int
		files map[string]int64
	}{
		{"Emma", 1, map[string]int64{"Emma/Part 1.mp3": 180000, "Emma/Part 2.mp3": 180400}},
		{"Mansfield Park", 1000, map[string]int64{"Mansfield Park/Afterword.mp3": 60000, "Mansfield Park/Credits.mp3": 60000}},
	} {
		var book model.PID
		for rel, dur := range set.files {
			in := bookSpecInput(lib.ID, bookSpec{path: file(rel, rel), essence: "na-" + rel, content: rel,
				title: set.title, author: "Jane Austen", position: set.pos, durationMS: dur})
			in.File.Codec, in.File.SampleRate, in.File.Bitrate = "mp3", 44100, 128
			res, err := st.PutScannedBook(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			book = res.ItemPID
		}
		if d, err := st.BookByPID(ctx, book); err != nil || len(d.Files) != 2 {
			t.Errorf("%s = %+v (err %v), want two parts", set.title, d, err)
		}
	}
}

// TestBookPartsInUnrecognizedDiscFoldersStayParts: two files of one name and codec at one
// position, in disc folders the scan does not read as discs, are two parts: the same codec
// never makes one an encoding of the other.
func TestBookPartsInUnrecognizedDiscFoldersStayParts(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	var book model.PID
	for rel, dur := range map[string]int64{"Dune/Part 1/01.mp3": 1000, "Dune/Part 2/01.mp3": 1004} {
		in := bookSpecInput(lib.ID, bookSpec{path: file(rel, rel), essence: "ud-" + rel, content: rel,
			title: "Dune", author: "Frank Herbert", position: 1, durationMS: dur})
		in.File.Codec, in.File.SampleRate, in.File.Bitrate = "mp3", 44100, 128
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		book = res.ItemPID
	}
	if d, err := st.BookByPID(ctx, book); err != nil || len(d.Files) != 2 {
		t.Errorf("book = %+v (err %v), want two parts", d, err)
	}
}

// TestBookPartWithAnUnknownCodecIsAPart: a file whose codec was never recorded is no
// encoding apart from another, so two parts sharing a number stay two parts.
func TestBookPartWithAnUnknownCodecIsAPart(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	var book model.PID
	for rel, codec := range map[string]string{"Emma/Part 1.mp3": "mp3", "Emma/Part 2.mp3": ""} {
		in := bookSpecInput(lib.ID, bookSpec{path: file(rel, rel), essence: "uc-" + rel, content: rel,
			title: "Emma", author: "Jane Austen", position: 1, durationMS: 1000})
		in.File.Codec = codec
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		book = res.ItemPID
	}
	if d, err := st.BookByPID(ctx, book); err != nil || len(d.Files) != 2 {
		t.Errorf("book = %+v (err %v), want two parts", d, err)
	}
}

// TestUnnumberedBookEncodingsMakeOnePart: a single-file book with no part number, in two
// encodings, is one part and its alternate, not two parts of twice the length.
func TestUnnumberedBookEncodingsMakeOnePart(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	var book model.PID
	for _, in := range []model.PutScannedBookInput{
		encodedPart(lib.ID, file("Dune/Dune.mp3", "m"), "un-mp3", "mp3", 0, 1012),
		encodedPart(lib.ID, file("Dune/Dune.flac", "f"), "un-flac", "flac", 0, 1000),
	} {
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		book = res.ItemPID
	}
	d, err := st.BookByPID(ctx, book)
	if err != nil || len(d.Files) != 1 || filepath.Ext(d.Files[0].DisplayPath) != ".flac" || d.TotalDurationMS != 1000 {
		t.Errorf("book = %+v (err %v), want the FLAC its one part over 1000 ms", d, err)
	}
}

// TestMissingPartHandsItsPlaceToAnEncoding: an encoding read while its part's file had
// gone waits as an alternate, and the scan's reconciliation of the gone part puts it in
// the part's place.
func TestMissingPartHandsItsPlaceToAnEncoding(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	mp3 := encodedPart(lib.ID, file("Dune/01.mp3", "m"), "mp-mp3", "mp3", 1, 1000)
	res, err := st.PutScannedBook(ctx, mp3)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(string(mp3.File.Path)); err != nil {
		t.Fatal(err)
	}
	flac, err := st.PutScannedBook(ctx, encodedPart(lib.ID, file("Dune/01.flac", "f"), "mp-flac", "flac", 1, 1000))
	if err != nil || !flac.AttachedAsCopy {
		t.Fatalf("flac = %+v (err %v), want it waiting as an alternate", flac, err)
	}
	if _, err := st.MarkFilesMissing(ctx, []model.PID{res.FilePID}); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ItemFiles(ctx, res.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].FilePID != flac.FilePID || refs[0].Role != "primary" {
		t.Errorf("edges = %+v, want the FLAC in the gone part's place", refs)
	}
	if it, err := st.ItemByPID(ctx, res.ItemPID); err != nil || it.State != model.StatePresent {
		t.Errorf("book = %+v (err %v), want it present", it, err)
	}
}

// TestBookPartLossSkipsAnAlternateOfAnotherLength: losing a part promotes an alternate at
// its position only when it runs as long, so a file of another length waits there as an
// alternate rather than standing in for audio it does not hold.
func TestBookPartLossSkipsAnAlternateOfAnotherLength(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	put := func(rel, codec string, pos int, dur int64) *model.ScanItemResult {
		t.Helper()
		in := bookSpecInput(lib.ID, bookSpec{path: file(rel, rel), essence: "ls-" + rel, content: rel,
			title: "Dune", author: "Frank Herbert", position: pos, durationMS: dur})
		in.File.Codec, in.File.SampleRate = codec, 44100
		res, err := st.PutScannedBook(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	one := put("Dune/01.flac", "flac", 1, 1000)
	put("Dune/02.flac", "flac", 2, 2000)
	short := put("Dune/01.mp3", "mp3", 1, 1000)
	// The alternate's file later reads 30 s long: it holds other audio than part 1.
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE file SET duration_ms = 30000 WHERE pid = ?", string(short.FilePID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DetachFile(ctx, one.FilePID); err != nil {
		t.Fatal(err)
	}
	refs, err := st.ItemFiles(ctx, one.ItemPID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refs {
		if r.FilePID == short.FilePID && r.Role != "alternate" {
			t.Errorf("the other-length file = %s, want it left an alternate", r.Role)
		}
	}
}

// TestCopyTakingAGonePartsPlaceMovesNoPlace: a copy that takes a gone part's place in a
// missing book, arriving from an item of its own, leaves the book's places alone, since
// the timeline holds the same audio as before.
func TestCopyTakingAGonePartsPlaceMovesNoPlace(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	lib, file := diskLibrary(t, st)
	backup, backupFile := diskLibrary(t, st)
	p1, p2 := file("T/01.mp3", "gc1"), file("T/02.mp3", "gc2")
	first := putBook(t, st, lib.ID, bookSpec{path: p1, essence: "ge1", content: "gc1", title: "T", author: "A", position: 1, durationMS: 1000})
	second := putBook(t, st, lib.ID, bookSpec{path: p2, essence: "ge2", content: "gc2", title: "T", author: "A", position: 2, durationMS: 1000})
	if err := st.SetProgress(ctx, "", first.ItemPID, 1500, nil); err != nil {
		t.Fatal(err)
	}
	cp := backupFile("T/01.mp3", "gc1b")
	putTrack(t, st, backup.ID, trackSpec{path: cp, essence: "ge1", content: "gc1b", title: "One", durationMS: 1000})
	for _, p := range []string{p1, p2} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.MarkFilesMissing(ctx, []model.PID{first.FilePID, second.FilePID}); err != nil {
		t.Fatal(err)
	}
	putBook(t, st, backup.ID, bookSpec{path: cp, essence: "ge1", content: "gc1b", title: "T", author: "A", position: 1, durationMS: 1000})
	if ps, err := st.PlayStateFor(ctx, "", first.ItemPID); err != nil || ps.PositionMS != 1500 {
		t.Errorf("book state = %+v (err %v), want the place left at 1500", ps, err)
	}
}

// TestPartLeavingForNoItemLeavesItsPlacesAtItsStart: a part that leaves its book for no
// item (its file now a cue rip) takes no place along, and a place inside it moves to where
// it started rather than into the next part's audio.
func TestPartLeavingForNoItemLeavesItsPlacesAtItsStart(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	var book model.PID
	for i, n := range []string{"01", "02", "03"} {
		book = putBook(t, st, lib.ID, bookSpec{path: "/lib/A/T/" + n + ".flac", essence: "rp" + n, content: "rc" + n,
			title: "T", author: "A", position: i + 1, durationMS: 1000}).ItemPID
	}
	if err := st.SetProgress(ctx, "", book, 1500, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddBookmark(ctx, "", book, 2500, "mark"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackSpecInput(lib.ID, "/lib/A/T/02.flac", "rp02", "rc02", 1000,
		[][2]int64{{0, 22050}, {22050, 0}})); err != nil {
		t.Fatal(err)
	}
	if ps, err := st.PlayStateFor(ctx, "", book); err != nil || ps.PositionMS != 1000 {
		t.Errorf("book state = %+v (err %v), want the place at part 2's old start, 1000", ps, err)
	}
	if marks, err := st.Bookmarks(ctx, "", book); err != nil || len(marks) != 1 || marks[0].PositionMS != 1500 {
		t.Errorf("bookmarks = %+v (err %v), want the later mark moved back to 1500", marks, err)
	}
}

// vtrackSpecInput is the put of a rip of one file cut at windows, one cue track a window.
func vtrackSpecInput(libID int64, path, essence, content string, dur int64, windows [][2]int64) model.PutScannedVirtualTracksInput {
	tracks := make([]model.VirtualTrack, len(windows))
	for i, w := range windows {
		title := "Track " + string(rune('1'+i))
		tracks[i] = model.VirtualTrack{
			Item: model.PlayableItem{Kind: model.KindTrack, State: model.StatePresent, Title: title,
				SortKey: model.SortKey(title), IdentityKey: identity.VirtualTrackKey(essence, i+1, w[0])},
			Track:       model.Track{Artist: "A", AlbumArtist: "A", Album: "Rip", TrackNo: i + 1},
			StartFrames: w[0], EndFrames: w[1],
		}
	}
	return model.PutScannedVirtualTracksInput{
		LibraryID: libID,
		File: model.File{Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1, ContentHash: content, EssenceHash: essence,
			DurationMS: dur, ScanState: model.ScanIndexed},
		Tracks: tracks,
	}
}
