package waxbin_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// TestEditFieldDBOnly verifies a catalog-only edit updates the catalog and locks the
// field without touching the file's on-disk tags.
func TestEditFieldDBOnly(t *testing.T) {
	t.Parallel()
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

	if err := lib.EditField(ctx, pid, "artist", "New Artist", waxbin.EditOptions{Lock: model.LockOn}); err != nil {
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
	t.Parallel()
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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

// TestEditWriteBackNormalizesIdentifier verifies an identifier edit stores and
// writes the canonical form: the loosely-spelled ISRC lands normalized in the
// catalog column and in the file's tags alike, so the two can never diverge.
func TestEditWriteBackNormalizesIdentifier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Original", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Original")

	if err := lib.EditFields(ctx, pid, map[string]string{"isrc": "us-rc1-77-00001"},
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("edit with write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.ISRC != "USRC17700001" {
		t.Errorf("on-disk isrc = %q, want the normalized USRC17700001", fm.Tags.ISRC)
	}
	prov, _ := lib.Provenance(ctx, pid)
	for _, p := range prov {
		if p.Field == "isrc" && p.Value != "USRC17700001" {
			t.Errorf("provenance value = %q, want the normalized form", p.Value)
		}
	}

	// A malformed identifier is a usage error, refused before the catalog write.
	if err := lib.EditFields(ctx, pid, map[string]string{"isrc": "nope"},
		waxbin.EditOptions{Lock: model.LockOn, Force: true}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("malformed isrc = %v, want CodeInvalid", err)
	}
}

// TestEditItemsFieldsWriteBackPerItem verifies the per-item-map batch mirrors
// each item's own values to disk, and a single item whose file refuses the
// write (a shared/virtual file) is reported under its pid while the atomic
// catalog batch and the sibling's on-disk sync both stand.
func TestEditItemsFieldsWriteBackPerItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	one := filepath.Join(root, "one.mp3")
	two := filepath.Join(root, "two.mp3")
	writeFile(t, one, testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, two, testaudio.BuildMP3WithAudio("Two", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	p1 := itemPIDByTitle(t, ctx, lib, "One")
	p2 := itemPIDByTitle(t, ctx, lib, "Two")

	// The second item's backing file reads as virtual/shared, so its write-back
	// is refused while the first proceeds.
	makeBackingFileVirtual(t, ctx, db, p2)

	res, err := lib.EditItemsFields(ctx, []model.ItemFieldEdit{
		{ItemPID: p1, Fields: map[string]string{"comment": "first note"}},
		{ItemPID: p2, Fields: map[string]string{"comment": "second note"}},
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Edited) != 2 {
		t.Fatalf("edited = %v, want both (catalog batch is atomic)", res.Edited)
	}
	if len(res.WriteBackErrors) != 1 || res.WriteBackErrors[p2] == nil {
		t.Fatalf("write-back errors = %v, want exactly the refused item %s", res.WriteBackErrors, p2)
	}

	// The first item's file carries ITS map's value; the second's stays untouched.
	fm, err := meta.NewReader().Read(ctx, one)
	if err != nil {
		t.Fatalf("read one: %v", err)
	}
	if fm.Tags.Comment != "first note" {
		t.Errorf("one comment = %q, want the item's own value", fm.Tags.Comment)
	}
	fm, err = meta.NewReader().Read(ctx, two)
	if err != nil {
		t.Fatalf("read two: %v", err)
	}
	if fm.Tags.Comment == "second note" {
		t.Error("the refused file was written anyway")
	}
	// Both catalog rows carry their own values regardless.
	for pid, want := range map[model.PID]string{p1: "first note", p2: "second note"} {
		prov, err := lib.Provenance(ctx, pid)
		if err != nil {
			t.Fatalf("provenance %s: %v", pid, err)
		}
		found := false
		for _, p := range prov {
			if p.Field == "comment" && p.Value == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s provenance lacks comment=%q", pid, want)
		}
	}
}

// TestEditWriteBackSharedFileRefused verifies write-back to a file with an offset
// window (a virtual/shared file whose tags are global to it) is refused with a
// WriteBackError while the catalog edit still lands and a drift diagnostic is
// recorded.
func TestEditWriteBackSharedFileRefused(t *testing.T) {
	t.Parallel()
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
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
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
	t.Parallel()
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
	if err := lib.EditField(ctx, pid, "author", "John Ronald Tolkien", waxbin.EditOptions{Lock: model.LockOn}); err != nil {
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

	// A subtitle write-back lands its own SUBTITLE key, while the catalog-only author
	// edit before it left the on-disk ALBUMARTIST alone.
	if err := lib.EditField(ctx, pid, "subtitle", "There and Back Again", waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("book subtitle write-back: %v", err)
	}
	d, _ = lib.Book(ctx, pid)
	if d.Subtitle != "There and Back Again" {
		t.Fatalf("subtitle = %q, want the edit applied", d.Subtitle)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	meta.PromoteBookFields(&fm.Tags)
	if fm.Tags.Subtitle != "There and Back Again" {
		t.Errorf("on-disk SUBTITLE = %q, want the edited subtitle", fm.Tags.Subtitle)
	}
	if fm.Tags.AlbumArtist != "JRR Tolkien" {
		t.Errorf("on-disk ALBUMARTIST = %q, want JRR Tolkien (a catalog-only edit must not write tags)", fm.Tags.AlbumArtist)
	}
}

// TestEditBookWriteBackRoundTrip verifies a book field write-back embeds the audiobook
// tags a scan reads back (title→ALBUM, author→ALBUMARTIST, narrator→NARRATOR,
// series→GROUPING, genre→GENRE) into the primary part, and that a fresh scan of the
// rewritten file reconstructs the same catalog values from those tags.
func TestEditBookWriteBackRoundTrip(t *testing.T) {
	t.Parallel()
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("book write-back: %v", err)
	}

	// The audiobook tags were embedded into the file.
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	meta.PromoteBookFields(&fm.Tags)
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
	t.Parallel()
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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
	t.Parallel()
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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
	t.Parallel()
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

	err := lib.EditField(ctx, pid, "title", "Renamed", waxbin.EditOptions{Lock: model.LockOn, WriteBack: true})
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

// storedIdentityKey reads an item's identity key via a direct read.
func storedIdentityKey(t *testing.T, ctx context.Context, db string, pid model.PID) string {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var key string
	if err := raw.QueryRowContext(ctx, "SELECT identity_key FROM playable_item WHERE pid = ?", string(pid)).Scan(&key); err != nil {
		t.Fatalf("identity key: %v", err)
	}
	return key
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
	t.Parallel()
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

// TestEditBookIdentifierReanchors covers the trap that opened when asin and isbn
// started reaching disk: they feed identity.BookKey, so an edit that writes them
// without re-anchoring the stored key leaves the next scan resolving a different item,
// orphaning this one's pid and everything hanging off it.
func TestEditBookIdentifierReanchors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "J.R.R. Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "The Hobbit")

	// Hyphenated on the way in: the catalog normalizes, and the tag must carry the same
	// value or the next rescan reads the raw form back over it.
	if err := lib.EditFields(ctx, pid, map[string]string{"isbn": "978-0-261-10221-7"},
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("edit isbn: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	meta.PromoteBookFields(&fm.Tags)
	if fm.Tags.ISBN != "9780261102217" {
		t.Errorf("on-disk ISBN = %q, want the normalized form the catalog stored", fm.Tags.ISBN)
	}
	// The stored key follows the identifier now on disk, rather than leaving the scan's
	// ISBN adoption to paper over a key that names the author and title.
	if got, want := storedIdentityKey(t, ctx, db, pid), identity.BookKey("", "9780261102217", "J.R.R. Tolkien", "The Hobbit", ""); got != want {
		t.Errorf("identity key = %q, want %q", got, want)
	}

	// Forced, which is the scan that recomputes identity from tags. A plain rescan
	// fast-paths this file (the write-back left its size and mtime recorded), so it
	// would never notice the new key and would prove nothing.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book after a forced rescan (identity was not re-anchored, so the pid is orphaned): %v", err)
	}
	if d.ISBN != "9780261102217" {
		t.Errorf("isbn after rescan = %q, want it preserved", d.ISBN)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil {
		t.Fatalf("query books: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("book count after rescan = %d, want 1 (a re-key would leave two)", len(items))
	}
}

// TestEditBookEditionWriteBackReanchors is the edition twin of the identifier test:
// edition feeds identity.BookKey too, so once it reaches disk the stored key has to
// follow or the next forced scan resolves a different item. The subtitle and description
// written beside it read back through a fresh scan of the file.
func TestEditBookEditionWriteBackReanchors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "J.R.R. Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "The Hobbit")

	const blurb = "Bilbo Baggins is a hobbit who enjoys a comfortable life."
	if err := lib.EditFields(ctx, pid, map[string]string{
		"edition":     "75th Anniversary Edition",
		"subtitle":    "There and Back Again",
		"description": blurb,
	}, waxbin.EditOptions{WriteBack: true}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	meta.PromoteBookFields(&fm.Tags)
	if fm.Tags.Edition != "75th Anniversary Edition" || fm.Tags.Subtitle != "There and Back Again" || fm.Tags.Description != blurb {
		t.Errorf("on-disk edition/subtitle/description = %q/%q/%q, want the edited values",
			fm.Tags.Edition, fm.Tags.Subtitle, fm.Tags.Description)
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	d, err := lib.Book(ctx, pid)
	if err != nil {
		t.Fatalf("book after a forced rescan (the edition moved the key without a re-anchor): %v", err)
	}
	if d.Edition != "75th Anniversary Edition" || d.Subtitle != "There and Back Again" || d.Description != blurb {
		t.Errorf("after the rescan edition/subtitle/description = %q/%q/%q, want the values read back from the file",
			d.Edition, d.Subtitle, d.Description)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil {
		t.Fatalf("query books: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("book count after rescan = %d, want 1 (a re-key would leave two)", len(items))
	}
}

// TestCatalogOnlyEditOwesATagWrite: an edit kept in the catalog leaves the file behind,
// and the file's diagnostics say so field by field. The rows outlive a scan while the
// file still disagrees (a locked edit), go once a write-back lands the field, and go
// when a scan re-derives an unlocked field from the file.
func TestCatalogOnlyEditOwesATagWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Band", Album: "Album", Genre: "Rock", Year: 2001,
	}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	owed := func() map[string]string {
		t.Helper()
		diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
		if err != nil {
			t.Fatalf("diagnostics: %v", err)
		}
		out := map[string]string{}
		for _, d := range diags {
			if d.Origin != model.OriginEdit || d.Severity != model.SeverityInfo {
				t.Fatalf("owed row %+v, want an info row from the edit writer", d)
			}
			out[d.TagKey] = d.Detail
		}
		return out
	}
	if got := owed(); len(got) != 0 {
		t.Fatalf("owed before any edit = %v", got)
	}

	if err := lib.EditFields(ctx, pid, map[string]string{"genre": "Jazz", "comment": "note"},
		waxbin.EditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("catalog-only edit: %v", err)
	}
	got := owed()
	if len(got) != 2 || !strings.Contains(got["genre"], "genre") || !strings.Contains(got["comment"], "comment") {
		t.Fatalf("owed = %v, want genre and comment, each named", got)
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owed(); len(got) != 2 {
		t.Fatalf("owed after a scan = %v, want both kept while the file disagrees", got)
	}

	if err := lib.EditFields(ctx, pid, map[string]string{"genre": "Jazz"},
		waxbin.EditOptions{Lock: model.LockOn, Force: true, WriteBack: true}); err != nil {
		t.Fatalf("write-back: %v", err)
	}
	if got := owed(); len(got) != 1 || got["comment"] == "" {
		t.Fatalf("owed after writing genre back = %v, want only comment left", got)
	}

	if err := lib.EditFields(ctx, pid, map[string]string{"year": "1999"}, waxbin.EditOptions{}); err != nil {
		t.Fatalf("unlocked edit: %v", err)
	}
	if got := owed(); got["year"] == "" {
		t.Fatalf("owed = %v, want year added", got)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if got := owed(); len(got) != 1 || got["comment"] == "" {
		t.Fatalf("owed after the scan re-derived year = %v, want only comment left", got)
	}
}

// TestCatalogOnlyCreditOwesATagWrite: a credit set without a write-back owes its role's
// tag to the file until a write-back lands it, and a book translator, which no tag can
// carry, owes nothing.
func TestCatalogOnlyCreditOwesATagWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Band", Album: "Album", Audio: testaudio.AudioWithSeed(1),
	}))
	writeFile(t, filepath.Join(root, "book.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Tome", Artist: "Writer", Album: "Tome", Audio: testaudio.AudioWithSeed(2),
		TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}},
	}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	owed := func(pid model.PID) []string {
		t.Helper()
		diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteOwed})
		if err != nil {
			t.Fatalf("diagnostics: %v", err)
		}
		var out []string
		for _, d := range diags {
			out = append(out, d.TagKey)
		}
		return out
	}
	song := itemPIDByTitle(t, ctx, lib, "Song")
	if _, _, err := lib.SetCredits(ctx, song, model.RoleComposer, []string{"Someone"}, waxbin.CreditEditOptions{}); err != nil {
		t.Fatalf("set composer: %v", err)
	}
	if got := owed(song); len(got) != 1 || got[0] != "credit.composer" {
		t.Fatalf("owed = %v, want credit.composer", got)
	}
	if _, _, err := lib.SetCredits(ctx, song, model.RoleComposer, []string{"Someone"}, waxbin.CreditEditOptions{WriteBack: true}); err != nil {
		t.Fatalf("write composer back: %v", err)
	}
	if got := owed(song); len(got) != 0 {
		t.Fatalf("owed after the write-back = %v, want none", got)
	}

	book := itemPIDByTitle(t, ctx, lib, "Tome")
	if _, _, err := lib.SetCredits(ctx, book, model.RoleTranslator, []string{"Linguist"}, waxbin.CreditEditOptions{}); err != nil {
		t.Fatalf("set translator: %v", err)
	}
	if got := owed(book); len(got) != 0 {
		t.Fatalf("book owed = %v, want nothing for a role no tag carries", got)
	}
}

// TestEditTitleSortOrdersAndWritesBack: a title_sort edit keys the item by the spelling and
// writes it where a scan reads it back, a track's TITLESORT and a book's ALBUMSORT, so a
// forced rescan keeps both.
func TestEditTitleSortOrdersAndWritesBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "tokyo.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "東京", Artist: "Band", Album: "Album", Track: 1, Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(root, "berlin.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Berlin", Artist: "Band", Album: "Album", Track: 2, Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(root, "hobbit.m4b"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Chapter 1", Artist: "Tolkien", AlbumArtist: "Tolkien", Album: "Hobbit", Track: 1,
		Audio: testaudio.AudioWithSeed(3)}))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	tokyo := itemPIDByTitle(t, ctx, lib, "東京")
	book := itemPIDByTitle(t, ctx, lib, "Hobbit")
	for pid, spelling := range map[model.PID]string{tokyo: "Zz Tokyo", book: "Hobbit, The"} {
		if err := lib.EditField(ctx, pid, "title_sort", spelling, waxbin.EditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
			t.Fatalf("edit title_sort %s: %v", pid, err)
		}
	}
	for _, c := range []struct{ file, key, want string }{
		{"tokyo.mp3", "TITLESORT", "Zz Tokyo"}, {"hobbit.m4b", "ALBUMSORT", "Hobbit, The"},
	} {
		fm, err := meta.NewReader().Read(ctx, filepath.Join(root, c.file))
		if err != nil {
			t.Fatal(err)
		}
		got := fm.Tags.TitleSort
		if c.key == "ALBUMSORT" {
			got = fm.Tags.AlbumSort
		}
		if got != c.want {
			t.Errorf("%s %s = %q, want %q", c.file, c.key, got, c.want)
		}
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	tracks, err := lib.Query(ctx, query.New(query.EntityTracks).Build(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titlesOf(tracks), ","); got != "Berlin,東京" {
		t.Errorf("track order = %s, want Berlin then 東京 under its spelling", got)
	}
	if v, err := lib.Get(ctx, book); err != nil || v.TitleSort != "Hobbit, The" {
		t.Errorf("book after the rescan = %+v (err %v), want its spelling read back", v, err)
	}
}

// beatlesPair scans a file credited to "The Beatles" and one to "Beatles" that states its
// artist's MusicBrainz id, merges the second artist into the first, and returns the
// library, the second file's item and the survivor it now sits on.
func beatlesPair(t *testing.T, ctx context.Context) (lib *waxbin.Library, root string, item, survivor model.PID) {
	t.Helper()
	root = t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("One", "The Beatles", "One", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Two", Artist: "Beatles", Album: "Two", Track: 1,
		TXXX:  []testaudio.TXXXFrame{{Desc: "MusicBrainz Artist Id", Value: beatlesMBID}},
		Audio: testaudio.AudioWithSeed(2)}))
	lib = openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	survivor = artistPIDByName(t, ctx, db, "The Beatles")
	loser := artistPIDByName(t, ctx, db, "Beatles")
	if _, err := lib.Merge(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	item = itemPIDByTitle(t, ctx, lib, "Two")
	if v, err := lib.Get(ctx, item); err != nil || v.ArtistPID != survivor {
		t.Fatalf("after the merge Two = %+v (err %v), want it on the survivor", v, err)
	}
	return lib, root, item, survivor
}

const beatlesMBID = "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d"

// TestUnfoldWithReResolveFilesTheKeptFilesUnderTheirOwnName: an unfold that re-resolves
// reads again the files the fold kept on the survivor, so each is filed under the name it
// states, its MusicBrainz id with it, and the report names what it read.
func TestUnfoldWithReResolveFilesTheKeptFilesUnderTheirOwnName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, item, survivor := beatlesPair(t, ctx)
	job, err := lib.StartUnfold(ctx, model.MergeArtist, []string{"beatles"}, waxbin.UnfoldOptions{ReResolve: true})
	if err != nil {
		t.Fatalf("start unfold: %v", err)
	}
	j := waitForJobDone(t, ctx, lib, job)
	var rep waxbin.UnfoldReport
	// Every item credited to the artist is read again, the survivor's own One with Two.
	if err := json.Unmarshal([]byte(j.Result), &rep); err != nil || rep.Folds != 1 || len(rep.Reread) != 2 ||
		!slices.Contains(rep.Reread, item) || len(rep.Skipped) != 0 {
		t.Fatalf("unfold report %s (err %v), want one fold and both items read again", j.Result, err)
	}
	v, err := lib.Get(ctx, item)
	if err != nil || v.ArtistPID == survivor {
		t.Fatalf("Two after the unfold = %+v (err %v), want it off the survivor", v, err)
	}
	info, err := lib.EntityByPID(ctx, read.EntityArtist, v.ArtistPID)
	if err != nil || info.Name != "Beatles" || info.MBID != beatlesMBID {
		t.Errorf("Two's artist = %+v (err %v), want Beatles with its MusicBrainz id", info, err)
	}
}

