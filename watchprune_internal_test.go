package waxbin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// warnings records the warnings a library logs.
type warnings struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnings) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (w *warnings) Handle(_ context.Context, r slog.Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.msgs = append(w.msgs, r.Message)
	return nil
}

func (w *warnings) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnings) WithGroup(string) slog.Handler      { return w }

func (w *warnings) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.msgs
	w.msgs = nil
	return out
}

// TestWatchRescanOfARemovedFolderIsQuiet: the watcher rescans the folders its events
// name, and a folder a delete pruned is gone by then. That rescan logs no warning, and an
// item still cataloged in a folder removed by hand is reconciled as missing.
func TestWatchRescanOfARemovedFolderIsQuiet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	logged := &warnings{}
	lib, err := Open(ctx, Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"), Logger: slog.New(logged),
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	pruned, removed := filepath.Join(root, "A", "One"), filepath.Join(root, "B", "Two")
	for _, dir := range []string{pruned, removed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeRaw(t, filepath.Join(pruned, "1.mp3"), testaudio.BuildMP3WithAudio("One", "A", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(pruned, "cover.jpg"), []byte("jpeg"))
	writeRaw(t, filepath.Join(removed, "2.mp3"), testaudio.BuildMP3WithAudio("Two", "B", "Two", 1, testaudio.AudioWithSeed(2)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	libs, err := lib.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (err %v), want two", len(items), err)
	}
	var one model.PID
	for _, it := range items {
		if it.Title == "One" {
			one = it.PID
		}
	}
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{one}, model.DeleteTrash)
	if err != nil {
		t.Fatal(err)
	}
	if rep, err := lib.ApplyDelete(ctx, plan); err != nil || rep.DirsPruned != 2 {
		t.Fatalf("delete = %+v (err %v), want One and A pruned", rep, err)
	}
	if err := os.RemoveAll(removed); err != nil {
		t.Fatal(err)
	}
	logged.take()
	engine := &watchEngine{lib: lib}
	if _, err := engine.Rescan(ctx, libs[0].PID, pruned, false); err != nil {
		t.Fatalf("rescan of the pruned folder: %v", err)
	}
	changed, err := engine.Rescan(ctx, libs[0].PID, removed, false)
	if err != nil || !changed {
		t.Fatalf("rescan of the removed folder: changed %v (err %v), want its item reconciled", changed, err)
	}
	if got := logged.take(); len(got) != 0 {
		t.Errorf("rescans of removed folders warned: %q", got)
	}
}
