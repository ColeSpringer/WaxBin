package waxbin_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
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
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleAuthor, []string{"J.R.R. Tolkien"},
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
	_, _, err = lib.SetCredits(ctx, pid, model.RoleTranslator, []string{"A. Translator"},
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

// TestSetCreditsBatchWriteBackGroupsRolesPerItem verifies the batch groups its
// write-back by item: a book edited under three roles gets one report holding every
// role's value, with the two roles no scan reconstructs from a tag refused while the
// author's write beside them still lands, and the sibling item's tag written too.
func TestSetCreditsBatchWriteBackGroupsRolesPerItem(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	song := filepath.Join(root, "song.mp3")
	book := filepath.Join(root, "book.m4b")
	writeFile(t, song, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, book, testaudio.BuildMP3("The Hobbit", "JRR Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	songPID := itemPIDByTitle(t, ctx, lib, "Song")
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}
	bookPID := books[0].PID

	res, err := lib.SetCreditsBatch(ctx, []model.ItemCreditEdit{
		{ItemPID: songPID, Role: model.RoleComposer, Names: []string{"Comp One"}},
		{ItemPID: bookPID, Role: model.RoleAuthor, Names: []string{"J.R.R. Tolkien"}},
		{ItemPID: bookPID, Role: model.RoleTranslator, Names: []string{"A. Translator"}},
		{ItemPID: bookPID, Role: model.RoleEditor, Names: []string{"E. Editor"}},
	}, waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOn})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Edited) != 4 {
		t.Fatalf("edited = %v, want all four (the catalog batch is atomic)", res.Edited)
	}
	// The book's three roles were mirrored in one pass, so it gets one report naming
	// every field the pass covered, with one refusal per untaggable role.
	if len(res.WriteBackErrors) != 1 || res.WriteBackErrors[bookPID] == nil {
		t.Fatalf("write-back errors = %v, want exactly one for the book %s", res.WriteBackErrors, bookPID)
	}
	wbe := res.WriteBackErrors[bookPID]
	if len(wbe.Failures) != 2 {
		t.Fatalf("failures = %+v, want the translator and editor refusals", wbe.Failures)
	}
	for _, field := range []string{"credit.author", "credit.translator", "credit.editor"} {
		if _, ok := wbe.Edits[field]; !ok {
			t.Errorf("edits %v missing %s, want the item's whole pass named", wbe.Edits, field)
		}
	}

	fm, err := meta.NewReader().Read(ctx, song)
	if err != nil {
		t.Fatalf("read song tags: %v", err)
	}
	if fm.Tags.Composer != "Comp One" {
		t.Errorf("on-disk composer = %q, want the edited credit", fm.Tags.Composer)
	}
	// The author's write landed despite the refusals beside it.
	bm, err := meta.NewReader().Read(ctx, book)
	if err != nil {
		t.Fatalf("read book tags: %v", err)
	}
	if bm.Tags.AlbumArtist != "J.R.R. Tolkien" {
		t.Errorf("on-disk ALBUMARTIST = %q, want the edited author", bm.Tags.AlbumArtist)
	}
	// The refused entries' catalog edits still stand.
	d, err := lib.Book(ctx, bookPID)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if len(d.Translators) != 1 || d.Translators[0] != "A. Translator" {
		t.Errorf("translators = %v, want [A. Translator] (catalog edit must stand)", d.Translators)
	}
	if len(d.Editors) != 1 || d.Editors[0] != "E. Editor" {
		t.Errorf("editors = %v, want [E. Editor] (catalog edit must stand)", d.Editors)
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
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleArtist, want,
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

// TestEditEntityWriteBackStripsClearedAlbumMBID covers the durable half of the whole-album
// escape hatch. The catalog clear re-keys the chain, but the member files still name the
// release, so the next scan that re-resolves them forks the identity back; with write-back
// the release id comes off every member and a forced rescan lands on the re-keyed row. The
// release group's id stays on disk, since the clear disowned the release alone and the
// re-keyed album carries the group segment.
func TestEditEntityWriteBackStripsClearedAlbumMBID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "17171717-1717-1717-1717-171717171717"
	const rgMBID = "18181818-1818-1818-1818-181818181818"
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
	album := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM album WHERE match_key = ?", "mbid:"+relMBID))

	rep, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn})
	if err != nil {
		t.Fatalf("clear with write-back: %v", err)
	}
	if rep.MergedInto != "" {
		t.Fatalf("report = %+v, want no survivor: no twin owns the derived key", rep)
	}

	r := meta.NewReader()
	for _, p := range []string{first, second} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.MBReleaseID != "" {
			t.Errorf("%s still names release %q, want it stripped", filepath.Base(p), fm.Tags.MBReleaseID)
		}
		if fm.Tags.MBReleaseGroupID != rgMBID {
			t.Errorf("%s release-group id = %q, want it left alone", filepath.Base(p), fm.Tags.MBReleaseGroupID)
		}
	}

	// A forced rescan re-resolves every file, and the stripped tags leave the members on
	// the re-keyed row instead of forking a fresh identified album.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the forced rescan = %d, want the re-keyed row alone", n)
	}
	if p := catalogScalar[string](t, ctx, db, "SELECT pid FROM album"); model.PID(p) != album {
		t.Errorf("surviving album pid = %q, want the re-keyed %q", p, album)
	}
}

