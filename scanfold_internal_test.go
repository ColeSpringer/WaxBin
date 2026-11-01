package waxbin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
)

// cancelAfterPut cancels its scan once the first track lands.
type cancelAfterPut struct {
	model.Catalog
	cancel func()
}

func (c cancelAfterPut) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	res, err := c.Catalog.PutScannedTrack(ctx, in)
	c.cancel()
	return res, err
}

// TestACanceledScanReportsItsFolds: a scan canceled partway through a library still answers
// with, and records in its job, what it did to items' pids before the cancel.
func TestACanceledScanReportsItsFolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "a-rip")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const rate = 22050
	writeRaw(t, filepath.Join(dir, "album.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 20, testaudio.AltPartials, 9)))
	writeRaw(t, filepath.Join(dir, "album.cue"), []byte("PERFORMER \"Rip Artist\"\nTITLE \"Rip Album\"\nFILE \"album.wav\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:10:00\n"))
	if err := os.MkdirAll(filepath.Join(root, "b-later"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, filepath.Join(root, "b-later", "later.mp3"), testaudio.BuildMP3WithAudio("Later", "Band", "Record", 1, testaudio.AudioWithSeed(5)))
	lib, err := Open(ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	two := ripTrack(t, lib, "Two")
	one := ripTrack(t, lib, "One")
	if err := os.Remove(filepath.Join(dir, "album.cue")); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lib.scanner = scan.New(cancelAfterPut{Catalog: lib.store, cancel: cancel}, meta.NewReader(), lib.log)
	res, err := lib.Scan(cctx, ScanRequest{})
	if err == nil {
		t.Fatal("scan succeeded, want the cancel")
	}
	if res == nil || res.Total.Absorbed[two] != one {
		t.Fatalf("result = %+v, want Two folded back into %s", res, one)
	}
	j, err := lib.Job(ctx, res.JobPID)
	if err != nil {
		t.Fatal(err)
	}
	var recorded scan.Result
	if err := json.Unmarshal([]byte(j.Result), &recorded); err != nil || recorded.Absorbed[two] != one {
		t.Errorf("job %s result %q (err %v), want Two folded into %s", j.State, j.Result, err, one)
	}
}

// ripTrack is the pid of the item titled title.
func ripTrack(t *testing.T, lib *Library, title string) model.PID {
	t.Helper()
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).Where("title", query.OpIs, title).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("%s = %d items (err %v)", title, len(items), err)
	}
	return items[0].PID
}
