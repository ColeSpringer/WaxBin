package organize

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
)

// sidecarExts are the per-track companion files moved and renamed alongside their
// audio (lyrics, cue sheets, captions, interop metadata, per-track art). The audio
// extensions themselves are deliberately absent so a second encoding of the same
// track in the directory is never swept up as a sidecar.
var sidecarExts = []string{
	".lrc", ".cue", ".srt", ".vtt", ".nfo", ".opf", ".txt", ".json",
	".jpg", ".jpeg", ".png", ".webp",
}

// dirArtNames are directory-level cover images carried (keeping their name) when
// the audio leaves the directory.
var dirArtNames = []string{
	"cover.jpg", "cover.jpeg", "cover.png", "folder.jpg", "folder.png",
}

// moveSidecars relocates the sidecars of one moved audio file. Same-basename
// companions are renamed to match the audio's new basename so players keep
// associating them; directory cover art keeps its name in the new directory. It
// returns the number of sidecars moved. Failures and destination collisions are
// logged and skipped, never fatal: the audio (the cataloged entity) has already
// moved, and a sidecar is best-effort.
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

// SidecarMoves enumerates the on-disk sidecars to carry with an audio file moving
// from srcAudio to dstAudio: same-basename companions (renamed to the new
// basename) plus directory cover art (kept by name, only when the directory
// changes). It probes candidate names by construction rather than listing the
// directory, so it stays O(1) per moved file even in a large flat folder. Shared
// by organize and the importer so both relocate the same sidecar set.
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
	// Directory art moves only when the audio actually changes directory; within
	// the same directory it stays put for the other tracks.
	if !sameDir(srcDir, dstDir) {
		for _, name := range dirArtNames {
			s := filepath.Join(srcDir, name)
			if isRegularFile(s) {
				moves = append(moves, SidecarMove{s, filepath.Join(dstDir, name)})
			}
		}
	}
	return moves
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
