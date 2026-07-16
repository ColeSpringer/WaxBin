package scan

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// countingReader wraps a real meta.Reader and counts full parses, so a test can
// prove the fast-path skipped tag parsing (and, by extension, hashing) for an
// unchanged file.
type countingReader struct {
	inner meta.Reader
	reads int
}

func (c *countingReader) Read(ctx context.Context, path string) (*meta.FileMeta, error) {
	c.reads++
	return c.inner.Read(ctx, path)
}

func fastPathFixture(t *testing.T) (*sqlite.Store, *model.Library, *Scanner, *countingReader, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure lib: %v", err)
	}
	cr := &countingReader{inner: meta.NewReader()}
	sc := New(st, cr, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return st, lib, sc, cr, root
}

func writeMP3(t *testing.T, path, title string, seed byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := testaudio.BuildMP3WithAudio(title, "Artist", "Album", 1, testaudio.AudioWithSeed(seed))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func scanAll(t *testing.T, sc *Scanner, lib *model.Library, force bool) *Result {
	t.Helper()
	res, err := sc.Scan(context.Background(), Request{Library: lib, Force: force}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

// TestFastPathSkipsUnchanged verifies a rescan of an untouched tree parses nothing
// and reports every file as unchanged.
func TestFastPathSkipsUnchanged(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)

	r1 := scanAll(t, sc, lib, false)
	if r1.AudioFiles != 2 || r1.ItemsCreated != 2 {
		t.Fatalf("first scan = %+v, want 2 audio / 2 created", r1)
	}
	if cr.reads != 2 {
		t.Fatalf("first scan parsed %d files, want 2", cr.reads)
	}

	readsBefore := cr.reads
	r2 := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("unchanged rescan parsed %d more files, want 0", cr.reads-readsBefore)
	}
	if r2.Unchanged != 2 || r2.ItemsCreated != 0 || r2.ItemsUpdated != 0 || r2.Relinked != 0 {
		t.Errorf("unchanged rescan = %+v, want 2 unchanged / 0 created,updated,relinked", r2)
	}
}

// TestFastPathReprocessesOnTouch verifies a changed mtime OR size re-parses just
// that file.
func TestFastPathReprocessesOnTouch(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	scanAll(t, sc, lib, false)

	// Bump only a.mp3's mtime (same bytes): only it re-parses.
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(a, future, future); err != nil {
		t.Fatal(err)
	}
	readsBefore := cr.reads
	r := scanAll(t, sc, lib, false)
	if cr.reads-readsBefore != 1 {
		t.Errorf("touched-mtime rescan parsed %d files, want 1", cr.reads-readsBefore)
	}
	if r.Unchanged != 1 {
		t.Errorf("touched-mtime rescan Unchanged = %d, want 1", r.Unchanged)
	}
}

// TestFastPathForce re-parses everything under --force.
func TestFastPathForce(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	scanAll(t, sc, lib, false)

	readsBefore := cr.reads
	r := scanAll(t, sc, lib, true)
	if cr.reads-readsBefore != 2 {
		t.Errorf("forced rescan parsed %d files, want 2", cr.reads-readsBefore)
	}
	if r.Unchanged != 0 {
		t.Errorf("forced rescan Unchanged = %d, want 0", r.Unchanged)
	}
}

// TestFastPathPicksUpLyricSidecar confirms an added .lrc over unchanged audio is
// ingested and emits a delta.
//
// A changed .lrc routes to the full path on purpose. Re-deriving its lyrics_partial
// diagnostic is full-path work, and routing only a break there would strand a stale
// diagnostic once the .lrc was repaired. The price is one re-parse of a file whose
// sidecar just changed, and it stops there: the stat gate short-circuits the next
// scan, which this test pins below.
func TestFastPathPicksUpLyricSidecar(t *testing.T) {
	st, lib, sc, cr, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	scanAll(t, sc, lib, false)
	itemPID := currentItemPID(t, st, "A")

	// Add a .lrc beside the audio; the audio bytes are untouched.
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte("[00:00.00]hi\n[00:01.00]there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seqBefore, _ := st.LatestChangeSeq(context.Background())
	readsBefore := cr.reads
	r2 := scanAll(t, sc, lib, false)
	if cr.reads-readsBefore != 1 {
		t.Errorf("lyric-sidecar rescan parsed %d audio files, want 1 (the changed .lrc routes to the full path)",
			cr.reads-readsBefore)
	}
	// The counter that keeps watch mode alive: the audio did not change, so neither
	// ItemsCreated nor ItemsUpdated moves, and only SidecarsUpdated makes the scan
	// report changed=true.
	if r2.SidecarsUpdated != 1 {
		t.Errorf("SidecarsUpdated = %d, want 1: a sidecar-only change must report as a change",
			r2.SidecarsUpdated)
	}
	if r2.ItemsUpdated != 0 {
		t.Errorf("ItemsUpdated = %d, want 0: the audio bytes did not change", r2.ItemsUpdated)
	}
	ly, err := st.LyricsByItem(context.Background(), itemPID)
	if err != nil || len(ly.Synced) != 2 {
		t.Fatalf("lyrics not ingested: %v %+v", err, ly)
	}
	seqAfter, _ := st.LatestChangeSeq(context.Background())
	if seqAfter <= seqBefore {
		t.Error("lyric-sidecar change emitted no change_log delta")
	}

	// A third scan with no change stays silent and re-parses nothing. The stat gate
	// short-circuits before the .lrc is read, which is what keeps routing every .lrc
	// change to the full path from becoming a permanent per-scan cost.
	seqBefore2, _ := st.LatestChangeSeq(context.Background())
	readsBefore2 := cr.reads
	r3 := scanAll(t, sc, lib, false)
	seqAfter2, _ := st.LatestChangeSeq(context.Background())
	if seqAfter2 != seqBefore2 {
		t.Error("no-op rescan emitted a change_log delta")
	}
	if cr.reads != readsBefore2 {
		t.Errorf("no-op rescan re-parsed %d audio files, want 0: the stat gate must short-circuit",
			cr.reads-readsBefore2)
	}
	if r3.Unchanged != 1 {
		t.Errorf("no-op rescan Unchanged = %d, want 1", r3.Unchanged)
	}
}

// TestFastPathReconcilesDelete marks an item missing when its file is deleted, with
// the other file present (above the survival floor).
func TestFastPathReconcilesDelete(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	scanAll(t, sc, lib, false)
	aPID := currentItemPID(t, st, "A")

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	r := scanAll(t, sc, lib, false)
	if r.Missing != 1 {
		t.Fatalf("delete rescan Missing = %d, want 1", r.Missing)
	}
	if s := itemStateByPID(t, st, aPID); s != string(model.StateMissing) {
		t.Errorf("deleted file's item state = %q, want missing", s)
	}
}

// TestSurvivalGateEmptyRoot: a root that exists but has no files leaves every row
// intact (the transient-mount-loss guard).
func TestSurvivalGateEmptyRoot(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	scanAll(t, sc, lib, false)
	aPID := currentItemPID(t, st, "A")

	// Everything vanishes but the mount point remains: treat as transient, keep rows.
	if err := os.Remove(filepath.Join(root, "a.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	r := scanAll(t, sc, lib, false)
	if r.Missing != 0 {
		t.Fatalf("empty-root rescan Missing = %d, want 0 (survival gate)", r.Missing)
	}
	if s := itemStateByPID(t, st, aPID); s != string(model.StatePresent) {
		t.Errorf("empty-root rescan changed state to %q, want present (rows intact)", s)
	}
}

// TestSurvivalGateBelowFloor: seeing fewer than half the known files skips
// reconciliation, keeping the vanished ones present until a healthier scan.
func TestSurvivalGateBelowFloor(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	writeMP3(t, filepath.Join(root, "c.mp3"), "C", 3)
	scanAll(t, sc, lib, false)
	bPID := currentItemPID(t, st, "B")

	// Remove 2 of 3 (66% gone): below the 50%-seen floor, so skip reconciliation.
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "c.mp3")); err != nil {
		t.Fatal(err)
	}
	r := scanAll(t, sc, lib, false)
	if r.Missing != 0 {
		t.Fatalf("below-floor rescan Missing = %d, want 0 (survival gate)", r.Missing)
	}
	if s := itemStateByPID(t, st, bPID); s != string(model.StatePresent) {
		t.Errorf("below-floor rescan changed state to %q, want present", s)
	}
}

// TestFastPathRestoresMissingItem: a file marked missing, then restored with the
// same size+mtime, must be reprocessed back to present (not left missing by a
// fast-path skip).
func TestFastPathRestoresMissingItem(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	scanAll(t, sc, lib, false)
	aPID := currentItemPID(t, st, "A")

	// Capture a.mp3's on-disk state, then delete it and reconcile to missing (b keeps
	// the survival floor).
	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	origMtime := info.ModTime()
	data, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if r := scanAll(t, sc, lib, false); r.Missing != 1 {
		t.Fatalf("delete rescan Missing = %d, want 1", r.Missing)
	}
	if s := itemStateByPID(t, st, aPID); s != string(model.StateMissing) {
		t.Fatalf("state after delete = %q, want missing", s)
	}

	// Restore with identical bytes AND mtime (the fast-path's blind spot). It must
	// still be reprocessed to present.
	if err := os.WriteFile(a, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a, origMtime, origMtime); err != nil {
		t.Fatal(err)
	}
	seqBefore := latestSeq(t, st)
	scanAll(t, sc, lib, false)
	if s := itemStateByPID(t, st, aPID); s != string(model.StatePresent) {
		t.Errorf("restored item state = %q, want present", s)
	}
	// The missing -> present transition must emit a delta (symmetric with MarkFilesMissing),
	// so a delta consumer's cache does not stay "missing" after the restore.
	if latestSeq(t, st) <= seqBefore {
		t.Error("restore emitted no change_log delta for the missing->present transition")
	}
}

// TestFastPathReconcilesDeletedLyricSidecar: deleting a .lrc over unchanged audio is
// reconciled (lyrics reverting to embedded, here none, so cleared).
func TestFastPathReconcilesDeletedLyricSidecar(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte("[00:00.00]hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	pid := currentItemPID(t, st, "A")
	if _, err := st.LyricsByItem(ctx, pid); err != nil {
		t.Fatalf("lyrics not stored on first scan: %v", err)
	}

	// Delete the .lrc; the audio is untouched. The next scan must clear the stale
	// lyrics rather than leave them.
	if err := os.Remove(filepath.Join(root, "a.lrc")); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	if _, err := st.LyricsByItem(ctx, pid); err == nil {
		t.Error("lyrics survived .lrc deletion; a vanished sidecar was not reconciled")
	}
}

// TestCoverChangedFast verifies the stat-gated cover change detection: an unchanged
// cover is cheap (false), while a changed or newly-appeared cover reports true (so
// the caller routes through the full path and resolveCover keeps embedded precedence).
func TestCoverChangedFast(t *testing.T) {
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "cover.jpg"), 40, 40)
	audio := filepath.Join(dir, "a.mp3")
	coverPath := filepath.Join(dir, "cover.jpg")
	info, err := os.Stat(coverPath)
	if err != nil {
		t.Fatal(err)
	}

	obs := model.AuxObservation{Kind: model.AuxCover, Path: []byte(coverPath), Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}

	// Unchanged cover (stored stat == current): not changed.
	if coverChangedFast(audio, map[string]model.AuxObservation{model.AuxCover: obs}, newArtCache()) {
		t.Error("unchanged cover reported changed")
	}
	// Changed cover (stored mtime differs from disk): changed.
	stale := obs
	stale.MTimeNS = obs.MTimeNS - 1
	if !coverChangedFast(audio, map[string]model.AuxObservation{model.AuxCover: stale}, newArtCache()) {
		t.Error("changed cover not detected")
	}
	// Newly-appeared cover (no stored cover obs, cover on disk): changed.
	if !coverChangedFast(audio, map[string]model.AuxObservation{}, newArtCache()) {
		t.Error("newly-appeared cover not detected")
	}
	// Vanished cover (stored obs, file gone): changed.
	gone := model.AuxObservation{Kind: model.AuxCover, Path: []byte(filepath.Join(dir, "nope.jpg")), Size: 1, MTimeNS: 1}
	if !coverChangedFast(audio, map[string]model.AuxObservation{model.AuxCover: gone}, newArtCache()) {
		t.Error("vanished cover not detected")
	}
}

func currentItemPID(t *testing.T, st *sqlite.Store, title string) model.PID {
	t.Helper()
	q := query.New(query.EntityItems).Where("title", query.OpIs, title).Build()
	items, err := st.QueryItems(context.Background(), q, "")
	if err != nil || len(items) == 0 {
		t.Fatalf("find item %q: %v (n=%d)", title, err, len(items))
	}
	return items[0].PID
}

// TestForceReconcileRecoversLargeDeletion: a >50% deletion is skipped by the
// survival gate, but ForceReconcile reconciles it (the recovery path).
func TestForceReconcileRecoversLargeDeletion(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	writeMP3(t, filepath.Join(root, "b.mp3"), "B", 2)
	writeMP3(t, filepath.Join(root, "c.mp3"), "C", 3)
	scanAll(t, sc, lib, false)
	bPID := currentItemPID(t, st, "B")

	// Remove 2 of 3 (below the floor); a normal scan keeps them present.
	if err := os.Remove(filepath.Join(root, "b.mp3")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "c.mp3")); err != nil {
		t.Fatal(err)
	}
	if r := scanAll(t, sc, lib, false); r.Missing != 0 {
		t.Fatalf("normal scan Missing = %d, want 0 (survival gate)", r.Missing)
	}
	if s := itemStateByPID(t, st, bPID); s != string(model.StatePresent) {
		t.Fatalf("B state after gated scan = %q, want present", s)
	}

	// The recovery path: --reconcile-deletions reconciles past the floor.
	res, err := sc.Scan(context.Background(), Request{Library: lib, ForceReconcile: true}, nil)
	if err != nil {
		t.Fatalf("reconcile scan: %v", err)
	}
	if res.Missing != 2 {
		t.Fatalf("ForceReconcile Missing = %d, want 2", res.Missing)
	}
	if s := itemStateByPID(t, st, bPID); s != string(model.StateMissing) {
		t.Errorf("B state after ForceReconcile = %q, want missing", s)
	}
}

