package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// stateOf reads one item's lifecycle state, failing the test on a read error rather
// than dereferencing the nil view a failed Get returns.
func stateOf(t *testing.T, ctx context.Context, lib *waxbin.Library, pid model.PID) model.ItemState {
	t.Helper()
	got, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get(%s): %v", pid, err)
	}
	return got.State
}

// TestMarkMissingVerifiesBeforeWriting walks the verification the facade adds over
// the store's state rule: a file that is really on disk is reported back instead of
// recorded, a deleted file marks, and a repeat is idempotent.
func TestMarkMissingVerifiesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	path := filepath.Join(root, "album", "one.mp3")
	writeFile(t, path, testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	pid := items[0].PID

	// The bytes are there, so the catalog was right and the caller's failure is
	// something else. Nothing is written.
	outcome, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("mark with the file present: %v", err)
	}
	if outcome != model.OutcomeFilesPresent {
		t.Errorf("outcome = %q, want files-present", outcome)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a files-present answer must not write)", st)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outcome, err = lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("mark after deletion: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("outcome = %q, want marked", outcome)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StateMissing {
		t.Errorf("state = %q, want missing", st)
	}

	outcome, err = lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("re-mark: %v", err)
	}
	if outcome != model.OutcomeAlreadyMissing {
		t.Errorf("re-mark outcome = %q, want already-missing", outcome)
	}
}

// TestMarkMissingAdmitsDeletedFolder is the case the mount gate is deliberately not
// tightened for: a user removing a whole album folder is the ordinary genuine
// deletion, and a gate on each file's parent directory would refuse exactly that,
// teaching callers to reach for Force and skip every guard.
func TestMarkMissingAdmitsDeletedFolder(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	album := filepath.Join(root, "album")
	writeFile(t, filepath.Join(album, "one.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}

	if err := os.RemoveAll(album); err != nil {
		t.Fatal(err)
	}
	outcome, err := lib.MarkMissing(ctx, items[0].PID, waxbin.MarkMissingOptions{})
	if err != nil {
		t.Fatalf("a deleted album folder must still mark: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("outcome = %q, want marked", outcome)
	}
}

// TestMarkMissingRefusesUnreachableRoot pins the failure the gate exists for: with
// the library root gone the files' absence proves nothing, so the call is refused
// rather than recording a whole library as deleted. Force is the override.
func TestMarkMissingRefusesUnreachableRoot(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "mount")
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "album", "one.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	pid := items[0].PID

	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("unreachable root = %v, want CodeIO", err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a refused call must not write)", st)
	}

	outcome, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{Force: true})
	if err != nil {
		t.Fatalf("forced mark: %v", err)
	}
	if outcome != model.OutcomeMarked {
		t.Errorf("forced outcome = %q, want marked", outcome)
	}
}

// TestMarkMissingMultiFileBookRefusesPartialView covers a book whose parts disagree:
// one is genuinely deleted and the other's storage is unreadable. The answer is a
// refusal rather than a decision made from the half of the book it could see.
func TestMarkMissingMultiFileBookRefusesPartialView(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the unreadable part cannot be staged")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "in", "p1.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Sanderson", "Mistborn", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "vault", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Sanderson", "Mistborn", 2, testaudio.AudioWithSeed(0x55)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1 (two parts grouped)", len(books))
	}
	pid := books[0].PID

	// One part deleted outright, the other behind a directory that cannot be
	// traversed, so its stat fails with something other than "does not exist".
	if err := os.Remove(filepath.Join(root, "in", "p1.m4b")); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(root, "vault")
	if err := os.Chmod(vault, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(vault, 0o755) })

	if _, err := lib.MarkMissing(ctx, pid, waxbin.MarkMissingOptions{}); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("unreadable part = %v, want CodeIO", err)
	}
	if st := stateOf(t, ctx, lib, pid); st != model.StatePresent {
		t.Errorf("state = %q, want present (a refused call must not write)", st)
	}
}
