package waxbin

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// heldEval is a root checker whose stat of root answers at once and whose resolution of
// root waits until release is closed, counting the resolutions of root it was asked for.
func heldEval(t *testing.T, root string, timeout time.Duration) (c fsx.RootChecker, resolves *atomic.Int32) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	resolves = &atomic.Int32{}
	return fsx.RootChecker{Timeout: timeout,
		Eval: func(p string) (string, error) {
			if filepath.Clean(p) != filepath.Clean(root) {
				return fsx.ResolveLinks(p)
			}
			resolves.Add(1)
			<-release
			return p, nil
		}}, resolves
}

// TestAddRootStatsAndResolvesTogether: the bounded stat of a candidate root and the
// bounded resolution of the root set share one probe of the candidate and run at once
// with the rest, so a candidate that does not answer costs one timeout, not two, and
// holds one goroutine.
func TestAddRootStatsAndResolvesTogether(t *testing.T) {
	t.Parallel()
	lib, _, _, _ := ownershipFixture(t)
	ctx := context.Background()
	root := t.TempDir()
	const timeout = time.Second
	checker, resolves := heldEval(t, root, timeout)
	lib.roots = checker
	start := time.Now()
	_, err := lib.AddRoot(ctx, config.Root{Path: root, Mode: model.ModeInPlace})
	if waited := time.Since(start); !waxerr.Is(err, waxerr.CodeIO) || waited > 2*timeout-200*time.Millisecond {
		t.Fatalf("AddRoot with the resolution held = %v after %v, want CodeIO after one timeout", err, waited)
	}
	if n := resolves.Load(); n != 1 {
		t.Errorf("the candidate was resolved %d times, want once, by the probe its stat shares", n)
	}
}

// TestRelocateRootReportsACancel: a relocation canceled while the root set resolves, the
// new root's own stat having answered, reports the cancel and moves nothing rather than
// reporting success.
func TestRelocateRootReportsACancel(t *testing.T) {
	t.Parallel()
	lib, _, _, _ := ownershipFixture(t)
	ctx := context.Background()
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) == 0 {
		t.Fatalf("libraries = %v (err %v)", libs, err)
	}
	var target *model.Library
	for _, l := range libs {
		if l.Mode != model.ModePodcast {
			target = l
			break
		}
	}
	// The new root answers at once; the resolution of the set waits on a registered root.
	newRoot := t.TempDir()
	checker, resolves := heldEval(t, target.DisplayRoot, time.Minute)
	lib.roots = checker
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- lib.RelocateRoot(cctx, target.PID, newRoot) }()
	for resolves.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("RelocateRoot canceled during its resolution = %v, want CodeCanceled", err)
	}
	after, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range after {
		if l.PID == target.PID && l.DisplayRoot != target.DisplayRoot {
			t.Errorf("library root after a canceled relocation = %s, want %s unchanged", l.DisplayRoot, target.DisplayRoot)
		}
	}
}
