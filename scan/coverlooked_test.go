package scan

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// TestResolveCoverSaysWhetherItLooked separates a finding of no cover from a lookup that
// could not finish. A folder with no cover file in it is a finding. A cover file that is
// there and gives no picture is not, whether it is empty, cut short or will not read:
// that is a cover caught mid-copy as much as a broken one.
func TestResolveCoverSaysWhetherItLooked(t *testing.T) {
	t.Parallel()
	audio := func(dir string) string { return filepath.Join(dir, "song.mp3") }

	bare := t.TempDir()
	if got, looked := resolveCover(audio(bare), nil, newArtCache()); got != nil || !looked {
		t.Errorf("a folder with no cover = %v (looked %v), want a finding of none", got, looked)
	}

	for name, bytes := range map[string][]byte{"empty": nil, "cut short": []byte("\xff\xd8\xff")} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, looked := resolveCover(audio(dir), nil, newArtCache()); got != nil || looked {
			t.Errorf("a cover file that is %s = %v (looked %v), want no finding", name, got, looked)
		}
	}

	gone := filepath.Join(t.TempDir(), "missing")
	if got, looked := resolveCover(audio(gone), nil, newArtCache()); got != nil || looked {
		t.Errorf("a folder that will not list = %v (looked %v), want no finding", got, looked)
	}

	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("file permissions cannot make a cover unreadable here")
	}
	shut := t.TempDir()
	cover := filepath.Join(shut, "cover.jpg")
	writeJPEG(t, cover, 64, 64)
	if err := os.Chmod(cover, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cover, 0o644) })
	if got, looked := resolveCover(audio(shut), nil, newArtCache()); got != nil || looked {
		t.Errorf("a cover that will not read = %v (looked %v), want no finding", got, looked)
	}
	// A lower-priority cover that does read is the cover all the same.
	writeJPEG(t, filepath.Join(shut, "folder.jpg"), 32, 32)
	if got, looked := resolveCover(audio(shut), nil, newArtCache()); got == nil || got.Width != 32 || !looked {
		t.Errorf("with a readable folder.jpg beside it = %v (looked %v), want that picture", got, looked)
	}
}

// coverPuts records what each track put said about its cover.
type coverPuts struct {
	model.Catalog
	noCover    []bool
	tagsUnread []bool
}

func (c *coverPuts) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	c.noCover = append(c.noCover, in.NoCover && in.CoverArt == nil)
	c.tagsUnread = append(c.tagsUnread, in.TagsUnread)
	return c.Catalog.PutScannedTrack(ctx, in)
}

