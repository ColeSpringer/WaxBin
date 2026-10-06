package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func audioName(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".mp3" || ext == ".flac"
}

func pruneOpts(root string, dispose func(string) error) PruneOptions {
	return PruneOptions{Root: root, IsAudio: audioName, Junk: IsJunk, Companion: IsCompanion, Dispose: dispose}
}

func TestPruneRemovesAnEmptyChainBelowTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	mkdirs(t, album)
	rep, err := PruneDirs(album, pruneOpts(root, nil))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{album, filepath.Join(root, "Artist")}; !slices.Equal(rep.Removed, want) {
		t.Fatalf("Removed = %v, want %v", rep.Removed, want)
	}
	if !exists(root) || exists(filepath.Join(root, "Artist")) {
		t.Fatalf("root kept %v, Artist gone %v; want the root kept and Artist gone", exists(root), !exists(filepath.Join(root, "Artist")))
	}
}

func TestPruneNeverRemovesTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rep, err := PruneDirs(root, pruneOpts(root, nil))
	if err != nil || len(rep.Removed) != 0 || !exists(root) {
		t.Fatalf("Removed = %v (err %v), root kept %v; want nothing removed", rep.Removed, err, exists(root))
	}
	// A folder outside the root is not the prune's to touch.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	mkdirs(t, outside)
	if rep, err := PruneDirs(outside, pruneOpts(root, nil)); err != nil || len(rep.Removed) != 0 || !exists(outside) {
		t.Fatalf("outside: Removed = %v (err %v); want nothing removed", rep.Removed, err)
	}
}

func TestPruneDeletesJunkWithItsFolder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	for _, name := range []string{".DS_Store", "Thumbs.db", "desktop.ini", "._01 Song.mp3"} {
		writeFile(t, filepath.Join(album, name), "x")
	}
	writeFile(t, filepath.Join(root, "Artist", "THUMBS.DB"), "x")
	rep, err := PruneDirs(album, pruneOpts(root, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 2 || len(rep.Disposed) != 0 || exists(filepath.Join(root, "Artist")) {
		t.Fatalf("Removed = %v, Disposed = %v; want Album and Artist removed with their junk", rep.Removed, rep.Disposed)
	}
}

func TestPruneLeavesJunkInAFolderThatStays(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, ".DS_Store"), "x")
	writeFile(t, filepath.Join(album, "02 Song.mp3"), "audio")
	rep, err := PruneDirs(album, pruneOpts(root, nil))
	if err != nil || len(rep.Removed) != 0 || !exists(filepath.Join(album, ".DS_Store")) {
		t.Fatalf("Removed = %v (err %v), junk kept %v; want the folder and its junk left", rep.Removed, err, exists(filepath.Join(album, ".DS_Store")))
	}
}

func TestPruneKeepsCompanionsWithoutDispose(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "Cover.jpg"), "jpeg")
	writeFile(t, filepath.Join(album, ".DS_Store"), "x")
	rep, err := PruneDirs(album, pruneOpts(root, nil))
	if err != nil || len(rep.Removed) != 0 {
		t.Fatalf("Removed = %v (err %v); want the folder kept", rep.Removed, err)
	}
	if !exists(filepath.Join(album, "Cover.jpg")) || !exists(filepath.Join(album, ".DS_Store")) {
		t.Fatal("a folder that stays lost a file")
	}
}

func TestPruneHandsCompanionsToDispose(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dest := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	for _, name := range []string{"Cover.jpg", "booklet.pdf", "album.nfo", ".DS_Store"} {
		writeFile(t, filepath.Join(album, name), name)
	}
	var got []string
	dispose := func(p string) error {
		got = append(got, filepath.Base(p))
		return os.Rename(p, filepath.Join(dest, filepath.Base(p)))
	}
	rep, err := PruneDirs(album, pruneOpts(root, dispose))
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"Cover.jpg", "album.nfo", "booklet.pdf"}; !slices.Equal(got, want) {
		t.Fatalf("disposed %v, want %v (junk is deleted, not disposed)", got, want)
	}
	if len(rep.Disposed) != 3 || len(rep.Removed) != 2 || exists(filepath.Join(root, "Artist")) {
		t.Fatalf("Disposed = %v, Removed = %v; want three companions disposed and both folders removed", rep.Disposed, rep.Removed)
	}
	if !exists(filepath.Join(dest, "booklet.pdf")) || exists(filepath.Join(dest, ".DS_Store")) {
		t.Fatal("the companions did not reach Dispose's destination, or the junk did")
	}
}

