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
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
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

// frontOf is the hash of the cover a reference draws, empty for none.
func frontOf(t *testing.T, ctx context.Context, lib *waxbin.Library, ref model.EntityRef) string {
	t.Helper()
	got, err := lib.ArtHashes(ctx, []model.EntityRef{ref})
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	return got[ref]
}

// TestAnImportedFolderCoverIsTheItemsAtOnce imports an album folder the catalog has
// never held. The import catalogs the audio before it places the folder's cover, and
// once the cover has landed the item draws it, with no scan to find it. A folder with no
// cover leaves its file stating none. The scan that follows reads neither file again.
func TestAnImportedFolderCoverIsTheItemsAtOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	cover := folderCover(t, 4)
	writeFile(t, filepath.Join(staging, "Covered", "01 - Song.mp3"),
		testaudio.BuildMP3WithAudio("Song", "Artist", "Covered", 1, testaudio.AudioWithSeed(60)))
	writeFile(t, filepath.Join(staging, "Covered", "cover.png"), cover)
	writeFile(t, filepath.Join(staging, "Bare", "01 - Tune.mp3"),
		testaudio.BuildMP3WithAudio("Tune", "Artist", "Bare", 1, testaudio.AudioWithSeed(61)))

	lib := openManaged(t, ctx, db, root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil || plan.Importable() != 2 {
		t.Fatalf("plan = %+v (err %v), want both files importable", plan, err)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 2 {
		t.Fatalf("import = %+v (err %v), want both files imported", rep, err)
	}
	song, tune := itemPIDByTitle(t, ctx, lib, "Song"), itemPIDByTitle(t, ctx, lib, "Tune")
	if got := frontOf(t, ctx, lib, model.EntityRef{Type: model.ArtTrack, PID: song}); got != art.Hash(cover) {
		t.Errorf("front = %q after the import, want the folder cover it placed", got)
	}
	if got := frontOf(t, ctx, lib, model.EntityRef{Type: model.ArtTrack, PID: tune}); got != "" {
		t.Errorf("the file with no cover beside it draws %s", got)
	}
	if n := catalogScalar[int](t, ctx, db, "SELECT COUNT(*) FROM file WHERE kind = 'audio' AND front_hash IS NULL"); n != 0 {
		t.Errorf("%d imported files are left with their cover unknown", n)
	}
	rep, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep.Total.Unchanged != 2 || rep.Total.Reread != 0 {
		t.Errorf("the scan after the import = %+v, want both files unchanged and unread", rep.Total)
	}
}

// TestACopysCoverSurvivesTheTrash is the item two files back, its primary in a folder
// with no cover and a copy in one that has a cover. The item draws the copy's cover,
// loses it when the copy is trashed, with no read of the primary, and has it back when
// the copy is restored.
func TestACopysCoverSurvivesTheTrash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	audio := testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(62))
	bare, covered := filepath.Join(root, "A", "song.mp3"), filepath.Join(root, "B", "song.mp3")
	cover := folderCover(t, 4)
	writeFile(t, bare, audio)
	writeFile(t, covered, audio)
	writeFile(t, filepath.Join(root, "B", "cover.png"), cover)

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	files, err := lib.ItemFiles(ctx, pid)
	if err != nil || len(files) != 2 {
		t.Fatalf("item files = %+v (err %v), want the two copies", files, err)
	}
	if string(files[0].Path) != bare {
		t.Fatalf("the primary is %s, want the file with no cover beside it", files[0].Path)
	}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(cover) {
		t.Fatalf("front = %q, want the cover beside the copy", got)
	}

	plan, err := lib.PlanDeleteFiles(ctx, []model.PID{files[1].FilePID}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := frontOf(t, ctx, lib, ref); got != "" {
		t.Errorf("front = %s with the file that stated it in the trash", got)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v), want the one entry", entries, err)
	}
	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(cover) {
		t.Errorf("front = %q once the file is restored, want its cover back", got)
	}
}

