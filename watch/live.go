package watch

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/fsnotify/fsnotify"
)

// hint appends the platform's advice for a failed arm (watchHint) to log attributes,
// when it has any. Scheduled rescans still cover the library regardless.
func hint(args ...any) []any {
	if watchHint != "" {
		return append(args, "hint", watchHint)
	}
	return args
}

// liveEventBuffer bounds the internal queue between the read loop and the event
// worker. It is generous so a burst (an album drop, a big move) is absorbed without
// the reader ever blocking on the worker's os.Stat / tree walk; on overflow the reader
// falls back to scheduling the directory directly rather than stalling.
const liveEventBuffer = 4096

// underRoot reports whether path is within root, using the single shared
// containment helper so the watcher matches scan/organize/config exactly.
func underRoot(root, path string) bool { return pathx.UnderRoot(root, path) }

// runLive services filesystem events, coalescing each to its containing directory
// and issuing a debounced, directory-scoped rescan. Coalescing to the directory
// (rather than resolving a sidecar to its sibling item) handles a fresh album drop
// where cover.jpg/track01.lrc events can arrive before track01.mp3 is cataloged, and
// sidesteps the "a .lrc path is not an audio path" problem: a directory rescan
// picks up whatever landed. If no watch can be armed (ENOSPC, unsupported fs), it
// marks the watcher degraded and returns, leaving scheduled rescans as the mechanism.
// sets carries the root set each tick reads, which the event worker follows.
func (w *Watcher) runLive(ctx context.Context, reqs chan<- rescanReq, sets <-chan []Root) {
	src, err := w.openSource()
	if err != nil {
		w.degraded.Store(true)
		w.log.Warn("watch: live events unavailable, scheduled rescans only", hint("err", err)...)
		return
	}
	w.serveLive(ctx, src, reqs, sets)
}

// errCoverageLost is what a source sends when its watch on a root ended for good, with
// the root and the cause; the scheduled rescans cover the root from then on.
var errCoverageLost = errors.New("watch: live coverage lost")

