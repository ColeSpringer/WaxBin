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
// is a [start, end) pair, and the track number is its 1-based position, so the
// offset-anchored identity key is stable when the trailing window is dropped.
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
			Track:   model.Track{Artist: "Rip Artist", AlbumArtist: "Rip Artist", Album: "Rip Album", TrackNo: n},
			StartMS: w[0], EndMS: w[1],
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

	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 300, [][2]int64{{0, 100}, {100, 300}})
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
	if !t1.Virtual || t1.StartMS != 0 || t1.EndMS != 100 || t1.DurationMS != 100 {
		t.Errorf("track 1 = virtual %v [%d,%d] dur %d, want virtual [0,100] dur 100", t1.Virtual, t1.StartMS, t1.EndMS, t1.DurationMS)
	}
	if !t2.Virtual || t2.StartMS != 100 || t2.EndMS != 300 || t2.DurationMS != 200 {
		t.Errorf("track 2 = virtual %v [%d,%d] dur %d, want virtual [100,300] dur 200", t2.Virtual, t2.StartMS, t2.EndMS, t2.DurationMS)
	}
	if t1.FilePID != t2.FilePID || t1.FilePID != res.FilePID {
		t.Fatalf("virtual tracks must share one file: %s / %s / %s", t1.FilePID, t2.FilePID, res.FilePID)
	}

	// The library total sums the two windows (300), not the file duration once per
	// track (600). Because db verify uses the same expression, it also stays clean.
	stats, err := st.Stats(ctx, "", 5)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.TotalDuration != 300 {
		t.Fatalf("stats total = %d, want 300 (window sum, not inflated per-track file duration)", stats.TotalDuration)
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
			[][2]int64{{0, 100}, {100, 300}, {300, 600}})
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
		[][2]int64{{0, 100}, {100, 300}})
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

	// The file is 100 ms, but track 2 (the last) starts at 200 ms with an open end, so
	// its window would compute as 100 - 200 = -100 without the floor.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 100,
		[][2]int64{{0, 100}, {200, 0}})
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
	if stats.TotalDuration != 100 {
		t.Fatalf("stats total = %d, want 100 (the out-of-range window floors to 0)", stats.TotalDuration)
	}
	assertConsistent(t, st)
}

// TestVirtualTrackLyricsUsesWindowDuration: lyrics enrichment must key on a virtual
// track's window, not the whole shared file, or a provider that matches on duration
// (LRCLIB) is fed the wrong length and never matches.
func TestVirtualTrackLyricsUsesWindowDuration(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)

	// A 60 s file carved into a 3 s opening track and a 57 s remainder.
	in := vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 60000,
		[][2]int64{{0, 3000}, {3000, 60000}})
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
		t.Errorf("track 2 lyrics duration = %d s, want 57 s (window 3000..60000)", got)
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
	if _, err := st.PutScannedVirtualTracks(ctx, vtrackInput(lib.ID, "/lib/album.mp3", "sha256:VE", "sha256:VC", 300,
		[][2]int64{{0, 100}, {100, 300}})); err != nil {
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
