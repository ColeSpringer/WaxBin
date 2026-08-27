package waxbin_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
	"golang.org/x/image/bmp"
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

	if _, err := lib.EditEntity(ctx, model.MergeAlbum, albumPID, map[string]string{
		"barcode": "0123456789012",
		"sort":    "Night Moves, The",
	}, waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
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

	if _, err := lib.EditEntity(ctx, model.MergeArtist, xavier, map[string]string{"sort": "Xavier, DJ"},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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

// TestEditBPMWriteBackRoundTrip walks the bpm column through the whole loop: a scan
// reads TBPM into it, an edit with write-back stamps the new number onto the file, and
// a forced rescan reads that number back rather than reverting.
func TestEditBPMWriteBackRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Song", Artist: "Band", Album: "Album", Track: 1, BPM: "90.4",
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if v.BPM != 90 {
		t.Fatalf("scanned BPM = %d, want the rounded 90", v.BPM)
	}

	if err := lib.EditFields(ctx, pid, map[string]string{"bpm": "128"},
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("edit with write-back: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.BPM != 128 {
		t.Errorf("on-disk BPM = %d, want 128", fm.Tags.BPM)
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if v, err = lib.Get(ctx, pid); err != nil {
		t.Fatalf("get after rescan: %v", err)
	}
	if v.BPM != 128 {
		t.Errorf("BPM after forced rescan = %d, want 128", v.BPM)
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
	}, waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("curate composer_sort: %v", err)
	}
	// Force writes through the composer's own lock from the first edit without releasing
	// it; the subject here is the sort tag, which the locked composer_sort must keep.
	if err := lib.EditField(ctx, pid, "composer", "Third Composer",
		waxbin.EditOptions{Lock: model.LockOn, Force: true, WriteBack: true}); err != nil {
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
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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
		waxbin.EditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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

	if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("set item art write-back: %v", err)
	}
	assertFrontCover(t, ctx, src)

	// Only the front cover has an embedded representation, so --write-back with any
	// other role is refused before the catalog write: the back slot stays empty.
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("back + write-back = %v, want CodeInvalid", err)
	}
	if _, err := lib.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("refused write-back still wrote the catalog row: %v", err)
	}
	// Without write-back the back slot sets fine.
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOff}); err != nil {
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

	if err := lib.SetItemArt(ctx, books[0].PID, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
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

	if err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
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
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
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
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOn})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("book translator write-back: want *WriteBackError, got %v", err)
	}
	d, _ := lib.Book(ctx, pid)
	if len(d.Translators) != 1 || d.Translators[0] != "A. Translator" {
		t.Errorf("translators = %v, want [A. Translator] (catalog edit must stand)", d.Translators)
	}
}

