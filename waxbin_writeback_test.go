package waxbin_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
)

// albumPIDByTitle resolves an album's public id by its title via a direct read (the
// facade's item views carry the album name, not its entity pid).
func albumPIDByTitle(t *testing.T, ctx context.Context, db, title string) model.PID {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var pid string
	if err := raw.QueryRowContext(ctx, "SELECT pid FROM album WHERE title = ?", title).Scan(&pid); err != nil {
		t.Fatalf("album pid for %q: %v", title, err)
	}
	return model.PID(pid)
}

// artistPIDByName resolves an artist entity's public id by name via a direct read.
func artistPIDByName(t *testing.T, ctx context.Context, db, name string) model.PID {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var pid string
	if err := raw.QueryRowContext(ctx, "SELECT pid FROM artist WHERE name = ?", name).Scan(&pid); err != nil {
		t.Fatalf("artist pid for %q: %v", name, err)
	}
	return model.PID(pid)
}

func coverPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

// TestEditEntityWriteBackFanOut verifies an album identifier/sort edit with --write-back
// fans the values across every member track's on-disk tags (BARCODE, ALBUMSORT), while
// a release-group-style DB-only value would not be written.
func TestEditEntityWriteBackFanOut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	one := filepath.Join(root, "01.mp3")
	two := filepath.Join(root, "02.mp3")
	writeFile(t, one, testaudio.BuildMP3WithAudio("Track One", "The Foobars", "Night Moves", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, two, testaudio.BuildMP3WithAudio("Track Two", "The Foobars", "Night Moves", 2, testaudio.AudioWithSeed(2)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	albumPID := albumPIDByTitle(t, ctx, db, "Night Moves")

	if err := lib.EditEntity(ctx, model.MergeAlbum, albumPID, map[string]string{
		"barcode": "0123456789012",
		"sort":    "Night Moves, The",
	}, waxbin.EntityEditOptions{WriteBack: true, Lock: true}); err != nil {
		t.Fatalf("entity edit write-back: %v", err)
	}

	r := meta.NewReader()
	for _, p := range []string{one, two} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.Barcode != "0123456789012" {
			t.Errorf("%s BARCODE = %q, want the fanned barcode", filepath.Base(p), fm.Tags.Barcode)
		}
		if fm.Tags.AlbumSort != "Night Moves, The" {
			t.Errorf("%s ALBUMSORT = %q, want the fanned sort", filepath.Base(p), fm.Tags.AlbumSort)
		}
	}
}

// TestEditEntityArtistSortOnlyPrimaryArtist verifies an artist sort write-back writes
// ARTISTSORT only to files where the artist is the PRIMARY artist, not to files where it
// is merely the album-artist (which would overwrite that track's real primary-artist sort
// on the next scan).
func TestEditEntityArtistSortOnlyPrimaryArtist(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Track A: Xavier is the primary artist. Track B: Yolanda is the primary artist and
	// Xavier is only the album-artist. Both share the album so they group together.
	primary := filepath.Join(root, "a.mp3")
	albumArtistOnly := filepath.Join(root, "b.mp3")
	writeFile(t, primary, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "A", Artist: "Xavier", AlbumArtist: "Xavier", Album: "Split", Track: 1, Audio: testaudio.AudioWithSeed(1),
	}))
	writeFile(t, albumArtistOnly, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "B", Artist: "Yolanda", AlbumArtist: "Xavier", Album: "Split", Track: 2, Audio: testaudio.AudioWithSeed(2),
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	xavier := artistPIDByName(t, ctx, db, "Xavier")

	if err := lib.EditEntity(ctx, model.MergeArtist, xavier, map[string]string{"sort": "Xavier, DJ"},
		waxbin.EntityEditOptions{WriteBack: true, Lock: true}); err != nil {
		t.Fatalf("artist sort write-back: %v", err)
	}

	r := meta.NewReader()
	fmA, _ := r.Read(ctx, primary)
	if fmA.Tags.ArtistSort != "Xavier, DJ" {
		t.Errorf("primary-artist track ARTISTSORT = %q, want the fanned sort", fmA.Tags.ArtistSort)
	}
	fmB, _ := r.Read(ctx, albumArtistOnly)
	if fmB.Tags.ArtistSort != "" {
		t.Errorf("album-artist-only track ARTISTSORT = %q, want empty (must not be corrupted with Xavier's sort)", fmB.Tags.ArtistSort)
	}
}