// TestAnAlbumCoverWrittenBackIsItsTracksOwn sets an album cover with write-back over
// tracks that drew their folder's cover. Each file embeds the new picture then, which a
// read of it would put ahead of the folder's, so each track's own cover is the one
// written, and a forced scan finds nothing left to change.
func TestAnAlbumCoverWrittenBackIsItsTracksOwn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for n, title := range []string{"One", "Two"} {
		writeFile(t, filepath.Join(root, "Album", title+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Artist", "Album", n+1, testaudio.AudioWithSeed(byte(63+n))))
	}
	writeFile(t, filepath.Join(root, "Album", "cover.png"), folderCover(t, 4))
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("query: %d items (err %v), want 2", len(items), err)
	}
	album := model.EntityRef{Type: model.ArtAlbum, PID: items[0].AlbumPID}
	if err := lib.SetEntityArt(ctx, model.ArtAlbum, album.PID, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOn, WriteBack: true}); err != nil {
		t.Fatalf("set album art: %v", err)
	}
	want := frontOf(t, ctx, lib, album)
	own := func(when string) {
		t.Helper()
		for _, it := range items {
			prov, err := lib.ArtProvenance(ctx, it.ArtRef(), model.ArtRoleFront)
			if err != nil || prov.Level != model.ArtTrack || prov.SourceHash != want || prov.Source != model.SourceTag {
				t.Errorf("%s: %s draws %+v (err %v), want the written cover as its own, from its tags", when, it.Title, prov, err)
			}
		}
	}
	own("after the write")
	rep, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true})
	if err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if rep.Total.SidecarsUpdated != 0 || rep.Total.ItemsUpdated != 0 {
		t.Errorf("the forced scan = %+v, want nothing left for it to change", rep.Total)
	}
	own("after a forced scan")
}

// TestAClearedCoverWrittenBackLeavesTheFoldersCover strips a file's embedded picture
// beside a folder cover. The file states the folder's cover from then on, which the
// catalog records without a scan, and an item whose cover is not held empty draws it.
func TestAClearedCoverWrittenBackLeavesTheFoldersCover(t *testing.T) {
	t.Parallel()
	for _, lock := range []model.LockChange{model.LockOn, model.LockOff} {
		ctx := context.Background()
		root := t.TempDir()
		db := filepath.Join(t.TempDir(), "catalog.db")
		writeFile(t, filepath.Join(root, "Album", "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
		beside := folderCover(t, 4)
		writeFile(t, filepath.Join(root, "Album", "cover.png"), beside)
		lib := openManaged(t, ctx, db, root)
		if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
			t.Fatalf("scan: %v", err)
		}
		pid := itemPIDByTitle(t, ctx, lib, "Song")
		ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
		if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t),
			waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
			t.Fatalf("set item art: %v", err)
		}
		embedded := frontOf(t, ctx, lib, ref)
		stated := func() string {
			t.Helper()
			return catalogScalar[string](t, ctx, db, "SELECT COALESCE(front_hash, 'unknown') FROM file WHERE kind = 'audio'")
		}
		if embedded == art.Hash(beside) || stated() != embedded {
			t.Fatalf("lock %v: the file states %q and the item draws %q, want the embedded cover for both", lock, stated(), embedded)
		}
		if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, nil, waxbin.ArtEditOptions{Lock: lock, WriteBack: true}); err != nil {
			t.Fatalf("lock %v: clear item art: %v", lock, err)
		}
		if got := stated(); got != art.Hash(beside) {
			t.Errorf("lock %v: after the strip the file states %q, want the folder's cover", lock, got)
		}
		want := ""
		if lock == model.LockOff {
			want = art.Hash(beside)
		}
		if got := frontOf(t, ctx, lib, ref); got != want {
			t.Errorf("lock %v: front = %q after the clear, want %q", lock, got, want)
		}
	}
}

// TestAClearThatLeavesABackCoverStatesIt strips the front cover from a file that also
// embeds a back cover. A read of the file falls back to the back cover, so that is what
// the file states after the write and what the catalog records, and a forced scan finds
// nothing left to change. Recording the file as bare would hold until the next forced
// scan and then flip the item's cover.
func TestAClearThatLeavesABackCoverStatesIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	song := filepath.Join(root, "Album", "song.mp3")
	writeFile(t, song, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, coverPNG(t),
		waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
		t.Fatalf("set item art: %v", err)
	}
	back := folderCover(t, 6)
	doc, err := waxlabel.ParseFile(ctx, song)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := doc.Edit().AddPicture(waxlabel.Picture{Type: waxlabel.PicBackCover, Data: back}).Prepare(waxlabel.WithVerifyEssence())
	if err != nil {
		t.Fatalf("prepare the back cover: %v", err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatalf("embed the back cover: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := frontOf(t, ctx, lib, ref); got == art.Hash(back) || got == "" {
		t.Fatalf("front = %q with both pictures embedded, want the front cover", got)
	}

	if err := lib.SetItemArt(ctx, pid, model.ArtRoleFront, nil, waxbin.ArtEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
		t.Fatalf("clear item art: %v", err)
	}
	stated := catalogScalar[string](t, ctx, db, "SELECT COALESCE(front_hash, 'unknown') FROM file WHERE kind = 'audio'")
	if stated != art.Hash(back) {
		t.Errorf("after the strip the file states %q, want the back cover a read falls back to", stated)
	}
	front := frontOf(t, ctx, lib, ref)
	rep, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true})
	if err != nil {
		t.Fatalf("forced scan: %v", err)
	}
	if rep.Total.SidecarsUpdated != 0 || rep.Total.ItemsUpdated != 0 || frontOf(t, ctx, lib, ref) != front {
		t.Errorf("the forced scan = %+v and the cover moved from %q to %q, want nothing left for it to change",
			rep.Total, front, frontOf(t, ctx, lib, ref))
	}
}

