package fsx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// TestRootUnreachableCtxReturnsOnTheDeadline: a stat the kernel holds answers
// ErrUnverified once the timeout passes, and a canceled context its own error, while the
// stat is left to finish on its own.
func TestRootUnreachableCtxReturnsOnTheDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		c := RootChecker{Stat: func(string) error { <-release; return nil }}
		root := "/deadline/" + t.Name()
		start := time.Now()
		if err := c.Check(context.Background(), root); !errors.Is(err, ErrUnverified) {
			t.Fatalf("held stat: err = %v, want ErrUnverified", err)
		}
		if waited := time.Since(start); waited != RootCheckTimeout {
			t.Errorf("waited %v, want the %v timeout", waited, RootCheckTimeout)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := c.Check(ctx, root); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled check: err = %v, want context.Canceled", err)
		}
		close(release)
		synctest.Wait()
	})
}

// TestRootUnreachableCtxCoalescesOnePath: checks of one root while its stat runs share that
// stat, and a check after it answered stats again.
func TestRootUnreachableCtxCoalescesOnePath(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		release := make(chan struct{})
		c := RootChecker{Stat: func(string) error { calls.Add(1); <-release; return ErrNotDir }}
		root := "/coalesce/" + t.Name()
		errs := make(chan error, 2)
		for range 2 {
			go func() { errs <- c.Check(context.Background(), root) }()
		}
		synctest.Wait()
		close(release)
		for range 2 {
			if err := <-errs; !errors.Is(err, ErrNotDir) {
				t.Errorf("err = %v, want the shared stat's ErrNotDir", err)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("stat ran %d times for two checks at once, want 1", n)
		}
		if err := c.Check(context.Background(), root); !errors.Is(err, ErrNotDir) || calls.Load() != 2 {
			t.Fatalf("a later check: err = %v after %d stats, want a stat of its own", err, calls.Load())
		}
	})
}

// TestRootUnreachableCtxAnswersAReachableRoot: the bounded check of a folder that is there
// is nil, and of one that is not the stat's own error.
func TestRootUnreachableCtxAnswersAReachableRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := RootUnreachableCtx(context.Background(), dir, time.Minute); err != nil {
		t.Fatalf("a folder that is there: %v", err)
	}
	if err := RootUnreachableCtx(context.Background(), dir+"/gone", time.Minute); err == nil || errors.Is(err, ErrUnverified) {
		t.Fatalf("a folder that is not there: err = %v, want the stat's own", err)
	}
}

// TestRootGateChecksEachRootOnceAndStatsOnlyUnderOneThatAnswers: a root is checked once
// however many paths below it are asked about, a path under a root that does not answer
// is never statted (its stat would wait on the same mount), and the gate hears of that
// root once.
func TestRootGateChecksEachRootOnceAndStatsOnlyUnderOneThatAnswers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	up, down := t.TempDir(), t.TempDir()
	song, held := filepath.Join(up, "song.mp3"), filepath.Join(down, "held.mp3")
	for _, p := range []string{song, held} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	checks := map[string]int{}
	var downs, statted []string
	g := RootGate{
		Checker: RootChecker{Stat: func(root string) error {
			checks[root]++
			if root == down {
				return errors.New("not mounted")
			}
			return nil
		}},
		Down: func(root string, err error) { downs = append(downs, root) },
	}
	g.stat = func(p string) (fs.FileInfo, error) {
		statted = append(statted, p)
		return os.Stat(p)
	}
	for range 2 {
		if _, err := g.Stat(ctx, up, song); err != nil {
			t.Errorf("stat of a present file = %v", err)
		}
		if _, err := g.Stat(ctx, up, filepath.Join(up, "gone.mp3")); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRootDown) {
			t.Errorf("stat of a gone file = %v, want not-exist under a root that answers", err)
		}
		if _, err := g.Stat(ctx, down, held); !errors.Is(err, ErrRootDown) {
			t.Errorf("stat under a root that does not answer = %v, want ErrRootDown", err)
		}
	}
	if err := g.Root(ctx, down); err == nil {
		t.Error("the root that does not answer reads as up")
	}
	if checks[up] != 1 || checks[down] != 1 {
		t.Errorf("checks = %v, want each root once", checks)
	}
	if slices.Contains(statted, held) {
		t.Errorf("statted %v, want nothing under the root that does not answer", statted)
	}
	if !slices.Equal(downs, []string{down}) {
		t.Errorf("heard of %v, want the down root once", downs)
	}
}

// TestRootGateForgetsACheckCutOffByItsContext: a check ctx ends first says nothing of the
// root, so it is returned as ctx's error, the gate hears nothing, and the next check is
// made afresh.
func TestRootGateForgetsACheckCutOffByItsContext(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var downs int
	g := RootGate{Checker: RootChecker{Stat: func(string) error { <-release; return nil }}, Down: func(string, error) { downs++ }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Stat(ctx, root, filepath.Join(root, "a.mp3")); !errors.Is(err, context.Canceled) || errors.Is(err, ErrRootDown) {
		t.Errorf("stat under a canceled check = %v, want the cancel alone", err)
	}
	if _, ok := g.seen[root]; ok || downs != 0 {
		t.Errorf("a canceled check was remembered (%v) or heard (%d)", g.seen, downs)
	}
}