// TestScanSplitCreditRoundTripsThroughCreditWriteBack closes the loop on the track
// artist credit: a scanned "feat." credit splits into one artist per name, an edited
// credit written back lands as a repeated ARTIST frame, and the next scan reads that
// frame verbatim rather than re-splitting it, so the curated list survives.
func TestScanSplitCreditRoundTripsThroughCreditWriteBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Empire State of Mind", "Jay-Z feat. Alicia Keys", "The Blueprint 3", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Empire State of Mind")

	// The scan split the credit into one artist each.
	credits, err := lib.Credits(ctx, pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	var scanned []string
	for _, c := range credits {
		if c.Role == model.RoleArtist {
			scanned = append(scanned, c.Name)
		}
	}
	if len(scanned) != 2 || scanned[0] != "Jay-Z" || scanned[1] != "Alicia Keys" {
		t.Fatalf("scanned artist credits = %v, want [Jay-Z Alicia Keys]", scanned)
	}

	// An edited credit writes a repeated ARTIST frame. One of the names carries a
	// splitter marker of its own, which is what makes the rescan below meaningful.
	want := []string{"Jay-Z", "Run-D.M.C. vs. Jason Nevins"}
	if _, err := lib.SetCredits(ctx, pid, model.RoleArtist, want,
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOff}); err != nil {
		t.Fatalf("artist credit write-back: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if len(fm.Tags.Artists) != 2 || fm.Tags.Artists[0] != want[0] || fm.Tags.Artists[1] != want[1] {
		t.Fatalf("on-disk ARTIST values = %v, want %v", fm.Tags.Artists, want)
	}

	// A rescan reads that repeated frame verbatim. Had the splitter run on it, the
	// second name would have become two more artists behind the user's back.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan --force: %v", err)
	}
	credits, err = lib.Credits(ctx, pid)
	if err != nil {
		t.Fatalf("credits after rescan: %v", err)
	}
	var after []string
	for _, c := range credits {
		if c.Role == model.RoleArtist {
			after = append(after, c.Name)
		}
	}
	if len(after) != 2 || after[0] != want[0] || after[1] != want[1] {
		t.Errorf("artist credits after rescan = %v, want %v", after, want)
	}

	// The display survives too. model.Tags.Artist is only the first repeated value, so
	// without joining the list the rescan would silently drop every artist after it,
	// taking artist_sort and the organize path along.
	item, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if item.Artist != strings.Join(want, ", ") {
		t.Errorf("track artist after rescan = %q, want %q", item.Artist, strings.Join(want, ", "))
	}

	// And the file carries no stale ARTISTSORT to revert the regenerated sort key.
	fm, err = meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("re-read tags: %v", err)
	}
	if fm.Tags.ArtistSort != "" {
		t.Errorf("file kept ARTISTSORT %q; it would revert the credit's sort on the next scan", fm.Tags.ArtistSort)
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

	err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true})
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

// TestSetItemArtFormatHint drives the format hint through the facade, which is the one
// place a caller's spelling is normalized. A media type and the bare token both have to
// reach the store as the one token art_source holds, the hint has to stay a fallback,
// and a picture nobody can name has to stay refused: the hint rescues a cover, it does
// not turn every file into one.
func TestSetItemArtFormatHint(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")

	// Bytes nothing here reads or sniffs, distinct per case: art_source keeps one row per
	// content address, so the first writer's format is the one that sticks.
	exotic := func(tag byte) []byte {
		b := append([]byte("\x00exotic"), make([]byte, 60)...)
		b[2] = tag
		return b
	}
	// A real BMP, which needs no hint at all: x/image decodes it, dimensions and all.
	var bmpBuf bytes.Buffer
	if err := bmp.Encode(&bmpBuf, image.NewRGBA(image.Rect(0, 0, 11, 7))); err != nil {
		t.Fatalf("bmp encode: %v", err)
	}

	for _, tc := range []struct {
		name   string
		role   model.ArtRole
		raw    []byte
		format string
		want   string
		wantW  int
		wantH  int
	}{
		{"media type", model.ArtRoleBack, exotic(1), "image/jxl; charset=binary", "jxl", 0, 0},
		{"token", model.ArtRoleDisc, exotic(2), "jxl", "jxl", 0, 0},
		{"decoded bytes win", model.ArtRoleBooklet, coverPNG(t), "jxl", "png", 4, 4},
		{"bmp needs no hint", model.ArtRoleBackground, bmpBuf.Bytes(), "", "bmp", 11, 7},
	} {
		if err := lib.SetItemArt(ctx, pid, tc.role, tc.raw,
			waxbin.ArtEditOptions{Format: tc.format, Lock: model.LockUnchanged}); err != nil {
			t.Fatalf("%s: set item art: %v", tc.name, err)
		}
		roles, err := lib.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
		if err != nil {
			t.Fatalf("%s: art roles: %v", tc.name, err)
		}
		var got model.ArtRoleInfo
		for _, r := range roles {
			if r.Role == tc.role {
				got = r
			}
		}
		if got.Format != tc.want || got.Width != tc.wantW || got.Height != tc.wantH {
			t.Errorf("%s: stored %s %dx%d, want %s %dx%d",
				tc.name, got.Format, got.Width, got.Height, tc.want, tc.wantW, tc.wantH)
		}
	}

	// Nobody can name it: still refused, which is what keeps a wrong file from storing.
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, exotic(9),
		waxbin.ArtEditOptions{Lock: model.LockUnchanged}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("unnamed unreadable set = %v, want CodeInvalid", err)
	}
	// And a format that names no image is no hint at all, so the same bytes stay refused
	// however the caller spells the transport's answer.
	for _, f := range []string{"application/octet-stream", "text/html", "image/*", "-->"} {
		if err := lib.SetItemArt(ctx, pid, model.ArtRoleBack, exotic(9),
			waxbin.ArtEditOptions{Format: f, Lock: model.LockUnchanged}); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("set with format %q = %v, want CodeInvalid", f, err)
		}
	}
}

