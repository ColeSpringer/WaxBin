package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// twoTrackRipCue is a single-file album rip's cue: album header plus two TRACKs,
// the first with its own performer, the second inheriting the album performer. The
// second starts at 00:00:05 (5 cue frames = 66 ms), well within the fixture's audio.
const twoTrackRipCue = `PERFORMER "Album Performer"
TITLE "The Album"
REM GENRE "Jazz"
FILE "album.mp3" MP3
  TRACK 01 AUDIO
    TITLE "First"
    PERFORMER "Alice"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Second"
    INDEX 01 00:00:05
`

func writeCue(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itemsByTrack(t *testing.T, st *sqlite.Store) []*model.ItemView {
	t.Helper()
	items, err := st.QueryItems(context.Background(), query.New(query.EntityItems).OrderBy("track", false).Build(), "")
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	return items
}

func assertScanConsistent(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.Consistent() {
		t.Fatalf("db verify not clean: %+v", rep)
	}
}

// TestScanVirtualTracksFromCue: a non-book single file with a multi-track .cue is
// carved into browseable virtual tracks with offset-derived durations, sharing one
// backing file whose tags are guarded against a per-track write-back.
func TestScanVirtualTracksFromCue(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()

	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole File", "Tagged Artist", "Tagged Album", 1, testaudio.AudioWithSeed(9)))
	writeCue(t, filepath.Join(root, "album.cue"), twoTrackRipCue)

	if r := scanAll(t, sc, lib, false); r.AudioFiles != 1 {
		t.Fatalf("AudioFiles = %d, want 1", r.AudioFiles)
	}

	items := itemsByTrack(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(items))
	}
	t1, t2 := items[0], items[1]
	if !t1.Virtual || t1.StartFrames != 0 || t1.EndFrames != 5 || t1.DurationMS != 66 {
		t.Errorf("track 1 = virtual %v [%d,%d) frames dur %d, want virtual [0,5) dur 66",
			t1.Virtual, t1.StartFrames, t1.EndFrames, t1.DurationMS)
	}
	if t1.Title != "First" || t1.Artist != "Alice" {
		t.Errorf("track 1 = %q by %q, want First by Alice", t1.Title, t1.Artist)
	}
	// Album-level cue header wins over the file's own tags.
	if t1.Album != "The Album" || t1.AlbumArtist != "Album Performer" {
		t.Errorf("track 1 album = %q / %q, want The Album / Album Performer", t1.Album, t1.AlbumArtist)
	}
	// Track 2 has no performer of its own, so it inherits the album performer, and it
	// runs to the end of the file.
	if !t2.Virtual || t2.StartFrames != 5 || t2.StartMS != 66 || t2.Artist != "Album Performer" {
		t.Errorf("track 2 = virtual %v start %d frames / %d ms by %q, want virtual start 5 / 66 by Album Performer",
			t2.Virtual, t2.StartFrames, t2.StartMS, t2.Artist)
	}
	f, err := st.FileByPID(ctx, t2.FilePID)
	if err != nil {
		t.Fatalf("file by pid: %v", err)
	}
	if f.DurationMS <= 66 {
		t.Fatalf("fixture file duration %d ms too short for the test", f.DurationMS)
	}
	// The final track's end is left OPEN rather than copying the file's probed
	// duration: ms->frames is the lossy direction and must not exist. The duration
	// still reads back as the remainder of the file, through the COALESCE.
	if t2.EndFrames != 0 || t2.EndMS != 0 {
		t.Errorf("track 2 end = %d frames / %d ms, want 0 (the final track's end stays open)",
			t2.EndFrames, t2.EndMS)
	}
	if t2.DurationMS != f.DurationMS-66 {
		t.Errorf("track 2 dur = %d, want %d (the file's duration less the 5-frame start)",
			t2.DurationMS, f.DurationMS-66)
	}
	if t1.FilePID != t2.FilePID {
		t.Fatalf("virtual tracks must share one file: %s vs %s", t1.FilePID, t2.FilePID)
	}

	shared, err := st.FileSharedOrVirtual(ctx, t1.FilePID)
	if err != nil || !shared {
		t.Fatalf("FileSharedOrVirtual = %v (err %v), want true for a virtual-track file", shared, err)
	}
	assertScanConsistent(t, st)
}

