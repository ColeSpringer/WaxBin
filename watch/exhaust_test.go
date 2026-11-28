package watch

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// spentSource is a fake source whose Add fails from the given call on.
type spentSource struct {
	*fakeSource
	failFrom int
	err      error
}

func (s *spentSource) Add(dir string) error {
	_ = s.fakeSource.Add(dir)
	if len(s.adds) >= s.failFrom {
		return s.err
	}
	return nil
}

// spentTree makes root with the folders a to e below it, which a walk reads in that order.
func spentTree(t *testing.T) (root string, at func(name string) string) {
	t.Helper()
	root = t.TempDir()
	at = func(name string) string { return filepath.Join(root, name) }
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.Mkdir(at(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, at
}

// TestAddTreeEndsLiveWatchingWhenDescriptorsRunOut: a source that holds a descriptor per
// watch (kqueue) fails an add once the process has none left, which the catalog needs
// more than the watcher does. The walk closes the source, which hands every one of them
// back, and no later walk asks it for a watch.
func TestAddTreeEndsLiveWatchingWhenDescriptorsRunOut(t *testing.T) {
	t.Parallel()
	for _, errno := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE} {
		root, at := spentTree(t)
		// fsnotify wraps the errno with the file it could not open.
		src := &spentSource{fakeSource: newFakeSource(), failFrom: 5, err: fmt.Errorf("%q: %w", at("d/x.mp3"), errno)}
		lw := &liveWatch{src: src, log: quietLog}
		added, exhausted := lw.addTree(root, true)
		if added != 0 || !exhausted {
			t.Fatalf("%v: added=%d exhausted=%v, want nothing held and a spent limit", errno, added, exhausted)
		}
		if want := []string{root, at("a"), at("b"), at("c"), at("d")}; !slices.Equal(src.adds, want) {
			t.Errorf("%v: added %q, want the walk to stop at d: %q", errno, src.adds, want)
		}
		if !src.closed() || lw.total() != 0 || lw.watched(root) {
			t.Errorf("%v: closed=%v total=%d, want the source closed and no folder held", errno, src.closed(), lw.total())
		}
		if added, exhausted := lw.addTree(at("e"), false); added != 0 || !exhausted || len(src.adds) != 5 {
			t.Errorf("%v: a later walk added=%d exhausted=%v adds=%d, want it to ask for nothing", errno, added, exhausted, len(src.adds))
		}
	}
}

// TestServeLiveEndsWhenDescriptorsRunOut: arming the roots at the start runs out of
// descriptors. The live layer says so once, ends, and leaves the watcher degraded, with
// no warning for each folder left.
func TestServeLiveEndsWhenDescriptorsRunOut(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	var logs lockedBuffer
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, slog.New(slog.NewTextHandler(&logs, nil)))
	src := &spentSource{fakeSource: newFakeSource(), failFrom: 3, err: syscall.EMFILE}
	spent := make(chan bool, 1)
	go func() { spent <- w.serveLive(context.Background(), src, make(chan rescanReq, 8), nil) }()
	select {
	case got := <-spent:
		if !got || !w.Degraded() || !src.closed() {
			t.Errorf("spent=%v degraded=%v closed=%v, want a session ended for want of descriptors", got, w.Degraded(), src.closed())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the live layer kept serving a closed source")
	}
	if n := logs.count("open-file limit reached"); n != 1 {
		t.Errorf("said the limit was reached %d times, want once:\n%s", n, logs.buf.String())
	}
	if logs.count("could not add directory watch") != 0 || logs.count("no filesystem watches could be armed") != 0 {
		t.Errorf("a spent limit was logged as something else:\n%s", logs.buf.String())
	}
}

// TestAddTreeEndsLiveWatchingWhenItsWalkRunsOut: the walk's own read of a folder fails for
// want of a descriptor, which is the same shortage as a failed add.
func TestAddTreeEndsLiveWatchingWhenItsWalkRunsOut(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	src := newFakeSource()
	lw := &liveWatch{src: src, log: quietLog, walk: func(dir string, fn fs.WalkDirFunc) error {
		return fn(dir, nil, &fs.PathError{Op: "open", Path: dir, Err: syscall.EMFILE})
	}}
	if added, exhausted := lw.addTree(root, true); added != 0 || !exhausted || !src.closed() {
		t.Errorf("added=%d exhausted=%v closed=%v, want the source closed", added, exhausted, src.closed())
	}
}

// TestServeLiveEndsOnASpentSourceError: a source reports on its error channel a shortage
// it met on its own, as the folder source does for a listing that could not open its
// folder. It ends the session like a failed add, said once.
func TestServeLiveEndsOnASpentSourceError(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	var logs lockedBuffer
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, slog.New(slog.NewTextHandler(&logs, nil)))
	src := newFakeSource()
	spent := make(chan bool, 1)
	go func() { spent <- w.serveLive(context.Background(), src, make(chan rescanReq, 8), nil) }()
	waitFor(t, "the root was never armed", func() bool { return src.told(root, false) == 1 })
	select {
	case src.errs <- &fs.PathError{Op: "open", Path: root, Err: syscall.ENFILE}:
	case <-time.After(3 * time.Second):
		t.Fatal("the live layer never read the error")
	}
	select {
	case got := <-spent:
		if !got || !src.closed() || !w.Degraded() {
			t.Errorf("spent=%v closed=%v degraded=%v, want a session ended for want of descriptors", got, src.closed(), w.Degraded())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the live layer kept serving")
	}
	if logs.count("open-file limit reached") != 1 || logs.count("live events ended") != 0 || logs.count("filesystem events error") != 0 {
		t.Errorf("want the limit said once and nothing else:\n%s", logs.buf.String())
	}
}

// TestServeLiveKeepsTheRescansOfASpentSession: a folder arrives, its watch runs the
// process out of descriptors, and the session ends. The folder's rescan is asked for at
// once, not dropped with the session nor left to its settle.
func TestServeLiveKeepsTheRescansOfASpentSession(t *testing.T) {
	t.Parallel()
	root, at := spentTree(t)
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{WriteSettle: time.Hour}, quietLog)
	src := &spentSource{fakeSource: newFakeSource(), failFrom: 7, err: syscall.EMFILE}
	reqs := make(chan rescanReq, 8)
	spent := make(chan bool, 1)
	go func() { spent <- w.serveLive(context.Background(), src, reqs, nil) }()
	waitFor(t, "the tree was never armed", func() bool { return src.told(at("e"), false) == 1 })
	arrived := at("new")
	if err := os.Mkdir(arrived, 0o755); err != nil {
		t.Fatal(err)
	}
	select {
	case src.events <- fsnotify.Event{Name: arrived, Op: fsnotify.Create}:
	case <-time.After(3 * time.Second):
		t.Fatal("the live layer never read the event")
	}
	select {
	case r := <-reqs:
		if r.subPath != arrived || r.libPID != "L1" {
			t.Errorf("rescan asked for %+v, want the folder that arrived", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the folder's rescan went with the session")
	}
	select {
	case got := <-spent:
		if !got {
			t.Error("the session did not end for want of descriptors")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the live layer kept serving")
	}
}

// sourcesInTurn hands a watcher each source in turn, then a spent one for every open
// after, and counts the opens.
type sourcesInTurn struct {
	mu     sync.Mutex
	next   []source
	opened int
}

func (o *sourcesInTurn) open() (source, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opened++
	if len(o.next) == 0 {
		return &spentSource{fakeSource: newFakeSource(), failFrom: 1, err: syscall.EMFILE}, nil
	}
	src := o.next[0]
	o.next = o.next[1:]
	return src, nil
}

func (o *sourcesInTurn) opens() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.opened
}

// TestRunLiveTriesAgainAfterDescriptorsRunOut: a shortage that passes costs the live
// layer one wait. The next source arms the roots, and the watcher is no longer degraded.
func TestRunLiveTriesAgainAfterDescriptorsRunOut(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, quietLog)
	w.liveRetry, w.liveSteady = []time.Duration{time.Millisecond}, time.Hour
	first := &spentSource{fakeSource: newFakeSource(), failFrom: 3, err: syscall.EMFILE}
	second := newFakeSource()
	turn := &sourcesInTurn{next: []source{first, second}}
	w.openSource = turn.open
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.runLive(ctx, make(chan rescanReq, 8), nil); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the live layer never came back", func() bool {
		return first.closed() && second.told(root, false) == 1 && !w.Degraded()
	})
}

