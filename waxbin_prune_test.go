package waxbin_test

import (
	"context"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/trash"
)

// oneTrackCue is a sheet carving a single track, which leaves its file one track.
const oneTrackCue = "FILE \"01 Song.mp3\" MP3\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"

// filesUnder lists the files below dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}

func trashOne(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID, mode model.DeleteMode) *trash.Report {
	t.Helper()
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{pid}, mode)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	return rep
}

// TestTrashTakesSidecarsAndTheFoldersItEmpties: a trashed track's own lyrics and cue sheet
// go into its trash entry, the album folder it leaves holding only a cover and junk goes
// with the cover kept in the entry, the artist folder above goes too, and a restore puts
// every file back where it was.
func TestTrashTakesSidecarsAndTheFoldersItEmpties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	writeFile(t, filepath.Join(album, "01 Song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, filepath.Join(album, "01 Song.lrc"), []byte("[00:00.00]la"))
	writeFile(t, filepath.Join(album, "01 Song.cue"), []byte(oneTrackCue))
	writeFile(t, filepath.Join(album, "Cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(album, ".DS_Store"), []byte("x"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Song")

	rep := trashOne(t, ctx, lib, pid, model.DeleteTrash)
	if rep.Trashed != 1 || rep.DirsPruned != 2 {
		t.Fatalf("report = %+v, want one trashed and two folders pruned", rep)
	}
	if fileExists(filepath.Join(root, "Artist")) {
		t.Fatal("the folders the trash emptied are still there")
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %v (err %v), want one entry", entries, err)
	}
	entryDir := filepath.Dir(entries[0].TrashDisplay)
	if got := filesUnder(t, entryDir); !slices.Equal(got, []string{"01 Song.cue", "01 Song.lrc", "01 Song.mp3"}) {
		t.Fatalf("trash entry holds %v, want the song and its sidecars", got)
	}
	store := filepath.Join(root, model.TrashDirName, "folders")
	if got := filesUnder(t, store); len(got) != 1 || !strings.HasSuffix(got[0], filepath.Join("Artist", "Album", "Cover.jpg")) {
		t.Fatalf("the trash's folder store holds %v, want the album's cover under its path", got)
	}

	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := filesUnder(t, album); !slices.Equal(got, []string{"01 Song.cue", "01 Song.lrc", "01 Song.mp3", "Cover.jpg"}) {
		t.Fatalf("album after restore holds %v, want the song, its sidecars and the cover", got)
	}
	if fileExists(entryDir) || fileExists(store) {
		t.Error("the restore left the entry's folder or the folder store behind")
	}
	if it, err := lib.Get(ctx, pid); err != nil || it.State != model.StatePresent {
		t.Fatalf("item after restore = %+v (err %v), want present", it, err)
	}
}

// TestPurgeRemovesAnEntrysSidecars: purging an entry removes the sidecars the trash took
// with its file.
func TestPurgeRemovesAnEntrysSidecars(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "01 Song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(album, "01 Song.lrc"), []byte("[00:00.00]la"))
	writeFile(t, filepath.Join(album, "02 Other.mp3"), testaudio.BuildMP3WithAudio("Other", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep := trashOne(t, ctx, lib, itemPIDByTitle(t, ctx, lib, "Song"), model.DeleteTrash); rep.Trashed != 1 || rep.DirsPruned != 0 {
		t.Fatalf("report = %+v, want one trashed and the album kept", rep)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %v (err %v), want one entry", entries, err)
	}
	if _, err := lib.PurgeTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if fileExists(filepath.Dir(entries[0].TrashDisplay)) || fileExists(filepath.Join(album, "01 Song.lrc")) {
		t.Fatal("the purged entry's lyrics survived it")
	}
}

// TestTrashInPlaceKeepsTheFolderACompanionHolds: in an in-place library a trashed
// track's own lyrics still go with it, but the folder's cover is the user's, so the
// folder stays.
func TestTrashInPlaceKeepsTheFolderACompanionHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "01 Song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, filepath.Join(album, "01 Song.lrc"), []byte("[00:00.00]la"))
	writeFile(t, filepath.Join(album, "Cover.jpg"), []byte("jpeg"))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rep := trashOne(t, ctx, lib, itemPIDByTitle(t, ctx, lib, "Song"), model.DeleteTrash); rep.Trashed != 1 || rep.DirsPruned != 0 {
		t.Fatalf("report = %+v, want one trashed and nothing pruned", rep)
	}
	if got := filesUnder(t, album); !slices.Equal(got, []string{"Cover.jpg"}) {
		t.Fatalf("album holds %v, want its cover alone", got)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %v (err %v), want one entry", entries, err)
	}
	if got := filesUnder(t, filepath.Dir(entries[0].TrashDisplay)); !slices.Equal(got, []string{"01 Song.lrc", "01 Song.mp3"}) {
		t.Fatalf("trash entry holds %v, want the song and its lyrics", got)
	}
}

// TestPermanentDeleteTakesSidecarsAndCompanions: a permanent delete removes a file's own
// sidecars everywhere, and in a managed library the companions of a folder it empties,
// pruning the folders; an in-place folder keeps its cover and stays.
func TestPermanentDeleteTakesSidecarsAndCompanions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	managed, inPlace := t.TempDir(), t.TempDir()
	mAlbum, iAlbum := filepath.Join(managed, "Artist", "Album"), filepath.Join(inPlace, "Album")
	writeFile(t, filepath.Join(mAlbum, "01 Song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(iAlbum, "02 Other.mp3"), testaudio.BuildMP3WithAudio("Other", "Band", "Record", 2, testaudio.AudioWithSeed(2)))
	for _, dir := range []string{mAlbum, iAlbum} {
		writeFile(t, filepath.Join(dir, "Cover.jpg"), []byte("jpeg"))
	}
	writeFile(t, filepath.Join(mAlbum, "01 Song.lrc"), []byte("[00:00.00]la"))
	writeFile(t, filepath.Join(iAlbum, "02 Other.lrc"), []byte("[00:00.00]la"))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: managed, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: inPlace, Mode: model.ModeInPlace},
		}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPIDByTitle(t, ctx, lib, "Song"), itemPIDByTitle(t, ctx, lib, "Other")}, model.DeletePermanent)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	if rep.Deleted != 2 || rep.DirsPruned != 2 {
		t.Fatalf("report = %+v, want two deleted and the managed album and artist folders pruned", rep)
	}
	if fileExists(filepath.Join(managed, "Artist")) {
		t.Error("the managed folders the delete emptied are still there")
	}
	if got := filesUnder(t, iAlbum); !slices.Equal(got, []string{"Cover.jpg"}) {
		t.Errorf("in-place album holds %v, want its cover alone", got)
	}
	if entries, _ := lib.Trash(ctx, false, 0); len(entries) != 0 {
		t.Errorf("a permanent delete wrote %d trash entries", len(entries))
	}
	if fileExists(filepath.Join(managed, model.TrashDirName, "folders")) {
		t.Error("the companions a permanent delete held were not deleted")
	}
}

