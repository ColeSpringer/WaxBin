package sqlite

import (
	"context"
	"path/filepath"
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
	year                   int
	genres                 []string
	position               int
	durationMS             int64
	chapters               []model.Chapter
}

func putBook(t *testing.T, st *Store, libID int64, s bookSpec) *model.ScanItemResult {
	t.Helper()
	key := identity.BookKey(s.asin, s.isbn, s.author, s.title, s.edition)
	if key == "" {
		key = "essence:" + s.essence
	}
	genre := ""
	if len(s.genres) > 0 {
		genre = s.genres[0]
	}
	in := model.PutScannedBookInput{
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
			ASIN: s.asin, ISBN: s.isbn, Edition: s.edition, Year: s.year,
			Genres: s.genres, Genre: genre,
		},
		Position: s.position,
		Chapters: s.chapters,
	}
	res, err := st.PutScannedBook(context.Background(), in)
	if err != nil {
		t.Fatalf("put book %s: %v", s.path, err)
	}
	return res
}

func TestPutScannedBookSingleFile(t *testing.T) {
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
	f, err := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "")
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

func TestZeroDurationPartAdvancesTimeline(t *testing.T) {
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
	st, lib := entityFixture(t)
	ctx := context.Background()
	res := putBook(t, st, lib.ID, bookSpec{
		path: "/lib/b/h.m4b", essence: "sb1", content: "sbc1", title: "The Hobbit",
		author: "J.R.R. Tolkien", asin: "BH2", year: 1937, durationMS: 1000,
	})

	// Stats acknowledges the book and a played book shows its author, not a blank.
	if err := st.MarkPlayed(ctx, "", res.ItemPID, true); err != nil {
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
	if err := st.DetachFile(ctx, p2pid); err != nil {
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
	f, _ := st.Facet(ctx, query.New(query.EntityItems).Build(), read.GroupArtist, "")
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
