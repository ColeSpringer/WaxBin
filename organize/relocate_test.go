package organize

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
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
	want := []string{
		"cover.jpg -> cover.jpg",    // directory art keeps its name
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
	o := New(nil, nil)
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
