package scan

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
// ingested (and emits a delta) without re-parsing the audio.
func TestFastPathPicksUpLyricSidecar(t *testing.T) {
	st, lib, sc, cr, root := fastPathFixture(t)
	a := filepath.Join(root, "a.mp3")
	writeMP3(t, a, "A", 1)
	r1 := scanAll(t, sc, lib, false)
	itemPID := currentItemPID(t, st, "A")
	_ = r1

	// Add a .lrc beside the audio; the audio bytes are untouched.
	if err := os.WriteFile(filepath.Join(root, "a.lrc"), []byte("[00:00.00]hi\n[00:01.00]there\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seqBefore, _ := st.LatestChangeSeq(context.Background())
	readsBefore := cr.reads
	r2 := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("lyric-sidecar rescan parsed %d audio files, want 0", cr.reads-readsBefore)
	}
	if r2.Unchanged != 1 {
		t.Errorf("lyric-sidecar rescan Unchanged = %d, want 1", r2.Unchanged)
	}
	ly, err := st.LyricsByItem(context.Background(), itemPID)
	if err != nil || len(ly.Synced) != 2 {
		t.Fatalf("lyrics not ingested on fast-path: %v %+v", err, ly)
	}
	seqAfter, _ := st.LatestChangeSeq(context.Background())
	if seqAfter <= seqBefore {
		t.Error("lyric-sidecar change emitted no change_log delta")
	}

	// A third scan with no change stays silent (no delta) and re-parses nothing.
	seqBefore2, _ := st.LatestChangeSeq(context.Background())
	scanAll(t, sc, lib, false)
	seqAfter2, _ := st.LatestChangeSeq(context.Background())
	if seqAfter2 != seqBefore2 {
		t.Error("no-op rescan emitted a change_log delta")
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
	items, err := st.QueryItems(context.Background(), q)
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
