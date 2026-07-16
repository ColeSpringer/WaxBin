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
// re-resolves the author contributor and shows up in the read view, and a write-back
// on a book is refused, since audiobook tags need their own design, while the catalog
// edit still stands.
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
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
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

	// Write-back on a book is refused, and the catalog edit still applies.
	err = lib.EditField(ctx, pid, "subtitle", "There and Back Again", waxbin.EditOptions{Lock: true, WriteBack: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("book write-back: want *WriteBackError, got %v", err)
	}
	d, _ = lib.Book(ctx, pid)
	if d.Subtitle != "There and Back Again" {
		t.Fatalf("subtitle = %q, want the edit applied despite refused write-back", d.Subtitle)
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
	res, err := raw.ExecContext(ctx, `UPDATE item_file SET end_ms = 1000
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