// TestRunLiveGivesUpWhenEveryTryRunsOut: a library that needs more descriptors than the
// process may hold fails each try the same way, so the tries end and say so once.
func TestRunLiveGivesUpWhenEveryTryRunsOut(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	var logs lockedBuffer
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, slog.New(slog.NewTextHandler(&logs, nil)))
	w.liveRetry, w.liveSteady = []time.Duration{time.Millisecond, time.Millisecond}, time.Hour
	turn := &sourcesInTurn{}
	w.openSource = turn.open
	done := make(chan struct{})
	go func() { w.runLive(context.Background(), make(chan rescanReq, 8), nil); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the live layer kept trying")
	}
	if turn.opens() != 3 || !w.Degraded() {
		t.Errorf("opened %d sources, degraded=%v, want the first try and two more, left degraded", turn.opens(), w.Degraded())
	}
	if n := logs.count("live events stay off"); n != 1 {
		t.Errorf("said the tries ended %d times, want once:\n%s", n, logs.buf.String())
	}
}

// TestRunLiveStartsItsTriesOverAfterASteadySession: a session that held for the steady
// time was no failed try, so a shortage after it is counted from the start.
func TestRunLiveStartsItsTriesOverAfterASteadySession(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	w := New(&mockEngine{}, []Root{{LibraryPID: "L1", Path: root}}, Options{}, quietLog)
	// Every session is steady, so no run of failed tries ever reaches the one allowed.
	w.liveRetry, w.liveSteady = []time.Duration{time.Millisecond}, 0
	turn := &sourcesInTurn{}
	w.openSource = turn.open
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.runLive(ctx, make(chan rescanReq, 8), nil); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "the tries ended though every session was steady", func() bool { return turn.opens() >= 6 })
}

