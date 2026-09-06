package organize

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/scan"
)

// coverPlan runs the batch planner over one src/dst pair, the shape the old per-file
// tests used.
func coverPlan(srcAudio, dstAudio string) []CoverMove {
	return CoverMoves([]SidecarMove{{Src: srcAudio, Dst: dstAudio}}, scan.IsAudio)
}

// TestCoverMovesCarriesExoticCover confirms a directory cover in an exotic format
// (AVIF/HEIC), now recognized by the scanner, is carried with the album, not left
// behind in the old directory.
func TestCoverMovesCarriesExoticCover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "cover.avif"), []byte("avifdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "cover.webp"), []byte("webpdata"), 0o644); err != nil {
		t.Fatal(err)
	}

	moves := coverPlan(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"))

	found := map[string]bool{}
	for _, m := range moves {
		found[filepath.Base(m.Src)] = true
		if filepath.Dir(m.Dst) != dstDir {
			t.Errorf("cover dst dir = %q, want %q", filepath.Dir(m.Dst), dstDir)
		}
	}
	for _, name := range []string{"cover.avif", "cover.webp"} {
		if !found[name] {
			t.Errorf("%s not carried by organize (would be stranded in the old directory)", name)
		}
	}
}

// TestCoverMovesCarriesMixedCaseCover confirms a mixed-case cover filename (which the
// scanner matches case-insensitively) is also carried, not stranded on a case-sensitive
// filesystem.
func TestCoverMovesCarriesMixedCaseCover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Cover.JPG"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	moves := coverPlan(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"))
	found := false
	for _, m := range moves {
		if filepath.Base(m.Src) == "Cover.JPG" {
			found = true
			if filepath.Base(m.Dst) != "Cover.JPG" {
				t.Errorf("cover renamed on move: dst=%q, want Cover.JPG (keep name)", filepath.Base(m.Dst))
			}
		}
	}
	if !found {
		t.Error("mixed-case Cover.JPG not carried by organize (stranded on a case-sensitive fs)")
	}
}

// TestCoverMovesSkipsSameDir confirms directory art is not touched when the audio stays
// in the same directory (only same-basename companions move then).
func TestCoverMovesSkipsSameDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cover.avif"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if moves := coverPlan(filepath.Join(dir, "a.mp3"), filepath.Join(dir, "01 - a.mp3")); len(moves) != 0 {
		t.Errorf("planned %+v, want nothing (the cover stays for the other tracks)", moves)
	}
}

// TestSidecarMovesLeavesDirectoryArtAlone: the per-file enumeration plans a file's own
// companions and nothing else, so the directory's cover is the batch planner's job.
func TestSidecarMovesLeavesDirectoryArtAlone(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	for _, name := range []string{"track.lrc", "cover.jpg"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	moves := SidecarMoves(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"))
	if len(moves) != 1 || filepath.Base(moves[0].Src) != "track.lrc" {
		t.Errorf("sidecar moves = %+v, want the same-basename lyrics alone", moves)
	}
}

// applyPlan runs a plan through the same helper Execute uses, so a test asserts on
// what actually lands on disk rather than on the plan alone.
func applyPlan(t *testing.T, moves []CoverMove) {
	t.Helper()
	for _, m := range moves {
		if err := fsx.MoveOrCopy(m.Src, m.Dst, m.Copy); err != nil {
			t.Fatalf("apply %+v: %v", m, err)
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TestCoverMovesSplitsAcrossDestinations is the gap this planner closes: a compilation
// split per artist used to leave every destination but the first bare.
func TestCoverMovesSplitsAcrossDestinations(t *testing.T) {
	srcDir, dstA, dstB := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	// The audio has already moved by the time the planner runs, so the source directory
	// holds only the cover.
	mustWrite(t, filepath.Join(dstA, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstB, "01 - Two.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstA, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstB, "01 - Two.mp3")},
	}, scan.IsAudio)
	if len(moves) != 2 {
		t.Fatalf("planned %+v, want one entry per destination", moves)
	}
	if !moves[0].Copy || moves[1].Copy {
		t.Errorf("planned %+v, want a copy then a move (the directory is emptied)", moves)
	}
	applyPlan(t, moves)

	for _, dir := range []string{dstA, dstB} {
		if !exists(filepath.Join(dir, "cover.jpg")) {
			t.Errorf("%s has no cover", dir)
		}
	}
	if exists(filepath.Join(srcDir, "cover.jpg")) {
		t.Error("the emptied source directory is still holding its cover")
	}
}

// TestCoverMovesKeepsTheCoverWhenAudioStays: one track of three leaves, so the
// destination takes a copy and the tracks left behind keep their picture.
func TestCoverMovesKeepsTheCoverWhenAudioStays(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(srcDir, "two.mp3"))
	mustWrite(t, filepath.Join(srcDir, "three.mp3"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
	}, scan.IsAudio)
	if len(moves) != 1 || !moves[0].Copy {
		t.Fatalf("planned %+v, want one copy", moves)
	}
	applyPlan(t, moves)
	if !exists(filepath.Join(srcDir, "cover.jpg")) {
		t.Error("the source directory still holds audio but lost its cover")
	}
	if !exists(filepath.Join(dstDir, "cover.jpg")) {
		t.Error("the destination has no cover")
	}
}

// TestCoverMovesMovesWhenEmptiedToOnePlace is the previous behaviour, unchanged: a whole
// album relocating carries its cover rather than leaving an orphan behind.
func TestCoverMovesMovesWhenEmptiedToOnePlace(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstDir, "02 - Two.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstDir, "02 - Two.mp3")},
	}, scan.IsAudio)
	if len(moves) != 1 || moves[0].Copy {
		t.Fatalf("planned %+v, want one move", moves)
	}
	applyPlan(t, moves)
	if exists(filepath.Join(srcDir, "cover.jpg")) {
		t.Error("the emptied source directory is still holding its cover")
	}
}

// TestCoverMovesSkipsAnExistingDestinationCover: a destination that already has one
// keeps it rather than being overwritten or reported as a failure.
func TestCoverMovesSkipsAnExistingDestinationCover(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
	}, scan.IsAudio)
	if len(moves) != 0 {
		t.Errorf("planned %+v, want nothing", moves)
	}
}

// TestCoverMovesLeavesTheSecondCoverOnAMerge: two directories emptied into one leave the
// second cover where it was, since the first one's already sits at the destination.
// Choosing between two covers is not this planner's call.
func TestCoverMovesLeavesTheSecondCoverOnAMerge(t *testing.T) {
	dir := t.TempDir()
	srcA, srcB, dstDir := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "dst")
	for _, d := range []string{srcA, srcB, dstDir} {
		mustMkdir(t, d)
	}
	mustWrite(t, filepath.Join(srcA, "cover.jpg"))
	mustWrite(t, filepath.Join(srcB, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstDir, "02 - Two.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcA, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
		{Src: filepath.Join(srcB, "two.mp3"), Dst: filepath.Join(dstDir, "02 - Two.mp3")},
	}, scan.IsAudio)
	if len(moves) != 1 || moves[0].Src != filepath.Join(srcA, "cover.jpg") {
		t.Fatalf("planned %+v, want the first directory's cover alone", moves)
	}
	applyPlan(t, moves)
	if !exists(filepath.Join(srcB, "cover.jpg")) {
		t.Error("the second cover was taken; it has nowhere to land and must stay put")
	}
}