// catalogScalar reads one scalar from the catalog directly, for the entity state the
// facade's views do not surface (match keys, row counts).
func catalogScalar[T any](t *testing.T, ctx context.Context, db, q string, args ...any) T {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer raw.Close()
	var v T
	if err := raw.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

// TestEditEntityWriteBackAfterMBIDClearMerge covers the write-back side of the mbid escape
// hatch. A clear that merges the album into its heuristic twin deletes the pid the caller
// named, so the fan-out must not go looking for that entity's member files: a lone mbid
// clear fans no tag at all, and the combination that would have fanned one is refused
// before anything commits.
func TestEditEntityWriteBackAfterMBIDClearMerge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "13131313-1313-1313-1313-131313131313"
	// One track carries the release id and the other does not, so the scan keys them onto
	// an mbid album and a heuristic twin under one release group.
	tagged := filepath.Join(root, "01.mp3")
	untagged := filepath.Join(root, "02.mp3")
	writeFile(t, tagged, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Twin", Track: 1,
		Audio: testaudio.AudioWithSeed(1),
		TXXX:  []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: relMBID}},
	}))
	writeFile(t, untagged, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Twin", Track: 2,
		Audio: testaudio.AudioWithSeed(2),
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want the mbid album and its heuristic twin", n)
	}
	identified := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM album WHERE match_key = ?", "mbid:"+relMBID))
	twin := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM album WHERE match_key <> ?", "mbid:"+relMBID))

	// A fannable sibling field alongside the clear is refused, so no tag is written and
	// the catalog is untouched.
	_, err := lib.EditEntity(ctx, model.MergeAlbum, identified,
		map[string]string{"mbid": "", "label": "Blue Note"},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn})
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("clear plus label with write-back = %v, want CodeConflict", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows after the refusal = %d, want 2", n)
	}
	r := meta.NewReader()
	for _, p := range []string{tagged, untagged} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.Label != "" {
			t.Errorf("%s LABEL = %q, want nothing written by a refused edit", filepath.Base(p), fm.Tags.Label)
		}
	}

	// The lone clear commits and merges the album away. Write-back stays clean: an mbid
	// has no fanned tag, so the fan-out never asks the catalog for the deleted pid's files.
	if _, err := lib.EditEntity(ctx, model.MergeAlbum, identified, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
		t.Fatalf("lone clear with write-back: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the clear = %d, want the twin alone", n)
	}
	if p := catalogScalar[string](t, ctx, db, "SELECT pid FROM album"); model.PID(p) != twin {
		t.Fatalf("surviving album pid = %q, want the twin %q", p, twin)
	}
}

