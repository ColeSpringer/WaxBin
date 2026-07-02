// Package watch runs a long-lived watcher that keeps the catalog in sync with the
// filesystem while WaxBin holds the write lock. Scheduled rescans are the
// first-class mechanism: filesystem events are unreliable on the target
// filesystems (WSL2, NFS, SMB, bind mounts) and the incremental fast-path is blind
// to a same-size, mtime-preserving change, so a periodic full-content rescan is the
// backstop. An optional fsnotify layer coalesces live events into targeted
// directory rescans on top, degrading cleanly to scheduled-only when it cannot arm.
package watch

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Options configures a Watcher.
type Options struct {
	// Interval is the scheduled fast-path rescan cadence (default 30s).
	Interval time.Duration
	// FullRescanInterval runs a long-cadence full-content rescan (--force) that
	// re-hashes everything, catching a same-size/mtime-preserved change the fast-path
	// misses. Zero or negative DISABLES it (there is no defaulting here); the CLI
	// supplies the friendly 6h default via its flag, so `--full-interval 0` turns the
	// periodic full rescan off.
	FullRescanInterval time.Duration
	// Live enables the fsnotify optimization on top of scheduled rescans.
	Live bool
	// MaxWatchDirs caps how many directories the live layer arms with fsnotify
	// watches. 0 means unlimited, though the kernel's own limit still applies (an
	// inotify exhaustion is detected and degrades gracefully). A positive value bounds
	// watch consumption on a very large library. The unwatched remainder stays covered
	// by scheduled rescans, and the watcher reports Degraded.
	MaxWatchDirs int
	// WriteSettle is how long a directory must be event-quiet before a live rescan
	// fires, so a multi-file album drop coalesces into one directory rescan (default 2s).
	WriteSettle time.Duration
	// Analyze runs the analyze pass after a rescan that changed something.
	Analyze bool
	// SyncSources runs podcast/source sync each scheduled cycle.
	SyncSources bool
	// Notify, when set, is called after each rescan cycle with a short summary, for a
	// CLI heartbeat. It must not block.
	Notify func(Activity)
}

// Activity summarizes one rescan cycle for a heartbeat consumer.
type Activity struct {
	Trigger string // initial | scheduled | full | live
	Changed bool   // the cycle changed the catalog
}

func (o *Options) withDefaults() {
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	// FullRescanInterval is intentionally NOT defaulted: 0/negative means "disabled",
	// so an explicit `--full-interval 0` can turn off the periodic full rescan. The CLI
	// flag carries the 6h default.
	if o.WriteSettle <= 0 {
		o.WriteSettle = 2 * time.Second
	}
}

// Root is one watched library root.
type Root struct {
	LibraryPID model.PID
	Path       string
}

// Engine is the set of operations the watcher drives. The facade injects it so the
// watch package need not import the facade (which imports scan/jobs and would cycle).
type Engine interface {
	// Rescan runs a scan of one library, optionally scoped to a sub-path, optionally
	// forced (bypassing the fast-path). It reports whether the catalog changed. A
	// CodeConflict error means another filesystem mutator holds the shared lease; the
	// watcher treats that as "busy" and retries on the next cycle.
	Rescan(ctx context.Context, libPID model.PID, subPath string, force bool) (changed bool, err error)
	// Analyze runs the analyze pass (loudness/peaks/fingerprint). Best-effort.
	Analyze(ctx context.Context) error
	// SyncSources runs podcast/source acquisition (feed sync, downloads, retention).
	// Best-effort; a nil implementation is fine when the deployment has no sources.
	SyncSources(ctx context.Context) error
}

// Watcher keeps the catalog in sync with a set of roots until its context is
// canceled.
type Watcher struct {
	engine   Engine
	roots    []Root
	opts     Options
	log      *slog.Logger
	degraded atomic.Bool
}

// New builds a watcher over an engine and roots.
func New(engine Engine, roots []Root, opts Options, log *slog.Logger) *Watcher {
	opts.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Watcher{engine: engine, roots: roots, opts: opts, log: log}
}

// Degraded reports whether the live filesystem-event layer failed to arm (so the
// watcher is running on scheduled rescans only). It is never fatal.
func (w *Watcher) Degraded() bool { return w.degraded.Load() }