// TestRescanReResolveRelinksUnchangedFiles: a rescan with re-resolve links unchanged files
// to the entities their tags name, which a fold forgotten by a plain unfold leaves them
// apart from until then.
func TestRescanReResolveRelinksUnchangedFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, item, survivor := beatlesPair(t, ctx)
	if err := lib.UnfoldEntity(ctx, model.MergeArtist, "beatles"); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if v, _ := lib.Get(ctx, item); v.ArtistPID != survivor {
		t.Fatal("a forced rescan without re-resolve relinked the unchanged file; the test's premise is gone")
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{ReResolve: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := lib.Get(ctx, item); err != nil || v.ArtistPID == survivor || v.ArtistPID == "" {
		t.Fatalf("Two after a re-resolving rescan = %+v (err %v), want it filed under an artist of its own", v, err)
	}
}

// TestUnfoldReResolveSkipsAnOfflineFileAndNamesIt: an item whose file is not on disk is not
// read again, and the report names it.
func TestUnfoldReResolveSkipsAnOfflineFileAndNamesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, item, _ := beatlesPair(t, ctx)
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	job, err := lib.StartUnfold(ctx, model.MergeArtist, []string{"beatles"}, waxbin.UnfoldOptions{ReResolve: true})
	if err != nil {
		t.Fatal(err)
	}
	var rep waxbin.UnfoldReport
	if err := json.Unmarshal([]byte(waitForJobDone(t, ctx, lib, job).Result), &rep); err != nil ||
		rep.Folds != 1 || len(rep.Reread) != 1 || slices.Contains(rep.Reread, item) || !slices.Equal(rep.Skipped, []model.PID{item}) {
		t.Fatalf("unfold report %+v (err %v), want Two skipped and named, One read", rep, err)
	}
}