func TestPruneStopsAtWhatItCannotClear(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, entry string }{
		{"subfolder", filepath.Join("Scans", "page1.jpg")},
		{"audio", "03 Song.flac"},
		{"unknown file", "notes.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			album := filepath.Join(root, "Artist", "Album")
			writeFile(t, filepath.Join(album, tc.entry), "x")
			writeFile(t, filepath.Join(album, "cover.jpg"), "jpeg")
			disposed := false
			rep, err := PruneDirs(album, pruneOpts(root, func(string) error { disposed = true; return nil }))
			if err != nil || len(rep.Removed) != 0 || disposed {
				t.Fatalf("Removed = %v, disposed %v (err %v); want the walk stopped before touching the folder", rep.Removed, disposed, err)
			}
			if !exists(filepath.Join(album, tc.entry)) || !exists(filepath.Join(album, "cover.jpg")) {
				t.Fatal("a folder that stays lost a file")
			}
		})
	}
}

func TestPruneNeverRemovesASymlinkedFolder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	real := filepath.Join(root, "real")
	artist := filepath.Join(root, "Artist")
	mkdirs(t, real, filepath.Join(artist, "Album"))
	link := filepath.Join(artist, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if rep, err := PruneDirs(link, pruneOpts(root, nil)); err != nil || len(rep.Removed) != 0 || !exists(link) || !exists(real) {
		t.Fatalf("from the link: Removed = %v (err %v); want nothing removed", rep.Removed, err)
	}
	// The link is not a file the folder holding it may lose either.
	rep, err := PruneDirs(filepath.Join(artist, "Album"), pruneOpts(root, nil))
	if err != nil || !slices.Equal(rep.Removed, []string{filepath.Join(artist, "Album")}) || !exists(link) {
		t.Fatalf("from Album: Removed = %v (err %v); want Album alone removed", rep.Removed, err)
	}
}

// fakeDirs is the OS with a device per folder and refusals injected by path.
type fakeDirs struct {
	osDirs
	device    map[string]uint64
	removeErr map[string]error
	readErr   map[string]error
}

// Device reports a symlink on device 1, where the folder holding it is, and a folder
// on the device its path is given, else 1.
func (f fakeDirs) Device(path string, info fs.FileInfo) (uint64, bool) {
	if d, ok := f.device[path]; ok && info.Mode()&fs.ModeSymlink == 0 {
		return d, true
	}
	return 1, true
}

func (f fakeDirs) Remove(path string) error {
	if err := f.removeErr[path]; err != nil {
		return err
	}
	return f.osDirs.Remove(path)
}

func (f fakeDirs) RemoveAll(path string) error {
	if err := f.removeErr[path]; err != nil {
		return err
	}
	return f.osDirs.RemoveAll(path)
}

func (f fakeDirs) ReadDir(path string) ([]fs.DirEntry, error) {
	if err := f.readErr[path]; err != nil {
		return nil, err
	}
	return f.osDirs.ReadDir(path)
}

func TestPruneStopsAtAMountPoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mnt := filepath.Join(root, "Mounted")
	sub := filepath.Join(mnt, "Sub")
	mkdirs(t, sub)
	fsys := fakeDirs{device: map[string]uint64{mnt: 2, sub: 2}}
	rep, err := pruneDirs(fsys, sub, pruneOpts(root, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Removed, []string{sub}) || !exists(mnt) {
		t.Fatalf("Removed = %v, mount point kept %v; want Sub removed and the mount point kept", rep.Removed, exists(mnt))
	}
}

// TestPruneFollowsALinkedRoot: a library root that is a symlink to another filesystem is
// the folder it points at, so its first-level folders are no mount points.
func TestPruneFollowsALinkedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, root := filepath.Join(dir, "real"), filepath.Join(dir, "link")
	mkdirs(t, filepath.Join(real, "Artist"))
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlink: %v", err)
	}
	artist := filepath.Join(root, "Artist")
	fsys := fakeDirs{device: map[string]uint64{root: 2, artist: 2}}
	rep, err := pruneDirs(fsys, artist, pruneOpts(root, nil))
	if err != nil || !slices.Equal(rep.Removed, []string{artist}) {
		t.Fatalf("Removed = %v (err %v), want Artist removed below the linked root", rep.Removed, err)
	}
}

