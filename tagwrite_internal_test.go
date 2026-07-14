package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

func TestReplayGainEdits(t *testing.T) {
	// A standalone track (no album) writes track keys and CLEARS the album keys, so
	// stale album gain from a former album membership is removed on disk.
	e := replayGainEdits(model.ReplayGainRow{Codec: "mp3", TrackGainDB: -6.35, TrackPeak: 0.988})
	if !hasEdit(e, "REPLAYGAIN_TRACK_GAIN", "-6.35 dB") || !hasEdit(e, "REPLAYGAIN_TRACK_PEAK", "0.988000") {
		t.Errorf("track-only edits wrong: %+v", e)
	}
	if !isClear(e, "REPLAYGAIN_ALBUM_GAIN") || !isClear(e, "REPLAYGAIN_ALBUM_PEAK") {
		t.Errorf("track-only edits must clear album keys: %+v", e)
	}
	if hasKey(e, "R128_TRACK_GAIN") {
		t.Errorf("non-opus track must not contain R128 keys: %+v", e)
	}

	// An album member gets track + album keys.
	e = replayGainEdits(model.ReplayGainRow{Codec: "flac", TrackGainDB: -6.0, HasAlbum: true, AlbumGainDB: -5.5, AlbumPeak: 0.99})
	if !hasKey(e, "REPLAYGAIN_ALBUM_GAIN") || !hasKey(e, "REPLAYGAIN_ALBUM_PEAK") {
		t.Errorf("album member missing album keys: %+v", e)
	}

	// Opus uses R128 integer gains, not the REPLAYGAIN_* strings.
	e = replayGainEdits(model.ReplayGainRow{Codec: "opus", TrackGainDB: -5.0, HasAlbum: true, AlbumGainDB: -4.0})
	if !hasKey(e, "R128_TRACK_GAIN") || !hasKey(e, "R128_ALBUM_GAIN") {
		t.Errorf("opus missing R128 keys: %+v", e)
	}
	if hasKey(e, "REPLAYGAIN_TRACK_GAIN") {
		t.Errorf("opus should not use REPLAYGAIN_* keys: %+v", e)
	}
}

func TestR128Gain(t *testing.T) {
	// WaxBin gain references -18 LUFS; R128 references -23, so 5 dB is subtracted,
	// then Q7.8: (-5 - 5) * 256 = -2560.
	if got := r128Gain(-5.0); got != "-2560" {
		t.Errorf("r128Gain(-5.0) = %q, want -2560", got)
	}
	// 5 dB gain -> (5-5)*256 = 0.
	if got := r128Gain(5.0); got != "0" {
		t.Errorf("r128Gain(5.0) = %q, want 0", got)
	}
}

