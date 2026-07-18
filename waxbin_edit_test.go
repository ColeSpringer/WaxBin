package waxbin_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEditFieldDBOnly verifies a catalog-only edit updates the catalog and locks the
// field without touching the file's on-disk tags.
func TestEditFieldDBOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	if err := lib.EditField(ctx, pid, "artist", "New Artist", waxbin.EditOptions{Lock: true}); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// Catalog reflects the edit.
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Artist != "New Artist" {
		t.Fatalf("catalog artist = %q, want New Artist", v.Artist)
	}
	// Provenance records a locked user edit.
	prov, _ := lib.Provenance(ctx, pid)
	if len(prov) != 1 || prov[0].Field != "artist" || prov[0].Source != model.SourceUser || !prov[0].Locked {
		t.Fatalf("provenance = %+v, want one locked user artist row", prov)
	}
	// On-disk tags are UNCHANGED (DB-only edit).
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Artist != "Old Artist" {
		t.Fatalf("on-disk artist = %q, want Old Artist (DB-only edit must not write tags)", fm.Tags.Artist)
	}
}

// TestEditFieldWriteBack verifies --write-back mirrors the edit into the on-disk
// tags, readable by re-parsing the file.
func TestEditFieldWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	if err := lib.EditFields(ctx, pid, map[string]string{
		"artist": "New Artist", "genre": "Jazz",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("edit with write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Artist != "New Artist" {
		t.Errorf("on-disk artist = %q, want New Artist", fm.Tags.Artist)
	}
	if fm.Tags.Genre != "Jazz" {
		t.Errorf("on-disk genre = %q, want Jazz", fm.Tags.Genre)
	}
}

// TestEditWriteBackSharedFileRefused verifies write-back to a file with an offset
// window (a virtual/shared file whose tags are global to it) is refused with a
// WriteBackError while the catalog edit still lands and a drift diagnostic is
// recorded.
func TestEditWriteBackSharedFileRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Original", "Old Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	// Give the item's backing file an offset window so it reads as virtual/shared.
	makeBackingFileVirtual(t, ctx, db, pid)

	err := lib.EditFields(ctx, pid, map[string]string{"artist": "New Artist"},
		waxbin.EditOptions{Lock: true, WriteBack: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("want *WriteBackError for a shared file, got %v", err)
	}
	if len(wbErr.Failures) != 1 {
		t.Fatalf("write-back failures = %d, want 1", len(wbErr.Failures))
	}

	// The catalog edit still applied.
	v, _ := lib.Get(ctx, pid)
	if v.Artist != "New Artist" {
		t.Errorf("catalog artist = %q, want New Artist (edit must apply even when write-back is refused)", v.Artist)
	}
	// On-disk tags were NOT clobbered.
	fm, _ := meta.NewReader().Read(ctx, src)
	if fm.Tags.Artist != "Old Artist" {
		t.Errorf("on-disk artist = %q, want Old Artist (shared file must not be rewritten)", fm.Tags.Artist)
	}
	// The drift is recorded as a queryable per-file diagnostic.
	if n := countEditDiagnostics(t, ctx, db); n != 1 {
		t.Errorf("edit-origin diagnostics = %d, want 1 (drift must be queryable)", n)
	}
}

// TestEditBookFacade exercises book-field editing through the facade. A DB-only edit
// re-resolves the author contributor and shows up in the read view, and a write-back on
// a DB-only book field such as subtitle is a clean no-op: the field has no on-disk tag a
// scan reads back, so nothing is written and the catalog edit stands.
func TestEditBookFacade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// A .m4b holding valid MP3 bytes classifies as a book. Album is the title and the
	// album artist is the author.
	src := filepath.Join(root, "the-hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "JRR Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}
	pid := books[0].PID

	// DB-only author edit re-resolves the contributor and reflects in the view.
	if err := lib.EditField(ctx, pid, "author", "John Ronald Tolkien", waxbin.EditOptions{Lock: true}); err != nil {
		t.Fatalf("edit book author: %v", err)
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book detail: %v", err)
	}
	if len(d.Authors) != 1 || d.Authors[0] != "John Ronald Tolkien" {
		t.Fatalf("authors = %v, want [John Ronald Tolkien]", d.Authors)
	}
	prov, _ := lib.Provenance(ctx, pid)
	if len(prov) != 1 || prov[0].Field != "author" || !prov[0].Locked {
		t.Fatalf("provenance = %+v, want one locked author row", prov)
	}

	// subtitle is a DB-only book field (no tag a scan reconstructs it from), so a
	// write-back writes nothing on disk and returns no error while the edit stands.
	if err := lib.EditField(ctx, pid, "subtitle", "There and Back Again", waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("book subtitle write-back should be a clean no-op, got %v", err)
	}
	d, _ = lib.Book(ctx, pid)
	if d.Subtitle != "There and Back Again" {
		t.Fatalf("subtitle = %q, want the edit applied", d.Subtitle)
	}
	// The on-disk book title (ALBUM) is untouched: the edited fields so far are DB-only.
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Album != "The Hobbit" {
		t.Fatalf("on-disk ALBUM = %q, want The Hobbit (DB-only edits must not write tags)", fm.Tags.Album)
	}
}

