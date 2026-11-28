package watch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/fsnotify/fsnotify"
)

// TestFollowArmsARootTheSourceLetGo: a source can let a watch go without a word, as
// inotify does when the filesystem under it is unmounted. The next tick arms the root
// again, where it used to stay unwatched for the watcher's life with nothing degraded.
func TestFollowArmsARootTheSourceLetGo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	src.lose(root)
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	if n := src.told(root, false); n != 2 {
		t.Fatalf("the root was armed %d times, want once more after its watch went", n)
	}
}

// TestFollowWaitsForALostRootToComeBack: a root whose watch went with its filesystem,
// and whose mount point went too, is out of reach, said once, and armed when a tick finds
// it back.
func TestFollowWaitsForALostRootToComeBack(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "mount")
	mkdirs(t, root)
	var logs lockedBuffer
	src := newFakeSource()
	lw := &liveWatch{src: src, log: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.armRoot(ctx, root)
	src.lose(root)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	lw.follow(ctx, set)
	lw.follow(ctx, set)
	if n := src.told(root, false); n != 1 || logs.count("not reachable") != 1 {
		t.Fatalf("armed %d times, said %d times that it is out of reach; want no arm and one word:\n%s", n, logs.count("not reachable"), logs.buf.String())
	}
	mkdirs(t, root)
	lw.follow(ctx, set)
	if n := src.told(root, false); n != 2 {
		t.Fatalf("the root was armed %d times, want once more when it was back", n)
	}
}

// TestFollowArmsAFolderTheSourceLetGoAsItStands: a folder below a root whose watch went,
// as a volume mounted there and unmounted takes it, is armed again as the folder that
// stands there now.
func TestFollowArmsAFolderTheSourceLetGoAsItStands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, album := filepath.Join(root, "Vault"), filepath.Join(root, "Vault", "Album")
	mkdirs(t, album)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	src.lose(vault, album)
	if err := os.Remove(album); err != nil { // the mount point stands empty
		t.Fatal(err)
	}
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	if n := src.told(vault, false); n != 2 || !lw.watched(vault) || lw.watched(album) {
		t.Fatalf("vault armed %d times (watched %v), album watched %v; want vault alone armed again", n, lw.watched(vault), lw.watched(album))
	}
}

// TestFollowLeavesALostFolderThatIsGoneToItsEvent: a folder whose watch went because the
// folder did is left to the Remove on its way, which then rescans the folder itself and
// not the one above it, a whole library for a first-level folder.
func TestFollowLeavesALostFolderThatIsGoneToItsEvent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	gone := filepath.Join(root, "Gone")
	mkdirs(t, gone)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	src.lose(gone)
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: quietLog}
	w.handleEvent(lw, deb, fsnotify.Event{Name: gone, Op: fsnotify.Remove})
	if dirs := pending(deb); !slices.Equal(dirs, []string{gone}) {
		t.Errorf("rescans scheduled for %v, want the folder that went", dirs)
	}
}

// TestFollowArmsALostTreeOnce: folders let go together, one below another, are armed
// again by the walk of the top one, each once.
func TestFollowArmsALostTreeOnce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, album := filepath.Join(root, "Vault"), filepath.Join(root, "Vault", "Album")
	mkdirs(t, album)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	src.lose(vault, album)
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	if src.told(vault, false) != 2 || src.told(album, false) != 2 {
		t.Errorf("armed vault %d and album %d times, want each once more", src.told(vault, false), src.told(album, false))
	}
}

// TestFollowArmsALostTreeOnceBesideNamesThatSortBetween: "Vault 2" and "Vault.old" sort
// between "Vault" and "Vault/Album", which is still armed with Vault and only once.
func TestFollowArmsALostTreeOnceBesideNamesThatSortBetween(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, album := filepath.Join(root, "Vault"), filepath.Join(root, "Vault", "Album")
	other, old := filepath.Join(root, "Vault 2"), filepath.Join(root, "Vault.old")
	mkdirs(t, album, other, old)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	src.lose(vault, album, other, old)
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	for _, dir := range []string{vault, album, other, old} {
		if n := src.told(dir, false); n != 2 {
			t.Errorf("%s armed %d times, want once more", dir, n)
		}
	}
}