// TestEditComposerSortWriteBack verifies a composer/composer_sort edit with
// --write-back lands COMPOSER and COMPOSERSORT in the file's tags, and that the
// locked catalog values survive a forced rescan of the rewritten file.
func TestEditComposerSortWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Band", Album: "Album", Composer: "Old Composer", Track: 1,
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")

	if err := lib.EditFields(ctx, pid, map[string]string{
		"composer": "Amy Arranger", "composer_sort": "Arranger, Amy",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("edit with write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Composer != "Amy Arranger" {
		t.Errorf("on-disk COMPOSER = %q, want Amy Arranger", fm.Tags.Composer)
	}
	if fm.Tags.ComposerSort != "Arranger, Amy" {
		t.Errorf("on-disk COMPOSERSORT = %q, want Arranger, Amy", fm.Tags.ComposerSort)
	}

	// A forced rescan folds the tag through SortKey; the lock is what keeps the
	// catalog's literal value.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.Composer != "Amy Arranger" || v.ComposerSort != "Arranger, Amy" {
		t.Errorf("after forced rescan = (%q, %q), want the locked literal pair", v.Composer, v.ComposerSort)
	}
}

// TestEditComposerWriteBackClearsStaleSortTag verifies a display-name edit's
// write-back clears the derived sort tags the file carried: without the clears,
// a stale COMPOSERSORT or ARTISTSORT would feed the next scan's derivation and
// revert the regenerated catalog sort (in a fresh catalog always, and in this
// one wherever the field is unlocked). A curated, locked sort keeps its tag.
func TestEditComposerWriteBackClearsStaleSortTag(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Old Artist", Album: "Album", Composer: "Old Composer", Track: 1,
		TXXX: []testaudio.TXXXFrame{
			{Desc: "COMPOSERSORT", Value: "Composer, Old"},
			{Desc: "ARTISTSORT", Value: "Artist, Old"},
		},
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")

	if err := lib.EditFields(ctx, pid, map[string]string{
		"composer": "New Composer", "artist": "New Artist",
	}, waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("edit with write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Composer != "New Composer" || fm.Tags.Artist != "New Artist" {
		t.Fatalf("on-disk names = (%q, %q), want the edited values", fm.Tags.Composer, fm.Tags.Artist)
	}
	if fm.Tags.ComposerSort != "" {
		t.Errorf("on-disk COMPOSERSORT = %q, want cleared (the stale sort would revert the derivation)", fm.Tags.ComposerSort)
	}
	if fm.Tags.ArtistSort != "" {
		t.Errorf("on-disk ARTISTSORT = %q, want cleared", fm.Tags.ArtistSort)
	}

	// A fresh catalog now derives the sorts from the new display names, matching
	// what this catalog holds.
	db2 := filepath.Join(t.TempDir(), "catalog2.db")
	lib2 := openManaged(t, ctx, db2, root)
	if _, err := lib2.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("fresh scan: %v", err)
	}
	v2, err := lib2.Get(ctx, itemPIDByTitle(t, ctx, lib2, "Song"))
	if err != nil {
		t.Fatalf("fresh get: %v", err)
	}
	if v2.ComposerSort != model.SortKey("New Composer") {
		t.Errorf("fresh-catalog composer_sort = %q, want %q", v2.ComposerSort, model.SortKey("New Composer"))
	}

	// A locked composer_sort keeps its tag through a later composer edit: the
	// curated value stays represented on disk.
	if err := lib.EditField(ctx, pid, "composer_sort", "Curated, Sort",
		waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("curate composer_sort: %v", err)
	}
	// Force clears the composer's own lock from the first edit; the subject here
	// is the sort tag, which the locked composer_sort must keep.
	if err := lib.EditField(ctx, pid, "composer", "Third Composer",
		waxbin.EditOptions{Lock: true, Force: true, WriteBack: true}); err != nil {
		t.Fatalf("composer edit over locked sort: %v", err)
	}
	fm, err = meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.ComposerSort != "Curated, Sort" {
		t.Errorf("on-disk COMPOSERSORT = %q, want the locked curated value kept", fm.Tags.ComposerSort)
	}
}

