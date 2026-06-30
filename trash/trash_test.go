package trash

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRestoreIsIdempotent covers the disk side of restore, which must tolerate a
// retry after a re-scan failure: once the file is back at its original path, a
// second Restore is a no-op rather than an error, so a failed restore can be
// re-run. It also checks the occupied and gone cases.
func TestRestoreIsIdempotent(t *testing.T) {
	s := New(nil, nil) // Restore is pure disk; it does not touch the store
	dir := t.TempDir()
	orig := filepath.Join(dir, "lib", "Artist", "Album", "01 - Song.mp3")
	trashed := filepath.Join(dir, "lib", ".waxbin-trash", "abc", "01 - Song.mp3")
	if err := os.MkdirAll(filepath.Dir(trashed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trashed, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := model.TrashEntry{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(trashed)}

	// First restore moves the file back.
	if err := s.Restore(entry); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	if !fileExists(orig) || fileExists(trashed) {
		t.Fatal("first restore did not move the file back")
	}

	// Second restore (a retry after, say, a failed re-scan) is a clean no-op.
	if err := s.Restore(entry); err != nil {
		t.Fatalf("retry restore should be a no-op, got: %v", err)
	}
	if !fileExists(orig) {
		t.Fatal("retry restore lost the file")
	}
}

func TestRestoreRefusesOccupiedAndGone(t *testing.T) {
	s := New(nil, nil)
	dir := t.TempDir()

	// Occupied: both the original path and the trash file exist.
	orig := filepath.Join(dir, "orig.mp3")
	trashed := filepath.Join(dir, "t", "orig.mp3")
	mustWrite(t, orig)
	if err := os.MkdirAll(filepath.Dir(trashed), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, trashed)
	if err := s.Restore(model.TrashEntry{OrigPath: []byte(orig), OrigDisplay: orig, TrashPath: []byte(trashed)}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("occupied original: want CodeConflict, got %v", err)
	}

	// Gone: neither the original nor the trash file exists.
	gone := model.TrashEntry{
		OrigPath: []byte(filepath.Join(dir, "nope.mp3")), OrigDisplay: filepath.Join(dir, "nope.mp3"),
		TrashPath: []byte(filepath.Join(dir, "also-nope.mp3")),
	}
	if err := s.Restore(gone); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("gone file: want CodeNotFound, got %v", err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
