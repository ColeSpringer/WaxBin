package organize

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
)

// sidecarExts are the per-track companion files moved and renamed alongside their
// audio (lyrics, cue sheets, captions, interop metadata, per-track art). The audio
// extensions themselves are deliberately absent so a second encoding of the same
// track in the directory is never swept up as a sidecar.
var sidecarExts = []string{
	".lrc", ".cue", ".srt", ".vtt", ".nfo", ".opf", ".txt", ".json",
	".jpg", ".jpeg", ".png", ".webp", ".avif", ".heic", ".heif",
}

// moveSidecars relocates the sidecars of one moved audio file, renaming each
// same-basename companion to match the audio's new basename so players keep
// associating them. It returns the number of sidecars moved. Failures and destination
// collisions are logged and skipped, never fatal: the audio (the cataloged entity) has
// already moved, and a sidecar is best-effort. The directory's own cover is not here;
// applyCoverMoves carries that once the batch is done.
func (o *Organizer) moveSidecars(srcAudio, dstAudio string) int {
	moved := 0
	for _, m := range SidecarMoves(srcAudio, dstAudio) {
		switch err := moveSidecar(m.Src, m.Dst); {
		case err == nil:
			moved++
		case errors.Is(err, errSidecarExists):
			o.log.Warn("sidecar not moved: destination exists", "src", m.Src, "dst", m.Dst)
		default:
			o.log.Warn("sidecar move failed", "src", m.Src, "dst", m.Dst, "err", err)
		}
	}
	return moved
}

// SidecarMove is one sidecar's source and (renamed) destination.
type SidecarMove struct{ Src, Dst string }

// SidecarMoves enumerates one audio file's own companions to carry with it from
// srcAudio to dstAudio: same-basename files, renamed to the new basename. It probes
// candidate names by construction rather than listing the directory, so it stays O(1)
// per moved file even in a large flat folder. Shared by organize and the importer so
// both relocate the same sidecar set.
//
// Directory cover art is not here. It belongs to the directory rather than to any one
// file, so CoverMoves plans it over a whole batch, after the audio moved.
func SidecarMoves(srcAudio, dstAudio string) []SidecarMove {
	srcDir, dstDir := filepath.Dir(srcAudio), filepath.Dir(dstAudio)
	srcBase, dstBase := baseNoExt(srcAudio), baseNoExt(dstAudio)

	var moves []SidecarMove
	for _, ext := range sidecarExts {
		s := filepath.Join(srcDir, srcBase+ext)
		if isRegularFile(s) {
			moves = append(moves, SidecarMove{s, filepath.Join(dstDir, dstBase+ext)})
		}
	}
	return moves
}

// CoverMove is one directory cover to carry: copied when the source directory keeps
// audio that still needs it, or when its audio is going to more than one place; moved
// when the directory is being emptied of audio.
type CoverMove struct {
	Src, Dst string
	Copy     bool
}