// TestOrganizeCarriesCompanionsAndPrunesTheSource: an album folder whose tracks all go to
// one new folder sends its cover, nfo and booklet after them, and the folders it leaves
// empty are removed.
func TestOrganizeCarriesCompanionsAndPrunesTheSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := filepath.Join(root, "Incoming", "Rip")
	writeFile(t, filepath.Join(rip, "a.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(rip, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	for _, name := range []string{"Cover.jpg", "album.nfo", "booklet.pdf", ".DS_Store"} {
		writeFile(t, filepath.Join(rip, name), []byte(name))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil || rep.Moved != 2 {
		t.Fatalf("apply: %+v (err %v), want two moved", rep, err)
	}
	if rep.DirsPruned != 2 || fileExists(filepath.Join(root, "Incoming")) {
		t.Fatalf("pruned %d, Incoming kept %v; want Rip and Incoming removed", rep.DirsPruned, fileExists(filepath.Join(root, "Incoming")))
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "One"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Dir(it.DisplayPath)
	for _, name := range []string{"Cover.jpg", "album.nfo", "booklet.pdf"} {
		if !fileExists(filepath.Join(dst, name)) {
			t.Errorf("%s did not follow the album to %s", name, dst)
		}
	}
}

// TestOrganizeSplitFolderKeepsItsCompanions: a folder whose tracks go to two albums keeps
// its companions and stays, and each album takes a copy of the cover.
func TestOrganizeSplitFolderKeepsItsCompanions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	mixed := filepath.Join(root, "Mixed")
	writeFile(t, filepath.Join(mixed, "a.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "First", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(mixed, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "Artist", "Second", 1, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(mixed, "cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(mixed, "album.nfo"), []byte("nfo"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil || rep.Moved != 2 || rep.DirsPruned != 0 {
		t.Fatalf("apply: %+v (err %v), want two moved and nothing pruned", rep, err)
	}
	if got := filesUnder(t, mixed); !slices.Equal(got, []string{"album.nfo", "cover.jpg"}) {
		t.Fatalf("the split folder holds %v, want its companions", got)
	}
	for _, title := range []string{"One", "Two"} {
		it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, title))
		if err != nil {
			t.Fatal(err)
		}
		if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), "cover.jpg")) {
			t.Errorf("%s's album has no cover", title)
		}
	}
}

