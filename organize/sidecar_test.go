package organize

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/fsx"
)

// coverPlan runs the batch planner over one src/dst pair, the shape the old per-file
// tests used.
func coverPlan(srcAudio, dstAudio string) []CoverMove {
	return CoverMoves([]SidecarMove{{Src: srcAudio, Dst: dstAudio}}, PruneOptions(filepath.Dir(filepath.Dir(srcAudio)), nil))
}

// TestCoverMovesCarriesExoticCover confirms a directory cover in an exotic format
// (AVIF/HEIC), now recognized by the scanner, is carried with the album, not left
// behind in the old directory.
func TestCoverMovesCarriesExoticCover(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	for _, name := range []string{"track.lrc", "cover.jpg"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	moves := SidecarMoves(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"), nil)
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
	t.Parallel()
	srcDir, dstA, dstB := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	// The audio has already moved by the time the planner runs, so the source directory
	// holds only the cover.
	mustWrite(t, filepath.Join(dstA, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstB, "01 - Two.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstA, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstB, "01 - Two.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
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
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(srcDir, "two.mp3"))
	mustWrite(t, filepath.Join(srcDir, "three.mp3"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
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
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstDir, "02 - Two.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstDir, "02 - Two.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
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
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "cover.jpg"))
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))

	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
	if len(moves) != 0 {
		t.Errorf("planned %+v, want nothing", moves)
	}
}

// TestCoverMovesLeavesTheSecondCoverOnAMerge: two directories emptied into one leave the
// second cover where it was, since the first one's already sits at the destination.
// Choosing between two covers is not this planner's call.
func TestCoverMovesLeavesTheSecondCoverOnAMerge(t *testing.T) {
	t.Parallel()
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
	}, PruneOptions(dir, nil))
	if len(moves) != 1 || moves[0].Src != filepath.Join(srcA, "cover.jpg") {
		t.Fatalf("planned %+v, want the first directory's cover alone", moves)
	}
	applyPlan(t, moves)
	if !exists(filepath.Join(srcB, "cover.jpg")) {
		t.Error("the second cover was taken; it has nowhere to land and must stay put")
	}
}

// TestSidecarMovesMarksASidecarAnotherEncodingShares: lyrics named for a track that has
// another encoding left in the folder are shared with it, and the moved file's alone once
// that encoding is gone.
func TestSidecarMovesMarksASidecarAnotherEncodingShares(t *testing.T) {
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "Song.flac"))
	mustWrite(t, filepath.Join(srcDir, "Song.lrc"))
	mustWrite(t, filepath.Join(srcDir, "Other.mp3"))
	src, dst := filepath.Join(srcDir, "Song.mp3"), filepath.Join(dstDir, "01 - Song.mp3")
	if moves := SidecarMoves(src, dst, nil); len(moves) != 1 || !moves[0].Shared {
		t.Fatalf("moves = %+v, want the lyrics shared with Song.flac", moves)
	}
	if err := os.Rename(filepath.Join(srcDir, "Song.flac"), filepath.Join(srcDir, "Song.FLAC")); err != nil {
		t.Fatal(err)
	}
	if moves := SidecarMoves(src, dst, nil); len(moves) != 1 || !moves[0].Shared {
		t.Fatalf("moves = %+v, want the lyrics shared with Song.FLAC", moves)
	}
	if err := os.Remove(filepath.Join(srcDir, "Song.FLAC")); err != nil {
		t.Fatal(err)
	}
	if moves := SidecarMoves(src, dst, nil); len(moves) != 1 || moves[0].Shared {
		t.Fatalf("moves = %+v, want the lyrics the moved file's alone", moves)
	}
	// A file not moved yet is no sibling of itself.
	mustWrite(t, src)
	if moves := SidecarMoves(src, dst, nil); len(moves) != 1 || moves[0].Shared {
		t.Fatalf("moves = %+v, want the lyrics the file's alone while it is still in place", moves)
	}
}

// TestMoveSidecarsCopiesASharedSidecar: organize moving one encoding of a track leaves its
// lyrics for the encoding that stays and takes a copy along.
func TestMoveSidecarsCopiesASharedSidecar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	srcDir, dstDir := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	mustMkdir(t, srcDir)
	mustWrite(t, filepath.Join(srcDir, "Song.flac"))
	mustWrite(t, filepath.Join(srcDir, "Song.lrc"))
	o := New(nil, nil, nil)
	if n := o.moveSidecars(fsx.NewSpeller(dir, nil), filepath.Join(srcDir, "Song.mp3"), filepath.Join(dstDir, "01 - Song.mp3"), nil); n != 1 {
		t.Fatalf("carried %d sidecars, want 1", n)
	}
	if !exists(filepath.Join(srcDir, "Song.lrc")) || !exists(filepath.Join(dstDir, "01 - Song.lrc")) {
		t.Fatal("want the lyrics both beside the encoding that stayed and beside the moved one")
	}
}