// TestUnfoldReResolveFindsAFeaturedCredit: a track whose credit names the folded artist
// among others is read again too, and lands on that artist rather than the survivor.
func TestUnfoldReResolveFindsAFeaturedCredit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, credit := range []string{"Shawn Carter", "Jay-Z", "Jay-Z feat. Rihanna"} {
		writeFile(t, filepath.Join(root, string(rune('a'+i))+".mp3"),
			testaudio.BuildMP3WithAudio("T"+string(rune('a'+i)), credit, "Album "+string(rune('a'+i)), 1, testaudio.AudioWithSeed(byte(i+1))))
	}
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	survivor := artistPIDByName(t, ctx, db, "Shawn Carter")
	if _, err := lib.Merge(ctx, model.MergeArtist, survivor, artistPIDByName(t, ctx, db, "Jay-Z")); err != nil {
		t.Fatal(err)
	}
	featured := itemPIDByTitle(t, ctx, lib, "Tc")
	if v, _ := lib.Get(ctx, featured); v.ArtistPID != survivor {
		t.Fatalf("after the merge the featured track sits on %s, want the survivor", v.ArtistPID)
	}
	rep, err := lib.Unfold(ctx, model.MergeArtist, []string{"jay z"}, waxbin.UnfoldOptions{ReResolve: true})
	if err != nil || !slices.Contains(rep.Reread, featured) {
		t.Fatalf("unfold = %+v (err %v), want the featured track read again", rep, err)
	}
	if v, err := lib.Get(ctx, featured); err != nil || v.ArtistPID == survivor {
		t.Fatalf("the featured track after the unfold = %+v (err %v), want it off the survivor", v, err)
	}
}

