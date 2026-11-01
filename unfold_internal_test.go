package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// ripLibrary opens a library holding one two-track cue rip and returns its two tracks.
func ripLibrary(t *testing.T) (*Library, []model.PID) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	const rate = 22050
	if err := os.WriteFile(filepath.Join(root, "album.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "album.cue"), []byte("PERFORMER \"Rip Artist\"\nTITLE \"Rip Album\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"), 0o644); err != nil {
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
	items, err := lib.store.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %+v (err %v), want the rip's two tracks", items, err)
	}
	return lib, []model.PID{items[0].PID, items[1].PID}
}

// TestAnUnfoldReadsARipOnce: the tracks a cue rip carves out of one file share it, and one
// read of the file re-resolves them all, so the file is read once for both.
func TestAnUnfoldReadsARipOnce(t *testing.T) {
	t.Parallel()
	lib, tracks := ripLibrary(t)
	reads, skipped, err := lib.unfoldReads(context.Background(), tracks)
	if err != nil || len(skipped) != 0 || len(reads) != 1 || len(reads[0].items) != 2 {
		t.Fatalf("reads = %+v, skipped %v (err %v), want the rip once for both tracks", reads, skipped, err)
	}
}

// TestACanceledUnfoldSkipsNothing: a cancel while an unfold reads files ends it canceled,
// naming no item as skipped, rather than taking its own cancel for a file it cannot reach.
func TestACanceledUnfoldSkipsNothing(t *testing.T) {
	t.Parallel()
	lib, tracks := ripLibrary(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lib.roots = fsx.RootChecker{Stat: func(p string) error { cancel(); return fsx.RootUnreachable(p) }}
	reads, skipped, err := lib.unfoldReads(ctx, tracks)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("reads = %+v, skipped %v (err %v)", reads, skipped, err)
	}
	reread, skipped, err := lib.rereadAll(ctx, reads, func(int, int) error { return nil })
	if !waxerr.Is(err, waxerr.CodeCanceled) || len(skipped) != 0 || len(reread) != 0 {
		t.Fatalf("reread %v, skipped %v (err %v), want the cancel and nothing skipped", reread, skipped, err)
	}
}

// TestAnUnfoldGathersWithoutTouchingTheDisk: grouping the items by file reads the catalog
// alone, so the roots are checked only as the files are read, between heartbeats.
func TestAnUnfoldGathersWithoutTouchingTheDisk(t *testing.T) {
	t.Parallel()
	lib, tracks := ripLibrary(t)
	lib.roots = fsx.RootChecker{Stat: func(p string) error {
		t.Errorf("gathering checked the root %s", p)
		return nil
	}}
	if reads, _, err := lib.unfoldReads(context.Background(), tracks); err != nil || len(reads) != 1 {
		t.Fatalf("reads = %+v (err %v), want the rip once", reads, err)
	}
}
