//go:build windows

package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// awaitEvent reads the source until op arrives for name, passing over the rest (NTFS
// reports a write on a folder whose entries changed).
func awaitEvent(t *testing.T, src source, name string, op fsnotify.Op) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-src.Events():
			if !ok {
				t.Fatalf("the stream closed before %s %s", op, name)
			}
			if ev.Name == name && ev.Has(op) {
				return
			}
		case err := <-src.Errors():
			t.Fatalf("source error before %s %s: %v", op, name, err)
		case <-deadline:
			t.Fatalf("no %s %s within 5s", op, name)
		}
	}
}

// heldRoots counts the roots the source holds a watch on.
func heldRoots(src source) int {
	s := src.(*recursiveSource)
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// TestRecursiveSourceReportsBelowTheRoot: one watch on the root reports changes at
// any depth, in fsnotify's shapes, and adding a folder below the root arms nothing.
func TestRecursiveSourceReportsBelowTheRoot(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	src := testSource(t)
	for _, dir := range []string{root, deep} {
		if err := src.Add(dir); err != nil {
			t.Fatalf("add %s: %v", dir, err)
		}
	}
	if n := heldRoots(src); n != 1 {
		t.Fatalf("roots held = %d, want the one root", n)
	}

	file := filepath.Join(deep, "x.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, file, fsnotify.Create)
	renamed := filepath.Join(deep, "y.mp3")
	if err := os.Rename(file, renamed); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, file, fsnotify.Rename)
	awaitEvent(t, src, renamed, fsnotify.Create)
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, renamed, fsnotify.Remove)
	if err := os.Remove(deep); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, deep, fsnotify.Remove)
}

// TestRecursiveSourceHoldsOnlyTheRoot: with folders below the root watched, moving one
// of them away succeeds, which a handle per folder refused; the move out of the root
// reads as a removal.
func TestRecursiveSourceHoldsOnlyTheRoot(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "b")
	if err := os.MkdirAll(filepath.Join(sub, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := testSource(t)
	for _, dir := range []string{root, sub, filepath.Join(sub, "c")} {
		if err := src.Add(dir); err != nil {
			t.Fatalf("add %s: %v", dir, err)
		}
	}
	if err := os.Rename(sub, filepath.Join(t.TempDir(), "b")); err != nil {
		t.Fatalf("a folder with a watched folder below it could not move: %v", err)
	}
	awaitEvent(t, src, sub, fsnotify.Remove)
}

// TestRecursiveSourceServesAnAddBehindAnUnreadEvent: a second root is armed while the
// first's change waits for a reader, as at startup, where the watcher arms every root
// before it reads.
func TestRecursiveSourceServesAnAddBehindAnUnreadEvent(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	src := testSource(t)
	if err := src.Add(first); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(first, "x.mp3")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // let the change complete a read nobody reads
	added := make(chan error, 1)
	go func() { added <- src.Add(second) }()
	select {
	case err := <-added:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Add waited behind an event nobody had read")
	}
	awaitEvent(t, src, file, fsnotify.Create)
}

// TestRecursiveSourceReportsAMovedRoot: a root moved under its watch, which the handle
// follows, is reported renamed at the next change below it and released, so a watch
// can be armed at its new path.
func TestRecursiveSourceReportsAMovedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := testSource(t)
	if err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "r2")
	if err := os.Rename(root, moved); err != nil {
		t.Fatalf("the root could not move under its own watch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moved, "x.mp3"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, root, fsnotify.Rename)
	if err := src.Add(moved); err != nil {
		t.Fatalf("add the moved root: %v", err)
	}
	if n := heldRoots(src); n != 1 {
		t.Fatalf("roots held = %d, want the moved root alone", n)
	}
}

// TestRecursiveSourceReportsARootRemoved: a root removed under its watch is reported
// gone, and nothing can be added at its path after.
func TestRecursiveSourceReportsARootRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "r")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := testSource(t)
	if err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	awaitEvent(t, src, root, fsnotify.Remove)
	if err := src.Add(root); err == nil {
		t.Fatal("adding the removed root returned nil")
	}
}

// TestRecursiveSourceCloseEndsTheStreams: Close ends the event stream, refuses later
// adds, and is fine to repeat.
func TestRecursiveSourceCloseEndsTheStreams(t *testing.T) {
	root := t.TempDir()
	src, err := newSource()
	if err != nil {
		t.Skipf("filesystem events unavailable: %v", err)
	}
	if err := src.Add(root); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-src.Events(); ok {
		t.Fatal("the event stream is open after Close")
	}
	if err := src.Add(root); err == nil {
		t.Fatal("Add after Close returned nil")
	}
	if err := src.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