// TestStartUnfoldRefusesAKeyNoFoldHoldsBeforeItsJob: a key no fold holds is refused at once,
// by StartUnfold and Unfold alike, rather than starting a job that fails.
func TestStartUnfoldRefusesAKeyNoFoldHoldsBeforeItsJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, _ := beatlesPair(t, ctx)
	before, err := lib.Jobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lib.StartUnfold(ctx, model.MergeArtist, []string{"beatles", "nobody"}, waxbin.UnfoldOptions{ReResolve: true}); !waxerr.Is(err, waxerr.CodeNotFound) ||
		!strings.Contains(err.Error(), "nobody") {
		t.Fatalf("start unfold of an unknown key = %v, want CodeNotFound naming it", err)
	}
	if _, err := lib.Unfold(ctx, model.MergeArtist, []string{"nobody"}, waxbin.UnfoldOptions{}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unfold of an unknown key = %v, want CodeNotFound", err)
	}
	if after, err := lib.Jobs(ctx, 100); err != nil || len(after) != len(before) {
		t.Errorf("jobs = %d after the refusals (err %v), want %d: no job started", len(after), err, len(before))
	}
	var rep waxbin.UnfoldReport
	if err := json.Unmarshal([]byte(`{"folds":2,"reread":["a"],"skipped":["b"]}`), &rep); err != nil || rep.Folds != 2 || len(rep.Reread) != 1 || len(rep.Skipped) != 1 {
		t.Errorf("report from camelCase JSON = %+v (err %v), want each field read", rep, err)
	}
}