// TestEditAuthorWriteBackClearsStaleSortTag is the book variant: an author edit's
// write-back clears a stale ALBUMARTISTSORT so the file's derivation follows the
// new author.
func TestEditAuthorWriteBackClearsStaleSortTag(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "book.m4b")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "The Book", Artist: "Old Author", AlbumArtist: "Old Author", Album: "The Book", Track: 1,
		TXXX: []testaudio.TXXXFrame{{Desc: "ALBUMARTISTSORT", Value: "Author, Old"}},
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}

	if err := lib.EditField(ctx, books[0].PID, "author", "New Author",
		waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("author write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.AlbumArtist != "New Author" {
		t.Fatalf("on-disk ALBUMARTIST = %q, want New Author", fm.Tags.AlbumArtist)
	}
	if fm.Tags.AlbumArtistSort != "" {
		t.Errorf("on-disk ALBUMARTISTSORT = %q, want cleared", fm.Tags.AlbumArtistSort)
	}

	// A fresh catalog derives the author sort from the new author.
	db2 := filepath.Join(t.TempDir(), "catalog2.db")
	lib2 := openManaged(t, ctx, db2, root)
	if _, err := lib2.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("fresh scan: %v", err)
	}
	books2, err := lib2.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books2) != 1 {
		t.Fatalf("fresh book query: %d books (err %v)", len(books2), err)
	}
	if books2[0].AuthorSort != model.SortKey("New Author") {
		t.Errorf("fresh-catalog author_sort = %q, want %q", books2[0].AuthorSort, model.SortKey("New Author"))
	}
}

// TestEditAuthorSortWriteBack verifies a book author_sort edit with --write-back
// lands ALBUMARTISTSORT (the key the audiobook scanner's author_sort derive reads
// first) in every part's tags.
func TestEditAuthorSortWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "book.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Book", "Jane Author", "The Book", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}

	if err := lib.EditField(ctx, books[0].PID, "author_sort", "Author, Jane",
		waxbin.EditOptions{Lock: true, WriteBack: true}); err != nil {
		t.Fatalf("author_sort write-back: %v", err)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.AlbumArtistSort != "Author, Jane" {
		t.Errorf("on-disk ALBUMARTISTSORT = %q, want Author, Jane", fm.Tags.AlbumArtistSort)
	}

	// The locked literal survives a forced rescan (an unlocked one would fold the
	// tag through SortKey).
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	v, err := lib.Get(ctx, books[0].PID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.AuthorSort != "Author, Jane" {
		t.Errorf("after forced rescan author_sort = %q, want the locked literal", v.AuthorSort)
	}
}

// TestSetItemArtWriteBack verifies an item cover set with --write-back embeds the cover
// into the item's backing file.
func TestSetItemArtWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")

	if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t), true, false, true); err != nil {
		t.Fatalf("set item art write-back: %v", err)
	}
	assertFrontCover(t, ctx, src)

	// Only the front cover has an embedded representation, so --write-back with any
	// other role is refused before the catalog write: the back slot stays empty.
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, coverPNG(t), false, false, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("back + write-back = %v, want CodeInvalid", err)
	}
	if _, err := lib.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("refused write-back still wrote the catalog row: %v", err)
	}
	// Without write-back the back slot sets fine.
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, coverPNG(t), false, false, false); err != nil {
		t.Errorf("back without write-back: %v", err)
	}
}

// TestSetItemArtWriteBackMultiFileBook verifies an item cover write-back embeds the
// cover into every part of a multi-file book, not just the primary, so an external
// player sees the same cover on each part.
func TestSetItemArtWriteBackMultiFileBook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	parts := make([]string, 3)
	for i, seed := range []byte{21, 22, 23} {
		p := filepath.Join(root, "part"+string(rune('1'+i))+".m4b")
		writeFile(t, p, testaudio.BuildMP3WithAudio("Chapter "+string(rune('1'+i)), "Tolkien", "The Hobbit", i+1, testaudio.AudioWithSeed(seed)))
		parts[i] = p
	}

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v), want 1", len(books), err)
	}

	if err := lib.SetItemArt(ctx, books[0].PID, model.ArtRoleFront, coverPNG(t), true, false, true); err != nil {
		t.Fatalf("set item art write-back: %v", err)
	}
	for _, p := range parts {
		assertFrontCover(t, ctx, p)
	}
}

