package waxbin

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

// openTwoTracks opens a managed library over two scanned tracks and returns it with
// the tracks' views.
func openTwoTracks(t *testing.T) (*Library, string, []*model.ItemView) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := Open(ctx, Options{DBPath: db, Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	writeRaw(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("A", "The Band", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("B", "The Band", "One", 2, testaudio.AudioWithSeed(2)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("query items: %v (n=%d)", err, len(items))
	}
	return lib, db, items
}

// flagOnlyLibrary flags the library's one root read-only, or clears it.
func flagOnlyLibrary(t *testing.T, lib *Library, ro bool) {
	t.Helper()
	ctx := context.Background()
	libs, err := lib.Libraries(ctx)
	if err != nil || len(libs) != 1 {
		t.Fatalf("libraries = %v (err %v)", libs, err)
	}
	if _, err := lib.SetLibraryReadOnly(ctx, libs[0].PID, ro); err != nil {
		t.Fatalf("set read-only: %v", err)
	}
}

// TestEnrichmentWriteBackSkipsALibraryFlaggedAfterTheQuery: the flag can land after
// the write-back read its rows, so each write checks it again. The files are left
// alone and owed, counted apart from the other skips, and written by the first pass
// after the flag clears.
func TestEnrichmentWriteBackSkipsALibraryFlaggedAfterTheQuery(t *testing.T) {
	ctx := context.Background()
	lib, db, items := openTwoTracks(t)
	for _, it := range items {
		enrichItemFields(t, ctx, lib, db, it.PID, map[string]string{"composer": "Someone"})
	}
	rows, err := lib.store.EnrichmentWriteback(ctx, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("owed rows = %d (err %v), want 2", len(rows), err)
	}
	labels, err := lib.store.EnrichedAlbumLabelFiles(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	flagOnlyLibrary(t, lib, true)

	c, err := lib.writeEnrichmentRows(ctx, rows, labels)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if c.readOnly != 2 || c.written+c.failed+c.unrepresented+c.skipped != 0 {
		t.Fatalf("counts = %+v, want both files counted read-only and nothing else", c)
	}
	for _, it := range items {
		fm, err := meta.NewReader().Read(ctx, string(it.Path))
		if err != nil || fm.Tags.Composer != "" {
			t.Fatalf("%s composer = %q (err %v), want the file untouched", it.Path, fm.Tags.Composer, err)
		}
	}

	flagOnlyLibrary(t, lib, false)
	c, err = lib.writeEnrichmentTags(ctx, nil)
	if err != nil || c.written != 2 {
		t.Fatalf("pass after clearing = %+v (err %v), want both files written", c, err)
	}
}

// TestReplayGainWriteBackSkipsALibraryFlaggedAfterTheQuery is the same check for the
// ReplayGain write-back.
func TestReplayGainWriteBackSkipsALibraryFlaggedAfterTheQuery(t *testing.T) {
	ctx := context.Background()
	lib, _, items := openTwoTracks(t)
	for _, it := range items {
		f, err := lib.store.FileByPID(ctx, it.FilePID)
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.store.PutAnalysis(ctx, model.AnalysisInput{
			AnalysisVersion: 1,
			Fingerprint:     model.FingerprintInput{FilePID: it.FilePID, EssenceHash: f.EssenceHash, AlgoVersion: 1, FP: []byte{}},
			Loudness:        &model.LoudnessData{IntegratedLUFS: -12, TrackGainDB: -6, TrackPeak: 0.9},
		}); err != nil {
			t.Fatalf("put analysis: %v", err)
		}
	}
	rows, err := lib.store.ReplayGainWriteback(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("owed rows = %d (err %v), want 2", len(rows), err)
	}
	flagOnlyLibrary(t, lib, true)

	c, err := lib.writeReplayGainRows(ctx, rows)
	if err != nil || c.written+c.failed+c.unrepresented != 0 {
		t.Fatalf("counts = %+v (err %v), want nothing written or failed", c, err)
	}
	for _, it := range items {
		doc, err := waxlabel.ParseFile(ctx, string(it.Path))
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := doc.Tags().First(tag.ReplayGainTrackGain); ok && v != "" {
			t.Fatalf("%s carries a track gain, want the file untouched", it.Path)
		}
	}

	flagOnlyLibrary(t, lib, false)
	c, err = lib.writeReplayGainTags(ctx)
	if err != nil || c.written != 2 {
		t.Fatalf("pass after clearing = %+v (err %v), want both files written", c, err)
	}
}

// warnCounter counts warnings.
type warnCounter struct{ warns []string }

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler       { return w }
func (w *warnCounter) WithGroup(string) slog.Handler            { return w }
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.warns = append(w.warns, r.Message)
	}
	return nil
}

// TestWatchInboxQuietWhileEveryLibraryIsReadOnly: a read-only library is a state the
// user chose, so the watch loop's inbox import passes over it without a warning each
// tick.
func TestWatchInboxQuietWhileEveryLibraryIsReadOnly(t *testing.T) {
	ctx := context.Background()
	root, inbox := t.TempDir(), t.TempDir()
	writeRaw(t, filepath.Join(inbox, "a.mp3"), testaudio.BuildMP3("A", "Band", "One", 1))
	logs := &warnCounter{}
	lib, err := Open(ctx, Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		Inbox:  []string{inbox},
		Logger: slog.New(logs),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lib.Close()
	flagOnlyLibrary(t, lib, true)
	if err := (&watchEngine{lib: lib}).SyncSources(ctx); err != nil {
		t.Fatal(err)
	}
	if len(logs.warns) != 0 {
		t.Fatalf("warnings = %q, want none for a read-only library", logs.warns)
	}
}