// TestACoverYetToArriveIsNoFinding: a read of a file whose folder cover follows it, as an
// import's does, says nothing about the cover, where the same read otherwise says the
// file has none.
func TestACoverYetToArriveIsNoFinding(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	path := filepath.Join(root, "song.mp3")
	writeMP3(t, path, "Song", 1)
	puts := &coverPuts{Catalog: st}
	sc := New(puts, meta.NewReader(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{CoverPending: true}); err != nil {
		t.Fatalf("scan with the cover pending: %v", err)
	}
	if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(puts.noCover) != 2 || puts.noCover[0] || !puts.noCover[1] {
		t.Errorf("no-cover findings = %v, want none while the cover is pending and one after", puts.noCover)
	}
}

// frontOf is the hash of the cover an item draws, empty for none.
func frontOf(t *testing.T, st *sqlite.Store, pid model.PID) string {
	t.Helper()
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	got, err := st.ArtHashes(context.Background(), []model.EntityRef{ref})
	if err != nil {
		t.Fatalf("ArtHashes: %v", err)
	}
	return got[ref]
}

// coverUnknown reports whether the catalog knows what cover the file at path states.
func coverUnknown(t *testing.T, st *sqlite.Store, lib *model.Library, path string) bool {
	t.Helper()
	idx, err := st.LoadScopedFileIndex(context.Background(), lib.ID, nil)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	f, ok := idx[path]
	if !ok {
		t.Fatalf("%s is not in the index", path)
	}
	return f.CoverUnknown
}

// TestAFileWhoseTagsWereNotReadSpeaksForItsFolders: a container the reader has no parser
// for comes back with no tags read, so what its read found is what stands beside it. The
// put says so, and a folder cover such a file took is given back once it is gone from
// the folder, which a file that says nothing never would.
func TestAFileWhoseTagsWereNotReadSpeaksForItsFolders(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	path := filepath.Join(root, "song.mp3")
	writeMP3(t, path, "Song", 1)
	cover := filepath.Join(root, "cover.jpg")
	writeJPEG(t, cover, 64, 64)
	puts := &coverPuts{Catalog: st}
	ctx := context.Background()
	sc := New(puts, unparsedReader{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, out, err := sc.ScanFileWith(ctx, lib, path, FileOptions{})
	if err != nil {
		t.Fatalf("scan with no parser: %v", err)
	}
	if frontOf(t, st, out.ItemPID) == "" {
		t.Fatal("the file took no cover from its folder")
	}
	if err := os.Remove(cover); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{}); err != nil {
		t.Fatalf("scan with the cover gone: %v", err)
	}
	if len(puts.noCover) != 2 || puts.noCover[0] || !puts.noCover[1] || !puts.tagsUnread[0] || !puts.tagsUnread[1] {
		t.Errorf("no-cover findings = %v, tags unread = %v, want a finding of none from the second read and both marked unread",
			puts.noCover, puts.tagsUnread)
	}
	if got := frontOf(t, st, out.ItemPID); got != "" {
		t.Errorf("front = %s after the folder cover went, want none", got)
	}
}

// TestAnUnknownCoverIsSettledFromBesideTheFile: a file whose cover the catalog does not
// know has no picture in its tags, so the scan that fast-paths it says what stands
// beside it, a cover or none, without reading its audio.
func TestAnUnknownCoverIsSettledFromBesideTheFile(t *testing.T) {
	t.Parallel()
	for _, withCover := range []bool{false, true} {
		st, lib, sc, cr, root := fastPathFixture(t)
		ctx := context.Background()
		path := filepath.Join(root, "album", "song.mp3")
		writeMP3(t, path, "Song", 1)
		if withCover {
			writeJPEG(t, filepath.Join(root, "album", "cover.jpg"), 64, 64)
		}
		// The first read states what the file has, and a strip the writer could not follow
		// up leaves that unknown, with the item's cover cleared as the strip's edit did.
		_, out, err := sc.ScanFileWith(ctx, lib, path, FileOptions{})
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if withCover != (frontOf(t, st, out.ItemPID) != "") {
			t.Fatalf("withCover=%v: front = %q after the first read", withCover, frontOf(t, st, out.ItemPID))
		}
		if _, err := st.UpdateFileStateIfUnchanged(ctx, stripped(t, st, lib, path)); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetItemArt(ctx, out.ItemPID, model.ArtRoleFront, nil, "", model.Attribution{}, model.LockOff, false); err != nil {
			t.Fatal(err)
		}
		if !coverUnknown(t, st, lib, path) {
			t.Fatalf("withCover=%v: the catalog knows the file's cover before the scan that settles it", withCover)
		}
		reads := cr.reads
		res, err := sc.Scan(ctx, Request{Library: lib}, nil)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if cr.reads != reads || res.Reread != 0 {
			t.Errorf("withCover=%v: the scan read the file (%d reads, %d reread) to settle its cover", withCover, cr.reads-reads, res.Reread)
		}
		if coverUnknown(t, st, lib, path) {
			t.Errorf("withCover=%v: the file's cover is still unknown after the scan", withCover)
		}
		if got := frontOf(t, st, out.ItemPID); withCover != (got != "") {
			t.Errorf("withCover=%v: front = %q", withCover, got)
		}
		if want := map[bool]int{false: 0, true: 1}[withCover]; res.SidecarsUpdated != want || res.Unchanged != 1-want {
			t.Errorf("withCover=%v: scan = %+v, want %d cover change", withCover, res, want)
		}
		// The next scan has nothing left to settle.
		if res, err := sc.Scan(ctx, Request{Library: lib}, nil); err != nil || res.Unchanged != 1 || cr.reads != reads {
			t.Errorf("withCover=%v: the scan after = %+v (err %v), want the file unchanged and unread", withCover, res, err)
		}
	}
}

// stripped is the file state update of a write that stripped the file's picture and
// could not say what it states now.
func stripped(t *testing.T, st *sqlite.Store, lib *model.Library, path string) model.FileStateUpdate {
	t.Helper()
	idx, err := st.LoadScopedFileIndex(context.Background(), lib.ID, nil)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	f := idx[path]
	return model.FileStateUpdate{FilePID: f.FilePID, ExpectedSize: f.Size, ExpectedMTimeNS: f.MTimeNS,
		NewSize: f.Size, NewMTimeNS: f.MTimeNS, NewContentHash: "stripped", Cover: &model.FileCover{}}
}

// TestACoverPlacedAfterTheAudioIsSettled is the import's order: the file is cataloged,
// its folder's cover lands, and the catalog is told what stands beside the file. The item
// has its cover then, and the next scan does not read the file again to find it.
func TestACoverPlacedAfterTheAudioIsSettled(t *testing.T) {
	t.Parallel()
	st, lib, sc, cr, root := fastPathFixture(t)
	ctx := context.Background()
	covered, bare := filepath.Join(root, "a", "song.mp3"), filepath.Join(root, "b", "song.mp3")
	writeMP3(t, covered, "One", 1)
	writeMP3(t, bare, "Two", 2)
	pids := map[string]model.PID{}
	var placed []PlacedFile
	for _, path := range []string{covered, bare} {
		_, out, err := sc.ScanFileWith(ctx, lib, path, FileOptions{CoverPending: true})
		if err != nil {
			t.Fatalf("scan %s: %v", path, err)
		}
		pids[path] = out.ItemPID
		placed = append(placed, PlacedFile{FilePID: out.FilePID, Path: path})
	}
	writeJPEG(t, filepath.Join(root, "a", "cover.jpg"), 64, 64)
	reads := cr.reads
	if n := sc.SettleFolderCovers(ctx, lib, placed); n != 1 {
		t.Errorf("SettleFolderCovers moved %d covers, want the one", n)
	}
	if frontOf(t, st, pids[covered]) == "" {
		t.Error("the file with a cover beside it has none in the catalog")
	}
	if frontOf(t, st, pids[bare]) != "" {
		t.Error("the file with nothing beside it took a cover")
	}
	for _, path := range []string{covered, bare} {
		if coverUnknown(t, st, lib, path) {
			t.Errorf("%s: the cover is still unknown", path)
		}
	}
	res, err := sc.Scan(ctx, Request{Library: lib}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if cr.reads != reads || res.Unchanged != 2 {
		t.Errorf("the scan after = %+v with %d reads, want both files unchanged and unread", res, cr.reads-reads)
	}
	// A file whose cover the catalog knows is left alone.
	if n := sc.SettleFolderCovers(ctx, lib, placed); n != 0 {
		t.Errorf("a second SettleFolderCovers moved %d covers, want none", n)
	}
}

// failingCovers refuses every folder cover, as a store does while it is closing.
type failingCovers struct{ model.Catalog }

func (failingCovers) PutFolderCover(context.Context, model.FolderCoverInput) (bool, error) {
	return false, errors.New("the store is busy")
}

// TestAFolderCoverThatWillNotSettleLeavesItsFileUnchanged: telling the catalog what
// stands beside an unchanged file is best done and never owed to this scan. When the
// store refuses, the file is still unchanged, and the next scan tries again.
func TestAFolderCoverThatWillNotSettleLeavesItsFileUnchanged(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "album", "song.mp3")
	writeMP3(t, path, "Song", 1)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := New(st, meta.NewReader(), quiet).ScanFileWith(ctx, lib, path, FileOptions{CoverPending: true}); err != nil {
		t.Fatalf("scan with the cover pending: %v", err)
	}
	res := scanAll(t, New(failingCovers{st}, meta.NewReader(), quiet), lib, false)
	if res.Errored != 0 || res.Unchanged != 1 {
		t.Errorf("scan = %+v, want the file unchanged and no error", res)
	}
	if !coverUnknown(t, st, lib, path) {
		t.Error("the file's cover is known after a settle the store refused")
	}
	if res := scanAll(t, New(st, meta.NewReader(), quiet), lib, false); res.Unchanged != 1 || coverUnknown(t, st, lib, path) {
		t.Errorf("the scan after = %+v, want the cover settled then", res)
	}
}

// TestACoverSettledOnAFileTheFolderRuleReadsAgainIsCountedOnce: the walk passes an
// unchanged file, settles its unknown cover and counts it for that, and then the folder
// rule reads it again to join a book a later file made. The file takes the count of that
// read in place of the one it had, and no other counter pays for it.
func TestACoverSettledOnAFileTheFolderRuleReadsAgainIsCountedOnce(t *testing.T) {
	t.Parallel()
	st, lib, sc, root := kindFixture(t, model.MediaMixed)
	ctx := context.Background()
	writeUnder(t, root, "Author/Tome/02.mp3", testaudio.MP3Spec{Title: "Chapter 2", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 2, Audio: testaudio.AudioWithSeed(2)})
	second := filepath.Join(root, "Author", "Tome", "02.mp3")
	writeJPEG(t, filepath.Join(root, "Author", "Tome", "cover.jpg"), 64, 64)
	_, out, err := sc.ScanFileWith(ctx, lib, second, FileOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := st.UpdateFileStateIfUnchanged(ctx, stripped(t, st, lib, second)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetItemArt(ctx, out.ItemPID, model.ArtRoleFront, nil, "", model.Attribution{}, model.LockOff, false); err != nil {
		t.Fatal(err)
	}
	writeUnder(t, root, "Author/Tome/01.mp3", testaudio.MP3Spec{Title: "Chapter 1", Artist: "Author", AlbumArtist: "Author",
		Album: "Tome", Track: 1, TXXX: []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}, Audio: testaudio.AudioWithSeed(1)})
	res := scanAll(t, sc, lib, false)
	oneBook(t, st, 2)
	if res.Unchanged != 0 || res.SidecarsUpdated != 0 || res.ItemsCreated+res.ItemsUpdated != 2 {
		t.Errorf("scan = %+v, want the new part and the part that joined it, each counted once", res)
	}
}
