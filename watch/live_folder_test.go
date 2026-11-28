package watch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fakeQueue stands in for kqueue over real folders: open hands out the lowest number not
// open, as the kernel does, and wait returns the batches a test sends.
type fakeQueue struct {
	mu      sync.Mutex
	fds     map[int]string    // the descriptors open, by number
	devs    map[string]uint64 // the device of a folder and those below it; 1 elsewhere
	opened  int
	closed  []int
	isShut  bool
	muted   bool // wakes are held back, as when a note and a listing done come in one batch
	batches chan []folderNote
	woken   chan struct{}
	fails   chan error // a wait that fails, as kevent can
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{fds: map[int]string{}, devs: map[string]uint64{}, batches: make(chan []folderNote), woken: make(chan struct{}, 1), fails: make(chan error)}
}

func (q *fakeQueue) open(dir string) (int, uint64, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return -1, 0, err
	}
	if !info.IsDir() {
		return -1, 0, &os.PathError{Op: "open", Path: dir, Err: syscall.ENOTDIR}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	fd := 3
	for q.fds[fd] != "" {
		fd++
	}
	q.fds[fd] = dir
	q.opened++
	dev, at := uint64(1), ""
	for p, d := range q.devs {
		if underRoot(p, dir) && len(p) > len(at) {
			dev, at = d, p
		}
	}
	return fd, dev, nil
}

func (q *fakeQueue) watch(int) error { return nil }

func (q *fakeQueue) close(fd int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.fds, fd)
	q.closed = append(q.closed, fd)
}

func (q *fakeQueue) wait() ([]folderNote, error) {
	select {
	case b := <-q.batches:
		return b, nil
	case <-q.woken:
		return nil, nil
	case err := <-q.fails:
		return nil, err
	}
}

func (q *fakeQueue) opens() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.opened
}

func (q *fakeQueue) wake() {
	q.mu.Lock()
	muted := q.muted
	q.mu.Unlock()
	if muted {
		return
	}
	select {
	case q.woken <- struct{}{}:
	default:
	}
}

func (q *fakeQueue) shut() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.isShut = true
}

func (q *fakeQueue) mute(on bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.muted = on
}

// mount puts dir and every folder below it on device dev, for folders opened after.
func (q *fakeQueue) mount(dir string, dev uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.devs[dir] = dev
}

// fdOf is the descriptor open for dir.
func (q *fakeQueue) fdOf(t *testing.T, dir string) int {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	for fd, p := range q.fds {
		if p == dir {
			return fd
		}
	}
	t.Fatalf("no descriptor is open for %s", dir)
	return -1
}

func (q *fakeQueue) state() (open, closed []int, shut bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for fd := range q.fds {
		open = append(open, fd)
	}
	slices.Sort(open)
	return open, slices.Clone(q.closed), q.isShut
}

// folderRig drives a folderSource over a fakeQueue, running the listings it starts by
// hand so a test knows when the source has nothing left to do.
type folderRig struct {
	t       *testing.T
	q       *fakeQueue
	src     *folderSource
	mu      sync.Mutex
	pending []func()
}

func newFolderRig(t *testing.T) *folderRig {
	r := &folderRig{t: t, q: newFakeQueue()}
	r.src = newFolderSource(r.q, func(list func()) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.pending = append(r.pending, list)
	})
	t.Cleanup(func() { r.src.Close() })
	return r
}

func (r *folderRig) add(dirs ...string) {
	r.t.Helper()
	for _, dir := range dirs {
		if err := r.src.Add(dir); err != nil {
			r.t.Fatalf("Add(%s): %v", dir, err)
		}
	}
}

func (r *folderRig) note(dir string, op noteOp) folderNote {
	r.t.Helper()
	return folderNote{fd: r.q.fdOf(r.t, dir), op: op}
}

// settle hands the source notes as one batch and works it through every listing that
// follows, returning the events it sent on the way.
func (r *folderRig) settle(notes ...folderNote) []fsnotify.Event {
	r.t.Helper()
	got := r.deliver(notes)
	for {
		// The second empty batch is taken only once the first is done with.
		got = append(got, r.deliver(nil)...)
		got = append(got, r.deliver(nil)...)
		if r.runListings() == 0 {
			return got
		}
	}
}

// deliver hands the source one batch, reading the events it sends until it takes it.
func (r *folderRig) deliver(notes []folderNote) []fsnotify.Event {
	r.t.Helper()
	var got []fsnotify.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r.q.batches <- notes:
			return got
		case ev, ok := <-r.src.Events():
			if !ok {
				r.t.Fatal("the source closed its events")
			}
			got = append(got, ev)
		case <-deadline:
			r.t.Fatal("the source never took the batch")
		}
	}
}