// TestCoverMovesCarriesEveryCompanionToOnePlace: a folder whose audio all went to one
// place, left holding companions and junk, sends every companion after it.
func TestCoverMovesCarriesEveryCompanionToOnePlace(t *testing.T) {
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	for _, name := range []string{"Cover.jpg", "album.nfo", "booklet.pdf", "rip.log", ".DS_Store"} {
		mustWrite(t, filepath.Join(srcDir, name))
	}
	mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))
	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
	applyPlan(t, moves)
	for _, name := range []string{"Cover.jpg", "album.nfo", "booklet.pdf", "rip.log"} {
		if !exists(filepath.Join(dstDir, name)) || exists(filepath.Join(srcDir, name)) {
			t.Errorf("%s was not moved after the audio", name)
		}
	}
	if exists(filepath.Join(dstDir, ".DS_Store")) {
		t.Error("junk was carried")
	}
}

// TestCoverMovesLeavesASplitFolderItsCompanions: a folder whose tracks went two ways keeps
// its companions, its cover included, and each destination takes a copy of the cover.
func TestCoverMovesLeavesASplitFolderItsCompanions(t *testing.T) {
	t.Parallel()
	srcDir, dstA, dstB := t.TempDir(), t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "cover.jpg"))
	mustWrite(t, filepath.Join(srcDir, "album.nfo"))
	mustWrite(t, filepath.Join(dstA, "01 - One.mp3"))
	mustWrite(t, filepath.Join(dstB, "01 - Two.mp3"))
	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstA, "01 - One.mp3")},
		{Src: filepath.Join(srcDir, "two.mp3"), Dst: filepath.Join(dstB, "01 - Two.mp3")},
	}, PruneOptions(filepath.Dir(srcDir), nil))
	applyPlan(t, moves)
	for _, dir := range []string{dstA, dstB} {
		if !exists(filepath.Join(dir, "cover.jpg")) || exists(filepath.Join(dir, "album.nfo")) {
			t.Errorf("%s: want a copy of the cover and no nfo", dir)
		}
	}
	if !exists(filepath.Join(srcDir, "cover.jpg")) || !exists(filepath.Join(srcDir, "album.nfo")) {
		t.Error("the split folder lost a companion")
	}
}

// TestCoverMovesTakesNothingFromTheRoot: a loose file leaving the library root takes none
// of the root's files with it; they are no album's.
func TestCoverMovesTakesNothingFromTheRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dstDir := filepath.Join(root, "Artist", "Album")
	mustMkdir(t, dstDir)
	mustWrite(t, filepath.Join(root, "cover.jpg"))
	mustWrite(t, filepath.Join(root, "Favorites.m3u"))
	mustWrite(t, filepath.Join(dstDir, "01 - Loose.mp3"))
	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(root, "loose.mp3"), Dst: filepath.Join(dstDir, "01 - Loose.mp3")},
	}, PruneOptions(root, nil))
	if len(moves) != 0 {
		t.Fatalf("planned %+v, want nothing taken from the root", moves)
	}
}

// TestCoverMovesCopiesTheCoverOfAFolderThatStays: a folder whose audio all left but that
// keeps a subfolder, or a file no companion rule names, stays, so it keeps its companions
// and gives its audio's new folder a copy of the cover only.
func TestCoverMovesCopiesTheCoverOfAFolderThatStays(t *testing.T) {
	t.Parallel()
	for _, keeper := range []string{filepath.Join("Scans", "page1.jpg"), "notes.md", "concert.mkv"} {
		t.Run(filepath.Base(keeper), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			srcDir, dstDir := filepath.Join(root, "Album"), filepath.Join(root, "Artist", "Album")
			mustMkdir(t, filepath.Dir(filepath.Join(srcDir, keeper)))
			mustMkdir(t, dstDir)
			for _, name := range []string{keeper, "cover.jpg", "album.nfo"} {
				mustWrite(t, filepath.Join(srcDir, name))
			}
			mustWrite(t, filepath.Join(dstDir, "01 - One.mp3"))
			moves := CoverMoves([]SidecarMove{
				{Src: filepath.Join(srcDir, "one.mp3"), Dst: filepath.Join(dstDir, "01 - One.mp3")},
			}, PruneOptions(root, nil))
			if len(moves) != 1 || filepath.Base(moves[0].Src) != "cover.jpg" || !moves[0].Copy {
				t.Fatalf("planned %+v, want a copy of the cover alone", moves)
			}
		})
	}
}

// TestCoverMovesLeavesAnArtistFolderItsArt: a single leaving an artist folder that holds
// album folders takes a copy of the folder's cover and leaves the artist's picture.
func TestCoverMovesLeavesAnArtistFolderItsArt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	artist := filepath.Join(root, "Artist")
	single := filepath.Join(artist, "Single")
	mustMkdir(t, filepath.Join(artist, "Album"))
	mustMkdir(t, single)
	mustWrite(t, filepath.Join(artist, "Album", "01 - Song.mp3"))
	mustWrite(t, filepath.Join(artist, "artist.jpg"))
	mustWrite(t, filepath.Join(artist, "folder.jpg"))
	mustWrite(t, filepath.Join(single, "01 - Single.mp3"))
	moves := CoverMoves([]SidecarMove{
		{Src: filepath.Join(artist, "single.mp3"), Dst: filepath.Join(single, "01 - Single.mp3")},
	}, PruneOptions(root, nil))
	if len(moves) != 1 || filepath.Base(moves[0].Src) != "folder.jpg" || !moves[0].Copy {
		t.Fatalf("planned %+v, want a copy of folder.jpg alone", moves)
	}
}

