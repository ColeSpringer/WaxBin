package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin/query"
)

// assertScanPartition checks the identities Result promises: every audio file takes
// exactly one outcome, and every file seen is audio or skipped.
func assertScanPartition(t *testing.T, r *Result) {
	t.Helper()
	if sum := r.ItemsCreated + r.ItemsUpdated + r.SidecarsUpdated + r.Copies + r.Unchanged + r.Errored; sum != r.AudioFiles {
		t.Errorf("outcomes sum to %d for %d audio files: %+v", sum, r.AudioFiles, r)
	}
	if r.FilesSeen != r.AudioFiles+r.Skipped {
		t.Errorf("files seen = %d, want %d audio + %d skipped: %+v", r.FilesSeen, r.AudioFiles, r.Skipped, r)
	}
}

func TestScanCountsACopy(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a", "1.mp3"), "One", 9)
	writeMP3(t, filepath.Join(root, "b", "1.mp3"), "One Again", 9)
	r := scanAll(t, sc, lib, false)
	if r.AudioFiles != 2 || r.ItemsCreated != 1 || r.Copies != 1 {
		t.Errorf("scan = %+v, want one item created and one copy", r)
	}
	if r = scanAll(t, sc, lib, true); r.Copies != 0 || r.Unchanged != 2 || r.Reread != 2 {
		t.Errorf("forced rescan = %+v, want both unchanged and re-read, no new copy", r)
	}
}

// unreadableAt makes the scanner's walk report dir as a directory it could not read, the
// way filepath.WalkDir reports a failed ReadDir, and skip its contents.
func unreadableAt(sc *Scanner, dir string) {
	sc.walk = func(root string, fn fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path != dir {
				return fn(path, d, err)
			}
			if err := fn(path, d, nil); err != nil {
				return err
			}
			if err := fn(path, d, errors.New("permission denied")); err != nil && err != fs.SkipDir {
				return err
			}
			return fs.SkipDir
		})
	}
}

// TestScanCountsWalkErrorsApart: a directory the walk cannot read is a walk error, not
// an audio file that failed.
func TestScanCountsWalkErrorsApart(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "ok.mp3"), "Fine", 1)
	locked := filepath.Join(root, "locked")
	writeMP3(t, filepath.Join(locked, "x.mp3"), "Hidden", 2)
	unreadableAt(sc, locked)
	r := scanAll(t, sc, lib, false)
	if r.WalkErrors != 1 || r.Errored != 0 || r.AudioFiles != 1 {
		t.Errorf("scan = %+v, want one walk error and no errored audio", r)
	}
}

// TestScanKeepsFilesUnderAnUnreadableFolder: files under a folder the walk could not
// read are not reconciled as gone.
func TestScanKeepsFilesUnderAnUnreadableFolder(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	for i := byte(1); i <= 3; i++ {
		writeMP3(t, filepath.Join(root, "a", fmt.Sprintf("%d.mp3", i)), fmt.Sprintf("A %d", i), i)
		writeMP3(t, filepath.Join(root, "b", fmt.Sprintf("%d.mp3", i)), fmt.Sprintf("B %d", i), 10+i)
	}
	scanAll(t, sc, lib, false)
	unreadableAt(sc, filepath.Join(root, "b"))
	if r := scanAll(t, sc, lib, false); r.Missing != 0 {
		t.Errorf("scan = %+v, want nothing under the unreadable folder marked missing", r)
	}
}

// TestScanKeepsAFileItCouldNotStat: an audio file the walk listed but could not stat
// (an I/O or permission error, not a missing file) is not reconciled as gone, and a scan
// that could read no file at all reconciles nothing.
func TestScanKeepsAFileItCouldNotStat(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = filepath.Join(root, fmt.Sprintf("%d.mp3", i))
		writeMP3(t, paths[i], fmt.Sprintf("Song %d", i), byte(i+1))
	}
	scanAll(t, sc, lib, false)
	sc.stat = func(p string) (fs.FileInfo, error) {
		if p == paths[0] {
			return nil, errors.New("input/output error")
		}
		return os.Stat(p)
	}
	if r := scanAll(t, sc, lib, false); r.Errored != 1 || r.Missing != 0 {
		t.Errorf("scan = %+v, want the unreadable file errored and nothing marked missing", r)
	}

	if err := os.Remove(paths[3]); err != nil {
		t.Fatal(err)
	}
	sc.stat = func(string) (fs.FileInfo, error) { return nil, errors.New("stale file handle") }
	if r := scanAll(t, sc, lib, false); r.Errored != 3 || r.Missing != 0 {
		t.Errorf("scan = %+v, want every file errored and nothing reconciled", r)
	}
}

// fakeClock is a clock that moves a fixed step each time it is read.
type fakeClock struct {
	now  time.Time
	step time.Duration
}

func (c *fakeClock) read() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

type beat struct {
	progress float64
	msg      string
	at       time.Time
}