// TestOrganizePrunesInEachManagedLibrary: with two managed libraries the plan spans both
// roots, and the folders emptied in each are removed up to that library's own root.
func TestOrganizePrunesInEachManagedLibrary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	music, books := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(music, "Incoming", "a.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	narrated := []testaudio.TXXXFrame{{Desc: "NARRATOR", Value: "Reader"}}
	writeFile(t, filepath.Join(books, "Incoming", "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Artist: "Author", Album: "Book", TXXX: narrated, Audio: testaudio.AudioWithSeed(2)}))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: music, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
			{Path: books, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"},
		}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil || rep.Moved != 2 {
		t.Fatalf("apply: %+v (err %v), want two moved", rep, err)
	}
	if rep.DirsPruned != 2 || fileExists(filepath.Join(music, "Incoming")) || fileExists(filepath.Join(books, "Incoming")) {
		t.Fatalf("pruned %d; want each library's emptied Incoming folder removed", rep.DirsPruned)
	}
}

// TestImportPrunesTheStagingFoldersItEmpties: an import that moves a staging folder's
// audio away takes its cover along and removes the folder, keeps a folder still holding a
// file it does not know, and never removes the staging root.
func TestImportPrunesTheStagingFoldersItEmpties(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	one, two := filepath.Join(staging, "One"), filepath.Join(staging, "Two")
	writeFile(t, filepath.Join(one, "a.mp3"), testaudio.BuildMP3WithAudio("First", "Artist", "One", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(one, "cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(one, ".DS_Store"), []byte("x"))
	writeFile(t, filepath.Join(two, "b.mp3"), testaudio.BuildMP3WithAudio("Second", "Artist", "Two", 1, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(two, "notes.md"), []byte("mine"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 2 || rep.DirsPruned != 1 {
		t.Fatalf("ApplyImport: %+v (err %v), want two imported and one folder pruned", rep, err)
	}
	if fileExists(one) || !fileExists(filepath.Join(two, "notes.md")) || !fileExists(staging) {
		t.Fatalf("One kept %v, Two's notes kept %v, staging kept %v; want One gone and the rest kept",
			fileExists(one), fileExists(filepath.Join(two, "notes.md")), fileExists(staging))
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "First"))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), "cover.jpg")) {
		t.Error("the staging folder's cover did not come in with its audio")
	}
}

// TestImportByCopyBringsTheCompanions: a copy import brings a folder's companions in as a
// move import would, and leaves the staging folder whole.
func TestImportByCopyBringsTheCompanions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	one := filepath.Join(staging, "One")
	writeFile(t, filepath.Join(one, "a.mp3"), testaudio.BuildMP3("First", "Artist", "One", 1))
	writeFile(t, filepath.Join(one, "cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(one, "booklet.pdf"), []byte("pdf"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging, Copy: true})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 1 || rep.DirsPruned != 0 {
		t.Fatalf("ApplyImport: %+v (err %v), want the copy imported and nothing pruned", rep, err)
	}
	if got := filesUnder(t, one); !slices.Equal(got, []string{"a.mp3", "booklet.pdf", "cover.jpg"}) {
		t.Fatalf("staging holds %v, want it whole", got)
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "First"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cover.jpg", "booklet.pdf"} {
		if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), name)) {
			t.Errorf("%s did not come in with the copy", name)
		}
	}
}

// TestImportOfOneFileTakesNothingFromItsFolder: a file handed over alone is not its
// folder's album, so the folder's images and papers stay where they are.
func TestImportOfOneFileTakesNothingFromItsFolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, downloads := t.TempDir(), t.TempDir()
	song := filepath.Join(downloads, "song.mp3")
	writeFile(t, song, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	for _, name := range []string{"cover.jpg", "photo.jpg", "paper.pdf"} {
		writeFile(t, filepath.Join(downloads, name), []byte(name))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: song}, model.KindTrack, waxbin.AcquiredMeta{SourceType: model.SourceManual})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("apply: %+v (err %v)", rep, err)
	}
	if got := filesUnder(t, downloads); !slices.Equal(got, []string{"cover.jpg", "paper.pdf", "photo.jpg"}) {
		t.Fatalf("downloads holds %v, want everything but the song", got)
	}
}

