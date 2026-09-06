package organize

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/store/sqlite"
)

func TestMarkCollisions(t *testing.T) {
	plan := &Plan{Actions: []Action{
		{Src: "/in/1.mp3", Dst: "/lib/A/Al/01 - X.mp3"},
		{Src: "/in/2.mp3", Dst: "/lib/A/Al/01 - X.mp3"}, // identical destination
		{Src: "/in/3.mp3", Dst: "/lib/A/Al/01 - x.mp3"}, // differs only by case
		{Src: "/in/4.mp3", Dst: "/lib/A/Al/02 - Y.mp3"}, // distinct
	}}
	markCollisions(plan)

	if plan.Actions[0].Skip {
		t.Fatal("first claimant of a destination should still move")
	}
	if !plan.Actions[1].Skip || plan.Actions[1].Reason == "" {
		t.Fatalf("exact destination collision should be skipped with a reason: %+v", plan.Actions[1])
	}
	if !plan.Actions[2].Skip {
		t.Fatal("case-only collision should be skipped (case-insensitive filesystems)")
	}
	if plan.Actions[3].Skip {
		t.Fatal("a distinct destination should not be flagged")
	}
	if plan.Pending() != 2 {
		t.Fatalf("pending = %d, want 2", plan.Pending())
	}
}

func TestSidecarMovesEnumeration(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	dstDir := filepath.Join(dir, "Artist", "Album")
	mustMkdir(t, srcDir)
	for _, name := range []string{"song.mp3", "song.lrc", "song.jpg", "song.cue", "cover.jpg", "notes.md"} {
		mustWrite(t, filepath.Join(srcDir, name))
	}

	moves := SidecarMoves(filepath.Join(srcDir, "song.mp3"), filepath.Join(dstDir, "01 - Song.mp3"))
	got := make([]string, 0, len(moves))
	for _, m := range moves {
		got = append(got, filepath.Base(m.Src)+" -> "+filepath.Base(m.Dst))
	}
	sort.Strings(got)
	// The directory's own cover.jpg is not here: it belongs to the directory rather than
	// to this file, and CoverMoves plans it over the whole batch.
	want := []string{
		"song.cue -> 01 - Song.cue", // same-basename companion, renamed
		"song.jpg -> 01 - Song.jpg", // per-track art, renamed
		"song.lrc -> 01 - Song.lrc", // lyrics, renamed
	}
	if len(got) != len(want) {
		t.Fatalf("sidecar moves = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sidecar moves = %v, want %v", got, want)
		}
	}
	// notes.md is not a recognized sidecar and must be left behind.
	for _, m := range moves {
		if filepath.Base(m.Src) == "notes.md" {
			t.Fatal("unrecognized file should not be swept up as a sidecar")
		}
	}
}