// serveLive arms the roots on src and services its events until ctx ends.
func (w *Watcher) serveLive(ctx context.Context, src source, reqs chan<- rescanReq, sets <-chan []Root) {
	defer src.Close()

	lw := &liveWatch{src: src, log: w.log, max: watchCap(w.opts.MaxWatchDirs)}
	exhausted := false
	for _, r := range w.rootSet() {
		if _, ex := lw.armRoot(r.Path); ex {
			exhausted = true
		}
	}
	switch {
	case lw.total() == 0 && lw.outOfReach():
		// Every root is out of reach, so no watch failed: each is armed when a tick finds
		// it back.
	case lw.total() == 0:
		w.degraded.Store(true)
		w.log.Warn("watch: no filesystem watches could be armed, scheduled rescans only", hint()...)
		return
	case exhausted:
		// Some subtrees are unwatched (watch cap or kernel inotify limit). Mark degraded
		// so this is not a SILENT partial coverage; scheduled rescans still cover the
		// unwatched remainder, so live is a best-effort accelerator, not the mechanism.
		w.degraded.Store(true)
		w.log.Warn("watch: filesystem watch capacity reached; some directories are live-unwatched, relying on scheduled rescans for them",
			hint("armed", lw.total())...)
	default:
		w.log.Info("watch: live filesystem events armed", "dirs", lw.total())
	}

	deb := newDebouncer(w.opts.WriteSettle, func(dir string) {
		libPID := w.libraryForPath(dir)
		if libPID == "" {
			return // an event outside every watched root
		}
		select {
		case reqs <- rescanReq{libPID: libPID, subPath: dir}:
		case <-ctx.Done():
		}
	})
	defer deb.stop()

	// Decouple the event read from the potentially-blocking os.Stat + tree walk in
	// handleEvent: on a slow/network mount those calls would stall the read and
	// overflow the source's queue (dropping events). A worker drains a buffered
	// channel; if it backs up, the reader schedules the directory directly rather than
	// blocking, so reading from the source always stays fast.
	// The root set each tick reads is followed on this worker too, not on the read loop:
	// arming a large tree is the same walk handleEvent makes, and the read loop exists
	// to stay clear of it. A root still armed is left alone; one whose watch went with
	// its folder (moved away, or replaced) is armed again, and one no longer in the set
	// (removed, or relocated) gives its watches up.
	events := make(chan fsnotify.Event, liveEventBuffer)
	go func() {
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}
				w.handleEvent(lw, deb, ev)
			case set := <-sets:
				if lw.follow(set) {
					w.degraded.Store(true)
				}
			}
		}
	}()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ev, ok := <-src.Events():
			if !ok {
				break loop
			}
			// An event under no root comes from a watch that outlived its root. One in the
			// library trash, which the recursive watch on Windows reports and no folder
			// watch did, would have a rescan start below the trash folder and re-catalog
			// what it holds.
			root, found := w.rootFor(ev.Name)
			if !found {
				continue
			}
			if rel, err := filepath.Rel(root.Path, ev.Name); err == nil && model.InTrash(rel) {
				continue
			}
			select {
			case events <- ev:
			default:
				// Worker backed up on slow I/O: the rules that need no look at the disk
				// still apply, then the directory is scheduled directly, so the reader never
				// blocks. A brand-new subtree arriving under this overload may not get live
				// watches until the next scheduled full rescan.
				if !lw.settle(deb, ev) {
					deb.schedule(filepath.Dir(ev.Name))
				}
			}
		case ferr, ok := <-src.Errors():
			if !ok {
				break loop
			}
			switch {
			case errors.Is(ferr, errCoverageLost):
				w.degraded.Store(true)
			case errors.Is(ferr, fsnotify.ErrEventOverflow):
				// Events were lost that nothing can name, so every root is rescanned now
				// rather than at the scheduled tick.
				for _, r := range w.rootSet() {
					deb.schedule(r.Path)
				}
			}
			w.log.Warn("watch: filesystem events error", "err", ferr)
		}
	}
	// Stop the worker. We do not wait for it: on a hung mount its in-flight stat/walk
	// could block, and shutdown (Ctrl-C) must stay responsive. The goroutine exits once
	// that call returns and it observes the closed channel.
	close(events)
	if ctx.Err() == nil {
		// The source ended on its own, and nothing live is left behind it.
		w.degraded.Store(true)
		w.log.Warn("watch: live events ended, scheduled rescans only")
	}
}

// handleEvent coalesces an event to its containing directory and schedules a
// debounced rescan. A newly created directory is also added to the watch set (and
// itself rescanned) so a moved-in subtree stays covered. It runs on the event worker
// goroutine, off the read loop, so its os.Stat / addTree walk cannot stall event
// reading.
func (w *Watcher) handleEvent(lw *liveWatch, deb *debouncer, ev fsnotify.Event) {
	if lw.settle(deb, ev) {
		return
	}
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if _, ex := lw.addTree(ev.Name); ex {
				w.degraded.Store(true)
			}
			deb.schedule(ev.Name)
			return
		}
	}
	deb.schedule(filepath.Dir(ev.Name))
}

// settle applies the rules that need no look at the disk and reports whether they
// dealt with the event. A removed folder is rescanned itself, which a scan reads as
// gone and reconciles; its parent, a whole library for a first-level folder, has
// nothing new. A renamed folder is only forgotten: its old and new paths are
// reconciled together by the parent's rescan, as one move, where rescanning each alone
// would read its files as missing and then as new. A write Windows reports on a folder
// whose entries changed says nothing the entry's own event does not.
func (lw *liveWatch) settle(deb *debouncer, ev fsnotify.Event) bool {
	switch {
	case ev.Has(fsnotify.Remove):
		if lw.forget(ev.Name) {
			deb.schedule(ev.Name)
			return true
		}
	case ev.Has(fsnotify.Rename):
		lw.forget(ev.Name)
	case ev.Has(fsnotify.Write):
		return lw.watched(ev.Name)
	}
	return false
}

// source delivers filesystem events for the folders it is told about. fsnotify watches
// one folder per Add everywhere but Windows, where each watch holds a handle open and
// NTFS refuses to rename a folder with an open handle below it; there one recursive
// watch per root is the source, and an Add under a watched root has nothing to do.
type source interface {
	Add(dir string) error
	Remove(dir string) error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	Close() error
}

