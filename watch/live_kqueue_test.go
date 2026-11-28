//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

func kqueueSource(t *testing.T, dirs ...string) source {
	t.Helper()
	src, err := newSource()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	for _, dir := range dirs {
		if err := src.Add(dir); err != nil {
			t.Fatalf("Add(%s): %v", dir, err)
		}
	}
	return src
}

// expect reads events until every one of want has come, in any order, failing on an
// error, the end of the stream or a wait of ten seconds. Other events may come between.
func expect(t *testing.T, src source, want ...fsnotify.Event) {
	t.Helper()
	var seen []fsnotify.Event
	deadline := time.After(10 * time.Second)
	for len(want) > 0 {
		select {
		case ev, ok := <-src.Events():
			if !ok {
				t.Fatalf("the events ended before %v; saw %v", want, seen)
			}
			if i := slices.Index(want, ev); i >= 0 {
				want = slices.Delete(want, i, i+1)
				continue
			}
			seen = append(seen, ev)
		case err := <-src.Errors():
			t.Fatalf("error before %v: %v", want, err)
		case <-deadline:
			t.Fatalf("no %v in 10s; saw %v", want, seen)
		}
	}
}

func TestKqueueReportsChanges(t *testing.T) {
	t.Parallel()
	t.Run("a file", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		src := kqueueSource(t, root)
		writeFile(t, filepath.Join(root, "a.flac"))
		expect(t, src, event(root, fsnotify.Write))
	})
	t.Run("a subfolder", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		src := kqueueSource(t, root)
		mkdirs(t, filepath.Join(root, "Album"))
		expect(t, src, event(filepath.Join(root, "Album"), fsnotify.Create))
	})
	t.Run("a rename", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		src := kqueueSource(t, root, album)
		if err := os.Rename(album, filepath.Join(root, "Album (2004)")); err != nil {
			t.Fatal(err)
		}
		// The parent's note can be read before the folder's own, so either may come first.
		expect(t, src, event(album, fsnotify.Rename), event(filepath.Join(root, "Album (2004)"), fsnotify.Create))
	})
	t.Run("a removal", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		src := kqueueSource(t, root, album)
		if err := os.Remove(album); err != nil {
			t.Fatal(err)
		}
		expect(t, src, event(album, fsnotify.Remove))
	})
	t.Run("an atomic replace", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "a.flac"))
		// The new file is there before the watch, so the rename over the old one is the
		// only change the kernel reports; the listing after it has the temporary name gone
		// and another file standing under the old one.
		writeFile(t, filepath.Join(root, ".a.flac.tmp"))
		src := kqueueSource(t, root)
		if err := os.Rename(filepath.Join(root, ".a.flac.tmp"), filepath.Join(root, "a.flac")); err != nil {
			t.Fatal(err)
		}
		expect(t, src, event(root, fsnotify.Write))
	})
}

// TestKqueueReportsUnderALinkedRoot: a root that is a link is watched as the folder it
// reaches, its events named under the link.
func TestKqueueReportsUnderALinkedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "lib")
	mkdirs(t, real)
	testpath.Symlink(t, real, link)
	src := kqueueSource(t, link)
	writeFile(t, filepath.Join(real, "a.flac"))
	expect(t, src, event(link, fsnotify.Write))
}

// openDescriptors counts the process's open descriptors through /dev/fd, which lists
// them on macOS (and on FreeBSD with fdescfs mounted).
func openDescriptors(t *testing.T) int {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skipf("no /dev/fd here: %v", err)
	}
	listed := false
	for _, e := range entries {
		listed = listed || e.Name() == strconv.Itoa(int(f.Fd()))
	}
	if !listed {
		t.Skip("/dev/fd does not list this process's descriptors here")
	}
	return len(entries) - 1
}

// TestKqueueHoldsADescriptorAFolder: a watched folder of 200 files costs one descriptor,
// where fsnotify's kqueue backend held 201, and Close gives every one back. Not
// parallel: it counts the whole process's descriptors.
func TestKqueueHoldsADescriptorAFolder(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Album")
	mkdirs(t, album)
	for i := range 200 {
		writeFile(t, filepath.Join(album, fmt.Sprintf("%03d.flac", i)))
	}
	before := openDescriptors(t)
	src, err := newSource()
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	queue := openDescriptors(t) - before // the kernel queue and its wake pipe
	lw := &liveWatch{src: src, log: quietLog}
	added, exhausted := lw.addTree(root, true)
	if added != 2 || exhausted {
		t.Fatalf("armed %d folders (exhausted %v), want the root and the album", added, exhausted)
	}
	if held := openDescriptors(t) - before - queue; held != added {
		t.Errorf("%d descriptors held for %d folders", held, added)
	}
	src.Close()
	if after := openDescriptors(t); after != before {
		t.Errorf("%d descriptors open after Close, %d before the source", after, before)
	}
}

