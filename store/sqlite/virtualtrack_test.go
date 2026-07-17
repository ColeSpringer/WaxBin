package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// vtrackInput builds a virtual-track scan input for one single-file rip: each window
// is a [start, end) pair in CD frames (75/sec, so 3 frames is 40 ms), and the track
// number is its 1-based position, so the offset-anchored identity key is stable when
// the trailing window is dropped. dur is the backing file's duration in
// milliseconds, which is what the file row stores.
func vtrackInput(libID int64, path, essence, content string, dur int64, windows [][2]int64) model.PutScannedVirtualTracksInput {
	tracks := make([]model.VirtualTrack, len(windows))
	for i, w := range windows {
		n := i + 1
		title := fmt.Sprintf("Track %d", n)
		tracks[i] = model.VirtualTrack{
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent,
				Title: title, SortKey: model.SortKey(title),
				IdentityKey: identity.VirtualTrackKey(essence, n, w[0]),
			},
			Track:       model.Track{Artist: "Rip Artist", AlbumArtist: "Rip Artist", Album: "Rip Album", TrackNo: n},
			StartFrames: w[0], EndFrames: w[1],
		}
	}
	return model.PutScannedVirtualTracksInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, Size: int64(len(content)), MTimeNS: 1,
			ContentHash: content, EssenceHash: essence, DurationMS: dur, ScanState: model.ScanIndexed,
		},
		Tracks: tracks,
	}
}

func vtItems(t *testing.T, st *sqlite.Store) []*model.ItemView {
	t.Helper()
	items, err := st.QueryItems(context.Background(), query.New(query.EntityItems).OrderBy("track", false).Build(), "")
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	return items
}

func assertConsistent(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v", rep)
	}
}

// TestVirtualTracksCreateAndOffsets: a single-file rip becomes N ordinary track
// items sharing one file, each with its offset window driving its duration, and the
// rollups/stats sum the windows rather than the whole file once per track.
func TestVirtualTracksCreateAndOffsets(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// Frames 0..9..27 over a 360 ms file: a 9-frame (120 ms) opener and an 18-frame
	// (240 ms) remainder.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 360, [][2]int64{{0, 9}, {9, 27}})
	res, err := st.PutScannedVirtualTracks(ctx, in)
	if err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	if !res.ItemCreated || !res.FileCreated {
		t.Fatalf("first put should create items + file: %+v", res)
	}

	items := vtItems(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(items))
	}
	t1, t2 := items[0], items[1]
	if !t1.Virtual || t1.StartFrames != 0 || t1.EndFrames != 9 || t1.DurationMS != 120 {
		t.Errorf("track 1 = virtual %v [%d,%d) frames dur %d, want virtual [0,9) dur 120",
			t1.Virtual, t1.StartFrames, t1.EndFrames, t1.DurationMS)
	}
	if !t2.Virtual || t2.StartFrames != 9 || t2.EndFrames != 27 || t2.DurationMS != 240 {
		t.Errorf("track 2 = virtual %v [%d,%d) frames dur %d, want virtual [9,27) dur 240",
			t2.Virtual, t2.StartFrames, t2.EndFrames, t2.DurationMS)
	}
	// The millisecond pair rides along, derived, for a player that seeks.
	if t2.StartMS != 120 || t2.EndMS != 360 {
		t.Errorf("track 2 ms window = [%d,%d), want [120,360)", t2.StartMS, t2.EndMS)
	}
	if t1.FilePID != t2.FilePID || t1.FilePID != res.FilePID {
		t.Fatalf("virtual tracks must share one file: %s / %s / %s", t1.FilePID, t2.FilePID, res.FilePID)
	}

	// The library total sums the two windows (360), not the file duration once per
	// track (720). Because db verify uses the same expression, it also stays clean.
	stats, err := st.Stats(ctx, "", 5)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalDuration != 360 {
		t.Fatalf("stats total = %d, want 360 (window sum, not inflated per-track file duration)", stats.TotalDuration)
	}
	assertConsistent(t, st)

	// The shared file's on-disk tags are unsafe to rewrite for one virtual track.
	shared, err := st.FileSharedOrVirtual(ctx, t1.FilePID)
	if err != nil || !shared {
		t.Fatalf("FileSharedOrVirtual = %v (err %v), want true for a virtual-track file", shared, err)
	}
}