// liveWatch owns the watched folder set and its bounded count. addTree runs from both
// startup and the event worker, so the count is mutex-guarded.
type liveWatch struct {
	src   source
	log   *slog.Logger
	max   int // 0 = unlimited (kernel limit still applies, detected via ENOSPC)
	mu    sync.Mutex
	armed map[string]bool
	// below holds the watched folders directly under each, so forgetting a folder
	// takes the folders below it without a walk over the whole set.
	below map[string]map[string]bool
	// roots is the root paths it was asked to arm, which a set without one releases, and
	// unreachable the ones among them that were not there when it tried.
	roots       map[string]bool
	unreachable map[string]bool
}

// armRoot arms a root's tree unless the root is still armed, and remembers it as a root.
// It reports how many folders it armed and whether it hit a ceiling, as addTree does. A
// root that is not there, or is not a folder, is a library out of reach (a drive not
// mounted, say), said once until it is back rather than taken for a failed watch.
func (lw *liveWatch) armRoot(root string) (int, bool) {
	root = filepath.Clean(root)
	lw.mu.Lock()
	if lw.roots == nil {
		lw.roots, lw.unreachable = map[string]bool{}, map[string]bool{}
	}
	lw.roots[root] = true
	held := lw.armed[root]
	lw.mu.Unlock()
	if held {
		return 0, false
	}
	err := fsx.RootUnreachable(root)
	reachable := err == nil
	lw.mu.Lock()
	was := lw.unreachable[root]
	if reachable {
		delete(lw.unreachable, root)
	} else {
		lw.unreachable[root] = true
	}
	lw.mu.Unlock()
	switch {
	case !reachable && !was:
		lw.log.Warn("watch: library root is not reachable; it is watched once it is back", "root", root, "err", err)
		return 0, false
	case !reachable:
		return 0, false
	case was:
		lw.log.Info("watch: library root is reachable again", "root", root)
	}
	return lw.addTree(root)
}

// outOfReach reports whether every root it was asked to arm was out of reach.
func (lw *liveWatch) outOfReach() bool {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return len(lw.roots) > 0 && len(lw.unreachable) == len(lw.roots)
}

// follow arms the roots in set it does not hold and releases those it armed that set no
// longer names, reporting whether an arm hit a ceiling.
func (lw *liveWatch) follow(set []Root) bool {
	keep := make(map[string]bool, len(set))
	for _, r := range set {
		keep[filepath.Clean(r.Path)] = true
	}
	lw.mu.Lock()
	var gone []string
	for root := range lw.roots {
		if !keep[root] {
			gone = append(gone, root)
		}
	}
	lw.mu.Unlock()
	for _, root := range gone {
		lw.release(root)
	}
	exhausted := false
	for _, r := range set {
		if _, ex := lw.armRoot(r.Path); ex {
			exhausted = true
		}
	}
	return exhausted
}

// release drops a root that left the watched set and every watched folder below it, so
// the watcher holds nothing open in a folder that is no longer a library.
func (lw *liveWatch) release(root string) {
	lw.mu.Lock()
	delete(lw.roots, root)
	delete(lw.unreachable, root)
	gone := lw.drop(root, nil)
	lw.unlink(root)
	lw.mu.Unlock()
	lw.unwatch(gone)
}

// forget drops a watched folder that is gone, and the watched folders below it: a move
// takes them along, as does a Recycle Bin delete, which arrives as a removal, and a
// removal from the bottom up has emptied them already. The count follows the tree, so
// the cap leaves room for the folders that arrive later. It reports whether dir was a
// watched folder.
func (lw *liveWatch) forget(dir string) bool {
	lw.mu.Lock()
	if !lw.armed[dir] {
		lw.mu.Unlock()
		return false
	}
	gone := lw.drop(dir, nil)
	lw.unlink(dir)
	lw.mu.Unlock()
	lw.unwatch(gone)
	return true
}

// drop forgets dir and every watched folder below it, under the lock, adding them to
// gone for the caller to unwatch once it lets the lock go.
func (lw *liveWatch) drop(dir string, gone []string) []string {
	for kid := range lw.below[dir] {
		gone = lw.drop(kid, gone)
	}
	delete(lw.below, dir)
	delete(lw.armed, dir)
	return append(gone, dir)
}