func TestPruneNeverEntersTheTrash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entry := filepath.Join(root, ".waxbin-trash", "01ENTRY")
	mkdirs(t, entry)
	rep, err := PruneDirs(entry, pruneOpts(root, nil))
	if err != nil || len(rep.Removed) != 0 || !exists(entry) {
		t.Fatalf("Removed = %v (err %v); want the trash left alone", rep.Removed, err)
	}
}

func TestPruneStopsQuietlyOnRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fsys  func(album string) fakeDirs
		quiet bool
	}{
		{"remove denied", func(a string) fakeDirs { return fakeDirs{removeErr: map[string]error{a: syscall.EACCES}} }, true},
		{"not empty", func(a string) fakeDirs { return fakeDirs{removeErr: map[string]error{a: syscall.ENOTEMPTY}} }, true},
		{"read denied", func(a string) fakeDirs { return fakeDirs{readErr: map[string]error{a: syscall.EACCES}} }, true},
		{"io error", func(a string) fakeDirs { return fakeDirs{removeErr: map[string]error{a: syscall.EIO}} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			album := filepath.Join(root, "Artist", "Album")
			mkdirs(t, album)
			rep, err := pruneDirs(tc.fsys(album), album, pruneOpts(root, nil))
			if len(rep.Removed) != 0 || !exists(album) {
				t.Fatalf("Removed = %v; want the walk stopped at Album", rep.Removed)
			}
			if tc.quiet != (err == nil) {
				t.Fatalf("err = %v, want quiet %v", err, tc.quiet)
			}
		})
	}
}

func TestPruneReturnsADisposeError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "folder.jpg"), "jpeg")
	boom := errors.New("boom")
	rep, err := PruneDirs(album, pruneOpts(root, func(string) error { return boom }))
	if !errors.Is(err, boom) || len(rep.Removed) != 0 || len(rep.Disposed) != 0 || !exists(filepath.Join(album, "folder.jpg")) {
		t.Fatalf("err = %v, Removed = %v, Disposed = %v; want boom and the folder kept", err, rep.Removed, rep.Disposed)
	}
}

func TestJunkAndCompanionNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".DS_Store", "Thumbs.db", "thumbs.db", "Desktop.ini", "._Song.mp3", "Icon\r", "@eaDir", ".AppleDouble", "@__thumb", ".@__thumb"} {
		if !IsJunk(name) {
			t.Errorf("IsJunk(%q) = false", name)
		}
	}
	for _, name := range []string{"Cover.JPG", "folder.png", "booklet.pdf", "album.nfo", "rip.log", "playlist.m3u", "disc.cue", "metadata.opf", "desc.txt", "metadata.json"} {
		if !IsCompanion(name) || IsJunk(name) {
			t.Errorf("%q: companion %v, junk %v; want a companion", name, IsCompanion(name), IsJunk(name))
		}
	}
	for _, name := range []string{"Song.mp3", "Song.mp3.part", "Icon", "concert.mkv"} {
		if IsCompanion(name) || IsJunk(name) {
			t.Errorf("%q: companion %v, junk %v; want neither", name, IsCompanion(name), IsJunk(name))
		}
	}
}

// TestPruneClearsJunkFolders: the folders a NAS or a Mac leaves beside media (Synology's
// @eaDir, netatalk's .AppleDouble) go with the folder like any junk.
func TestPruneClearsJunkFolders(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "@eaDir", "cover.jpg", "SYNOPHOTO_THUMB_M.jpg"), "x")
	writeFile(t, filepath.Join(album, ".AppleDouble", "cover.jpg"), "x")
	if runtime.GOOS != "windows" {
		writeFile(t, filepath.Join(album, "Icon\r"), "x")
	}
	rep, err := PruneDirs(album, pruneOpts(root, nil))
	if err != nil || !slices.Equal(rep.Removed, []string{album}) {
		t.Fatalf("Removed = %v (err %v), want Album removed with its junk folders", rep.Removed, err)
	}
}

