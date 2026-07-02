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

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/fsnotify/fsnotify"
)

// inotifyHint points at the usual cause of a failed watch arm on Linux, the
// per-user inotify watch limit, so a degraded warning is actionable. Scheduled
// rescans still cover the library regardless.
const inotifyHint = "on Linux, raise fs.inotify.max_user_watches (sysctl) if this is watch exhaustion"

// liveEventBuffer bounds the internal queue between the fsnotify reader and the
// event worker. It is generous so a burst (an album drop, a big move) is absorbed
// without the reader ever blocking on the worker's os.Stat / tree walk; on overflow
// the reader falls back to scheduling the directory directly rather than stalling.
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
func (w *Watcher) runLive(ctx context.Context, reqs chan<- rescanReq) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		w.degraded.Store(true)
		w.log.Warn("watch: live events unavailable, scheduled rescans only", "err", err, "hint", inotifyHint)
		return
	}
	defer watcher.Close()

	lw := &liveWatch{watcher: watcher, log: w.log, max: w.opts.MaxWatchDirs}
	exhausted := false
	for _, r := range w.roots {
		if _, ex := lw.addTree(r.Path); ex {
			exhausted = true
		}
	}
	switch {
	case lw.total() == 0:
		w.degraded.Store(true)
		w.log.Warn("watch: no filesystem watches could be armed, scheduled rescans only", "hint", inotifyHint)
		return
	case exhausted:
		// Some subtrees are unwatched (watch cap or kernel inotify limit). Mark degraded
		// so this is not a SILENT partial coverage; scheduled rescans still cover the
		// unwatched remainder, so live is a best-effort accelerator, not the mechanism.
		w.degraded.Store(true)
		w.log.Warn("watch: filesystem watch capacity reached; some directories are live-unwatched, relying on scheduled rescans for them",
			"armed", lw.total(), "hint", inotifyHint)
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

	// Decouple the kernel event read from the potentially-blocking os.Stat + tree walk
	// in handleEvent: on a slow/network mount those calls would stall the read and
	// overflow fsnotify's internal queue (dropping events). A worker drains a buffered
	// channel; if it backs up, the reader schedules the directory directly rather than
	// blocking, so reading from watcher.Events always stays fast.
	events := make(chan fsnotify.Event, liveEventBuffer)
	go func() {
		for ev := range events {
			w.handleEvent(lw, deb, ev)
		}
	}()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case ev, ok := <-watcher.Events:
			if !ok {
				break loop
			}
			select {
			case events <- ev:
			default:
				// Worker backed up on slow I/O: schedule the directory directly (cheap, no
				// stat) so the reader never blocks. A brand-new subtree arriving under this
				// overload may not get live watches until the next scheduled full rescan.
				deb.schedule(filepath.Dir(ev.Name))
			}
		case ferr, ok := <-watcher.Errors:
			if !ok {
				break loop
			}
			// An overflow/queue error means we may have missed events; the scheduled
			// rescan is the backstop, so log and continue rather than tearing down.
			w.log.Warn("watch: fsnotify error", "err", ferr)
		}
	}
	// Stop the worker. We do not wait for it: on a hung mount its in-flight stat/walk
	// could block, and shutdown (Ctrl-C) must stay responsive. The goroutine exits once
	// that call returns and it observes the closed channel.
	close(events)
}

// handleEvent coalesces an event to its containing directory and schedules a
// debounced rescan. A newly created directory is also added to the watch set (and
// itself rescanned) so a moved-in subtree stays covered. It runs on the event worker
// goroutine, off the fsnotify read path, so its os.Stat / addTree walk cannot stall
// event reading.
func (w *Watcher) handleEvent(lw *liveWatch, deb *debouncer, ev fsnotify.Event) {
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			if filepath.Base(ev.Name) == model.TrashDirName {
				return
			}
			if _, ex := lw.addTree(ev.Name); ex {
				w.degraded.Store(true)
			}
			deb.schedule(ev.Name)
			return
		}
	}
	deb.schedule(filepath.Dir(ev.Name))
}

// liveWatch owns the fsnotify watch set and its bounded directory count. addTree runs
// from both startup and the event worker, so the count is mutex-guarded.
type liveWatch struct {
	watcher *fsnotify.Watcher
	log     *slog.Logger
	max     int // 0 = unlimited (kernel limit still applies, detected via ENOSPC)
	mu      sync.Mutex
	count   int
}

func (lw *liveWatch) total() int {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.count
}

// addTree arms a watch on dir and every subdirectory, skipping the library trash
// directory and unreadable subtrees. It stops early once the configured max is
// reached or the kernel refuses a watch (an inotify exhaustion surfaces as ENOSPC),
// returning how many it armed and whether it hit either ceiling. Stopping on ENOSPC
// avoids a per-directory warning storm when the rest of the walk would fail the same
// way, and it lets the caller fall back to scheduled-only coverage for the remainder.
func (lw *liveWatch) addTree(dir string) (added int, exhausted bool) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == model.TrashDirName {
			return fs.SkipDir
		}
		lw.mu.Lock()
		atCap := lw.max > 0 && lw.count >= lw.max
		lw.mu.Unlock()
		if atCap {
			exhausted = true
			return fs.SkipAll
		}
		if aerr := lw.watcher.Add(path); aerr != nil {
			// A resource-limit failure means further adds will also fail: stop and signal
			// degradation rather than warn once per remaining directory.
			if errors.Is(aerr, syscall.ENOSPC) {
				lw.log.Warn("watch: inotify watch limit reached", "dir", path, "hint", inotifyHint)
				exhausted = true
				return fs.SkipAll
			}
			lw.log.Warn("watch: could not add directory watch", "dir", path, "err", aerr)
			return nil
		}
		lw.mu.Lock()
		lw.count++
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