// TestEditEntityWriteBackStripsReparentedAlbumFiles: a release-group clear settles each
// dependent album onto the chain its own members compute, which re-parents a
// differently-titled edition out of the group entirely. Those members are gone from the
// group's own fan by the time write-back runs, so the strip has to follow them; a file left
// naming the disowned group forks a fresh identified group on its next re-resolve, which is
// the whole failure the strip exists to prevent.
func TestEditEntityWriteBackStripsReparentedAlbumFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const rgMBID = "31313131-3131-3131-3131-313131313131"
	mbTags := []testaudio.TXXXFrame{{Desc: "MusicBrainz Release Group Id", Value: rgMBID}}
	plain := filepath.Join(root, "01 One", "01.mp3")
	deluxe := filepath.Join(root, "02 Two", "01.mp3")
	writeFile(t, plain, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Alpha", AlbumArtist: "Alpha", Album: "One", Track: 1,
		Audio: testaudio.AudioWithSeed(1), TXXX: mbTags,
	}))
	writeFile(t, deluxe, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Two", Track: 1,
		Audio: testaudio.AudioWithSeed(2), TXXX: mbTags,
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Fatalf("album rows = %d, want the two editions under one group", n)
	}
	group := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM release_group WHERE match_key = ?", "mbid:"+rgMBID))

	if _, err := lib.EditEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
		t.Fatalf("clear with write-back: %v", err)
	}

	// Both editions' files lose the id, the one still under the group and the one settled
	// onto a group of its own.
	r := meta.NewReader()
	for _, p := range []string{plain, deluxe} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.MBReleaseGroupID != "" {
			t.Errorf("%s still names group %q, want it stripped", filepath.Base(filepath.Dir(p)), fm.Tags.MBReleaseGroupID)
		}
	}

	// Nothing left on disk names the disowned group, so a forced rescan forks no
	// identified row back into the catalog.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db,
		"SELECT COUNT(*) FROM release_group WHERE match_key = ?", "mbid:"+rgMBID); n != 0 {
		t.Errorf("identified release groups after the forced rescan = %d, want none", n)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM release_group"); n != 2 {
		t.Errorf("release_group rows after the forced rescan = %d, want one per edition", n)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 2 {
		t.Errorf("album rows after the forced rescan = %d, want the two editions", n)
	}
	if dr, err := lib.VerifyDerived(ctx); err != nil || !dr.Consistent() {
		t.Errorf("verify derived = %+v (err %v), want clean", dr, err)
	}
}