func (r *folderRig) runListings() int {
	r.mu.Lock()
	lists := r.pending
	r.pending = nil
	r.mu.Unlock()
	for _, list := range lists {
		list()
	}
	return len(lists)
}

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func event(name string, op fsnotify.Op) fsnotify.Event { return fsnotify.Event{Name: name, Op: op} }

func wantEvents(t *testing.T, got []fsnotify.Event, want ...fsnotify.Event) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("events %v, want %v", got, want)
	}
}

// TestFolderSourceReportsWhatAWriteChanged: a note that the folder was written is turned,
// by listing it again, into what changed in it.
func TestFolderSourceReportsWhatAWriteChanged(t *testing.T) {
	t.Parallel()
	t.Run("a subfolder arrived", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		r.add(root)
		mkdirs(t, filepath.Join(root, "Album"))
		wantEvents(t, r.settle(r.note(root, noteWritten)), event(filepath.Join(root, "Album"), fsnotify.Create))
	})
	t.Run("a subfolder with no watch of its own left", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		r.add(root)
		if err := os.Remove(album); err != nil {
			t.Fatal(err)
		}
		wantEvents(t, r.settle(r.note(root, noteWritten)), event(album, fsnotify.Remove))
	})
	t.Run("its file names changed", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		r.add(root)
		writeFile(t, filepath.Join(root, "a.flac"))
		wantEvents(t, r.settle(r.note(root, noteWritten)), event(root, fsnotify.Write))
	})
	t.Run("a file was replaced under its own name", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		writeFile(t, filepath.Join(root, "a.flac"))
		r.add(root)
		// An editor's save: the new file is renamed over the old one.
		writeFile(t, filepath.Join(root, ".a.flac.tmp"))
		if err := os.Rename(filepath.Join(root, ".a.flac.tmp"), filepath.Join(root, "a.flac")); err != nil {
			t.Fatal(err)
		}
		wantEvents(t, r.settle(r.note(root, noteWritten)), event(root, fsnotify.Write))
	})
}

// TestFolderSourceSaysNothingMoreOfABurstOfFolders: folders dragged into a root post
// notes while the root is being listed, so it is listed again. That listing finds nothing
// the first did not, which is no Write on the root, a rescan of the whole library.
func TestFolderSourceSaysNothingMoreOfABurstOfFolders(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	r.add(root)
	names := []string{"Album 1", "Album 2", "Album 3"}
	for _, name := range names {
		mkdirs(t, filepath.Join(root, name))
	}
	got := r.deliver([]folderNote{r.note(root, noteWritten)})                // the first listing starts
	got = append(got, r.deliver([]folderNote{r.note(root, noteWritten)})...) // a note it may not show
	got = append(got, r.settle()...)
	var want []fsnotify.Event
	for _, name := range names {
		want = append(want, event(filepath.Join(root, name), fsnotify.Create))
	}
	wantEvents(t, got, want...)
}

// TestFolderSourceReportsAFolderThatWent: a watched folder deleted or renamed is reported
// at the path it was added under, and its parent is listed again, so a folder standing
// under the old name since is reported as arrived.
func TestFolderSourceReportsAFolderThatWent(t *testing.T) {
	t.Parallel()
	t.Run("renamed within its parent", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		r.add(root, album)
		if err := os.Rename(album, filepath.Join(root, "Album (2004)")); err != nil {
			t.Fatal(err)
		}
		wantEvents(t, r.settle(r.note(album, noteRenamed), r.note(root, noteWritten)),
			event(album, fsnotify.Rename), event(filepath.Join(root, "Album (2004)"), fsnotify.Create))
	})
	t.Run("renamed away and another made in its place", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		r.add(root, album)
		if err := os.Rename(album, filepath.Join(t.TempDir(), "Album")); err != nil {
			t.Fatal(err)
		}
		mkdirs(t, album)
		wantEvents(t, r.settle(r.note(album, noteRenamed), r.note(root, noteWritten)),
			event(album, fsnotify.Rename), event(album, fsnotify.Create))
	})
	t.Run("deleted and made again", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, filepath.Join(album, "CD1"))
		r.add(root, album, filepath.Join(album, "CD1"))
		if err := os.RemoveAll(album); err != nil {
			t.Fatal(err)
		}
		mkdirs(t, album)
		wantEvents(t, r.settle(r.note(album, noteDeleted), r.note(root, noteWritten)),
			event(album, fsnotify.Remove), event(album, fsnotify.Create))
		// The new folder is not the one watched, and neither is the one below the old.
		if lost := r.src.Dropped([]string{root, album, filepath.Join(album, "CD1")}); !slices.Equal(lost, []string{album, filepath.Join(album, "CD1")}) {
			t.Errorf("dropped %q, want the folder that went and the one below it", lost)
		}
		r.add(album)
		if lost := r.src.Dropped([]string{root, album}); len(lost) != 0 {
			t.Errorf("dropped %q after the new folder was added", lost)
		}
	})
}