// unlink takes dir out of its parent's list of watched folders, under the lock.
func (lw *liveWatch) unlink(dir string) {
	if parent := filepath.Dir(dir); parent != dir {
		delete(lw.below[parent], dir)
		if len(lw.below[parent]) == 0 {
			delete(lw.below, parent)
		}
	}
}

// unwatch removes the source's watches on dirs, off the lock: a source can wait on its
// reader, the reader on the read loop, and the read loop on the lock.
func (lw *liveWatch) unwatch(dirs []string) {
	for _, dir := range dirs {
		_ = lw.src.Remove(dir) // the kernel may have dropped it already
	}
}

func (lw *liveWatch) total() int {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return len(lw.armed)
}

// watched reports whether dir is a watched folder.
func (lw *liveWatch) watched(dir string) bool {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.armed[dir]
}

// addTree watches dir and every folder below it, skipping the library trash directory
// and unreadable subtrees. It stops early once the configured max is reached or the
// kernel refuses a watch (an inotify exhaustion surfaces as ENOSPC), returning how
// many it armed and whether it hit either ceiling. Stopping on ENOSPC avoids a
// per-directory warning storm when the rest of the walk would fail the same way, and
// it lets the caller fall back to scheduled-only coverage for the remainder.
func (lw *liveWatch) addTree(dir string) (added int, exhausted bool) {
	dir = filepath.Clean(dir)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if model.IsTrashName(d.Name()) {
			return fs.SkipDir
		}
		lw.mu.Lock()
		atCap := lw.max > 0 && len(lw.armed) >= lw.max
		lw.mu.Unlock()
		if atCap {
			exhausted = true
			return fs.SkipAll
		}
		if aerr := lw.src.Add(path); aerr != nil {
			// A resource-limit failure means further adds will also fail: stop and signal
			// degradation rather than warn once per remaining directory.
			if errors.Is(aerr, syscall.ENOSPC) {
				lw.log.Warn("watch: inotify watch limit reached", hint("dir", path)...)
				exhausted = true
				return fs.SkipAll
			}
			lw.log.Warn("watch: could not add directory watch", "dir", path, "err", aerr)
			return nil
		}
		lw.mu.Lock()
		if !lw.armed[path] {
			if lw.armed == nil {
				lw.armed, lw.below = map[string]bool{}, map[string]map[string]bool{}
			}
			lw.armed[path] = true
			if parent := filepath.Dir(path); parent != path {
				if lw.below[parent] == nil {
					lw.below[parent] = map[string]bool{}
				}
				lw.below[parent][path] = true
			}
		}
		lw.mu.Unlock()
		added++
		return nil
	})
	return added, exhausted
}

// debouncer coalesces bursts of schedule(dir) calls into one fn(dir) invocation per
// directory once the directory has been quiet for the settle window, so a
// multi-file drop into one folder yields a single directory rescan.
//
// Each schedule bumps a per-directory generation and arms a fresh timer; a timer's
// callback fires fn only if it is still the latest generation for that directory.
// This closes the reschedule race a plain Timer.Reset has: a timer that already
// fired but whose callback had not yet taken the lock would otherwise be rescheduled
// and fire a duplicate. Filtering by generation makes exactly the newest timer fire.
type debouncer struct {
	settle  time.Duration
	fn      func(string)
	mu      sync.Mutex
	timers  map[string]*time.Timer
	gen     map[string]uint64
	stopped bool
}

func newDebouncer(settle time.Duration, fn func(string)) *debouncer {
	return &debouncer{settle: settle, fn: fn, timers: map[string]*time.Timer{}, gen: map[string]uint64{}}
}

func (d *debouncer) schedule(dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	if t, ok := d.timers[dir]; ok {
		t.Stop() // best-effort; a late fire is filtered by the generation check below
	}
	d.gen[dir]++
	g := d.gen[dir]
	d.timers[dir] = time.AfterFunc(d.settle, func() {
		d.mu.Lock()
		latest := !d.stopped && d.gen[dir] == g
		if latest {
			delete(d.timers, dir)
			delete(d.gen, dir)
		}
		d.mu.Unlock()
		if latest {
			d.fn(dir)
		}
	})
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = map[string]*time.Timer{}
	d.gen = map[string]uint64{}
}