// TestEditEntityWriteBackStripsAfterGroupMergeAndMove composes the two redirections: the
// clear derives a key a heuristic twin already owns, so the group merges away, and one
// dependent edition is re-parented out in the same transaction. The strip has to cover the
// survivor's members and the moved album's, and a file backing neither the merge nor the
// move is written once and unchanged.
func TestEditEntityWriteBackStripsAfterGroupMergeAndMove(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const rgMBID = "32323232-3232-3232-3232-323232323232"
	mbTags := []testaudio.TXXXFrame{{Desc: "MusicBrainz Release Group Id", Value: rgMBID}}
	plain := filepath.Join(root, "01 One", "01.mp3")
	other := filepath.Join(root, "02 Two", "01.mp3")
	twinFile := filepath.Join(root, "03 One Alt", "01.mp3")
	writeFile(t, plain, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Alpha", AlbumArtist: "Alpha", Album: "One", Track: 1,
		Audio: testaudio.AudioWithSeed(1), TXXX: mbTags,
	}))
	writeFile(t, other, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Alpha", AlbumArtist: "Alpha", Album: "Two", Track: 1,
		Audio: testaudio.AudioWithSeed(2), TXXX: mbTags,
	}))
	// No id at all: this one already sits on the heuristic group key the clear derives.
	writeFile(t, twinFile, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One Again", Artist: "Alpha", AlbumArtist: "Alpha", Album: "One", Track: 1,
		Audio: testaudio.AudioWithSeed(3),
	}))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	group := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM release_group WHERE match_key = ?", "mbid:"+rgMBID))
	twin := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM release_group WHERE match_key <> ?", "mbid:"+rgMBID))

	rep, err := lib.EditEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn})
	if err != nil {
		t.Fatalf("clear with write-back: %v", err)
	}
	if rep.MergedInto != twin {
		t.Fatalf("report merged into %q, want the heuristic twin %q", rep.MergedInto, twin)
	}

	r := meta.NewReader()
	for _, p := range []string{plain, other, twinFile} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.MBReleaseGroupID != "" {
			t.Errorf("%s still names group %q, want it stripped", filepath.Base(filepath.Dir(p)), fm.Tags.MBReleaseGroupID)
		}
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db,
		"SELECT COUNT(*) FROM release_group WHERE match_key = ?", "mbid:"+rgMBID); n != 0 {
		t.Errorf("identified release groups after the forced rescan = %d, want none", n)
	}
	if dr, err := lib.VerifyDerived(ctx); err != nil || !dr.Consistent() {
		t.Errorf("verify derived = %+v (err %v), want clean", dr, err)
	}
}

// TestEditEntityWriteBackRefusedKeepsTheMBID: a member whose backing file the write-back
// engine will not rewrite per item (a virtual or shared file, which is what a cue-sheet
// rip looks like) comes back as a *WriteBackError while the catalog clear stands, and that
// member's file still names the release. The classification is what the CLI reads to
// decide the durability caveat still applies; the other member is stripped either way.
func TestEditEntityWriteBackRefusedKeepsTheMBID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "21212121-2121-2121-2121-212121212121"
	mbTags := []testaudio.TXXXFrame{{Desc: "MusicBrainz Album Id", Value: relMBID}}
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
	album := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM album WHERE match_key = ?", "mbid:"+relMBID))
	makeBackingFileVirtual(t, ctx, db, model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM playable_item WHERE title = ?", "One")))

	_, err := lib.EditEntity(ctx, model.MergeAlbum, album, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("clear with a refused strip = %v, want a *WriteBackError", err)
	}
	if len(wbErr.Failures) != 1 {
		t.Errorf("write-back failures = %+v, want the one refused file", wbErr.Failures)
	}
	if k := catalogScalar[string](t, ctx, db, "SELECT match_key FROM album WHERE pid = ?", string(album)); k == "mbid:"+relMBID {
		t.Errorf("album match_key = %q, want the catalog clear to stand", k)
	}

	r := meta.NewReader()
	refused, err := r.Read(ctx, first)
	if err != nil {
		t.Fatalf("read %s: %v", first, err)
	}
	if refused.Tags.MBReleaseID != relMBID {
		t.Errorf("refused member's release id = %q, want it still on disk", refused.Tags.MBReleaseID)
	}
	stripped, err := r.Read(ctx, second)
	if err != nil {
		t.Fatalf("read %s: %v", second, err)
	}
	if stripped.Tags.MBReleaseID != "" {
		t.Errorf("writable member still names release %q, want it stripped", stripped.Tags.MBReleaseID)
	}
}