// TestImportFromTheInboxLeavesItsOwnFiles: a configured inbox is no album's folder, so a
// file straight in it takes nothing of the inbox's along, while an album folder in it
// sends its cover with its audio and goes; the inbox itself stays.
func TestImportFromTheInboxLeavesItsOwnFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, inboxDir := t.TempDir(), t.TempDir()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Inbox:  []string{inboxDir},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	album := filepath.Join(inboxDir, "Album")
	writeFile(t, filepath.Join(inboxDir, "loose.mp3"), testaudio.BuildMP3WithAudio("Loose", "Artist", "Single", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(inboxDir, "cover.jpg"), []byte("inbox"))
	writeFile(t, filepath.Join(album, "a.mp3"), testaudio.BuildMP3WithAudio("First", "Artist", "Album", 1, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(album, "cover.jpg"), []byte("album"))
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, plan); err != nil || rep.Imported != 2 || rep.DirsPruned != 1 {
		t.Fatalf("ApplyImport: %+v (err %v), want both imported and the album folder pruned", rep, err)
	}
	if got := filesUnder(t, inboxDir); !slices.Equal(got, []string{"cover.jpg"}) || fileExists(album) {
		t.Fatalf("inbox holds %v (Album kept %v), want the inbox's own cover alone", got, fileExists(album))
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "First"))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), "cover.jpg")) {
		t.Error("the album folder's cover did not come in with its audio")
	}
	loose, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "Loose"))
	if err != nil {
		t.Fatal(err)
	}
	if fileExists(filepath.Join(filepath.Dir(loose.DisplayPath), "cover.jpg")) {
		t.Error("the inbox's own cover went with a file straight in it")
	}
}

// twoTrackAlbum catalogs an album folder of two tracks with a cover and a booklet and
// trashes both in one run, returning the library, the album folder and the two trash
// entries, the first track's first.
func twoTrackAlbum(t *testing.T, ctx context.Context) (*waxbin.Library, string, string, []model.TrashEntry) {
	t.Helper()
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	writeFile(t, filepath.Join(album, "01 One.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(album, "02 Two.mp3"), testaudio.BuildMP3WithAudio("Two", "Artist", "Album", 2, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(album, "Cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(album, "booklet.pdf"), []byte("pdf"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPIDByTitle(t, ctx, lib, "One"), itemPIDByTitle(t, ctx, lib, "Two")}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if rep, err := lib.ApplyDelete(ctx, plan); err != nil || rep.Trashed != 2 || rep.DirsPruned != 2 {
		t.Fatalf("delete = %+v (err %v), want both trashed and the album and artist folders pruned", rep, err)
	}
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("trash = %v (err %v), want two entries", entries, err)
	}
	slices.SortFunc(entries, func(a, b model.TrashEntry) int { return strings.Compare(a.OrigDisplay, b.OrigDisplay) })
	return lib, root, album, entries
}

// TestTrashedCompanionsComeBackWithAnyTrack: the folder's cover and booklet come back
// with whichever track is restored first, and purging the other track keeps them.
func TestTrashedCompanionsComeBackWithAnyTrack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, album, entries := twoTrackAlbum(t, ctx)
	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := filesUnder(t, album); !slices.Equal(got, []string{"01 One.mp3", "Cover.jpg", "booklet.pdf"}) {
		t.Fatalf("album after the first restore holds %v, want its cover and booklet back", got)
	}
	if _, err := lib.PurgeTrash(ctx, entries[1].PID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := filesUnder(t, album); !slices.Equal(got, []string{"01 One.mp3", "Cover.jpg", "booklet.pdf"}) {
		t.Fatalf("album after the purge holds %v, want the first track with its cover and booklet", got)
	}
	if fileExists(filepath.Join(root, model.TrashDirName, "folders")) {
		t.Error("the folder store outlived what it held")
	}
}

// TestPurgeKeepsCompanionsATrashedTrackStillNeeds: purging one track's entry keeps the
// folder's companions for the other, still in the trash, and its restore brings them back.
func TestPurgeKeepsCompanionsATrashedTrackStillNeeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, album, entries := twoTrackAlbum(t, ctx)
	if _, err := lib.PurgeTrash(ctx, entries[1].PID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := filesUnder(t, album); !slices.Equal(got, []string{"01 One.mp3", "Cover.jpg", "booklet.pdf"}) {
		t.Fatalf("album holds %v, want the restored track with its cover and booklet", got)
	}
}

// TestEmptyTrashSweepsTheFolderStore: emptying the trash removes the companions it kept
// for the folders its entries came from.
func TestEmptyTrashSweepsTheFolderStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, root, _, _ := twoTrackAlbum(t, ctx)
	if rep, err := lib.EmptyTrash(ctx, waxbin.EmptyTrashOptions{}); err != nil || rep.Purged != 2 {
		t.Fatalf("empty = %+v (err %v), want both purged", rep, err)
	}
	if got := filesUnder(t, filepath.Join(root, model.TrashDirName)); len(got) != 0 {
		t.Fatalf("the emptied trash still holds %v", got)
	}
}

