package fsx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/colespringer/waxbin/internal/pathx"
)

// RootCheckTimeout bounds how long a root check waits: a stat of a network mount whose
// server is gone can wait in the kernel for minutes.
const RootCheckTimeout = 5 * time.Second

// ErrUnverified is a bounded root check's answer when the stat did not answer in time. The
// root may be there, so it is neither reachable nor shown to be gone.
var ErrUnverified = errors.New("root check timed out")

// RootChecker runs RootUnreachable bounded in time. Its zero value stats with
// RootUnreachable and waits RootCheckTimeout; Stat and Timeout replace either, and Eval
// replaces the resolution Resolve runs (ResolveLinks).
type RootChecker struct {
	Stat    func(root string) error
	Eval    func(root string) (string, error)
	Timeout time.Duration
}

// RootUnreachableCtx is RootUnreachable bounded by ctx and timeout (RootChecker.Check).
func RootUnreachableCtx(ctx context.Context, root string, timeout time.Duration) error {
	return RootChecker{Timeout: timeout}.Check(ctx, root)
}

// inflight holds the probes running now, by root, so every check or resolution of a
// root shares one.
var inflight = struct {
	sync.Mutex
	stats map[string]*rootStat
}{stats: map[string]*rootStat{}}

// rootStat is one probe's answers: the stat's (reach) and the resolution's.
type rootStat struct {
	done  chan struct{}
	reach error
	path  string
	err   error
}

// Check answers what RootUnreachable would, ctx's error when ctx ends first, or
// ErrUnverified when the timeout does. The stat runs on a goroutine of its own, shared by
// every check or resolution of the root that comes while it runs, and goes on until the
// kernel answers, so a mount that never answers holds one thread however often it is
// asked.
func (c RootChecker) Check(ctx context.Context, root string) error {
	st, err := c.probe(ctx, root)
	if err != nil {
		return err
	}
	return st.reach
}

// Resolve returns the path root reaches with every link in it followed (ResolveLinks),
// bounded as Check is: ctx's error when ctx ends first, ErrUnverified when the timeout
// does, and otherwise the resolution's own answer.
func (c RootChecker) Resolve(ctx context.Context, root string) (string, error) {
	st, err := c.probe(ctx, root)
	if err != nil {
		return "", err
	}
	return st.path, st.err
}

// ResolveAll resolves paths together, each bounded as Resolve is, and maps each that
// answered to the path it reaches; one that did not answer is left out.
func (c RootChecker) ResolveAll(ctx context.Context, paths []string) map[string]string {
	type answer struct {
		path, resolved string
		err            error
	}
	answers := make(chan answer, len(paths))
	for _, p := range paths {
		go func() {
			resolved, err := c.Resolve(ctx, p)
			answers <- answer{p, resolved, err}
		}()
	}
	out := map[string]string{}
	for range paths {
		if a := <-answers; a.err == nil {
			out[a.path] = a.resolved
		}
	}
	return out
}

// probe stats and resolves root on one goroutine, shared by every check or resolution of
// root that comes while it runs, and answers ctx's error when ctx ends first or
// ErrUnverified when the timeout does; the goroutine goes on until the kernel answers.
func (c RootChecker) probe(ctx context.Context, root string) (*rootStat, error) {
	stat, eval, timeout := c.Stat, c.Eval, c.Timeout
	if stat == nil {
		stat = RootUnreachable
	}
	if eval == nil {
		eval = ResolveLinks
	}
	if timeout <= 0 {
		timeout = RootCheckTimeout
	}
	key := filepath.Clean(root)
	inflight.Lock()
	st := inflight.stats[key]
	if st == nil {
		st = &rootStat{done: make(chan struct{})}
		inflight.stats[key] = st
		go func() {
			st.reach = stat(root)
			st.path, st.err = eval(root)
			inflight.Lock()
			delete(inflight.stats, key)
			inflight.Unlock()
			close(st.done)
		}()
	}
	inflight.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-st.done:
		return st, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, ErrUnverified
	}
}

// ErrRootDown marks RootGate.Stat's answer for a path under a root that does not answer;
// the error wraps the root's check as well.
var ErrRootDown = errors.New("library root does not answer")

// RootGate checks each root one operation meets once, bounded (Checker), and stats a path
// only under a root that answered, since a stat under one that does not would wait on the
// same mount. Down, when set, hears once of each root that does not answer. A gate serves
// one operation on one goroutine; its zero value checks as the zero RootChecker does.
type RootGate struct {
	Checker RootChecker
	Down    func(root string, err error)
	seen    map[string]error
	stat    func(path string) (fs.FileInfo, error) // os.Stat when nil; tests count calls
}

// Root returns nil when root answers and its check's error when it does not, checking
// each root once. A check ctx ends first returns ctx's error and is not remembered.
func (g *RootGate) Root(ctx context.Context, root string) error {
	if err, ok := g.seen[root]; ok {
		return err
	}
	err := g.Checker.Check(ctx, root)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if g.seen == nil {
		g.seen = map[string]error{}
	}
	g.seen[root] = err
	if err != nil && g.Down != nil {
		g.Down(root, err)
	}
	return err
}

// Stat is os.Stat of path once root answers. Under a root that does not, it returns an
// error matching ErrRootDown, without statting path; a check ctx ends first returns ctx's
// error.
func (g *RootGate) Stat(ctx context.Context, root, path string) (fs.FileInfo, error) {
	if err := g.Root(ctx, root); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrRootDown, root, err)
	}
	if g.stat != nil {
		return g.stat(path)
	}
	return os.Stat(pathx.Long(path))
}