// TestScanVirtualTracksDropsUnindexedTrack: a cue TRACK whose INDEX 01 will not parse
// is dropped with a diagnostic rather than carved. Anchoring it at 0 would serve the
// head of the album under its name, because a virtual track's start is its content
// identity, not a seek hint.
func TestScanVirtualTracksDropsUnindexedTrack(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()

	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole", "A", "Al", 1, testaudio.AudioWithSeed(17)))
	// TRACK 02's INDEX names 90 seconds, which MM:SS:FF cannot spell.
	writeCue(t, filepath.Join(root, "album.cue"), "TITLE \"The Album\"\nFILE \"album.mp3\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Fine\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Broken\"\n    INDEX 01 00:90:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Also Fine\"\n    INDEX 01 00:00:10\n")
	scanAll(t, sc, lib, false)

	items := itemsByTrack(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2 (the unindexed TRACK 02 is dropped)", len(items))
	}
	for _, it := range items {
		if it.Title == "Broken" {
			t.Fatalf("the unindexed track was carved anyway, at frame %d", it.StartFrames)
		}
	}
	// The survivors keep their own starts; nothing is anchored at 0 in TRACK 02's place.
	if items[0].Title != "Fine" || items[0].StartFrames != 0 {
		t.Errorf("track 1 = %q at frame %d, want Fine at 0", items[0].Title, items[0].StartFrames)
	}
	if items[1].Title != "Also Fine" || items[1].StartFrames != 10 {
		t.Errorf("track 2 = %q at frame %d, want Also Fine at 10", items[1].Title, items[1].StartFrames)
	}

	// The drop is visible, not silent.
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("file diagnostics: %v", err)
	}
	found := 0
	for _, d := range ds {
		if d.Code == model.DiagCueTrackDropped {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("cue_track_dropped diagnostics = %d, want 1: %+v", found, ds)
	}
}

// TestScanCueWithNoUsableTracksKeepsTheFile: a sheet whose every INDEX is malformed
// names no windows, so the file is not a rip and must stay an ordinary whole-file
// track. Carving it anyway committed the file with an empty track set: the pre-cue
// track was detached and deleted, nothing replaced it, and the audio became
// unreachable in the catalog.
func TestScanCueWithNoUsableTracksKeepsTheFile(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	cuePath := filepath.Join(root, "album.cue")
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole File", "A", "Al", 1, testaudio.AudioWithSeed(21)))

	scanAll(t, sc, lib, false)
	before := itemsByTrack(t, st)
	if len(before) != 1 || before[0].Virtual {
		t.Fatalf("first scan should yield one whole-file track, got %d", len(before))
	}

	// Every INDEX names 99 seconds, which MM:SS:FF cannot spell.
	writeCue(t, cuePath, "TITLE \"X\"\nFILE \"album.mp3\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"A\"\n    INDEX 01 00:99:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"B\"\n    INDEX 01 00:99:01\n")
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(cuePath, future, future)
	scanAll(t, sc, lib, false)

	after := itemsByTrack(t, st)
	if len(after) != 1 {
		t.Fatalf("items = %d, want the whole-file track to survive an unusable cue", len(after))
	}
	if after[0].Virtual {
		t.Error("a sheet that named no usable window must not produce a virtual track")
	}
	// The drop is reported even though the file never reached the rip path, and both
	// tracks are named in the one row file_diagnostic's key allows per code.
	ds, err := st.FileDiagnostics(ctx, model.DiagnosticFilter{})
	if err != nil {
		t.Fatalf("file diagnostics: %v", err)
	}
	var got []model.FileDiagnostic
	for _, d := range ds {
		if d.Code == model.DiagCueTrackDropped {
			got = append(got, d)
		}
	}
	if len(got) != 1 {
		t.Fatalf("cue_track_dropped diagnostics = %d, want exactly 1 summary row "+
			"(the code is part of file_diagnostic's primary key, so extras would collide "+
			"and vanish): %+v", len(got), got)
	}
	if !strings.Contains(got[0].Detail, "TRACK 01") || !strings.Contains(got[0].Detail, "TRACK 02") {
		t.Errorf("summary names only some of the dropped tracks: %q", got[0].Detail)
	}
	assertScanConsistent(t, st)
}

// TestScanCueWithOneUsableTrackStaysWholeFile: one window over a whole file is just
// the file. Counting the malformed track toward the >= 2 rip gate carved a single
// virtual track spanning everything, which is strictly worse than a plain track: its
// tags become unwritable and it exports no fingerprint.
func TestScanCueWithOneUsableTrackStaysWholeFile(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole File", "A", "Al", 1, testaudio.AudioWithSeed(22)))
	writeCue(t, filepath.Join(root, "album.cue"), "TITLE \"X\"\nFILE \"album.mp3\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Good\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Bad\"\n    INDEX 01 00:99:00\n")
	scanAll(t, sc, lib, false)

	items := itemsByTrack(t, st)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (one usable cue track is not a rip)", len(items))
	}
	if items[0].Virtual {
		t.Errorf("item = %q virtual, want a plain whole-file track", items[0].Title)
	}
	shared, err := st.FileSharedOrVirtual(ctx, items[0].FilePID)
	if err != nil {
		t.Fatalf("FileSharedOrVirtual: %v", err)
	}
	if shared {
		t.Error("a whole-file track's tags must stay writable; carving it as a virtual " +
			"track would refuse every tag write-back")
	}
	assertScanConsistent(t, st)
}