// TestVirtualTracksReconcileSet: a rescan reconciles the whole set against the file
// without forking siblings. An unchanged rescan is silent, and dropping a track
// deletes only it while the survivors keep their pids.
func TestVirtualTracksReconcileSet(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	three := func() model.PutScannedVirtualTracksInput {
		return vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 600,
			[][2]int64{{0, 9}, {9, 27}, {27, 45}})
	}
	if _, err := st.PutScannedVirtualTracks(ctx, three()); err != nil {
		t.Fatalf("initial put: %v", err)
	}
	before := vtItems(t, st)
	if len(before) != 3 {
		t.Fatalf("initial virtual tracks = %d, want 3", len(before))
	}
	p1, p2 := before[0].PID, before[1].PID

	// An identical rescan changes nothing and emits no change_log rows.
	seq1, _ := st.LatestChangeSeq(ctx)
	if _, err := st.PutScannedVirtualTracks(ctx, three()); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	seq2, _ := st.LatestChangeSeq(ctx)
	if seq2 != seq1 {
		t.Fatalf("idempotent rescan emitted %d change rows, want 0", seq2-seq1)
	}
	if got := vtItems(t, st); len(got) != 3 || got[0].PID != p1 || got[1].PID != p2 {
		t.Fatalf("idempotent rescan reshaped the set: %d items", len(got))
	}

	// Drop the trailing track: only it is deleted; the survivors keep their pids
	// (linkVirtualTrackFile never detaches or deletes a sibling).
	dropped := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 600,
		[][2]int64{{0, 9}, {9, 27}})
	if _, err := st.PutScannedVirtualTracks(ctx, dropped); err != nil {
		t.Fatalf("drop-track put: %v", err)
	}
	after := vtItems(t, st)
	if len(after) != 2 {
		t.Fatalf("after dropping a track, virtual tracks = %d, want 2", len(after))
	}
	if after[0].PID != p1 || after[1].PID != p2 {
		t.Fatalf("dropping a track forked its siblings: %s/%s -> %s/%s", p1, p2, after[0].PID, after[1].PID)
	}
	assertConsistent(t, st)
}

// TestVirtualTrackDurationNeverNegative: a malformed or mismatched cue whose final
// track starts past the file's probed duration must not yield a negative window
// duration; the effective-duration expression floors it at 0 so stats and rollups
// stay sane and db verify stays clean.
func TestVirtualTrackDurationNeverNegative(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// The file is 120 ms, but track 2 (the last) starts at frame 15 (200 ms) with an
	// open end, so its window would compute as 120 - 200 = -80 without the floor.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 120,
		[][2]int64{{0, 9}, {15, 0}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}
	for _, it := range vtItems(t, st) {
		if it.DurationMS < 0 {
			t.Fatalf("virtual track %s has negative duration %d", it.PID, it.DurationMS)
		}
	}
	stats, err := st.Stats(ctx, "", 5)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalDuration != 120 {
		t.Fatalf("stats total = %d, want 120 (the out-of-range window floors to 0)", stats.TotalDuration)
	}
	assertConsistent(t, st)
}