// CoverMoves plans directory cover art for a batch of audio moves as a whole, after the
// audio moved, where SidecarMoves plans one file's own companions. For each source
// directory holding a cover by a name the scanner recognizes, the cover goes to every
// destination directory its audio went to. When audio remains in the directory (isAudio
// decides what counts) every destination takes a copy and the directory keeps its cover;
// when the directory is emptied, every destination but the last takes a copy and the
// last takes the file itself, so nothing is stranded. A destination that already holds a
// cover by that name, and a pair whose directories are the same place, are skipped.
// Matching is case-insensitive against model.CoverArtNames, as before.
//
// Two source directories merging into one destination therefore leave the second cover
// behind in its emptied directory, since the first one's already sits there; that is
// what happened before too, and choosing between two covers is not this function's call.
func CoverMoves(moved []SidecarMove, isAudio func(string) bool) []CoverMove {
	// Destinations per source directory, in first-appearance order so the move lands
	// deterministically.
	dests := map[string][]string{}
	for _, m := range moved {
		srcDir, dstDir := filepath.Dir(m.Src), filepath.Dir(m.Dst)
		if sameDir(srcDir, dstDir) {
			continue
		}
		if !slices.ContainsFunc(dests[srcDir], func(d string) bool { return sameDir(d, dstDir) }) {
			dests[srcDir] = append(dests[srcDir], dstDir)
		}
	}
	srcDirs := make([]string, 0, len(dests))
	for dir := range dests {
		srcDirs = append(srcDirs, dir)
	}
	slices.Sort(srcDirs)

	// Destinations this plan has already given a cover, so two directories merging into
	// one do not both plan a move onto it.
	claimed := map[string]bool{}
	var out []CoverMove
	for _, srcDir := range srcDirs {
		byLower := dirFilesByLower(srcDir)
		// Read from the directory as it is now, after the moves, so "emptied" is the real
		// outcome rather than what the plan intended.
		keepsAudio := dirHasAudio(byLower, srcDir, isAudio)
		for _, cand := range model.CoverArtNames {
			name, ok := byLower[cand]
			if !ok {
				continue
			}
			src := filepath.Join(srcDir, name)
			if !isRegularFile(src) {
				continue
			}
			// The destinations that still want a copy, decided before the copy-or-move
			// split so the move lands on one that will actually take it.
			var dsts []string
			for _, dstDir := range dests[srcDir] {
				dst := filepath.Join(dstDir, name)
				if !claimed[strings.ToLower(dst)] && !isRegularFile(dst) {
					dsts = append(dsts, dst)
				}
			}
			for i, dst := range dsts {
				claimed[strings.ToLower(dst)] = true
				last := i == len(dsts)-1
				out = append(out, CoverMove{Src: src, Dst: dst, Copy: keepsAudio || !last})
			}
		}
	}
	return out
}

// dirHasAudio reports whether the directory still holds an audio file, from the listing
// already taken for the cover match.
func dirHasAudio(byLower map[string]string, dir string, isAudio func(string) bool) bool {
	for _, name := range byLower {
		if isAudio(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// applyCoverMoves carries the planned directory covers, reporting how many landed. A
// failure or a destination collision is logged and skipped, the way a sidecar's is.
func (o *Organizer) applyCoverMoves(moves []CoverMove) int {
	moved := 0
	for _, m := range moves {
		switch err := fsx.MoveOrCopy(m.Src, m.Dst, m.Copy); {
		case err == nil:
			moved++
		case errors.Is(err, fsx.ErrExist):
			o.log.Warn("directory cover not moved: destination exists", "src", m.Src, "dst", m.Dst)
		default:
			o.log.Warn("directory cover move failed", "src", m.Src, "dst", m.Dst, "err", err)
		}
	}
	return moved
}

// dirFilesByLower lists dir once and maps each regular file's lowercased name to its
// actual name, for case-insensitive cover matching. Returns nil when unreadable.
func dirFilesByLower(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	byLower := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			byLower[strings.ToLower(e.Name())] = e.Name()
		}
	}
	return byLower
}

var errSidecarExists = errors.New("sidecar destination exists")

// moveSidecar moves one sidecar without clobbering an existing destination,
// creating the parent directory and falling back to copy+remove across
// filesystems. A pre-existing destination yields errSidecarExists so the caller
// can leave the source in place rather than lose either copy.
func moveSidecar(src, dst string) error {
	if src == dst {
		return nil
	}
	// fsx.Move is long-path-safe and creates the parent + cross-device fallback; an
	// existing destination becomes errSidecarExists so the caller leaves the source.
	if err := fsx.Move(src, dst); err != nil {
		if errors.Is(err, fsx.ErrExist) {
			return errSidecarExists
		}
		return err
	}
	return nil
}

func baseNoExt(p string) string {
	b := filepath.Base(p)
	return strings.TrimSuffix(b, filepath.Ext(b))
}

// sameDir reports whether two cleaned directory paths refer to the same place,
// folding case so a move that only re-cases the directory does not drag the cover
// art on a case-insensitive filesystem.
func sameDir(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func isRegularFile(p string) bool {
	info, err := os.Lstat(pathx.Long(p))
	return err == nil && info.Mode().IsRegular()
}