// TestFolderSourceTellsAParentNothingOfAChildThatLeft: a watched folder that leaves
// reports itself, and the note its parent gets for it makes no Write there, which would
// rescan the whole library for a first-level folder. The two notes can come in either
// order, in one batch or two.
func TestFolderSourceTellsAParentNothingOfAChildThatLeft(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"child first", "parent first", "parent's listing first"} {
		t.Run(order, func(t *testing.T) {
			t.Parallel()
			r, root := newFolderRig(t), t.TempDir()
			album := filepath.Join(root, "Album")
			mkdirs(t, album)
			r.add(root, album)
			if err := os.Remove(album); err != nil {
				t.Fatal(err)
			}
			var got []fsnotify.Event
			switch order {
			case "child first":
				got = r.settle(r.note(album, noteDeleted), r.note(root, noteWritten))
			case "parent first":
				got = r.settle(r.note(root, noteWritten), r.note(album, noteDeleted))
			default:
				got = append(r.settle(r.note(root, noteWritten)), r.settle(r.note(album, noteDeleted))...)
			}
			wantEvents(t, got, event(album, fsnotify.Remove))
		})
	}
}

// TestFolderSourceListsAgainWhatAListingMayNotShow: a listing that read the folder before
// a note came in, and is taken in after it, is no answer to the note. A child that left
// meanwhile is not reported as arrived, and a change made meanwhile is reported.
func TestFolderSourceListsAgainWhatAListingMayNotShow(t *testing.T) {
	t.Parallel()
	t.Run("a child left", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		album := filepath.Join(root, "Album")
		mkdirs(t, album)
		r.add(root, album)
		writeFile(t, filepath.Join(root, "a.flac"))
		got := r.deliver([]folderNote{r.note(root, noteWritten)})
		got = append(got, r.deliver(nil)...)
		r.q.mute(true)
		r.runListings() // it reads Album there
		if err := os.Remove(album); err != nil {
			t.Fatal(err)
		}
		got = append(got, r.deliver([]folderNote{r.note(album, noteDeleted)})...)
		r.q.mute(false)
		got = append(got, r.settle()...)
		wantEvents(t, got, event(album, fsnotify.Remove), event(root, fsnotify.Write))
	})
	t.Run("a change came after it read", func(t *testing.T) {
		t.Parallel()
		r, root := newFolderRig(t), t.TempDir()
		r.add(root)
		writeFile(t, filepath.Join(root, "a.flac"))
		got := r.deliver([]folderNote{r.note(root, noteWritten)})
		got = append(got, r.deliver(nil)...)
		r.q.mute(true)
		r.runListings()
		mkdirs(t, filepath.Join(root, "Album"))
		got = append(got, r.deliver([]folderNote{r.note(root, noteWritten)})...)
		r.q.mute(false)
		got = append(got, r.settle()...)
		wantEvents(t, got, event(root, fsnotify.Write), event(filepath.Join(root, "Album"), fsnotify.Create))
	})
}

// TestFolderSourceListsAWaitingFolderBeforeOneListedAgain: a folder that changes while it
// is listed is listed again behind the folders already waiting on its device, so one that
// never stops changing cannot keep the others from being listed.
func TestFolderSourceListsAWaitingFolderBeforeOneListedAgain(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	busy, quiet := filepath.Join(root, "Busy"), filepath.Join(root, "Quiet")
	mkdirs(t, busy, quiet)
	r.add(busy, quiet)
	writeFile(t, filepath.Join(busy, "1.flac"))
	got := r.deliver([]folderNote{r.note(busy, noteWritten)}) // Busy's listing starts
	writeFile(t, filepath.Join(quiet, "1.flac"))
	got = append(got, r.deliver([]folderNote{r.note(quiet, noteWritten)})...) // Quiet waits behind it
	writeFile(t, filepath.Join(busy, "2.flac"))
	got = append(got, r.deliver([]folderNote{r.note(busy, noteWritten)})...) // Busy wants another
	got = append(got, r.deliver(nil)...)
	for range 2 {
		r.runListings()
		got = append(got, r.deliver(nil)...)
		got = append(got, r.deliver(nil)...)
	}
	wantEvents(t, got, event(busy, fsnotify.Write), event(quiet, fsnotify.Write))
	r.settle()
}