// TestEditBookWriteBackRoundTrip verifies a book field write-back embeds the audiobook
// tags a scan reads back (title→ALBUM, author→ALBUMARTIST, narrator→NARRATOR,
// series→GROUPING, genre→GENRE) into the primary part, and that a fresh scan of the
// rewritten file reconstructs the same catalog values from those tags.
func TestEditBookWriteBackRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "JRR Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}
	pid := books[0].PID

	if err := lib.EditFields(ctx, pid, map[string]string{
		"title":    "The Hobbit: Illustrated",
		"author":   "J.R.R. Tolkien",
		"narrator": "Andy Serkis",
		"series":   "Middle-earth",
		"genre":    "Fantasy",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("book write-back: %v", err)
	}

	// The audiobook tags were embedded into the file.
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Album != "The Hobbit: Illustrated" {
		t.Errorf("on-disk ALBUM = %q, want the edited title", fm.Tags.Album)
	}
	if fm.Tags.AlbumArtist != "J.R.R. Tolkien" {
		t.Errorf("on-disk ALBUMARTIST = %q, want the edited author", fm.Tags.AlbumArtist)
	}
	if len(fm.Tags.Narrators) != 1 || fm.Tags.Narrators[0] != "Andy Serkis" {
		t.Errorf("on-disk narrators = %v, want [Andy Serkis]", fm.Tags.Narrators)
	}
	if fm.Tags.Series != "Middle-earth" {
		t.Errorf("on-disk series (GROUPING) = %q, want Middle-earth", fm.Tags.Series)
	}
	if fm.Tags.Genre != "Fantasy" {
		t.Errorf("on-disk GENRE = %q, want Fantasy", fm.Tags.Genre)
	}

	// A fresh scan into a new catalog reconstructs the book from the embedded tags,
	// proving the write-back round-trips through the scanner.
	db2 := filepath.Join(t.TempDir(), "catalog2.db")
	lib2 := openManaged(t, ctx, db2, root)
	if _, err := lib2.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	books2, err := lib2.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books2) != 1 {
		t.Fatalf("rescan book query: %d books (err %v)", len(books2), err)
	}
	d2, err := lib2.Book(ctx, books2[0].PID)
	if err != nil {
		t.Fatalf("rescanned book detail: %v", err)
	}
	if d2.Item.Title != "The Hobbit: Illustrated" {
		t.Errorf("rescanned title = %q, want The Hobbit: Illustrated", d2.Item.Title)
	}
	if len(d2.Authors) != 1 || d2.Authors[0] != "J.R.R. Tolkien" {
		t.Errorf("rescanned authors = %v, want [J.R.R. Tolkien]", d2.Authors)
	}
	if len(d2.Narrators) != 1 || d2.Narrators[0] != "Andy Serkis" {
		t.Errorf("rescanned narrators = %v, want [Andy Serkis]", d2.Narrators)
	}
	if d2.Series != "Middle-earth" {
		t.Errorf("rescanned series = %q, want Middle-earth", d2.Series)
	}
}

// TestEditBookWriteBackReanchorsIdentity verifies that writing a book's title and author
// (its identity anchor) to disk re-anchors the catalog's identity key, so a same-catalog
// scan --force resolves the same item and keeps its pid and its locks, rather than
// re-keying to the new on-disk title and dropping the curation.
func TestEditBookWriteBackReanchorsIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "JRR Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}
	pid := books[0].PID

	// Edit the two identity fields and propagate them to disk.
	if err := lib.EditFields(ctx, pid, map[string]string{
		"title":  "The Hobbit: Illustrated",
		"author": "J.R.R. Tolkien",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("book identity write-back: %v", err)
	}

	// A full re-derive from disk must resolve the same item, not a re-keyed new one.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan --force: %v", err)
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book pid did not survive scan --force (re-anchor failed): %v", err)
	}
	if d.Item.Title != "The Hobbit: Illustrated" {
		t.Errorf("title after re-anchor = %q, want the edited title", d.Item.Title)
	}
	// The locks must survive: the item was not re-created.
	prov, _ := lib.Provenance(ctx, pid)
	locked := map[string]bool{}
	for _, p := range prov {
		if p.Locked {
			locked[p.Field] = true
		}
	}
	if !locked["title"] || !locked["author"] {
		t.Errorf("locks after scan --force = %+v, want title and author still locked", prov)
	}
}