// TestAnAcquiredThumbnailStandsInForTheCover imports a download whose tags carry its
// video's thumbnail, marked as a stand-in, then a second track of the same album whose
// tags carry the real cover. The thumbnail shows until then, and the first track draws
// the album's real cover after. An episode refuses the mark.
func TestAnAcquiredThumbnailStandsInForTheCover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openMediaTyped(t, ctx, db, t.TempDir(), t.TempDir(), t.TempDir())
	stage := func(name, title string, n int, picture []byte) string {
		t.Helper()
		path := filepath.Join(acq, name)
		writeFile(t, path, testaudio.BuildMP3WithAudio(title, "Band", "Record", n, testaudio.AudioWithSeed(byte(60+n))))
		doc, err := waxlabel.ParseFile(ctx, path)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		plan, err := doc.Edit().AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: picture}).Prepare(waxlabel.WithVerifyEssence())
		if err != nil {
			t.Fatalf("prepare the picture: %v", err)
		}
		if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
			t.Fatalf("embed the picture: %v", err)
		}
		return path
	}
	importOne := func(path string, mark bool) {
		t.Helper()
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: path, PlaceholderCover: mark}, model.KindTrack,
			waxbin.AcquiredMeta{SourceType: model.SourceYouTube})
		if err != nil {
			t.Fatalf("ImportAcquired: %v", err)
		}
		if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
			t.Fatalf("apply = %+v (err %v)", rep, err)
		}
	}
	thumb, cover := folderCover(t, 4), folderCover(t, 9)
	importOne(stage("one.mp3", "One", 1, thumb), true)
	ref := model.EntityRef{Type: model.ArtTrack, PID: itemPIDByTitle(t, ctx, lib, "One")}
	prov, err := lib.ArtProvenance(ctx, ref, model.ArtRoleFront)
	if err != nil || prov.SourceHash != art.Hash(thumb) || !prov.Placeholder {
		t.Fatalf("imported thumbnail = %+v (err %v), want it standing in", prov, err)
	}
	importOne(stage("two.mp3", "Two", 2, cover), false)
	prov, err = lib.ArtProvenance(ctx, ref, model.ArtRoleFront)
	if err != nil || prov.SourceHash != art.Hash(cover) || prov.Level != model.ArtAlbum || prov.Placeholder {
		t.Errorf("after the real cover = %+v (err %v), want the album's cover from the second track", prov, err)
	}
	if _, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: stage("ep.mp3", "Ep", 3, thumb), PlaceholderCover: true},
		model.KindEpisode, waxbin.AcquiredMeta{}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("an episode's mark = %v, want CodeInvalid", err)
	}
}

// TestAMarkedThumbnailYieldsToTheCoverBesideIt: a track's tags carry a video thumbnail
// and a cover sits beside it. Once the thumbnail is marked a stand-in, the next scan
// takes the folder's cover as the track's, as it would over a corrupt embedded picture,
// and an unmark gives the thumbnail back at once.
func TestAMarkedThumbnailYieldsToTheCoverBesideIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	song := filepath.Join(root, "Album", "song.mp3")
	writeFile(t, song, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	thumb, cover := folderCover(t, 4), folderCover(t, 9)
	doc, err := waxlabel.ParseFile(ctx, song)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := doc.Edit().AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: thumb}).Prepare(waxlabel.WithVerifyEssence())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Album", "cover.png"), cover)
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	ref := model.EntityRef{Type: model.ArtTrack, PID: itemPIDByTitle(t, ctx, lib, "Song")}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(thumb) {
		t.Fatalf("front = %q before the mark, want the embedded thumbnail", got)
	}
	if _, err := lib.SetArtPlaceholder(ctx, art.Hash(thumb), true); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(cover) {
		t.Fatalf("front = %q after the mark and a scan, want the cover beside the file", got)
	}
	if _, err := lib.SetArtPlaceholder(ctx, art.Hash(thumb), false); err != nil {
		t.Fatal(err)
	}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(thumb) {
		t.Errorf("front = %q after the unmark, want the thumbnail in the tags again", got)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := frontOf(t, ctx, lib, ref); got != art.Hash(thumb) {
		t.Errorf("front = %q after a forced scan, want the thumbnail the read finds", got)
	}
}