// recordBeats scans with a heartbeat that records each call and the clock's reading.
func recordBeats(t *testing.T, sc *Scanner, clock *fakeClock, req Request) ([]beat, *Result) {
	t.Helper()
	sc.now = clock.read
	var beats []beat
	res, err := sc.Scan(context.Background(), req, func(p float64, msg string) error {
		beats = append(beats, beat{p, msg, clock.now})
		return nil
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertScanPartition(t, res)
	return beats, res
}

// TestScanProgressRisesToOne: the heartbeat reports a fraction of the files to visit,
// rising and inside (0,1) until the closing 1, on a first scan as on a rescan. The
// cadence counts every file seen, so a 50th entry that is not audio still beats.
func TestScanProgressRisesToOne(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	for i := range 120 {
		writeMP3(t, filepath.Join(root, fmt.Sprintf("a%03d.mp3", i)), fmt.Sprintf("T%d", i), byte(i))
	}
	if err := os.WriteFile(filepath.Join(root, "a048.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	for pass, force := range []bool{false, true} {
		beats, _ := recordBeats(t, sc, &fakeClock{step: time.Second}, Request{Library: lib, Force: force})
		if len(beats) < 3 {
			t.Fatalf("pass %d: beats = %+v, want several", pass, beats)
		}
		last := beats[len(beats)-1]
		if last.progress != 1 {
			t.Errorf("pass %d: final progress = %v, want 1", pass, last.progress)
		}
		prev := 0.0
		for _, b := range beats[:len(beats)-1] {
			if b.progress <= prev || b.progress >= 1 {
				t.Errorf("pass %d: progress %v after %v, want rising inside (0,1): %+v", pass, b.progress, prev, beats)
			}
			prev = b.progress
		}
		if beats[0].msg != "scanned 50 files" {
			t.Errorf("pass %d: first beat = %q, want one at the 50th file, a sidecar", pass, beats[0].msg)
		}
	}
}

// TestScanHeartbeatsAreSpaced: however fast files go by, beats come at least 250 ms
// apart, so a fast scan does not write the job row hundreds of times a second.
func TestScanHeartbeatsAreSpaced(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	for i := range 300 {
		writeMP3(t, filepath.Join(root, fmt.Sprintf("b%03d.mp3", i)), fmt.Sprintf("B%d", i), byte(i))
	}
	beats, _ := recordBeats(t, sc, &fakeClock{step: 100 * time.Millisecond}, Request{Library: lib})
	if len(beats) < 2 {
		t.Fatalf("beats = %+v, want several", beats)
	}
	for i := 1; i < len(beats)-1; i++ {
		if gap := beats[i].at.Sub(beats[i-1].at); gap < 250*time.Millisecond {
			t.Errorf("beats %d and %d are %v apart, want at least 250ms", i-1, i, gap)
		}
	}
}

// TestScanReadsAPromotedCopyNoOneReRead: a copy promoted by a write whose follow-up read
// never ran is read in full by the next plain scan, so its item takes the copy's tags.
func TestScanReadsAPromotedCopyNoOneReRead(t *testing.T) {
	ctx := context.Background()
	st, lib, sc, _, root := fastPathFixture(t)
	original := filepath.Join(root, "a", "1.mp3")
	writeMP3(t, original, "Original", 9)
	writeMP3(t, filepath.Join(root, "b", "1.mp3"), "Copy Title", 9)
	scanAll(t, sc, lib, false)
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 || items[0].Title != "Original" {
		t.Fatalf("items = %+v (err %v), want the one Original", items, err)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DetachFile(ctx, items[0].FilePID); err != nil {
		t.Fatal(err)
	}
	r := scanAll(t, sc, lib, false)
	it, err := st.ItemByPID(ctx, items[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Copy Title" || r.ItemsUpdated != 1 {
		t.Errorf("after a plain scan the item is %q (scan %+v), want the promoted copy read as Copy Title", it.Title, r)
	}
}

// TestScanCountsPromotionsAndDrops: reconciliation's promotions and dropped rows are
// counted beside the walk's outcomes.
func TestScanCountsPromotionsAndDrops(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	primary, gone := filepath.Join(root, "a", "1.mp3"), filepath.Join(root, "c", "1.mp3")
	writeMP3(t, primary, "Original", 9)
	writeMP3(t, filepath.Join(root, "b", "1.mp3"), "Copy", 9)
	writeMP3(t, gone, "Another Copy", 9)
	for i := byte(1); i <= 3; i++ {
		writeMP3(t, filepath.Join(root, "other", fmt.Sprintf("%d.mp3", i)), fmt.Sprintf("Other %d", i), i)
	}
	scanAll(t, sc, lib, false)
	for _, p := range []string{primary, gone} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if r := scanAll(t, sc, lib, false); r.Promoted != 1 || r.Dropped != 1 || r.Missing != 0 {
		t.Errorf("scan = %+v, want one copy promoted and one gone copy dropped", r)
	}
}

// TestScanCountsOnlyNewCopies: a backup folder moved as a whole relinks its copies, which
// were already attached and count as unchanged rather than as new copies.
func TestScanCountsOnlyNewCopies(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "a", "1.mp3"), "One", 9)
	writeMP3(t, filepath.Join(root, "backup", "1.mp3"), "One Again", 9)
	if r := scanAll(t, sc, lib, false); r.Copies != 1 {
		t.Fatalf("first scan = %+v, want one copy", r)
	}
	if err := os.Rename(filepath.Join(root, "backup"), filepath.Join(root, "old backup")); err != nil {
		t.Fatal(err)
	}
	if r := scanAll(t, sc, lib, false); r.Copies != 0 || r.Relinked != 1 || r.Unchanged != 2 {
		t.Errorf("scan after the move = %+v, want the moved copy relinked and unchanged", r)
	}
}

// TestScanOfARemovedRootReconcilesIt: a library root that no longer exists is a real
// removal, not an unreadable folder, so its items are marked missing.
func TestScanOfARemovedRootReconcilesIt(t *testing.T) {
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, "1.mp3"), "One", 1)
	writeMP3(t, filepath.Join(root, "2.mp3"), "Two", 2)
	scanAll(t, sc, lib, false)
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	res, err := sc.Scan(context.Background(), Request{Library: lib}, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.Missing != 2 {
		t.Errorf("scan = %+v, want both items missing", res)
	}
}