// TestEditBookWriteBackMultiFileStaysWhole verifies a multi-file book's identity-field
// write-back writes every part (not just the primary), so the parts keep a single shared
// identity key and a scan --force resolves one whole book rather than splitting it.
func TestEditBookWriteBackMultiFileStaysWhole(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Three .m4b parts of one book: same album/author, distinct essence, distinct part
	// titles (the file TITLE holds a part name; the book title is the ALBUM).
	for i, seed := range []byte{11, 12, 13} {
		p := filepath.Join(root, "part"+string(rune('1'+i))+".m4b")
		writeFile(t, p, testaudio.BuildMP3WithAudio("Chapter "+string(rune('1'+i)), "Tolkien", "The Hobbit", i+1, testaudio.AudioWithSeed(seed)))
	}

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v), want 1", len(books), err)
	}
	pid := books[0].PID
	d0, _ := lib.Book(ctx, pid)
	if len(d0.Files) != 3 {
		t.Fatalf("book parts = %d, want 3", len(d0.Files))
	}

	// Edit the identity fields and propagate to disk, then re-derive from disk.
	if err := lib.EditFields(ctx, pid, map[string]string{
		"title":  "The Hobbit Deluxe",
		"author": "J.R.R. Tolkien",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("book identity write-back: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan --force: %v", err)
	}

	// Exactly one book remains (no split), it is the same item, and it kept all 3 parts.
	books2, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books2) != 1 {
		t.Fatalf("after write-back: %d books, want 1 (a split means the parts diverged)", len(books2))
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("original book pid did not survive scan --force: %v", err)
	}
	if len(d.Files) != 3 {
		t.Errorf("book parts after = %d, want 3 (a part was lost to a split)", len(d.Files))
	}
	if d.Item.Title != "The Hobbit Deluxe" {
		t.Errorf("title = %q, want The Hobbit Deluxe", d.Item.Title)
	}
}

// TestEditWriteBackNoFiles verifies that a --write-back on a track item with no
// backing files reports a skipped write-back (not a silent success) while the catalog
// edit still applies.
func TestEditWriteBackNoFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "A")

	// Detach the item's backing file so it has none, then edit with write-back.
	detachBackingFiles(t, ctx, db, pid)

	err := lib.EditField(ctx, pid, "title", "Renamed", waxbin.EditOptions{Lock: true, WriteBack: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("want *WriteBackError for a fileless item, got %v", err)
	}
	if len(wbErr.Failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(wbErr.Failures))
	}
	// The catalog edit still applied.
	v, _ := lib.Get(ctx, pid)
	if v.Title != "Renamed" {
		t.Errorf("title = %q, want Renamed (edit applies even with no file to write)", v.Title)
	}
}

// detachBackingFiles removes an item's item_file edges via a direct connection, so
// the item has no backing files. The library's write connection is idle after a scan.
func detachBackingFiles(t *testing.T, ctx context.Context, db string, pid model.PID) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx,
		"DELETE FROM item_file WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)", string(pid)); err != nil {
		t.Fatalf("detach files: %v", err)
	}
}

// makeBackingFileVirtual gives the item's primary backing file an offset window, so
// FileSharedOrVirtual reports it as unsafe to write per item. It uses a direct
// connection; the library's write connection is idle after a scan, so the brief
// write does not contend.
func makeBackingFileVirtual(t *testing.T, ctx context.Context, db string, pid model.PID) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	res, err := raw.ExecContext(ctx, `UPDATE item_file SET end_frames = 75
		WHERE role = 'primary' AND item_id = (SELECT id FROM playable_item WHERE pid = ?)`, string(pid))
	if err != nil {
		t.Fatalf("mark virtual: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("marked %d edges virtual, want 1", n)
	}
}

// countEditDiagnostics counts edit-origin file diagnostics via a direct read.
func countEditDiagnostics(t *testing.T, ctx context.Context, db string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_diagnostic WHERE origin='edit'").Scan(&n); err != nil {
		t.Fatalf("count diagnostics: %v", err)
	}
	return n
}

// TestEditReadOnlyRefused verifies a read-only library refuses the edit before any
// write-back.
func TestEditReadOnlyRefused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "A")
	_ = lib.Close()

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()
	if err := ro.EditField(ctx, pid, "title", "X", waxbin.EditOptions{}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("read-only edit: want CodeUnsupported, got %v", err)
	}
}