// TestEditEntityWriteBackStripsClearedReleaseGroupMBID pins the other half of the strip
// rule: a release-group clear takes MUSICBRAINZ_RELEASEGROUPID off the member files and
// leaves MUSICBRAINZ_ALBUMID alone. The album below it is keyed on its own release id, a
// row the clear deliberately leaves standing, so a member that kept that id re-resolves
// onto it. Stripping it too would mint a heuristic twin and drain the identified row.
func TestEditEntityWriteBackStripsClearedReleaseGroupMBID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const relMBID = "19191919-1919-1919-1919-191919191919"
	const rgMBID = "20202020-2020-2020-2020-202020202020"
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
	album := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM album WHERE match_key = ?", "mbid:"+relMBID))
	group := model.PID(catalogScalar[string](t, ctx, db,
		"SELECT pid FROM release_group WHERE match_key = ?", "mbid:"+rgMBID))

	if _, err := lib.EditEntity(ctx, model.MergeReleaseGroup, group, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn}); err != nil {
		t.Fatalf("clear with write-back: %v", err)
	}

	r := meta.NewReader()
	for _, p := range []string{first, second} {
		fm, err := r.Read(ctx, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if fm.Tags.MBReleaseGroupID != "" {
			t.Errorf("%s still names group %q, want it stripped", filepath.Base(p), fm.Tags.MBReleaseGroupID)
		}
		if fm.Tags.MBReleaseID != relMBID {
			t.Errorf("%s release id = %q, want the album's own identity left alone", filepath.Base(p), fm.Tags.MBReleaseID)
		}
	}

	// A forced rescan keeps both rows: the group on the heuristic key the clear derived,
	// the album on the release id nothing disowned.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the forced rescan = %d, want the identified row alone", n)
	}
	if p := catalogScalar[string](t, ctx, db, "SELECT pid FROM album"); model.PID(p) != album {
		t.Errorf("surviving album pid = %q, want the untouched %q", p, album)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM release_group"); n != 1 {
		t.Fatalf("release_group rows after the forced rescan = %d, want the re-keyed row alone", n)
	}
	if p := catalogScalar[string](t, ctx, db, "SELECT pid FROM release_group"); model.PID(p) != group {
		t.Errorf("surviving release_group pid = %q, want the re-keyed %q", p, group)
	}
}

// TestEditEntityWriteBackAfterMBIDClearMerge covers the merging half of the mbid escape
// hatch. A clear that merges the album into its heuristic twin deletes the pid the caller
// named, so the strip has to fan over the survivor's member files instead: the tagged
// member loses the release id, the twin's own member never carried one to lose, and the
// sibling-field combination that would have fanned a value alongside it is refused before
// anything commits.
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

	// The lone clear commits and merges the album away. The strip follows the merge onto
	// the survivor, so it never asks the catalog for the deleted pid's files.
	rep, err := lib.EditEntity(ctx, model.MergeAlbum, identified, map[string]string{"mbid": ""},
		waxbin.EntityEditOptions{WriteBack: true, Lock: model.LockOn})
	if err != nil {
		t.Fatalf("lone clear with write-back: %v", err)
	}
	if rep.MergedInto != twin {
		t.Fatalf("report merged into %q, want the twin %q", rep.MergedInto, twin)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Fatalf("album rows after the clear = %d, want the twin alone", n)
	}
	if p := catalogScalar[string](t, ctx, db, "SELECT pid FROM album"); model.PID(p) != twin {
		t.Fatalf("surviving album pid = %q, want the twin %q", p, twin)
	}
	fm, err := r.Read(ctx, tagged)
	if err != nil {
		t.Fatalf("read %s: %v", tagged, err)
	}
	if fm.Tags.MBReleaseID != "" {
		t.Errorf("merged member still names release %q, want it stripped", fm.Tags.MBReleaseID)
	}

	// With the id gone from disk, a forced rescan leaves both members on the twin rather
	// than forking the identified album back into existence.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album rows after the forced rescan = %d, want the twin alone", n)
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

