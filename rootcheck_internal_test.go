package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// heldRoot is a root checker whose stat of root waits until release is closed, saying on
// entered when it starts; every other path is statted as usual.
func heldRoot(t *testing.T, root string, timeout time.Duration) (c fsx.RootChecker, entered chan struct{}) {
	t.Helper()
	entered, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(func() { close(release) })
	return fsx.RootChecker{Timeout: timeout, Stat: func(p string) error {
		if filepath.Clean(p) != filepath.Clean(root) {
			return fsx.RootUnreachable(p)
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}}, entered
}

// TestAddRootAllowAbsentPassesAnUnverifiedRoot: a root whose stat does not answer in time
// is registered under AllowAbsent, as a root not mounted yet is.
func TestAddRootAllowAbsentPassesAnUnverifiedRoot(t *testing.T) {
	t.Parallel()
	lib, _, _, _ := ownershipFixture(t)
	root := t.TempDir()
	lib.roots, _ = heldRoot(t, root, 10*time.Millisecond)
	if added, err := lib.AddRoot(context.Background(), config.Root{Path: root, Mode: model.ModeInPlace}, AllowAbsent()); err != nil || added == nil {
		t.Fatalf("add an unverified root under AllowAbsent = %+v (err %v), want it registered", added, err)
	}
}

// TestAddRootWithoutAllowAbsentRefusesAnUnverifiedRoot: without AllowAbsent a root whose
// stat does not answer in time is refused as an I/O failure that says so.
func TestAddRootWithoutAllowAbsentRefusesAnUnverifiedRoot(t *testing.T) {
	t.Parallel()
	lib, _, _, _ := ownershipFixture(t)
	root := t.TempDir()
	lib.roots, _ = heldRoot(t, root, 10*time.Millisecond)
	_, err := lib.AddRoot(context.Background(), config.Root{Path: root, Mode: model.ModeInPlace})
	if !waxerr.Is(err, waxerr.CodeIO) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("add an unverified root = %v, want CodeIO saying the check timed out", err)
	}
}

// TestRelocateRootStatsOutsideTheLease: a relocation waiting on its new root's stat does
// not hold the filesystem lease meanwhile, so a scan runs.
func TestRelocateRootStatsOutsideTheLease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lib, _, _, _ := ownershipFixture(t)
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var music model.PID
	for _, l := range libs {
		if l.Mode != model.ModePodcast {
			music = l.PID
		}
	}
	newRoot := t.TempDir()
	var entered chan struct{}
	lib.roots, entered = heldRoot(t, newRoot, time.Minute)
	done := make(chan error, 1)
	go func() { done <- lib.RelocateRoot(ctx, music, newRoot) }()
	<-entered
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan while the relocation waits on its stat: %v", err)
	}
	select {
	case err := <-done:
		t.Fatalf("relocation returned %v before its stat answered", err)
	default:
	}
}

// TestMarkMissingTakesAnUnverifiedRootForUnmounted: a file under a root whose stat does not
// answer in time cannot be confirmed gone, so mark-missing refuses as for a dropped mount,
// without statting the file.
func TestMarkMissingTakesAnUnverifiedRootForUnmounted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "song.mp3")
	if err := os.WriteFile(path, testaudio.BuildMP3("Song", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := OpenSeeded(t, ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	f, err := lib.store.FileByPath(ctx, []byte(path))
	if err != nil {
		t.Fatal(err)
	}
	item, _, err := lib.store.FileOwner(ctx, f.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	lib.roots, _ = heldRoot(t, root, 10*time.Millisecond)
	if out, err := lib.MarkMissing(ctx, item, MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) || !strings.Contains(err.Error(), "cannot be confirmed gone") {
		t.Fatalf("mark-missing under an unverified root = %q (err %v), want the unmounted refusal", out, err)
	}
}