// TestSetEntityArtAlbumFanOut verifies an album cover set with --write-back embeds the
// cover into every member track's file, while the same on a non-album entity (an artist)
// is a catalog-only no-op that embeds nothing on disk.
func TestSetEntityArtAlbumFanOut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	one := filepath.Join(root, "01.mp3")
	two := filepath.Join(root, "02.mp3")
	writeFile(t, one, testaudio.BuildMP3WithAudio("Track One", "The Foobars", "Night Moves", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, two, testaudio.BuildMP3WithAudio("Track Two", "The Foobars", "Night Moves", 2, testaudio.AudioWithSeed(2)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	albumPID := albumPIDByTitle(t, ctx, db, "Night Moves")

	if err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t), true); err != nil {
		t.Fatalf("set album art write-back: %v", err)
	}
	assertFrontCover(t, ctx, one)
	assertFrontCover(t, ctx, two)
}

// TestSetCreditsBookWriteBack verifies a book author credit with --write-back embeds
// ALBUMARTIST on the primary part, while a translator credit (which no scan reconstructs
// from a tag) is refused with the catalog edit standing.
func TestSetCreditsBookWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "book.m4b")
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

	// An author credit round-trips to ALBUMARTIST.
	if _, err := lib.SetCredits(ctx, pid, model.RoleAuthor, []string{"J.R.R. Tolkien"},
		waxbin.CreditEditOptions{WriteBack: true, Lock: true}); err != nil {
		t.Fatalf("book author credit write-back: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.AlbumArtist != "J.R.R. Tolkien" {
		t.Errorf("on-disk ALBUMARTIST = %q, want the edited author credit", fm.Tags.AlbumArtist)
	}
	// An author credit writes ALBUMARTIST (a book identity field), so a scan --force must
	// resolve the same item (the re-anchor), keeping its pid and locks.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan --force: %v", err)
	}
	if _, err := lib.Book(ctx, pid); err != nil {
		t.Fatalf("book pid did not survive scan --force after author credit write-back: %v", err)
	}

	// A translator credit has no scanner tag, so write-back is refused; the catalog
	// edit still stands.
	_, err = lib.SetCredits(ctx, pid, model.RoleTranslator, []string{"A. Translator"},
		waxbin.CreditEditOptions{WriteBack: true, Lock: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("book translator write-back: want *WriteBackError, got %v", err)
	}
	d, _ := lib.Book(ctx, pid)
	if len(d.Translators) != 1 || d.Translators[0] != "A. Translator" {
		t.Errorf("translators = %v, want [A. Translator] (catalog edit must stand)", d.Translators)
	}
}

// TestSetEntityArtAlbumFanOutRefusesSharedMember verifies the album cover fan-out
// embeds into the writable members and refuses (not fails) a member whose file is shared
// or carries an offset window, reporting it as a *WriteBackError while the catalog cover
// stands.
func TestSetEntityArtAlbumFanOutRefusesSharedMember(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	one := filepath.Join(root, "01.mp3")
	two := filepath.Join(root, "02.mp3")
	writeFile(t, one, testaudio.BuildMP3WithAudio("Track One", "The Foobars", "Night Moves", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, two, testaudio.BuildMP3WithAudio("Track Two", "The Foobars", "Night Moves", 2, testaudio.AudioWithSeed(2)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Mark track two's backing file virtual so its tags are global to the file.
	makeBackingFileVirtual(t, ctx, db, itemPIDByTitle(t, ctx, lib, "Track Two"))
	albumPID := albumPIDByTitle(t, ctx, db, "Night Moves")

	err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t), true)
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("want *WriteBackError for a shared member, got %v", err)
	}
	if len(wbErr.Failures) != 1 {
		t.Fatalf("fan-out failures = %d, want 1 (the shared member refused)", len(wbErr.Failures))
	}
	// The writable member still got the cover; the catalog cover stands regardless.
	assertFrontCover(t, ctx, one)
}

// assertFrontCover fails unless the file at path carries exactly one embedded front
// cover with non-empty bytes.
func assertFrontCover(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		t.Fatalf("reparse %s: %v", path, err)
	}
	pics := doc.Pictures()
	if len(pics) != 1 || pics[0].Type != waxlabel.PicFrontCover || len(pics[0].Data) == 0 {
		t.Fatalf("%s embedded pictures = %+v, want one front cover", filepath.Base(path), pics)
	}
}