// TestDetachWriteBackStripsMBTags covers the durable half of a per-member detach. The
// catalog re-resolve alone leaves the release ids in the member's own tags, so the next
// scan that re-resolves entities would re-adopt it; with write-back the two ids come off
// the file and a forced rescan leaves the member on its heuristic album.
func TestDetachWriteBackStripsMBTags(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "14141414-1414-1414-1414-141414141414"
	const rgMBID = "15151515-1515-1515-1515-151515151515"
	mbTags := []testaudio.TXXXFrame{
		{Desc: "MusicBrainz Album Id", Value: relMBID},
		{Desc: "MusicBrainz Release Group Id", Value: rgMBID},
	}
	first := filepath.Join(root, "01.mp3")
	second := filepath.Join(root, "02.mp3")
	writeFile(t, first, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: 1,
		Audio: testaudio.AudioWithSeed(1), TXXX: mbTags,
	}))
	writeFile(t, second, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: 2,
		Audio: testaudio.AudioWithSeed(2), TXXX: mbTags,
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows = %d, want the one identified album", n)
	}
	member := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM playable_item WHERE title = ?", "One"))

	rep, err := lib.Detach(ctx, member, waxbin.DetachOptions{WriteBack: true})
	if err != nil {
		t.Fatalf("detach with write-back: %v", err)
	}
	if rep.NewAlbumPID == "" || rep.NewAlbumPID == rep.OldAlbumPID {
		t.Fatalf("report = %+v, want a new album pid", rep)
	}

	r := meta.NewReader()
	fm, err := r.Read(ctx, first)
	if err != nil {
		t.Fatalf("read %s: %v", first, err)
	}
	if fm.Tags.MBReleaseID != "" || fm.Tags.MBReleaseGroupID != "" {
		t.Errorf("detached file still carries %q/%q, want both cleared",
			fm.Tags.MBReleaseID, fm.Tags.MBReleaseGroupID)
	}
	// Only the member's own files are rewritten; the album it left keeps its identity
	// on disk as well as in the catalog.
	fm2, err := r.Read(ctx, second)
	if err != nil {
		t.Fatalf("read %s: %v", second, err)
	}
	if fm2.Tags.MBReleaseID != relMBID {
		t.Errorf("the other member's release id = %q, want it untouched", fm2.Tags.MBReleaseID)
	}

	// A forced rescan re-resolves every file, and the stripped tags leave the member on
	// its heuristic album instead of putting it back.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	after := catalogScalar[string](t, ctx, db,
		`SELECT al.pid FROM album al JOIN track t ON t.album_id = al.id
			JOIN playable_item pi ON pi.id = t.item_id WHERE pi.pid = ?`, string(member))
	if model.PID(after) != rep.NewAlbumPID {
		t.Errorf("album after the forced rescan = %q, want the heuristic %q", after, rep.NewAlbumPID)
	}
}

// TestDetachWriteBackRefusedKeepsTheTags: a file the write-back engine will not rewrite
// per item (a virtual or shared backing file) comes back as a *WriteBackError while the
// catalog detach stands, and the release ids are still on disk. That classification is
// what the CLI reads to decide whether the durability caveat still applies.
func TestDetachWriteBackRefusedKeepsTheTags(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "16161616-1616-1616-1616-161616161616"
	mbTags := []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: relMBID}}
	first := filepath.Join(root, "01.mp3")
	writeFile(t, first, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: 1,
		Audio: testaudio.AudioWithSeed(1), TXXX: mbTags,
	}))
	writeFile(t, filepath.Join(root, "02.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Both", Track: 2,
		Audio: testaudio.AudioWithSeed(2), TXXX: mbTags,
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	member := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM playable_item WHERE title = ?", "One"))
	makeBackingFileVirtual(t, ctx, db, member)

	rep, err := lib.Detach(ctx, member, waxbin.DetachOptions{WriteBack: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("detach with a refused strip = %v, want a *WriteBackError", err)
	}
	if len(wbErr.Failures) == 0 {
		t.Errorf("write-back error carries no failures: %+v", wbErr)
	}
	if rep == nil || rep.NewAlbumPID == "" || rep.NewAlbumPID == rep.OldAlbumPID {
		t.Fatalf("report = %+v, want the catalog detach to stand", rep)
	}
	fm, err := meta.NewReader().Read(ctx, first)
	if err != nil {
		t.Fatalf("read %s: %v", first, err)
	}
	if fm.Tags.MBReleaseID != relMBID {
		t.Errorf("release id on disk = %q, want it still there after the refused strip", fm.Tags.MBReleaseID)
	}
}