// TestFolderSourceSaysOnceThatAVolumeWentAway: an unmount revokes every watched folder on
// the volume, in whatever order the notes come. The top folder on it is reported once
// and every folder below it let go, for a root and for a volume mounted below one.
func TestFolderSourceSaysOnceThatAVolumeWentAway(t *testing.T) {
	t.Parallel()
	for _, order := range []string{"top down", "bottom up"} {
		t.Run("a volume below a root, "+order, func(t *testing.T) {
			t.Parallel()
			r, root := newFolderRig(t), t.TempDir()
			artist, vault := filepath.Join(root, "Artist"), filepath.Join(root, "Vault")
			album := filepath.Join(vault, "Album")
			mkdirs(t, artist, album)
			r.q.mount(vault, 2)
			r.add(root, artist, vault, album)
			notes := []folderNote{r.note(vault, noteRevoked), r.note(album, noteRevoked)}
			if order == "bottom up" {
				slices.Reverse(notes)
			}
			wantEvents(t, r.settle(notes...), event(vault, fsnotify.Remove))
			if lost := r.src.Dropped([]string{root, artist, vault, album}); !slices.Equal(lost, []string{vault, album}) {
				t.Errorf("dropped %q, want the volume's folders", lost)
			}
		})
		t.Run("a root, "+order, func(t *testing.T) {
			t.Parallel()
			r, root := newFolderRig(t), t.TempDir()
			artist := filepath.Join(root, "Artist")
			mkdirs(t, artist)
			r.add(root, artist)
			notes := []folderNote{r.note(root, noteRevoked), r.note(artist, noteRevoked)}
			if order == "bottom up" {
				slices.Reverse(notes)
			}
			wantEvents(t, r.settle(notes...), event(root, fsnotify.Remove))
			if open, _, _ := r.q.state(); len(open) != 0 {
				t.Errorf("descriptors %v still open after the volume went", open)
			}
		})
	}
}

// TestFolderSourceClosesADroppedFolderWhenItsBatchEnds: a folder let go mid-batch keeps
// its descriptor open until the batch is done, so no folder opened meanwhile, by a
// request answered while an event waits to be read, takes its number and a later note in
// the batch for the old folder. The numbers are given out again lowest first after.
func TestFolderSourceClosesADroppedFolderWhenItsBatchEnds(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	gone, released := filepath.Join(root, "Gone"), filepath.Join(root, "Released")
	mkdirs(t, gone, released)
	r.add(root, gone, released)
	fdGone, fdReleased := r.q.fdOf(t, gone), r.q.fdOf(t, released)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	select {
	case r.q.batches <- []folderNote{{fd: fdGone, op: noteDeleted}, {fd: fdGone, op: noteDeleted}}:
	case <-time.After(5 * time.Second):
		t.Fatal("the source never took the batch")
	}
	// The source is sending Gone's Remove now, which nothing reads yet.
	if err := r.src.Remove(released); err != nil {
		t.Fatal(err)
	}
	added := t.TempDir()
	r.add(added)
	if fd := r.q.fdOf(t, added); fd == fdGone || fd == fdReleased {
		t.Fatalf("a folder added mid-batch was opened as %d, a number still to be closed", fd)
	}
	wantEvents(t, r.settle(), event(gone, fsnotify.Remove))
	if _, closed, _ := r.q.state(); !slices.Contains(closed, fdGone) || !slices.Contains(closed, fdReleased) {
		t.Errorf("closed %v after the batch, want %d and %d", closed, fdGone, fdReleased)
	}
	later := t.TempDir()
	r.add(later)
	if fd := r.q.fdOf(t, later); fd != min(fdGone, fdReleased) {
		t.Errorf("the next folder was opened as %d, want the lowest number given back, %d", fd, min(fdGone, fdReleased))
	}
}