// TestAddTreeStopsAtTheInotifyLimit: inotify answers a spent watch limit with ENOSPC, which
// costs the process nothing else, so the walk stops and keeps the watches it has.
func TestAddTreeStopsAtTheInotifyLimit(t *testing.T) {
	t.Parallel()
	root, at := spentTree(t)
	src := &spentSource{fakeSource: newFakeSource(), failFrom: 4, err: syscall.ENOSPC}
	lw := &liveWatch{src: src, log: quietLog}
	added, exhausted := lw.addTree(root, true)
	if !exhausted || added != 3 || lw.total() != 3 || src.closed() {
		t.Fatalf("added=%d exhausted=%v total=%d, want three armed and a spent limit", added, exhausted, lw.total())
	}
	if len(src.adds) != 4 || src.adds[3] != at("c") || len(src.drops) != 0 {
		t.Errorf("added %q, dropped %q, want the walk to stop at c and drop nothing", src.adds, src.drops)
	}
}

// TestAddTreeWalksPastAFolderItCannotWatch: a failure that is not a spent limit costs
// the one folder.
func TestAddTreeWalksPastAFolderItCannotWatch(t *testing.T) {
	t.Parallel()
	root, _ := spentTree(t)
	src := &onceSource{fakeSource: newFakeSource(), failAt: 3, err: syscall.EACCES}
	lw := &liveWatch{src: src, log: quietLog}
	added, exhausted := lw.addTree(root, true)
	if exhausted || added != 5 || len(src.adds) != 6 {
		t.Fatalf("added=%d exhausted=%v adds=%d, want five of six armed and no spent limit", added, exhausted, len(src.adds))
	}
}

// onceSource is a fake source whose Add fails on one call.
type onceSource struct {
	*fakeSource
	failAt int
	err    error
}

func (s *onceSource) Add(dir string) error {
	_ = s.fakeSource.Add(dir)
	if len(s.adds) == s.failAt {
		return s.err
	}
	return nil
}

func TestWatchHintNamesThePlatformLimit(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "android"} {
		if got := hintFor(goos); !strings.Contains(got, "max_user_watches") {
			t.Errorf("%s hint = %q, want the inotify limit", goos, got)
		}
	}
	for _, goos := range []string{"darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd"} {
		got := hintFor(goos)
		for _, want := range []string{"ulimit -n", "kern.maxfilesperproc", "kern.maxfiles"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s hint = %q, want it to name %s", goos, got, want)
			}
		}
	}
	for _, goos := range []string{"windows", "illumos", "solaris"} {
		if got := hintFor(goos); got != "" {
			t.Errorf("%s hint = %q, want none", goos, got)
		}
	}
}

// TestDescriptorBudget: where a watched folder holds a descriptor, the walk stops within
// three quarters of the open-file limit, or sooner at a cap asked for.
func TestDescriptorBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		limit       uint64
		asked, want int
	}{
		{limit: 256, want: 192},
		{limit: 256, asked: 100, want: 100},
		{limit: 256, asked: 500, want: 192},
		{limit: 61440, want: 46080},
		{limit: 1, want: 1},
		{limit: math.MaxUint64, want: math.MaxInt},
		{limit: 0, asked: 50, want: 50}, // a limit that could not be read
		{limit: 0, want: 0},
	}
	for _, tc := range tests {
		if got := descriptorBudget(tc.limit, tc.asked); got != tc.want {
			t.Errorf("descriptorBudget(%d, %d) = %d, want %d", tc.limit, tc.asked, got, tc.want)
		}
	}
}