// TestScanCueDropsEmptyWindow: two TRACKs on the same frame leave the first holding
// nothing. It cannot be stored, because an end of 0 is the sentinel for "runs to the
// end of the file", so an empty window would read back as the whole album under the
// first track's name. That is the failure the frame window exists to prevent.
func TestScanCueDropsEmptyWindow(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole", "A", "Al", 1, testaudio.AudioWithSeed(23)))
	writeCue(t, filepath.Join(root, "album.cue"), "TITLE \"X\"\nFILE \"album.mp3\" WAVE\n"+
		"  TRACK 01 AUDIO\n    TITLE \"Empty\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 02 AUDIO\n    TITLE \"Real\"\n    INDEX 01 00:00:00\n"+
		"  TRACK 03 AUDIO\n    TITLE \"Last\"\n    INDEX 01 00:00:10\n")
	scanAll(t, sc, lib, false)

	items := itemsByTrack(t, st)
	if len(items) != 2 {
		t.Fatalf("virtual tracks = %d, want 2 (the empty leading window is dropped): %+v",
			len(items), items)
	}
	for _, it := range items {
		if it.Title == "Empty" {
			t.Fatalf("the empty track was stored as [%d,%d), which reads as the whole file",
				it.StartFrames, it.EndFrames)
		}
		if it.EndFrames == 0 && it.Title != "Last" {
			t.Errorf("%q stored an open end; only the final track runs to the end", it.Title)
		}
	}
	assertScanConsistent(t, st)
}

// TestScanVirtualTracksRescanIdempotent: an unchanged rip fast-paths (no re-parse,
// no reshaping of the set).
func TestScanVirtualTracksRescanIdempotent(t *testing.T) {
	st, lib, sc, cr, root := fastPathFixture(t)
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole", "A", "Al", 1, testaudio.AudioWithSeed(11)))
	writeCue(t, filepath.Join(root, "album.cue"), twoTrackRipCue)
	scanAll(t, sc, lib, false)
	before := itemsByTrack(t, st)
	if len(before) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(before))
	}

	readsBefore := cr.reads
	r := scanAll(t, sc, lib, false)
	if cr.reads != readsBefore {
		t.Errorf("unchanged rescan parsed %d files, want 0 (fast-path)", cr.reads-readsBefore)
	}
	if r.Unchanged != 1 {
		t.Fatalf("unchanged rescan Unchanged = %d, want 1", r.Unchanged)
	}
	after := itemsByTrack(t, st)
	if len(after) != 2 || after[0].PID != before[0].PID || after[1].PID != before[1].PID {
		t.Fatalf("no-op rescan reshaped the virtual-track set")
	}
}

// TestScanVirtualTracksCueEditReconciles: a changed .cue over unchanged audio routes
// to the full path and reconciles the set (a new track appears, the originals keep
// their identity).
func TestScanVirtualTracksCueEditReconciles(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	cuePath := filepath.Join(root, "album.cue")
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole", "A", "Al", 1, testaudio.AudioWithSeed(12)))
	writeCue(t, cuePath, twoTrackRipCue)
	scanAll(t, sc, lib, false)
	before := itemsByTrack(t, st)
	if len(before) != 2 {
		t.Fatalf("virtual tracks = %d, want 2", len(before))
	}

	// Add a third track and bump the cue's mtime; the audio is untouched.
	writeCue(t, cuePath, twoTrackRipCue+"  TRACK 03 AUDIO\n    TITLE \"Third\"\n    INDEX 01 00:00:10\n")
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(cuePath, future, future)

	r := scanAll(t, sc, lib, false)
	if r.Unchanged != 0 {
		t.Fatalf("a changed .cue on a rip must route to the full path, got Unchanged=%d", r.Unchanged)
	}
	after := itemsByTrack(t, st)
	if len(after) != 3 {
		t.Fatalf("virtual tracks after cue edit = %d, want 3", len(after))
	}
	if after[0].PID != before[0].PID || after[1].PID != before[1].PID {
		t.Errorf("cue edit forked the existing virtual tracks")
	}
	assertScanConsistent(t, st)
}