// TestRenameEntityWriteBackSurvivesRescan is the durable half of an entity rename. The
// catalog move alone leaves the old title in every member's tags, so the next scan that
// re-resolves them splits the album back apart; with write-back the files carry the new
// name and a forced rescan lands on the same row, pid and all.
func TestRenameEntityWriteBackSurvivesRescan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, fmt.Sprintf("0%d.mp3", i+1)), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: title, Artist: "Alpha", AlbumArtist: "Alpha", Album: "Old Title", Track: i + 1,
			Audio: testaudio.AudioWithSeed(byte(i + 1)),
		}))
	}
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	albumPID := model.PID(catalogScalar[string](t, ctx, db, "SELECT pid FROM album"))

	rep, err := lib.RenameEntity(ctx, model.MergeAlbum, albumPID,
		map[string]string{"album": "New Title"}, waxbin.RenameOptions{WriteBack: true})
	if err != nil {
		t.Fatalf("rename with write-back: %v", err)
	}
	if rep.Outcome != model.EntityRenamed || rep.Members != 2 {
		t.Fatalf("report = %+v, want a two-member rename in place", rep)
	}

	r := meta.NewReader()
	for _, name := range []string{"01.mp3", "02.mp3"} {
		fm, err := r.Read(ctx, filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if fm.Tags.Album != "New Title" {
			t.Errorf("%s ALBUM = %q, want the renamed title", name, fm.Tags.Album)
		}
	}

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("forced rescan: %v", err)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM album"); n != 1 {
		t.Errorf("album rows after the rescan = %d, want the one renamed row", n)
	}
	if after := catalogScalar[string](t, ctx, db, "SELECT pid FROM album"); model.PID(after) != albumPID {
		t.Errorf("album pid after the rescan = %q, want the original %q", after, albumPID)
	}
}

// TestRenameArtistWriteBackCarriesTheCreditHalf: an artist performing on a track and
// producing it holds two references, one on a field and one on the credit surface. The
// rename moves both, and with write-back both reach the file, so the forced rescan that
// re-derives them does not fork the old spelling back.
func TestRenameArtistWriteBackCarriesTheCreditHalf(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("One", "Alpha", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "One")
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleProducer, []string{"Alpha"},
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOff}); err != nil {
		t.Fatalf("producer credit: %v", err)
	}

	artistPID := artistPIDByName(t, ctx, db, "Alpha")
	rep, err := lib.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, waxbin.RenameOptions{WriteBack: true, Lock: model.LockOff})
	if err != nil {
		t.Fatalf("rename with write-back: %v", err)
	}
	if rep.Members != 1 || rep.Credits != 1 {
		t.Fatalf("report = %+v, want one member and one credit", rep)
	}

	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.Artist != "Alpha Prime" {
		t.Errorf("on-disk ARTIST = %q, want the renamed artist", fm.Tags.Artist)
	}

	// The producer tag has no catalog column, so the rescan is what proves it landed.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("scan --force: %v", err)
	}
	credits, err := lib.Credits(ctx, pid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	var producers []string
	for _, c := range credits {
		if c.Role == model.RoleProducer {
			producers = append(producers, c.Name)
		}
	}
	if len(producers) != 1 || producers[0] != "Alpha Prime" {
		t.Errorf("producer credits after the rescan = %v, want [Alpha Prime]", producers)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM artist WHERE name = 'Alpha'"); n != 0 {
		t.Errorf("%d artist rows still spell the old name after the rescan", n)
	}
}

// TestRenameArtistWriteBackWritesEachFileOnce pins the grouping. An item carrying both a
// field edit and a credit edit is one write-back pass, so an unwritable file is one
// failure; fanning the halves out separately would rewrite the file twice and report the
// same file twice.
func TestRenameArtistWriteBackWritesEachFileOnce(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("One", "Alpha", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "One")
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleProducer, []string{"Alpha"},
		waxbin.CreditEditOptions{Lock: model.LockOff}); err != nil {
		t.Fatalf("producer credit: %v", err)
	}

	// The write is atomic (a rewrite into the directory), so removing the directory's
	// write bit is what makes it fail. Windows ignores that bit on a directory, so there
	// the blocker is an open handle on the target, which fails the replacing rename.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if runtime.GOOS == "windows" {
		h, err := os.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = h.Close() })
	}

	artistPID := artistPIDByName(t, ctx, db, "Alpha")
	_, err := lib.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "Alpha Prime"}, waxbin.RenameOptions{WriteBack: true, Lock: model.LockOff})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("rename into a read-only directory = %v, want *WriteBackError", err)
	}
	if len(wbErr.Failures) != 1 {
		t.Fatalf("failures = %d (%+v), want the one file reported once", len(wbErr.Failures), wbErr.Failures)
	}
}