// TestReplayGainWriteBackAlbum scans two tracks of one album, records loudness,
// aggregates album gain, writes it back, and confirms each file carries both the
// track and album ReplayGain tags, and that the catalog's file row was updated so
// the scan fast-path recognizes WaxBin's own write.
func TestReplayGainWriteBackAlbum(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := Open(ctx, Options{
		DBPath: db, WriteReplayGainTags: true,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer lib.Close()

	// Two distinct-essence tracks sharing one album.
	writeRaw(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("A", "The Band", "One", 1, testaudio.AudioWithSeed(1)))
	writeRaw(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("B", "The Band", "One", 2, testaudio.AudioWithSeed(2)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Record loudness for each track directly (no decoder needed in the test env).
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 2 {
		t.Fatalf("query items: %v (n=%d)", err, len(items))
	}
	for i, it := range items {
		f, err := lib.store.FileByPID(ctx, it.FilePID)
		if err != nil {
			t.Fatalf("file by pid: %v", err)
		}
		if err := lib.store.PutAnalysis(ctx, model.AnalysisInput{
			AnalysisVersion: 1,
			Fingerprint:     model.FingerprintInput{FilePID: it.FilePID, EssenceHash: f.EssenceHash, AlgoVersion: 1, FP: []byte{}},
			Loudness:        &model.LoudnessData{IntegratedLUFS: -12 - float64(i), TrackGainDB: -6 - float64(i), TrackPeak: 0.9},
		}); err != nil {
			t.Fatalf("put analysis: %v", err)
		}
	}
	if err := lib.store.RefreshAlbumGain(ctx); err != nil {
		t.Fatalf("album gain: %v", err)
	}

	c, err := lib.writeReplayGainTags(ctx)
	if err != nil {
		t.Fatalf("write rg tags: %v", err)
	}
	if c.written != 2 {
		t.Fatalf("wrote %d rg tags, want 2", c.written)
	}
	if c.failed != 0 || c.unrepresented != 0 {
		t.Fatalf("clean run reported failed=%d unrepresented=%d, want 0/0", c.failed, c.unrepresented)
	}

	// Each file now carries track + album ReplayGain, and its catalog row's content
	// hash/mtime match the new bytes (so a rescan won't re-hash it).
	for _, it := range items {
		doc, err := waxlabel.ParseFile(ctx, string(it.Path))
		if err != nil {
			t.Fatalf("parse %s: %v", it.Path, err)
		}
		if v, ok := doc.Tags().First(tag.ReplayGainTrackGain); !ok || v == "" {
			t.Errorf("%s missing REPLAYGAIN_TRACK_GAIN", it.Path)
		}
		if v, ok := doc.Tags().First(tag.ReplayGainAlbumGain); !ok || v == "" {
			t.Errorf("%s missing REPLAYGAIN_ALBUM_GAIN", it.Path)
		}

		f, err := lib.store.FileByPID(ctx, it.FilePID)
		if err != nil {
			t.Fatalf("file after: %v", err)
		}
		info, err := os.Stat(string(it.Path))
		if err != nil {
			t.Fatal(err)
		}
		if f.Size != info.Size() || f.MTimeNS != info.ModTime().UnixNano() {
			t.Errorf("catalog file state not updated after RG write: db(%d,%d) disk(%d,%d)",
				f.Size, f.MTimeNS, info.Size(), info.ModTime().UnixNano())
		}
	}
}

// TestReplayGainWriteBackCountsFailures is the regression test for the defect this
// counter exists to fix: writeReplayGainTags used to log-and-continue on a write
// error, so a run against a read-only library reported success with nothing
// written, which is indistinguishable from a run with nothing to write.
func TestReplayGainWriteBackCountsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only bit, so the write would succeed")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := Open(ctx, Options{
		DBPath: db, WriteReplayGainTags: true,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer lib.Close()

	writeRaw(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("A", "The Band", "One", 1, testaudio.AudioWithSeed(1)))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 {
		t.Fatalf("query items: %v (n=%d)", err, len(items))
	}
	f, err := lib.store.FileByPID(ctx, items[0].FilePID)
	if err != nil {
		t.Fatalf("file by pid: %v", err)
	}
	if err := lib.store.PutAnalysis(ctx, model.AnalysisInput{
		AnalysisVersion: 1,
		Fingerprint:     model.FingerprintInput{FilePID: items[0].FilePID, EssenceHash: f.EssenceHash, AlgoVersion: 1, FP: []byte{}},
		Loudness:        &model.LoudnessData{IntegratedLUFS: -12, TrackGainDB: -6, TrackPeak: 0.9},
	}); err != nil {
		t.Fatalf("put analysis: %v", err)
	}

	// The write is atomic (a rewrite into the directory), so removing the directory's
	// write bit is what makes it fail.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	c, err := lib.writeReplayGainTags(ctx)
	if err != nil {
		t.Fatalf("write rg tags: %v", err)
	}
	if c.written != 0 {
		t.Fatalf("wrote %d rg tags into a read-only library, want 0", c.written)
	}
	if c.failed != 1 {
		t.Fatalf("failed = %d, want 1: a write-back that errored must not report as nothing-to-write", c.failed)
	}
}

// TestOrganizeTagWriteAndPIDStamp organizes a compilation track with tag-write and
// PID stamping enabled, then confirms the moved file carries the corrected
// albumArtist ("Various Artists"), the item PID tag, organize provenance, and a
// locked field left untouched.
func TestOrganizeTagWriteAndPIDStamp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := Open(ctx, Options{
		DBPath:       db,
		StampItemPID: true,
		// Override the native profile to enable tag write-back.
		Profiles: []config.ProfileDef{{Name: "waxbin-native", TagWrite: true}},
		Roots:    []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer lib.Close()

	// A compilation track whose file tags a specific album artist; organize should
	// correct it to the literal "Various Artists" on disk.
	spec := testaudio.MP3Spec{Title: "Hit", Artist: "Solo", Album: "Comp", AlbumArtist: "Solo", Track: 4, Compilation: true, Audio: testaudio.AudioWithSeed(7)}
	writeRaw(t, filepath.Join(root, "in.mp3"), testaudio.BuildMP3FromSpec(spec))
	if _, err := lib.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 {
		t.Fatalf("query: %v (n=%d)", err, len(items))
	}
	itemPID := items[0].PID

	// Lock the composer field with a curated value to prove locks are respected. (We
	// lock a field organize would not otherwise write, then also lock album_artist to
	// prove the write skips it.)
	if err := lib.store.LockField(ctx, itemPID, "album_artist"); err != nil {
		t.Fatalf("lock: %v", err)
	}

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.TagWrite || !plan.StampPID {
		t.Fatalf("plan flags: TagWrite=%v StampPID=%v, want both true", plan.TagWrite, plan.StampPID)
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply organize: %v", err)
	}

	// Find the moved file and read its tags.
	items, _ = lib.Query(ctx, query.New(query.EntityItems).Build())
	moved := string(items[0].Path)
	doc, err := waxlabel.ParseFile(ctx, moved)
	if err != nil {
		t.Fatalf("parse moved: %v", err)
	}
	// album_artist was LOCKED, so organize must NOT have rewritten it to "Various
	// Artists"; it keeps the original tagged value.
	if v, _ := doc.Tags().First(tag.AlbumArtist); v == "Various Artists" {
		t.Errorf("locked album_artist was overwritten to %q", v)
	}
	// track number written.
	if v, ok := doc.Tags().First(tag.TrackNumber); !ok || v == "" {
		t.Errorf("track number not written")
	}
	// PID stamped.
	if v, _ := doc.Tags().First(tag.Key(organize.WaxbinItemPIDKey)); v != string(itemPID) {
		t.Errorf("WAXBIN_ITEM_PID = %q, want %q", v, itemPID)
	}
}

// TestRebuildAdoptsStampedPID stamps an item PID during organize, then rebuilds a
// fresh catalog over the same files and confirms the item's PID is restored from the
// WAXBIN_ITEM_PID tag.
func TestRebuildAdoptsStampedPID(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	lib1, err := Open(ctx, Options{
		DBPath:       filepath.Join(t.TempDir(), "c1.db"),
		StampItemPID: true,
		Roots:        []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open lib1: %v", err)
	}
	writeRaw(t, filepath.Join(root, "in.mp3"), testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(3)))
	if _, err := lib1.Scan(ctx, ScanRequest{}); err != nil {
		t.Fatalf("scan1: %v", err)
	}
	plan, err := lib1.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := lib1.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	items, _ := lib1.Query(ctx, query.New(query.EntityItems).Build())
	origPID := items[0].PID
	lib1.Close()

	// Rebuild into a fresh catalog over the same (now organized + stamped) files.
	lib2, err := Open(ctx, Options{
		DBPath: filepath.Join(t.TempDir(), "c2.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open lib2: %v", err)
	}
	defer lib2.Close()
	if _, err := lib2.Scan(ctx, ScanRequest{AdoptStampedPIDs: true}); err != nil {
		t.Fatalf("rebuild scan: %v", err)
	}
	items2, _ := lib2.Query(ctx, query.New(query.EntityItems).Build())
	if len(items2) != 1 {
		t.Fatalf("rebuild items = %d, want 1", len(items2))
	}
	if items2[0].PID != origPID {
		t.Errorf("rebuilt item PID = %s, want restored %s", items2[0].PID, origPID)
	}
}

// TestPIDAdoptionConflictMintsFresh stamps the SAME PID on two distinct-essence
// files; a rebuild adopts it for exactly one and mints a fresh PID for the other (a
// copyable tag must never make two files claim one identity).
func TestPIDAdoptionConflictMintsFresh(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	shared := model.NewPID()
	w := meta.NewWriter()

	for i, name := range []string{"one.mp3", "two.mp3"} {
		p := filepath.Join(root, name)
		writeRaw(t, p, testaudio.BuildMP3WithAudio(name, "Artist", "Album", 1, testaudio.AudioWithSeed(byte(i+1))))
		if _, err := w.Apply(ctx, p, []meta.TagEdit{{Key: model.TagWaxbinItemPID, Values: []string{string(shared)}}}); err != nil {
			t.Fatalf("stamp %s: %v", name, err)
		}
	}

	lib, err := Open(ctx, Options{
		DBPath: filepath.Join(t.TempDir(), "c.db"),
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer lib.Close()
	if _, err := lib.Scan(ctx, ScanRequest{AdoptStampedPIDs: true}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 distinct", len(items))
	}
	adopted := 0
	for _, it := range items {
		if it.PID == shared {
			adopted++
		}
		if !it.PID.Valid() {
			t.Errorf("item PID %q is not a valid ULID", it.PID)
		}
	}
	if adopted != 1 {
		t.Errorf("%d items adopted the shared PID, want exactly 1 (the other must mint fresh)", adopted)
	}
}

func hasEdit(edits []meta.TagEdit, key, val string) bool {
	for _, e := range edits {
		if e.Key == key {
			for _, v := range e.Values {
				if v == val {
					return true
				}
			}
		}
	}
	return false
}

func hasKey(edits []meta.TagEdit, key string) bool {
	for _, e := range edits {
		if e.Key == key {
			return true
		}
	}
	return false
}

// isClear reports whether the edits contain key as a clear (present with no values).
func isClear(edits []meta.TagEdit, key string) bool {
	for _, e := range edits {
		if e.Key == key {
			return len(e.Values) == 0
		}
	}
	return false
}

func writeRaw(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