// TestFolderSourceServesAnAddBehindAnUnreadEvent: the watcher can be adding a folder
// while an event waits for it to read, so the source answers it meanwhile.
func TestFolderSourceServesAnAddBehindAnUnreadEvent(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	album := filepath.Join(root, "Album")
	mkdirs(t, album)
	r.add(root, album)
	if err := os.Remove(album); err != nil {
		t.Fatal(err)
	}
	select {
	case r.q.batches <- []folderNote{r.note(album, noteDeleted)}:
	case <-time.After(5 * time.Second):
		t.Fatal("the source never took the batch")
	}
	added := make(chan error, 1)
	go func() { added <- r.src.Add(t.TempDir()) }()
	select {
	case err := <-added:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an Add waited on an event nothing read")
	}
	wantEvents(t, r.settle(), event(album, fsnotify.Remove))
}

// TestFolderSourceClosesWithAListingThatNeverReturns: a listing of a mount that hangs
// does not hold Close, and every descriptor is given back.
func TestFolderSourceClosesWithAListingThatNeverReturns(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	r.add(root, t.TempDir())
	writeFile(t, filepath.Join(root, "a.flac"))
	r.deliver([]folderNote{r.note(root, noteWritten)}) // the rig never runs the listing
	closed := make(chan struct{})
	go func() { r.src.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a listing")
	}
	if open, _, shut := r.q.state(); len(open) != 0 || !shut {
		t.Errorf("after Close: %v open, queue shut %v", open, shut)
	}
	if err := r.src.Add(t.TempDir()); !errors.Is(err, fsnotify.ErrClosed) {
		t.Errorf("Add after Close = %v, want ErrClosed", err)
	}
	if _, ok := <-r.src.Events(); ok {
		t.Error("the events stayed open after Close")
	}
}

// TestFolderSourceRefusesAddsOnceItsQueueFailed: a queue whose wait fails ends the source.
// The Adds the watcher goes on making, a tick re-arming everything the source no longer
// holds, are refused at once without opening anything, rather than the second waiting
// for good on a request nothing will read.
func TestFolderSourceRefusesAddsOnceItsQueueFailed(t *testing.T) {
	t.Parallel()
	q := newFakeQueue()
	src := newFolderSource(q, nil)
	defer src.Close()
	if err := src.Add(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	select {
	case q.fails <- errors.New("kevent: cannot allocate memory"):
	case <-time.After(5 * time.Second):
		t.Fatal("the source never waited")
	}
	select {
	case err := <-src.Errors():
		if err == nil {
			t.Fatal("the failure was not reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the failure was not reported")
	}
	for range src.Events() {
	}
	opened := q.opens()
	refused := make(chan error, 2)
	go func() {
		for range 2 {
			refused <- src.Add(t.TempDir())
		}
	}()
	for range 2 {
		select {
		case err := <-refused:
			if !errors.Is(err, fsnotify.ErrClosed) {
				t.Errorf("Add after the queue failed = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an Add after the queue failed waited for good")
		}
	}
	if n := q.opens(); n != opened {
		t.Errorf("%d folders opened after the queue failed", n-opened)
	}
}

// TestFolderSourceHoldsAFolderOnce: an Add of a folder held opens nothing, and a Remove
// lets it go.
func TestFolderSourceHoldsAFolderOnce(t *testing.T) {
	t.Parallel()
	r, root := newFolderRig(t), t.TempDir()
	r.add(root, root)
	if r.q.opened != 1 {
		t.Fatalf("opened %d descriptors for one folder", r.q.opened)
	}
	if err := r.src.Remove(root); err != nil {
		t.Fatal(err)
	}
	if lost := r.src.Dropped([]string{root}); !slices.Equal(lost, []string{root}) {
		t.Errorf("dropped %q after Remove, want the folder", lost)
	}
	r.settle()
	if open, _, _ := r.q.state(); len(open) != 0 {
		t.Errorf("descriptors %v still open after Remove", open)
	}
}

// TestFolderSourceListsOffItsGoroutine: with the listings on goroutines of their own, as
// they run outside tests, a note still comes back as what changed.
func TestFolderSourceListsOffItsGoroutine(t *testing.T) {
	t.Parallel()
	q, root := newFakeQueue(), t.TempDir()
	src := newFolderSource(q, nil)
	defer src.Close()
	if err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, filepath.Join(root, "Album"))
	select {
	case q.batches <- []folderNote{{fd: q.fdOf(t, root), op: noteWritten}}:
	case <-time.After(5 * time.Second):
		t.Fatal("the source never took the batch")
	}
	select {
	case ev := <-src.Events():
		if want := event(filepath.Join(root, "Album"), fsnotify.Create); ev != want {
			t.Errorf("event %v, want %v", ev, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event for the folder that arrived")
	}
}