// TestScanVirtualTracksLeadingRemovalCounts: removing the LEADING cue track is the one
// case where no survivor's window shifts, and it must still register as a change so the
// scan summary and watch-mode schedulers see it, not just the change_log.
func TestScanVirtualTracksLeadingRemovalCounts(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	cuePath := filepath.Join(root, "album.cue")
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole", "A", "Al", 1, testaudio.AudioWithSeed(16)))

	three := "TITLE \"The Album\"\nFILE \"album.mp3\" WAVE\n" +
		"  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n" +
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:00:05\n" +
		"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:00:10\n"
	writeCue(t, cuePath, three)
	scanAll(t, sc, lib, false)
	before := itemsByTrack(t, st)
	if len(before) != 3 {
		t.Fatalf("want 3 virtual tracks, got %d", len(before))
	}

	// Drop TRACK 01 only; TRACK 02 and 03 keep their numbers, starts, and (since their
	// neighbor 03 is untouched) their end offsets, so the survivors are byte-identical.
	removedFirst := "TITLE \"The Album\"\nFILE \"album.mp3\" WAVE\n" +
		"  TRACK 02 AUDIO\n    TITLE \"Two\"\n    INDEX 01 00:00:05\n" +
		"  TRACK 03 AUDIO\n    TITLE \"Three\"\n    INDEX 01 00:00:10\n"
	writeCue(t, cuePath, removedFirst)
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(cuePath, future, future)

	r := scanAll(t, sc, lib, false)
	after := itemsByTrack(t, st)
	if len(after) != 2 {
		t.Fatalf("after leading removal, want 2 tracks, got %d", len(after))
	}
	if after[0].PID != before[1].PID || after[1].PID != before[2].PID {
		t.Errorf("leading removal forked the surviving tracks")
	}
	// The deletion is a real change even though no survivor's bytes moved.
	if r.SidecarsUpdated == 0 && r.ItemsUpdated == 0 && r.ItemsCreated == 0 {
		t.Fatalf("leading-track removal reported no change to the scanner: %+v", r)
	}
	assertScanConsistent(t, st)
}

// TestScanBookCueStaysChapters: a book with a multi-track .cue keeps the chapter
// path, producing one book item rather than virtual tracks.
func TestScanBookCueStaysChapters(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()

	// The .m4b extension classifies it as a book even over MP3 bytes.
	writeMP3Raw(t, filepath.Join(root, "book.m4b"), testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
		Title: "T", Artist: "Auth", AlbumArtist: "Auth", Album: "A Book", Audio: testaudio.AudioWithSeed(15),
	}))
	writeCue(t, filepath.Join(root, "book.cue"), twoTrackRipCue)
	scanAll(t, sc, lib, false)

	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 || items[0].Kind != model.KindBook {
		t.Fatalf("a book with a cue must be one book item, got %d items", len(items))
	}
	if items[0].Virtual {
		t.Error("a book must not read back as a virtual track")
	}
	chs, err := st.Chapters(ctx, items[0].PID)
	if err != nil {
		t.Fatalf("chapters: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("book cue chapters = %d, want 2", len(chs))
	}
}

// TestScanCueAddedThenRemovedConverts: a whole-file track gains a cue (converting to
// virtual tracks), then loses it (reverting to one whole-file track).
func TestScanCueAddedThenRemovedConverts(t *testing.T) {
	st, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	cuePath := filepath.Join(root, "album.cue")
	writeMP3Raw(t, filepath.Join(root, "album.mp3"),
		testaudio.BuildMP3WithAudio("Whole File", "A", "Al", 1, testaudio.AudioWithSeed(13)))

	scanAll(t, sc, lib, false)
	plain := itemsByTrack(t, st)
	if len(plain) != 1 || plain[0].Virtual {
		t.Fatalf("first scan should yield one whole-file track, got %d (virtual %v)", len(plain), plain[0].Virtual)
	}
	plainPID := plain[0].PID

	// Add the cue: the whole-file track converts to two virtual tracks.
	writeCue(t, cuePath, twoTrackRipCue)
	scanAll(t, sc, lib, false)
	converted := itemsByTrack(t, st)
	if len(converted) != 2 {
		t.Fatalf("after adding a cue, want 2 virtual tracks, got %d", len(converted))
	}
	for _, it := range converted {
		if !it.Virtual {
			t.Errorf("item %s is not virtual after conversion", it.PID)
		}
	}
	if _, err := st.ItemByPID(ctx, plainPID); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("the whole-file track should be gone after conversion, got %v", err)
	}

	// Remove the cue: the file reverts to a single whole-file track.
	if err := os.Remove(cuePath); err != nil {
		t.Fatal(err)
	}
	scanAll(t, sc, lib, false)
	reverted := itemsByTrack(t, st)
	if len(reverted) != 1 {
		t.Fatalf("after removing the cue, want 1 whole-file track, got %d", len(reverted))
	}
	if reverted[0].Virtual {
		t.Error("reverted item should be a whole-file track, not virtual")
	}
	assertScanConsistent(t, st)
}