func TestMoveSidecarOnDiskAndCollision(t *testing.T) {
	o := New(nil, nil, nil)
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	dstDir := filepath.Join(dir, "dst")
	mustMkdir(t, srcDir)
	mustWrite(t, filepath.Join(srcDir, "song.lrc"))
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	// A cover already exists at the destination, so that one must be left in place.
	mustMkdir(t, dstDir)
	mustWrite(t, filepath.Join(dstDir, "cover.jpg"))

	moved := o.moveSidecars(filepath.Join(srcDir, "song.mp3"), filepath.Join(dstDir, "01 - Song.mp3"))
	if moved != 1 {
		t.Fatalf("moved %d sidecars, want 1 (lrc moved, conflicting cover left)", moved)
	}
	if _, err := os.Stat(filepath.Join(dstDir, "01 - Song.lrc")); err != nil {
		t.Fatalf("lyrics sidecar not at destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srcDir, "song.lrc")); !os.IsNotExist(err) {
		t.Fatal("lyrics sidecar still at source after move")
	}
	if _, err := os.Stat(filepath.Join(srcDir, "cover.jpg")); err != nil {
		t.Fatal("conflicting cover should be left at source, not lost")
	}
}

// TestExecuteSplitsADirectoryCoverAcrossDestinations is the batch planner at the level
// it runs: a plan routing one directory's tracks to two destinations leaves a cover in
// each, and both are counted.
func TestExecuteSplitsADirectoryCoverAcrossDestinations(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	dstA, dstB := filepath.Join(dir, "A"), filepath.Join(dir, "B")
	mustMkdir(t, srcDir)
	for _, name := range []string{"one.mp3", "two.mp3", "cover.jpg"} {
		mustWrite(t, filepath.Join(srcDir, name))
	}

	o := New(nil, nil, nil)
	plan := &Plan{Actions: []Action{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstA, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstB, "01 - Two.mp3")},
	}}
	// apply needs a store, so the moves are made here and Execute is driven over a plan
	// whose actions are already applied; what is under test is the cover pass after the
	// loop, which reads the directory as it now is.
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if err := fsx.Move(a.Src, a.Dst); err != nil {
			t.Fatalf("stage the move: %v", err)
		}
	}
	moved := []SidecarMove{
		{Src: plan.Actions[0].Src, Dst: plan.Actions[0].Dst},
		{Src: plan.Actions[1].Src, Dst: plan.Actions[1].Dst},
	}
	if n := o.applyCoverMoves(CoverMoves(moved, scan.IsAudio)); n != 2 {
		t.Fatalf("covers placed = %d, want one per destination", n)
	}
	for _, d := range []string{dstA, dstB} {
		if _, err := os.Stat(filepath.Join(d, "cover.jpg")); err != nil {
			t.Errorf("%s has no cover: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(srcDir, "cover.jpg")); !os.IsNotExist(err) {
		t.Error("the emptied source directory is still holding its cover")
	}
}

// TestExecuteCarriesCoversWhenCancelled: the audio of the first action has already
// moved by the time the cancel is seen on the second, so the pass that carries the
// directory covers has to run on that exit too, or the emptied directory keeps a cover
// with nothing to hold it. The cancel fires from the heartbeat, between the two
// actions, so nothing here races.
func TestExecuteCarriesCoversWhenCancelled(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path: filepath.Join(t.TempDir(), "c.db"), Owner: "test",
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	dstA, dstB := filepath.Join(root, "A"), filepath.Join(root, "B")
	mustMkdir(t, srcDir)
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte(root), DisplayRoot: root, Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}

	actions := make([]Action, 0, 2)
	for i, dst := range []string{filepath.Join(dstA, "01 - One.mp3"), filepath.Join(dstB, "01 - Two.mp3")} {
		name := []string{"one.mp3", "two.mp3"}[i]
		src := filepath.Join(srcDir, name)
		mustWrite(t, src)
		res, err := st.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File: model.File{
				Path: []byte(src), DisplayPath: src, RelPath: []byte(filepath.Join("src", name)),
				Kind: model.FileAudio, Size: 1, MTimeNS: 1, DurationMS: 1000,
				ContentHash: "c-" + name, EssenceHash: "e-" + name, ScanState: model.ScanIndexed,
			},
			Item: model.PlayableItem{
				Kind: model.KindTrack, State: model.StatePresent, Title: name,
				SortKey: model.SortKey(name), IdentityKey: "essence:e-" + name,
			},
			Track: model.Track{Artist: "A", AlbumArtist: "A", Album: "Al", TrackNo: i + 1},
		})
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		actions = append(actions, Action{
			FilePID: res.FilePID, ItemPID: res.ItemPID,
			Src: src, SrcBytes: []byte(src), Dst: dst,
		})
	}

	runCtx, cancel := context.WithCancel(ctx)
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rep, err := o.Execute(runCtx, &Plan{Actions: actions}, "",
		func(float64, string) error {
			cancel() // after the first action, so the second sees a canceled context
			return nil
		})
	if err == nil {
		t.Fatal("Execute returned no error on a canceled run")
	}
	if rep.Moved != 1 {
		t.Fatalf("moved = %d, want the one action before the cancel", rep.Moved)
	}
	if _, serr := os.Stat(filepath.Join(dstA, "cover.jpg")); serr != nil {
		t.Errorf("the cancel stranded the cover: %v", serr)
	}
	// A copy rather than a move, and the right one: the second track never left, so the
	// planner reads a directory that still holds audio and leaves its cover in place.
	if _, serr := os.Stat(filepath.Join(srcDir, "cover.jpg")); serr != nil {
		t.Errorf("the source still holds audio but lost its cover: %v", serr)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