// TestFollowArmsAFolderAgainOnTheVolumeMountedThere: a folder armed again as it stands
// after its watch went is watched on the folder under the mount point, so a tick that
// finds another volume there, mounted again later, arms it on that one.
func TestFollowArmsAFolderAgainOnTheVolumeMountedThere(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault := filepath.Join(root, "Vault")
	mkdirs(t, vault)
	vols := map[string]string{vault: "under"}
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, volume: func(dir string) (string, bool) {
		v, ok := vols[dir]
		return v, ok
	}}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.armRoot(ctx, root)
	src.lose(vault)
	lw.follow(ctx, set)
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 2 {
		t.Fatalf("vault armed %d times, want once more after its watch went and no more while nothing changed", n)
	}
	vols[vault] = "mounted"
	lw.follow(ctx, set)
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 3 {
		t.Fatalf("vault armed %d times, want once more on the volume mounted there", n)
	}
}

// TestHandleEventArmsARemovedFolderThatStillStands: the folder source reports the top
// folder of an ejected volume as removed, while its mount point stands. Its watch went and
// not the folder, so the next tick arms it again as it stands and watches its volume for
// one mounted there again. A folder that is gone is not armed.
func TestHandleEventArmsARemovedFolderThatStillStands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, gone := filepath.Join(root, "Vault"), filepath.Join(root, "Gone")
	mkdirs(t, vault, gone)
	vols := map[string]string{vault: "under"}
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, volume: func(dir string) (string, bool) {
		v, ok := vols[dir]
		return v, ok
	}}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.armRoot(ctx, root)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: quietLog}

	w.handleEvent(lw, deb, fsnotify.Event{Name: vault, Op: fsnotify.Remove})
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	w.handleEvent(lw, deb, fsnotify.Event{Name: gone, Op: fsnotify.Remove})
	if dirs := pending(deb); !slices.Equal(dirs, []string{gone, vault}) {
		t.Errorf("rescans scheduled for %v, want both folders", dirs)
	}
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 2 || !lw.watched(vault) {
		t.Fatalf("vault armed %d times (watched %v), want once more as it stands", n, lw.watched(vault))
	}
	if n := src.told(gone, false); n != 1 || lw.watched(gone) {
		t.Errorf("a folder that is gone was armed %d times (watched %v)", n, lw.watched(gone))
	}
	vols[vault] = "mounted"
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 3 {
		t.Fatalf("vault armed %d times, want once more on the volume mounted there", n)
	}
}

// TestHandleEventLeavesARecreatedFolderToItsCreate: a folder deleted and made again
// before the worker read its Remove stands, but its watch did not go with a volume: the
// Create the source sends for the new folder arms it, and no volume is watched for it.
func TestHandleEventLeavesARecreatedFolderToItsCreate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	mkdirs(t, album)
	vols := map[string]string{album: "disk"}
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, volume: func(dir string) (string, bool) {
		v, ok := vols[dir]
		return v, ok
	}}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.armRoot(ctx, root)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: quietLog}
	w.handleEvent(lw, deb, fsnotify.Event{Name: album, Op: fsnotify.Remove})
	w.handleEvent(lw, deb, fsnotify.Event{Name: album, Op: fsnotify.Create})
	lw.follow(ctx, set)
	vols[album] = "elsewhere"
	lw.follow(ctx, set)
	if n := src.told(album, false); n != 2 || !lw.watched(album) {
		t.Errorf("album armed %d times (watched %v), want once more, by its Create", n, lw.watched(album))
	}
}

// TestHandleEventArmsALinkedRootReportedRemoved: a root that is a link, reported removed
// while what it reaches stands (the volume behind it ejected, its mount point kept), is
// armed again with its volume watched, as a folder root is.
func TestHandleEventArmsALinkedRootReportedRemoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "lib")
	mkdirs(t, real)
	testpath.Symlink(t, real, link)
	vols := map[string]string{link: "under"}
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, volume: func(dir string) (string, bool) {
		v, ok := vols[dir]
		return v, ok
	}}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: link}}
	lw.armRoot(ctx, link)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: quietLog}
	w.handleEvent(lw, deb, fsnotify.Event{Name: link, Op: fsnotify.Remove})
	lw.follow(ctx, set)
	vols[link] = "mounted"
	lw.follow(ctx, set)
	if n := src.told(link, false); n != 3 {
		t.Errorf("the linked root was armed %d times, want once more as it stands and again on the volume mounted there", n)
	}
}