// TestSidecarMovesSharesSubtitlesWithAVideo: subtitles named for an audio file that has a
// video of the same name beside it are the video's too.
func TestSidecarMovesSharesSubtitlesWithAVideo(t *testing.T) {
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(srcDir, "Song.mkv"))
	mustWrite(t, filepath.Join(srcDir, "Song.srt"))
	if moves := SidecarMoves(filepath.Join(srcDir, "Song.mp3"), filepath.Join(dstDir, "01 - Song.mp3"), nil); len(moves) != 1 || !moves[0].Shared {
		t.Fatalf("moves = %+v, want the subtitles shared with Song.mkv", moves)
	}
}

// TestSidecarMovesFollowTheRun: a run's index reads a folder once and follows what the
// run moves, so the lyrics two encodings share stay for the one left and go with the last.
func TestSidecarMovesFollowTheRun(t *testing.T) {
	t.Parallel()
	srcDir, dstDir := t.TempDir(), t.TempDir()
	for _, name := range []string{"Song.flac", "Song.mp3", "Song.lrc"} {
		mustWrite(t, filepath.Join(srcDir, name))
	}
	x := NewSiblings()
	flac, mp3 := filepath.Join(srcDir, "Song.flac"), filepath.Join(srcDir, "Song.mp3")
	if moves := SidecarMoves(flac, filepath.Join(dstDir, "01 - Song.flac"), x); len(moves) != 1 || !moves[0].Shared {
		t.Fatalf("first moves = %+v, want the lyrics shared with the mp3", moves)
	}
	x.Left(flac)
	if moves := SidecarMoves(mp3, filepath.Join(dstDir, "01 - Song.mp3"), x); len(moves) != 1 || moves[0].Shared {
		t.Fatalf("last moves = %+v, want the lyrics the mp3's alone once the flac left", moves)
	}
	x.Arrived(filepath.Join(srcDir, "Song.ogg"))
	if moves := SidecarMoves(mp3, filepath.Join(dstDir, "01 - Song.mp3"), x); len(moves) != 1 || !moves[0].Shared {
		t.Fatalf("moves = %+v, want the lyrics shared with an encoding that arrived", moves)
	}
}

// TestFolderDisposal: a companion follows its folder's audio, a disc folder's included, to
// the one place it went, and stays (fsx.ErrKeep) when the audio went two ways, when it is a
// track's own lyrics, or when the place already holds its name; undo puts it back.
func TestFolderDisposal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	album, split := filepath.Join(root, "Album"), filepath.Join(root, "Split")
	dst, dstA, dstB := filepath.Join(root, "New"), filepath.Join(root, "A"), filepath.Join(root, "B")
	for _, d := range []string{dst, dstA, dstB} {
		mustMkdir(t, d)
	}
	mustMkdir(t, filepath.Join(album, "CD1"))
	mustMkdir(t, split)
	for _, p := range []string{filepath.Join(album, "cover.jpg"), filepath.Join(album, "Gone.lrc"), filepath.Join(album, "folder.jpg"),
		filepath.Join(dst, "folder.jpg"), filepath.Join(split, "cover.jpg")} {
		mustWrite(t, p)
	}
	d := FolderDisposal([]SidecarMove{
		{Src: filepath.Join(album, "CD1", "1.mp3"), Dst: filepath.Join(dst, "1-01 - One.mp3")},
		{Src: filepath.Join(split, "a.mp3"), Dst: filepath.Join(dstA, "01 - A.mp3")},
		{Src: filepath.Join(split, "b.mp3"), Dst: filepath.Join(dstB, "01 - B.mp3")},
	}, func(string) string { return root })
	if err := d.Dispose(filepath.Join(album, "cover.jpg")); err != nil || !exists(filepath.Join(dst, "cover.jpg")) {
		t.Fatalf("the disc album's cover: err %v, moved %v; want it in the new folder", err, exists(filepath.Join(dst, "cover.jpg")))
	}
	for _, p := range []string{filepath.Join(album, "Gone.lrc"), filepath.Join(album, "folder.jpg"), filepath.Join(split, "cover.jpg")} {
		if err := d.Dispose(p); !errors.Is(err, fsx.ErrKeep) || !exists(p) {
			t.Errorf("%s: err %v, kept %v; want it kept", p, err, exists(p))
		}
	}
	if err := d.Undo(filepath.Join(album, "cover.jpg")); err != nil || !exists(filepath.Join(album, "cover.jpg")) {
		t.Fatalf("undo: err %v; want the cover back", err)
	}
}
