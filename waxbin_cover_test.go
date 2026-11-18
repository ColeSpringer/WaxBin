package waxbin_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
)

// folderCover is a decodable picture side pixels square, whose byte length grows with
// side so a replacement is seen by its size and not by its mtime alone.
func folderCover(t *testing.T, side int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := range img.Pix {
		img.Pix[i] = uint8(i * 37)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestAFolderCoverChangeMovesEveryHashItFeeds follows one folder cover through a
// replacement and a removal, read back through ArtHashes for the two tracks beside it
// and the album that borrows it. Each scan is a plain one, so the unchanged audio takes
// the fast path and only the cover's own change sends the files back through the store.
func TestAFolderCoverChangeMovesEveryHashItFeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for n, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, "Album", title+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Artist", "Album", n+1, testaudio.AudioWithSeed(byte(40+n))))
	}
	coverPath := filepath.Join(root, "Album", "cover.png")
	first, second := folderCover(t, 4), folderCover(t, 9)
	if len(first) == len(second) {
		t.Fatalf("both covers are %d bytes, so the replacement would rest on the clock", len(first))
	}
	writeFile(t, coverPath, first)

	lib := openManaged(t, ctx, db, root)
	rescan := func() scan.Result {
		t.Helper()
		rep, err := lib.Scan(ctx, waxbin.ScanRequest{})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return rep.Total
	}
	rescan()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("query: %d items (err %v), want 2", len(items), err)
	}
	refs := []model.EntityRef{
		items[0].ArtRef(), items[1].ArtRef(), {Type: model.ArtAlbum, PID: items[0].AlbumPID},
	}
	check := func(when, want string) {
		t.Helper()
		got, err := lib.ArtHashes(ctx, refs)
		if err != nil {
			t.Fatalf("ArtHashes: %v", err)
		}
		for _, ref := range refs {
			if got[ref] != want {
				t.Errorf("%s: %s %s = %q, want %q", when, ref.Type, ref.PID, got[ref], want)
			}
		}
	}
	check("scanned", art.Hash(first))

	writeFile(t, coverPath, second)
	if total := rescan(); total.SidecarsUpdated != 2 {
		t.Errorf("a replaced cover updated %d files' sidecars, want both: %+v", total.SidecarsUpdated, total)
	}
	check("replaced", art.Hash(second))

	if err := os.Remove(coverPath); err != nil {
		t.Fatal(err)
	}
	if total := rescan(); total.SidecarsUpdated != 2 {
		t.Errorf("a removed cover updated %d files' sidecars, want both: %+v", total.SidecarsUpdated, total)
	}
	check("removed", "")
	if total := rescan(); total.Unchanged != 2 || total.SidecarsUpdated != 0 {
		t.Errorf("a scan after the removal settled = %+v, want both files unchanged", total)
	}
}

// TestAnUnreadableFolderCoverTakesNothingAway: a cover file that is there and will not
// read is not a finding of no cover. The track read again beside it keeps the picture it
// had, where a scan that took the failed read for an absence would drop it and, the
// cover's size and time being unchanged, never look again.
func TestAnUnreadableFolderCoverTakesNothingAway(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("file permissions cannot make a cover unreadable here")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	song := filepath.Join(root, "Album", "One.mp3")
	writeFile(t, song, testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(50)))
	coverPath := filepath.Join(root, "Album", "cover.png")
	cover := folderCover(t, 4)
	writeFile(t, coverPath, cover)

	lib := openManaged(t, ctx, db, root)
	scan := func() {
		t.Helper()
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	scan()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query: %d items (err %v), want 1", len(items), err)
	}
	ref := items[0].ArtRef()
	own := func(when string) {
		t.Helper()
		prov, err := lib.ArtProvenance(ctx, ref, model.ArtRoleFront)
		if err != nil || prov.SourceHash != art.Hash(cover) || prov.Level != model.ArtTrack {
			t.Errorf("%s: front = %+v (err %v), want the track's own folder cover", when, prov, err)
		}
	}
	own("scanned")

	if err := os.Chmod(coverPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(coverPath, 0o644) })
	// The audio changes, so the file is read in full beside a cover it cannot open.
	writeFile(t, song, testaudio.BuildMP3WithAudio("One Again", "Artist", "Album", 1, testaudio.AudioWithSeed(50)))
	scan()
	own("with the cover unreadable")
	if err := os.Chmod(coverPath, 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	own("readable again")
}

// TestAnImportKeepsTheCoverItIsAboutToPlace brings an album folder back through an
// import. The import reads each file before it places the folder's cover, and that read
// finding no cover yet is not a reason to take the item's cover away.
func TestAnImportKeepsTheCoverItIsAboutToPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	album := filepath.Join(root, "Artist", "Album")
	cover := folderCover(t, 4)
	writeFile(t, filepath.Join(album, "01 - Song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, filepath.Join(album, "cover.png"), cover)

	lib := openManaged(t, ctx, db, root)
	scanAll := func() {
		t.Helper()
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	scanAll()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("query: %d items (err %v), want 1", len(items), err)
	}
	ref := items[0].ArtRef()
	front := func() string {
		t.Helper()
		got, err := lib.ArtHashes(ctx, []model.EntityRef{ref})
		if err != nil {
			t.Fatalf("ArtHashes: %v", err)
		}
		return got[ref]
	}
	if front() != art.Hash(cover) {
		t.Fatal("the scanned track does not draw its folder cover")
	}

	if err := os.Rename(album, filepath.Join(staging, "Album")); err != nil {
		t.Fatal(err)
	}
	scanAll()
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil || plan.Importable() != 1 {
		t.Fatalf("plan = %+v (err %v), want the one file importable", plan, err)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 1 {
		t.Fatalf("import = %+v (err %v), want the one file imported", rep, err)
	}
	if got := front(); got != art.Hash(cover) {
		t.Errorf("front = %q after the import, want the cover the item already drew", got)
	}
	if !fileExists(filepath.Join(album, "cover.png")) {
		t.Error("the import did not place the folder's cover")
	}
}
