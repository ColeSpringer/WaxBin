package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// TestMoveCatalogRollsBackOnPartialFailure pins that the catalog and its sidecars
// move as a set. A -wal left behind beside a renamed main file would drop its
// uncheckpointed commits from the copy the reset exists to preserve.
func TestMoveCatalogRollsBackOnPartialFailure(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("x"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory cannot be replaced by renaming a file over it, so parking one at
	// the -wal destination fails that rename after the main file has already moved.
	dest := filepath.Join(dir, "dest.bak")
	if err := os.Mkdir(dest+"-wal", 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := moveCatalogTo(db, dest); err == nil {
		t.Fatal("moving onto an occupied -wal destination should fail")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(db + suffix); err != nil {
			t.Errorf("%s was not rolled back after the failed move: %v", db+suffix, err)
		}
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("the main file was left at the destination after a failed move")
	}
}

func TestMoveCatalogAsideMovesTheWholeSet(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(db+suffix, []byte("x"+suffix), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dest, err := moveCatalogAside(db)
	if err != nil {
		t.Fatalf("moveCatalogAside: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(dest + suffix); err != nil {
			t.Errorf("%s missing from the saved set: %v", dest+suffix, err)
		}
		if _, err := os.Stat(db + suffix); !os.IsNotExist(err) {
			t.Errorf("%s left behind at the original path", db+suffix)
		}
	}
}

// TestMoveCatalogAsideDoesNotClobberAnEarlierBackup covers two resets inside one
// second, where the timestamped name collides and os.Rename would silently replace
// the real backup with whatever followed it.
func TestMoveCatalogAsideDoesNotClobberAnEarlierBackup(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "catalog.db")

	if err := os.WriteFile(db, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := moveCatalogAside(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(db, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := moveCatalogAside(db)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("both resets used %s; the second overwrote the first", first)
	}
	got, err := os.ReadFile(first)
	if err != nil || string(got) != "first" {
		t.Errorf("first backup = %q (err %v), want it untouched", got, err)
	}
}

func TestMoveCatalogAsideWithNoCatalog(t *testing.T) {
	dest, err := moveCatalogAside(filepath.Join(t.TempDir(), "absent.db"))
	if err != nil || dest != "" {
		t.Fatalf("no catalog = (%q, %v), want (\"\", nil)", dest, err)
	}
}

// TestEnsureNoCatalogOwnerRefusesALiveOwner pins the check that keeps a reset from
// renaming the catalog out from under a running server or watcher.
func TestEnsureNoCatalogOwnerRefusesALiveOwner(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "holder"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ensureNoCatalogOwner(db); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("want CodeConflict while another process owns the catalog, got %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Released, so the reset may proceed.
	if err := ensureNoCatalogOwner(db); err != nil {
		t.Errorf("want no error once the owner released, got %v", err)
	}
	// An absent catalog is nothing to own.
	if err := ensureNoCatalogOwner(filepath.Join(t.TempDir(), "absent.db")); err != nil {
		t.Errorf("absent catalog: %v", err)
	}
}