// TestVirtualTrackLyricsUsesWindowDuration: lyrics enrichment must key on a virtual
// track's window, not the whole shared file, or a provider that matches on duration
// (LRCLIB) is fed the wrong length and never matches.
func TestVirtualTrackLyricsUsesWindowDuration(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// A 60 s file (4500 frames) carved into a 3 s opening track (225 frames) and a
	// 57 s remainder.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 60000,
		[][2]int64{{0, 225}, {225, 4500}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}

	targets, err := st.ItemsNeedingLyrics(ctx, false, 0, 100)
	if err != nil {
		t.Fatalf("items needing lyrics: %v", err)
	}
	byTitle := make(map[string]int, len(targets))
	for _, tg := range targets {
		byTitle[tg.Name] = tg.DurationSec
	}
	if got := byTitle["Track 1"]; got != 3 {
		t.Errorf("track 1 lyrics duration = %d s, want 3 s (its window, not the 60 s file)", got)
	}
	if got := byTitle["Track 2"]; got != 57 {
		t.Errorf("track 2 lyrics duration = %d s, want 57 s (window frames 225..4500)", got)
	}
}

// TestVirtualTrackWindowRoundTripsExactly is the store half of the guard the frame
// unit exists for: a window on frames not divisible by 3 must read back as the same
// frames it was written with, so a consumer converts them to the exact sample the
// disc named. Asserted on frames, never on the derived milliseconds, which are lossy
// by construction.
//
// It also pins the duration expression against the drift it would have if it
// converted each endpoint before subtracting: frames 1 to 3 is a 2-frame window,
// which is 26 ms, while 40 - 13 reports 27.
func TestVirtualTrackWindowRoundTripsExactly(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// Track 2 starts at cue time 03:15:22 = (3*60+15)*75 + 22 = 14647 frames, which at
	// 44.1 kHz is sample 8612436 exactly. The old millisecond path landed 15 samples
	// early.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 300000,
		[][2]int64{{1, 3}, {14647, 0}})
	if _, err := st.PutScannedVirtualTracks(ctx, in); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}

	items := vtItems(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(items))
	}
	t1, t2 := items[0], items[1]
	if t1.StartFrames != 1 || t1.EndFrames != 3 {
		t.Errorf("track 1 window = [%d,%d) frames, want [1,3)", t1.StartFrames, t1.EndFrames)
	}
	if t1.DurationMS != 26 {
		t.Errorf("track 1 duration = %d ms, want 26 (the frame delta converts as a unit; "+
			"converting each endpoint first reports 27)", t1.DurationMS)
	}
	if t2.StartFrames != 14647 {
		t.Errorf("track 2 start = %d frames, want 14647 exactly", t2.StartFrames)
	}
	if got := t2.StartFrames * 44100 / 75; got != 8612436 {
		t.Errorf("track 2 first sample at 44.1 kHz = %d, want 8612436", got)
	}
	// An open end reads back as 0 frames, and the window's duration falls through to
	// the file's own.
	if t2.EndFrames != 0 || t2.EndMS != 0 {
		t.Errorf("track 2 end = %d frames / %d ms, want 0 (runs to the end of the file)",
			t2.EndFrames, t2.EndMS)
	}
}

// TestVirtualTracksConvertFromWholeFile: a whole-file track scanned before the .cue
// existed is detached and deleted when the file is re-cataloged as virtual tracks.
func TestVirtualTracksConvertFromWholeFile(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// A plain whole-file track keyed on the essence.
	whole := input(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", "Whole File")
	r, err := st.PutScannedTrack(ctx, whole)
	if err != nil {
		t.Fatalf("put whole-file track: %v", err)
	}
	wholePID := r.ItemPID

	// Re-catalog the same file as a two-track rip.
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 360,
		[][2]int64{{0, 9}, {9, 27}})); err != nil {
		t.Fatalf("put virtual tracks: %v", err)
	}

	if _, err := st.ItemByPID(ctx, wholePID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("whole-file track should be gone after conversion, got %v", err)
	}
	items := vtItems(t, st)
	if len(items) != 2 {
		t.Fatalf("after conversion, items = %d, want 2 virtual tracks", len(items))
	}
	for _, it := range items {
		if !it.Virtual {
			t.Errorf("item %s is not virtual after conversion", it.PID)
		}
	}
	assertConsistent(t, st)
}