// TestFastPathEmptiedLyricSidecar: a .lrc edited to no usable synced lines reverts
// to embedded (here none, so cleared), like the full path, not left stale.
func TestFastPathEmptiedLyricSidecar(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	lrc := filepath.Join(root, "a.lrc")
	if err := os.WriteFile(lrc, []byte("[00:00.00]hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	pid := currentItemPID(t, st, "A")
	if _, err := st.LyricsByItem(ctx, pid); err != nil {
		t.Fatalf("lyrics not stored: %v", err)
	}

	// Edit the .lrc to content with no timed lines, then rescan.
	if err := os.WriteFile(lrc, []byte("just some prose, no timestamps\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(lrc, future, future)
	scanAll(t, sc, lib, false)
	if _, err := st.LyricsByItem(ctx, pid); err == nil {
		t.Error("stale synced lyrics survived an emptied .lrc; should revert to embedded (none)")
	}
}

// TestFastPathDeletedSidecarNotForeverFull: after a .lrc is deleted and the file is
// reconciled once (full path), later scans fast-path it again (the vanished aux row
// was pruned, so the file is not re-hashed forever).
func TestFastPathDeletedSidecarNotForeverFull(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	lrc := filepath.Join(root, "a.lrc")
	if err := os.WriteFile(lrc, []byte("[00:00.00]hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)

	// Delete the .lrc; the next scan full-reprocesses the file (to revert lyrics).
	if err := os.Remove(lrc); err != nil {
		t.Fatal(err)
	}
	readsBefore := cr.reads
	scanAll(t, sc, lib, false)
	if cr.reads == readsBefore {
		t.Fatal("expected a full reprocess (audio re-read) after the .lrc deletion")
	}

	// A subsequent scan must fast-path again (no forever-full re-hash): zero reads.
	readsBefore = cr.reads
	r := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("post-deletion scan re-read %d files; the vanished aux row was not pruned", cr.reads-readsBefore)
	}
	if r.Unchanged != 1 {
		t.Errorf("post-deletion scan Unchanged = %d, want 1 (fast-pathed)", r.Unchanged)
	}
}

// TestFullPathLyricsOnlyEmitsDelta: on the full path (here --force), a lyrics-only
// change with unchanged audio still emits an item change_log delta so consumers
// don't serve stale lyrics.
func TestFullPathLyricsOnlyEmitsDelta(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	scanAll(t, sc, lib, false)

	// Add a .lrc, then force a full rescan (audio unchanged).
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte("[00:00.00]hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seqBefore := latestSeq(t, st)
	scanAll(t, sc, lib, true) // --force runs the full path
	if latestSeq(t, st) <= seqBefore {
		t.Error("full-path lyrics-only change emitted no change_log delta")
	}
}

func latestSeq(t *testing.T, st *sqlite.Store) int64 {
	t.Helper()
	seq, err := st.LatestChangeSeq(context.Background())
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	return seq
}

func itemStateByPID(t *testing.T, st *sqlite.Store, pid model.PID) string {
	t.Helper()
	v, err := st.ItemByPID(context.Background(), pid)
	if err != nil {
		t.Fatalf("item by pid %s: %v", pid, err)
	}
	return string(v.State)
}

// TestLyricsPartialDiagnosticClearsOnRepair is the repair direction, and the reason a
// changed .lrc routes to the full path UNCONDITIONALLY.
//
// Routing only a break to the full path handles only half the story. Trace the
// repair: a .lrc fixed from partial back to clean would take the fast path,
// UpdateItemSidecars would run, PutScannedTrack would never run, the scan-origin
// diagnostic replace would never run, and the stale lyrics_partial row would live
// on, which is the staleness the diagnostics design exists to prevent.
func TestLyricsPartialDiagnosticClearsOnRepair(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	scanAll(t, sc, lib, false)

	lrc := filepath.Join(root, "a.lrc")
	partial := func() []model.FileDiagnostic {
		t.Helper()
		ds, err := st.FileDiagnostics(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []model.FileDiagnostic
		for _, d := range ds {
			if d.Code == model.DiagLyricsPartial {
				out = append(out, d)
			}
		}
		return out
	}

	// Break it: one good line, one bad timestamp, one untimed line.
	if err := os.WriteFile(lrc, []byte("[00:00.00]good\n[bogus]bad\nuntimed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	got := partial()
	if len(got) != 1 {
		t.Fatalf("lyrics_partial diagnostics = %d, want 1: %+v", len(got), got)
	}
	if got[0].Severity != model.SeverityWarn {
		t.Errorf("severity = %q, want warn", got[0].Severity)
	}
	// The detail is a bounded summary, never the serialized dropped list.
	if !strings.Contains(got[0].Detail, "first at line 2") {
		t.Errorf("detail = %q, want a count plus the first offending line", got[0].Detail)
	}

	// Repair it. The diagnostic must go.
	if err := os.WriteFile(lrc, []byte("[00:00.00]good\n[00:01.00]also good\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	if got := partial(); len(got) != 0 {
		t.Fatalf("stale lyrics_partial survived the repair: %+v", got)
	}
}

// TestOversizedSidecarSkipped verifies the read is bounded. WaxBin pulls a sidecar
// whole into memory before any parser sees it, so the bound must be on the read, not
// the parse, since a parser-side line cap never protected anything.
func TestOversizedSidecarSkipped(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	scanAll(t, sc, lib, false)
	itemPID := currentItemPID(t, st, "A")

	// A .lrc orders of magnitude larger than any real one: valid LRC, but far past the
	// memory guard.
	var big strings.Builder
	big.WriteString("[00:00.00]first\n")
	for big.Len() <= maxSidecarBytes {
		big.WriteString("[00:01.00]padding line to grow the file\n")
	}
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, true)

	// It must be skipped, not ingested: no lyrics row was created from it.
	if ly, err := st.LyricsByItem(context.Background(), itemPID); err == nil && ly != nil && len(ly.Synced) > 0 {
		t.Errorf("oversized .lrc was read and ingested (%d synced lines); it must be skipped", len(ly.Synced))
	}

	// And the skip must be visible. A sidecar sitting beside the audio doing nothing,
	// with nothing anywhere explaining why, is the exact failure this vocabulary exists
	// to prevent.
	ds, err := st.FileDiagnostics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var skipped []model.FileDiagnostic
	for _, d := range ds {
		if d.Code == model.DiagSidecarSkipped {
			skipped = append(skipped, d)
		}
	}
	if len(skipped) != 1 {
		t.Fatalf("sidecar_skipped diagnostics = %d, want 1: %+v", len(skipped), ds)
	}
	if !strings.Contains(skipped[0].Detail, "read limit") {
		t.Errorf("detail = %q, want it to name the limit that rejected the file", skipped[0].Detail)
	}
}

// TestOversizedSidecarDoesNotChurn is the other half of the skip: an oversized
// sidecar must cost one full-path pass, not one on every scan forever.
//
// A skipped file records a stat-only observation precisely so the fast path's
// size+mtime comparison matches on the next scan. Without it there is no
// observation to compare against, the sidecar reads as newly-appeared every time,
// and every scan re-routes to the full path and re-hashes the audio. It is the same
// trap the directory-cover stat fallback already exists to avoid.
func TestOversizedSidecarDoesNotChurn(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a.mp3"), "A", 1)
	scanAll(t, sc, lib, false)

	var big strings.Builder
	for big.Len() <= maxSidecarBytes {
		big.WriteString("[00:01.00]padding line to grow the file\n")
	}
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// First scan after it appears: one full-path pass, to record the skip.
	readsBefore := cr.reads
	scanAll(t, sc, lib, false)
	if cr.reads-readsBefore != 1 {
		t.Fatalf("oversized .lrc appearing parsed %d audio files, want 1", cr.reads-readsBefore)
	}

	// Every scan after: nothing. The stat-only observation short-circuits it.
	readsBefore = cr.reads
	r := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("re-parsed %d audio files with an unchanged oversized .lrc, want 0: it must not churn every scan",
			cr.reads-readsBefore)
	}
	if r.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", r.Unchanged)
	}
}

// TestOversizedCueSidecarSkipped pins the memory guard on the OTHER sidecar. The
// .cue read is the one that stats after reading, so it is the one that would slip
// past a guard applied only to the .lrc.
//
// A skipped sidecar must not look like an absent one. It reports readable, with a
// stat-only observation so the fast path stops re-routing to the full path and a
// diagnostic so the skip stays visible, but it contributes no chapters.
func TestOversizedCueSidecarSkipped(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "book.m4b")

	var big strings.Builder
	big.WriteString("FILE \"book.m4b\" WAVE\n")
	for big.Len() <= maxSidecarBytes {
		big.WriteString("  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "book.cue"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	sheet, obs, diags, ok := scanCueSidecar(audio)
	if !ok {
		t.Fatal("oversized .cue reported not-readable; it must report its skip, not vanish")
	}
	if sheet != nil {
		t.Errorf("sheet = %+v, want nil: the file was never read", sheet)
	}
	if obs.Size == 0 || len(obs.Hash) != 0 {
		t.Errorf("obs = %+v, want a stat-only observation (size set, no content hash)", obs)
	}
	if len(diags) != 1 || diags[0].Code != model.DiagSidecarSkipped {
		t.Fatalf("diagnostics = %+v, want one sidecar_skipped", diags)
	}

	// A normal .cue is still read, so the guard bounds rather than disables.
	small := filepath.Join(dir, "ok.m4b")
	if err := os.WriteFile(filepath.Join(dir, "ok.cue"),
		[]byte("FILE \"ok.m4b\" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, obs, diags, ok := scanCueSidecar(small); !ok || obs.Size == 0 || len(diags) != 0 {
		t.Errorf("normal .cue not read cleanly: ok=%v obs=%+v diags=%+v", ok, obs, diags)
	}

	// A truly-absent .cue still reports not-readable, and records nothing.
	if _, _, _, ok := scanCueSidecar(filepath.Join(dir, "missing.m4b")); ok {
		t.Error("absent .cue reported readable")
	}
}

// TestChapterlessCueOnTrackDoesNotChurn covers the sidecar the book branch used to
// own exclusively. A .cue beside a music track is ordinary (a whole-album rip), and
// a chapterless one routes to the full path, so if its observation is recorded only
// for books, the track re-parses and re-hashes its audio on every scan.
func TestChapterlessCueOnTrackDoesNotChurn(t *testing.T) {
	_, lib, sc, cr, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	// A .cue with no TRACK/INDEX entries: readable, but it yields no chapters.
	if err := os.WriteFile(filepath.Join(root, "a.cue"), []byte("REM just a comment\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scanAll(t, sc, lib, false) // full path: creates the item, records the .cue observation
	readsBefore := cr.reads
	r := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("re-parsed %d audio files with an unchanged chapterless .cue, want 0: "+
			"the observation must be recorded for a track too, not only inside the book branch",
			cr.reads-readsBefore)
	}
	if r.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", r.Unchanged)
	}
}