// TestSpillLeavesWhatNeedsALookToTheTick: an event the worker had no room for is settled
// without a look at the disk, so a watched folder reported removed that stands, and a
// folder that arrived, are armed by the next tick rather than left unwatched.
func TestSpillLeavesWhatNeedsALookToTheTick(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault, arrived := filepath.Join(root, "Vault"), filepath.Join(root, "New", "Album")
	mkdirs(t, vault, arrived)
	vols := map[string]string{vault: "under"}
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, volume: func(dir string) (string, bool) {
		v, ok := vols[dir]
		return v, ok
	}}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	lw.forget(filepath.Join(root, "New")) // as if it arrived after the arm
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	lw.spill(deb, fsnotify.Event{Name: vault, Op: fsnotify.Remove})
	lw.spill(deb, fsnotify.Event{Name: filepath.Join(root, "New"), Op: fsnotify.Create})
	if dirs := pending(deb); !slices.Equal(dirs, []string{root, vault}) {
		t.Errorf("rescans scheduled for %v, want the removed folder and the new one's parent", dirs)
	}
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.follow(ctx, set)
	if !lw.watched(vault) || !lw.watched(filepath.Join(root, "New")) || !lw.watched(arrived) {
		t.Errorf("after the tick: vault %v, New %v, its album %v; want all armed", lw.watched(vault), lw.watched(filepath.Join(root, "New")), lw.watched(arrived))
	}
	// The removed folder that stands lost its watch, so its volume is watched.
	vols[vault] = "mounted"
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 3 {
		t.Errorf("vault armed %d times, want once more on the volume mounted there", n)
	}
}

// TestRearmReadsTheVolumeBeforeItArms: a volume mounted on a folder while it is armed
// again is caught by the next tick, since the volume noted is the one there before the
// arm.
func TestRearmReadsTheVolumeBeforeItArms(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	vault := filepath.Join(root, "Vault")
	mkdirs(t, vault)
	vols := map[string]string{vault: "under"}
	rearming := false
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog,
		volume: func(dir string) (string, bool) {
			v, ok := vols[dir]
			return v, ok
		},
		walk: func(dir string, fn fs.WalkDirFunc) error {
			if rearming && dir == vault {
				vols[vault] = "mounted" // the mount lands while the walk runs
			}
			return filepath.WalkDir(dir, fn)
		},
	}
	ctx := context.Background()
	set := []Root{{LibraryPID: "L1", Path: root}}
	lw.armRoot(ctx, root)
	src.lose(vault)
	rearming = true
	lw.follow(ctx, set)
	rearming = false
	lw.follow(ctx, set)
	if n := src.told(vault, false); n != 3 {
		t.Errorf("vault armed %d times, want once more after its watch went and again for the volume mounted meanwhile", n)
	}
}

// TestLiveWatchRescansAFolderItsSourceSaysWasWritten: a source that reports writes on a
// folder in place of its files' (the folder source of macOS and the BSDs) has the folder
// rescanned.
func TestLiveWatchRescansAFolderItsSourceSaysWasWritten(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	artist := filepath.Join(root, "Artist")
	mkdirs(t, artist)
	lw := &liveWatch{src: newFakeSource(), log: quietLog, byFolder: true}
	lw.armRoot(context.Background(), root)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	w := &Watcher{log: quietLog}
	w.handleEvent(lw, deb, fsnotify.Event{Name: artist, Op: fsnotify.Write})
	if dirs := pending(deb); !slices.Equal(dirs, []string{artist}) {
		t.Fatalf("a written folder scheduled %v, want the folder", dirs)
	}
}

// TestDebouncerWaitsForAFolderToSettle: with looks, a folder fires once two looks half a
// settle apart agree. A quiet one fires at the settle, and one whose files are still
// growing is held until they stop.
func TestDebouncerWaitsForAFolderToSettle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		looks := map[string][]uint64{"/quiet": {7, 7}, "/copying": {1, 2, 3, 3}}
		fired := map[string]time.Duration{}
		start := time.Now()
		d := newDebouncer(time.Second, func(dir string) {
			mu.Lock()
			defer mu.Unlock()
			fired[dir] = time.Since(start)
		})
		d.look = func(dir string) uint64 {
			mu.Lock()
			defer mu.Unlock()
			seq := looks[dir]
			if len(seq) > 1 {
				looks[dir] = seq[1:]
			}
			return seq[0]
		}
		defer d.stop()
		d.schedule("/quiet")
		d.schedule("/copying")
		time.Sleep(10 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		if want := map[string]time.Duration{"/quiet": time.Second, "/copying": 2 * time.Second}; !maps.Equal(fired, want) {
			t.Errorf("fired at %v, want %v", fired, want)
		}
	})
}