// TestPruneDeletesJunkBeforeTakingCompanions: a junk file that cannot go (Thumbs.db held
// open by Explorer) stops the prune before any companion is taken.
func TestPruneDeletesJunkBeforeTakingCompanions(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "Thumbs.db"), "x")
	writeFile(t, filepath.Join(album, "Cover.jpg"), "jpeg")
	fsys := fakeDirs{removeErr: map[string]error{filepath.Join(album, "Thumbs.db"): syscall.EACCES}}
	taken := false
	rep, err := pruneDirs(fsys, album, pruneOpts(root, func(string) error { taken = true; return nil }))
	if err != nil || len(rep.Removed) != 0 || taken {
		t.Fatalf("Removed = %v, taken %v (err %v); want nothing taken and the folder kept", rep.Removed, taken, err)
	}
}

// TestPruneUndoesWhenTheFolderStays: companions taken from a folder that then cannot be
// removed (a file arrived, or the removal is refused) are put back.
func TestPruneUndoesWhenTheFolderStays(t *testing.T) {
	t.Parallel()
	root, dest := t.TempDir(), t.TempDir()
	album := filepath.Join(root, "Album")
	for _, name := range []string{"Cover.jpg", "booklet.pdf"} {
		writeFile(t, filepath.Join(album, name), name)
	}
	o := pruneOpts(root, func(p string) error { return os.Rename(p, filepath.Join(dest, filepath.Base(p))) })
	o.Undo = func(p string) error { return os.Rename(filepath.Join(dest, filepath.Base(p)), p) }
	fsys := fakeDirs{removeErr: map[string]error{album: syscall.ENOTEMPTY}}
	rep, err := pruneDirs(fsys, album, o)
	if err != nil || len(rep.Removed) != 0 || len(rep.Disposed) != 0 {
		t.Fatalf("Removed = %v, Disposed = %v (err %v); want the folder kept and nothing reported taken", rep.Removed, rep.Disposed, err)
	}
	for _, name := range []string{"Cover.jpg", "booklet.pdf"} {
		if !exists(filepath.Join(album, name)) {
			t.Errorf("%s was not put back", name)
		}
	}
}

// TestPruneKeepsWhatDisposeKeeps: a companion Dispose keeps (ErrKeep) keeps its folder,
// quietly, and the companions taken before it go back.
func TestPruneKeepsWhatDisposeKeeps(t *testing.T) {
	t.Parallel()
	root, dest := t.TempDir(), t.TempDir()
	album := filepath.Join(root, "Album")
	writeFile(t, filepath.Join(album, "Cover.jpg"), "jpeg")
	writeFile(t, filepath.Join(album, "Song.lrc"), "lyrics")
	o := pruneOpts(root, func(p string) error {
		if filepath.Ext(p) == ".lrc" {
			return ErrKeep
		}
		return os.Rename(p, filepath.Join(dest, filepath.Base(p)))
	})
	o.Undo = func(p string) error { return os.Rename(filepath.Join(dest, filepath.Base(p)), p) }
	rep, err := PruneDirs(album, o)
	if err != nil || len(rep.Removed) != 0 || !exists(filepath.Join(album, "Cover.jpg")) || !exists(filepath.Join(album, "Song.lrc")) {
		t.Fatalf("Removed = %v (err %v); want the folder kept whole", rep.Removed, err)
	}
}

// TestPruneAllPrunesEachFolderOnce: the shared loop prunes each folder once, sums what
// went, and passes an error on with the folder it stopped at.
func TestPruneAllPrunesEachFolderOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a, b := filepath.Join(root, "A"), filepath.Join(root, "B")
	mkdirs(t, a, b)
	writeFile(t, filepath.Join(b, "cover.jpg"), "jpeg")
	boom := errors.New("boom")
	calls := map[string]int{}
	var warned []string
	n := PruneAll([]string{a, b, a}, func(dir string) PruneOptions {
		calls[dir]++
		return pruneOpts(root, func(string) error { return boom })
	}, func(dir string, err error) {
		if errors.Is(err, boom) {
			warned = append(warned, dir)
		}
	})
	if n != 1 || calls[a] != 1 || !slices.Equal(warned, []string{b}) {
		t.Fatalf("pruned %d, calls %v, warned %v; want A pruned once and B's error passed on", n, calls, warned)
	}
}
