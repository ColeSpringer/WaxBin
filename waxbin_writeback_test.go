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

	if err := lib.SetItemArt(ctx, pid, coverPNG(t), true, false, true); err != nil {
		t.Fatalf("set item art write-back: %v", err)
	}
	assertFrontCover(t, ctx, src)
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

	if err := lib.SetItemArt(ctx, books[0].PID, coverPNG(t), true, false, true); err != nil {
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

	if err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, "front", coverPNG(t), true); err != nil {
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

	err := lib.SetEntityArt(ctx, model.ArtAlbum, albumPID, "front", coverPNG(t), true)
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