// TestRenameArtistWriteBackRefusesBookTranslator: a translator credit has no tag a scan
// reconstructs, so the rename's write-back refuses that half while the author's write
// beside it lands and the catalog rename stands.
func TestRenameArtistWriteBackRefusesBookTranslator(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "book.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("book query: %d books (err %v)", len(books), err)
	}
	pid := books[0].PID
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleTranslator, []string{"Tolkien"},
		waxbin.CreditEditOptions{Lock: model.LockOff}); err != nil {
		t.Fatalf("translator credit: %v", err)
	}

	artistPID := artistPIDByName(t, ctx, db, "Tolkien")
	rep, err := lib.RenameEntity(ctx, model.MergeArtist, artistPID,
		map[string]string{"name": "J.R.R. Tolkien"}, waxbin.RenameOptions{WriteBack: true, Lock: model.LockOff})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("rename over a book translator credit = %v, want *WriteBackError", err)
	}
	if len(wbErr.Failures) != 1 || !strings.Contains(wbErr.Failures[0].Reason, "translator") {
		t.Fatalf("failures = %+v, want the translator role refused", wbErr.Failures)
	}
	// The catalog rename stands, both halves of it.
	if rep == nil || rep.Members != 1 || rep.Credits != 1 {
		t.Fatalf("report = %+v, want the rename to stand with one member and one credit", rep)
	}
	if got := catalogScalar[string](t, ctx, db, "SELECT name FROM artist WHERE pid = ?", string(artistPID)); got != "J.R.R. Tolkien" {
		t.Errorf("artist name = %q, want the rename to stand", got)
	}
	fm, err := meta.NewReader().Read(ctx, src)
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if fm.Tags.AlbumArtist != "J.R.R. Tolkien" {
		t.Errorf("on-disk ALBUMARTIST = %q, want the author half written beside the refusal", fm.Tags.AlbumArtist)
	}
	// The refusal has to reach the review queue, not just the returned error. The author
	// write that landed on the same file replaces its edit-origin diagnostics wholesale,
	// so a refusal stamped ahead of that write would be cleared by it and the file would
	// read as fully synced.
	diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: pid, Code: model.DiagTagWriteUnsynced})
	if err != nil {
		t.Fatalf("file diagnostics: %v", err)
	}
	if len(diags) != 1 || !strings.Contains(diags[0].Detail, "translator") {
		t.Errorf("drift diagnostics = %+v, want the translator refusal still queryable", diags)
	}
}

// TestSetCreditsShapesSingleValuedRoles: a two-holder conductor credit lands on disk
// as one joined value rather than two, so the write reports clean and the typed
// projection reads both names, while a producer credit keeps one value per holder.
func TestSetCreditsShapesSingleValuedRoles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Symphony", "Orchestra", "Live", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Symphony")

	conductors := []string{"Ana Conductor", "Ben Conductor"}
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleConductor, conductors,
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOff}); err != nil {
		t.Fatalf("conductor credit write-back: %v", err)
	}
	producers := []string{"Pat Producer", "Quinn Producer"}
	if _, _, err := lib.SetCredits(ctx, pid, model.RoleProducer, producers,
		waxbin.CreditEditOptions{WriteBack: true, Lock: model.LockOff}); err != nil {
		t.Fatalf("producer credit write-back: %v", err)
	}

	doc, err := waxlabel.ParseFile(ctx, src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, _ := doc.Get(tag.Conductor); len(got) != 1 || got[0] != "Ana Conductor; Ben Conductor" {
		t.Errorf("CONDUCTOR on disk = %v, want the two names joined", got)
	}
	if got, _ := doc.Get(tag.Producer); len(got) != 2 || got[0] != producers[0] || got[1] != producers[1] {
		t.Errorf("PRODUCER on disk = %v, want %v", got, producers)
	}
	diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{Code: model.DiagTagWriteLost})
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if len(diags) != 0 {
		t.Errorf("tag_write_lost diagnostics = %+v, want none for shaped credits", diags)
	}
}