// TestImportCopiesLyricsAnotherEncodingKeeps: lyrics named for a track whose other
// encoding stays in staging (a duplicate of audio the catalog holds) are copied in, not
// taken from it.
func TestImportCopiesLyricsAnotherEncodingKeeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "Held", "held.mp3"), testaudio.BuildMP3WithAudio("Held", "Artist", "Held", 1, testaudio.AudioWithSeed(7)))
	album := filepath.Join(staging, "Album")
	writeFile(t, filepath.Join(album, "01 Song.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(7)))
	writeFile(t, filepath.Join(album, "01 Song.mpga"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(8)))
	writeFile(t, filepath.Join(album, "01 Song.lrc"), []byte("[00:00.00]la"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 1 || rep.Duplicates != 1 {
		t.Fatalf("ApplyImport: %+v (err %v), want the mpga in and the mp3 held as a duplicate", rep, err)
	}
	dst := rep.Files[0].Path
	lrc := strings.TrimSuffix(dst, filepath.Ext(dst)) + ".lrc"
	if !fileExists(lrc) || !fileExists(filepath.Join(album, "01 Song.lrc")) {
		t.Fatalf("lyrics imported %v, kept in staging %v; want both", fileExists(lrc), fileExists(filepath.Join(album, "01 Song.lrc")))
	}
}

// TestOrganizeTakesADiscAlbumsCover: an album kept in disc folders under one album folder
// moves into one new folder, its cover with it, and the old album folder goes once its
// disc folders have.
func TestOrganizeTakesADiscAlbumsCover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	album := filepath.Join(root, "Incoming", "Album")
	writeFile(t, filepath.Join(album, "CD1", "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Artist", Album: "Album", Track: 1, Disc: 1, Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(album, "CD2", "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Artist", Album: "Album", Track: 1, Disc: 2, Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(album, "cover.jpg"), []byte("jpeg"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil || rep.Moved != 2 {
		t.Fatalf("apply: %+v (err %v), want two moved", rep, err)
	}
	if fileExists(filepath.Join(root, "Incoming")) || rep.DirsPruned != 4 {
		t.Fatalf("pruned %d, Incoming kept %v; want the disc folders, the album folder and Incoming gone", rep.DirsPruned, fileExists(filepath.Join(root, "Incoming")))
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "One"))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), "cover.jpg")) {
		t.Error("the album's cover did not follow its discs")
	}
}

// TestImportTakesADiscAlbumsCover: the same for a staged album kept in disc folders.
func TestImportTakesADiscAlbumsCover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root, staging := t.TempDir(), t.TempDir()
	album := filepath.Join(staging, "Album")
	writeFile(t, filepath.Join(album, "CD1", "a.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "One", Artist: "Artist", Album: "Album", Track: 1, Disc: 1, Audio: testaudio.AudioWithSeed(1)}))
	writeFile(t, filepath.Join(album, "CD2", "b.mp3"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "Two", Artist: "Artist", Album: "Album", Track: 1, Disc: 2, Audio: testaudio.AudioWithSeed(2)}))
	writeFile(t, filepath.Join(album, "cover.jpg"), []byte("jpeg"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: staging})
	if err != nil {
		t.Fatalf("PlanImport: %v", err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil || rep.Imported != 2 || fileExists(album) {
		t.Fatalf("ApplyImport: %+v (err %v), album kept %v; want both in and the album folder gone", rep, err, fileExists(album))
	}
	it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, "One"))
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(filepath.Dir(it.DisplayPath), "cover.jpg")) {
		t.Error("the staged album's cover did not follow its discs")
	}
}

// TestOrganizeLeavesAStrayLyricsFile: a lyrics file whose track is not there travels with
// no audio, so it stays, and its folder keeps its cover.
func TestOrganizeLeavesAStrayLyricsFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := filepath.Join(root, "Incoming", "Rip")
	writeFile(t, filepath.Join(rip, "a.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))
	writeFile(t, filepath.Join(rip, "Gone.lrc"), []byte("[00:00.00]la"))
	writeFile(t, filepath.Join(rip, "cover.jpg"), []byte("jpeg"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Moved != 1 || rep.DirsPruned != 0 {
		t.Fatalf("apply: %+v (err %v), want one moved and nothing pruned", rep, err)
	}
	if got := filesUnder(t, rip); !slices.Equal(got, []string{"Gone.lrc", "cover.jpg"}) {
		t.Fatalf("the source folder holds %v, want the stray lyrics and its cover", got)
	}
}

// TestOrganizeTakesSharedLyricsWithTheLastFile: two files of one name moving to two
// albums each take the lyrics they share, the last by moving them, so the folder empties.
func TestOrganizeTakesSharedLyricsWithTheLastFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	rip := filepath.Join(root, "Incoming", "Rip")
	writeFile(t, filepath.Join(rip, "Song.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "First", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(rip, "Song.mpga"), testaudio.BuildMP3WithAudio("Two", "Artist", "Second", 1, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(rip, "Song.lrc"), []byte("[00:00.00]la"))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if rep, err := lib.ApplyOrganize(ctx, plan); err != nil || rep.Moved != 2 {
		t.Fatalf("apply: %+v (err %v), want both moved", rep, err)
	}
	if fileExists(rip) {
		t.Fatalf("the source folder stayed, holding %v", filesUnder(t, rip))
	}
	for _, title := range []string{"One", "Two"} {
		it, err := lib.Get(ctx, itemPIDByTitle(t, ctx, lib, title))
		if err != nil {
			t.Fatal(err)
		}
		if lrc := strings.TrimSuffix(it.DisplayPath, filepath.Ext(it.DisplayPath)) + ".lrc"; !fileExists(lrc) {
			t.Errorf("%s has no lyrics beside it", title)
		}
	}
}

// TestApplyFillsWhatAnOlderPlanLacks: a plan whose actions carry no root (built by hand or
// before this release) is given each file's library at apply time, so a managed folder is
// still emptied and pruned, and an in-place folder keeps its cover.
func TestApplyFillsWhatAnOlderPlanLacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	managed, inPlace := t.TempDir(), t.TempDir()
	mRip, iAlbum := filepath.Join(managed, "Incoming", "Rip"), filepath.Join(inPlace, "Album")
	writeFile(t, filepath.Join(mRip, "a.mp3"), testaudio.BuildMP3WithAudio("One", "Artist", "First", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(mRip, "cover.jpg"), []byte("jpeg"))
	writeFile(t, filepath.Join(iAlbum, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "Band", "Second", 1, testaudio.AudioWithSeed(2)))
	writeFile(t, filepath.Join(iAlbum, "cover.jpg"), []byte("jpeg"))
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: managed, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: inPlace, Mode: model.ModeInPlace},
		}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	oplan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{ProfileName: "waxbin-native"})
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	oplan.Root = ""
	for i := range oplan.Actions {
		oplan.Actions[i].Root = ""
	}
	if rep, err := lib.ApplyOrganize(ctx, oplan); err != nil || rep.Moved != 1 || rep.DirsPruned != 2 {
		t.Fatalf("organize: %+v (err %v), want one moved and Rip and Incoming pruned", rep, err)
	}
	dplan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPIDByTitle(t, ctx, lib, "Two")}, model.DeletePermanent)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	for i := range dplan.Actions {
		dplan.Actions[i].Root, dplan.Actions[i].InPlace = "", false
	}
	if rep, err := lib.ApplyDelete(ctx, dplan); err != nil || rep.Deleted != 1 || rep.DirsPruned != 0 {
		t.Fatalf("delete: %+v (err %v), want one deleted and the in-place folder kept", rep, err)
	}
	if !fileExists(filepath.Join(iAlbum, "cover.jpg")) {
		t.Error("the in-place cover was taken")
	}
}