// TestFolderLookSeesAFileGrow: a look changes while a file in the folder grows, and holds
// while nothing does, a folder that is gone included.
func TestFolderLookSeesAFileGrow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a.flac")
	writeFile(t, path)
	first := folderLook(dir)
	if again := folderLook(dir); again != first {
		t.Fatal("two looks at a quiet folder differ")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("more")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if folderLook(dir) == first {
		t.Error("a look did not change when a file grew")
	}
	missing := filepath.Join(dir, "gone")
	if gone, again := folderLook(missing), folderLook(missing); gone != again {
		t.Error("two looks at a folder that is gone differ")
	}
}

// TestFollowArmsAFolderAVolumeIsMountedOn: a volume mounted on a watched folder below a
// root once the watch is up is followed: the tick that finds it in the mount table arms
// the folder again, now over the volume, spelled under the root however the table spells
// it, a root that is a link included.
func TestFollowArmsAFolderAVolumeIsMountedOn(t *testing.T) {
	t.Parallel()
	for _, linked := range []bool{false, true} {
		dir := t.TempDir()
		real := filepath.Join(dir, "real")
		mkdirs(t, filepath.Join(real, "Vault"))
		root := real
		if linked {
			root = filepath.Join(dir, "lib")
			testpath.Symlink(t, real, root)
		}
		reached, err := fsx.ResolveLinks(real)
		if err != nil {
			t.Fatal(err)
		}
		table := []string{"/"}
		src := newFakeSource()
		lw := &liveWatch{src: src, log: quietLog, mounts: func() ([]string, bool) { return table, true }}
		ctx := context.Background()
		set := []Root{{LibraryPID: "L1", Path: root}}
		lw.armRoot(ctx, root)
		lw.follow(ctx, set)
		vault := filepath.Join(root, "Vault")
		table = []string{"/", filepath.Join(reached, "Vault")}
		lw.follow(ctx, set)
		lw.follow(ctx, set)
		if n := src.told(vault, false); n != 2 {
			t.Errorf("linked %v: %s armed %d times, want once more for the volume mounted on it", linked, vault, n)
		}
	}
}

// TestFollowArmsNothingARemovedLibraryLeftToTheTick: a folder an event left for the next
// tick, in a library removed or relocated before it, stays unarmed: its watches went with
// the library, and armed outside every root they would never be given back.
func TestFollowArmsNothingARemovedLibraryLeftToTheTick(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"a folder below it", "the root itself"} {
		root := t.TempDir()
		vault := filepath.Join(root, "Vault")
		mkdirs(t, vault)
		src := newFakeSource()
		lw := &liveWatch{src: src, log: quietLog}
		ctx := context.Background()
		lw.armRoot(ctx, root)
		deb := newDebouncer(time.Hour, func(string) {})
		w := &Watcher{log: quietLog}
		removed := vault
		if which == "the root itself" {
			removed = root
		}
		w.handleEvent(lw, deb, fsnotify.Event{Name: removed, Op: fsnotify.Remove})
		lw.follow(ctx, nil)
		if lw.total() != 0 {
			t.Errorf("%s: %d folders armed after the library left, want none", which, lw.total())
		}
		deb.stop()
	}
}

// TestSpillLeavesAFloodOfFilesToOneLook: an overload that spills thousands of new files
// leaves the tick one look at their folder, not one each, so live coverage is not said to
// be partial when no folder was missed, and a folder that arrived among them is armed.
func TestSpillLeavesAFloodOfFilesToOneLook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	mkdirs(t, album)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog}
	ctx := context.Background()
	lw.armRoot(ctx, root)
	deb := newDebouncer(time.Hour, func(string) {})
	defer deb.stop()
	for i := range liveEventBuffer + 100 {
		if lw.spill(deb, fsnotify.Event{Name: filepath.Join(album, fmt.Sprintf("%05d.flac", i)), Op: fsnotify.Create}) {
			t.Fatalf("spilling file %d said live coverage is partial", i)
		}
	}
	disc := filepath.Join(album, "CD2")
	mkdirs(t, disc)
	lw.spill(deb, fsnotify.Event{Name: disc, Op: fsnotify.Create})
	lw.follow(ctx, []Root{{LibraryPID: "L1", Path: root}})
	if !lw.watched(disc) {
		t.Error("the folder that arrived among the files was not armed")
	}
}