// TestKqueueEndsTheSessionWhenDescriptorsRunOut: with the open-file limit lowered below
// what the tree needs, an Add fails for want of a descriptor and the live session ends
// spent. Not parallel: it lowers the whole process's limit.
func TestKqueueEndsTheSessionWhenDescriptorsRunOut(t *testing.T) {
	root := t.TempDir()
	var folders []string
	for i := range 20 {
		folders = append(folders, filepath.Join(root, fmt.Sprintf("Album %02d", i)))
	}
	mkdirs(t, folders...)
	var orig unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &orig); err != nil {
		t.Fatal(err)
	}
	restore := func() {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &orig); err != nil {
			t.Errorf("putting the open-file limit back: %v", err)
		}
	}
	t.Cleanup(restore)
	// lower leaves the process three descriptors past the lowest one free.
	lower := func() {
		t.Helper()
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		free := f.Fd()
		f.Close()
		lim := orig
		setLimit(&lim.Cur, free+3)
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
			t.Fatal(err)
		}
	}

	src, err := newSource()
	if err != nil {
		t.Fatal(err)
	}
	lower()
	var added error
	for _, dir := range folders {
		if added = src.Add(dir); added != nil {
			break
		}
	}
	restore()
	src.Close()
	if !errors.Is(added, syscall.EMFILE) {
		t.Fatalf("Add past the limit = %v, want EMFILE", added)
	}

	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, quietLog)
	if src, err = newSource(); err != nil {
		t.Fatal(err)
	}
	// A session that does not end spent would serve until the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lower()
	spent := w.serveLive(ctx, src, make(chan rescanReq, 8), nil)
	restore()
	if !spent || !w.Degraded() {
		t.Errorf("serveLive past the limit: spent %v, degraded %v; want a session ended for want of descriptors", spent, w.Degraded())
	}
}

func setLimit[T int64 | uint64](cur *T, n uintptr) { *cur = T(n) }

// TestKqueueReportsAnEjectedVolumeOnce: a watched disk image detached from under its
// watch is reported once, as its mount point removed, however many of its folders were
// watched. The image is this test's own: packages run in parallel, and the fixture
// another package reads must stay mounted.
func TestKqueueReportsAnEjectedVolumeOnce(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("hdiutil is macOS's")
	}
	t.Parallel()
	dir := t.TempDir()
	img, mnt := filepath.Join(dir, "eject.dmg"), filepath.Join(dir, "mnt")
	mkdirs(t, mnt)
	hdiutil(t, "create", "-size", "16m", "-fs", "APFS", "-volname", "WaxBinEject", img)
	hdiutil(t, "attach", "-nobrowse", "-mountpoint", mnt, img)
	attached := true
	t.Cleanup(func() {
		if attached {
			_ = exec.Command("hdiutil", "detach", "-force", mnt).Run()
		}
	})
	album := filepath.Join(mnt, "Album")
	mkdirs(t, album)
	src := kqueueSource(t, mnt, album)
	// Not forced: the watch must leave the volume free to eject. Spotlight or fseventsd
	// can hold it busy for a moment, so a refusal is tried again.
	for try := 1; ; try++ {
		out, err := exec.Command("hdiutil", "detach", mnt).CombinedOutput()
		if err == nil {
			break
		}
		if try == 10 {
			t.Fatalf("hdiutil detach: %v\n%s", err, out)
		}
		time.Sleep(time.Second)
	}
	attached = false
	expect(t, src, event(mnt, fsnotify.Remove))
	settle := time.After(time.Second)
	for {
		select {
		case ev := <-src.Events():
			if ev.Has(fsnotify.Remove) {
				t.Errorf("a second removal after the volume went: %v", ev)
			}
		case err := <-src.Errors():
			t.Errorf("error after the volume went: %v", err)
		case <-settle:
			return
		}
	}
}

func hdiutil(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("hdiutil", args...).CombinedOutput(); err != nil {
		t.Fatalf("hdiutil %v: %v\n%s", args, err, out)
	}
}