// Run drives the watcher until ctx is canceled, returning a CodeCanceled error on a
// clean shutdown. It performs an initial catch-up rescan, then ticks the scheduled
// fast-path and full-content rescans, and (when Live is set) services coalesced
// filesystem events between ticks.
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.roots) == 0 {
		return waxerr.New(waxerr.CodeInvalid, "watch.Run", "no roots to watch")
	}

	// Initial catch-up so a freshly started watcher reflects changes made while it
	// was down, before the first tick.
	w.rescanAll(ctx, false, "initial")

	interval := time.NewTicker(w.opts.Interval)
	defer interval.Stop()

	var fullC <-chan time.Time
	if w.opts.FullRescanInterval > 0 {
		full := time.NewTicker(w.opts.FullRescanInterval)
		defer full.Stop()
		fullC = full.C
	}

	// Live events are debounced in a helper goroutine that sends coalesced directory
	// rescan requests here, so all rescans run on this single loop (never two at once,
	// which would self-conflict on the shared filesystem-mutator lease).
	reqs := make(chan rescanReq, 128)
	if w.opts.Live {
		go w.runLive(ctx, reqs)
	}

	for {
		select {
		case <-ctx.Done():
			return waxerr.New(waxerr.CodeCanceled, "watch.Run", "watch canceled")
		case <-interval.C:
			w.rescanAll(ctx, false, "scheduled")
		case <-fullC:
			w.log.Info("watch: full-content rescan")
			w.rescanAll(ctx, true, "full")
		case r := <-reqs:
			w.rescanOne(ctx, r)
		}
	}
}

// rescanReq is a coalesced live rescan of one directory under a library.
type rescanReq struct {
	libPID  model.PID
	subPath string
}

// rescanAll rescans every root, then runs the layered schedulers (analyze, source
// sync) once if anything changed or a forced pass ran.
func (w *Watcher) rescanAll(ctx context.Context, force bool, trigger string) {
	if ctx.Err() != nil {
		return
	}
	changed := false
	for _, r := range w.roots {
		c, _ := w.rescan(ctx, r.LibraryPID, "", force)
		changed = changed || c
	}
	w.runSchedulers(ctx, changed || force)
	w.notify(trigger, changed)
}

// rescanOne rescans a single directory (a coalesced live event) and runs analyze if
// it changed something.
func (w *Watcher) rescanOne(ctx context.Context, r rescanReq) {
	changed, _ := w.rescan(ctx, r.libPID, r.subPath, false)
	w.runSchedulers(ctx, changed)
	w.notify("live", changed)
}

// notify delivers a heartbeat to the optional consumer, ignoring a nil callback.
func (w *Watcher) notify(trigger string, changed bool) {
	if w.opts.Notify != nil {
		w.opts.Notify(Activity{Trigger: trigger, Changed: changed})
	}
}

// rescan runs one Engine.Rescan, mapping a busy (CodeConflict) result to a skip and
// any other error to a warning, so a transient failure never stops the watcher.
func (w *Watcher) rescan(ctx context.Context, libPID model.PID, subPath string, force bool) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	changed, err := w.engine.Rescan(ctx, libPID, subPath, force)
	switch {
	case err == nil:
		return changed, nil
	case waxerr.Is(err, waxerr.CodeConflict):
		// Another filesystem mutator (organize/import) holds the shared lease; skip and
		// retry next cycle rather than racing it.
		w.log.Debug("watch: rescan skipped, filesystem mutator busy", "library", libPID, "sub", subPath)
		return false, nil
	case waxerr.Is(err, waxerr.CodeCanceled):
		return false, err
	default:
		w.log.Warn("watch: rescan error", "library", libPID, "sub", subPath, "err", err)
		return false, err
	}
}

// runSchedulers runs the layered best-effort schedulers after a rescan. Analyze runs
// only when the rescan changed something (new/changed files to measure); source sync
// runs on its own cadence-free basis each cycle when enabled.
func (w *Watcher) runSchedulers(ctx context.Context, changed bool) {
	if ctx.Err() != nil {
		return
	}
	if w.opts.Analyze && changed {
		if err := w.engine.Analyze(ctx); err != nil && !waxerr.Is(err, waxerr.CodeCanceled) {
			w.log.Warn("watch: analyze error", "err", err)
		}
	}
	if w.opts.SyncSources {
		if err := w.engine.SyncSources(ctx); err != nil && !waxerr.Is(err, waxerr.CodeCanceled) {
			w.log.Warn("watch: source sync error", "err", err)
		}
	}
}

// libraryForPath returns the library PID whose root contains path, or "" when none
// does (an event outside every watched root).
func (w *Watcher) libraryForPath(path string) model.PID {
	best := ""
	var bestPID model.PID
	for _, r := range w.roots {
		if underRoot(r.Path, path) && len(r.Path) > len(best) {
			best, bestPID = r.Path, r.LibraryPID
		}
	}
	return bestPID
}
