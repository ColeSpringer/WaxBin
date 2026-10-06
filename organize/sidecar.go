package organize

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
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
func (o *Organizer) moveSidecars(sp *fsx.Speller, srcAudio, dstAudio string, x *Siblings) int {
	moved := 0
	for _, m := range SidecarMoves(srcAudio, dstAudio, x) {
		switch err := moveSidecar(sp, m.Src, m.Dst, m.Shared); {
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

// SidecarMove is one sidecar's source and (renamed) destination. Shared marks one that
// another encoding of the track, or a video of its name, left in the folder still uses,
// which a move copies.
type SidecarMove struct {
	Src, Dst string
	Shared   bool
}

// SidecarMoves enumerates one audio file's own companions to carry with it from
// srcAudio to dstAudio: same-basename files, renamed to the new basename. It probes
// candidate names by construction rather than listing the directory, and checks a
// sidecar another file of its name may share against x, which lists each folder once for
// a run (nil reads the folder for this call). Shared by organize, the importer and the
// trash, so all three take the same sidecar set.
//
// Directory cover art is not here. It belongs to the directory rather than to any one
// file, so CoverMoves plans it over a whole batch, after the audio moved.
func SidecarMoves(srcAudio, dstAudio string, x *Siblings) []SidecarMove {
	srcDir, dstDir := filepath.Dir(srcAudio), filepath.Dir(dstAudio)
	srcBase, dstBase := baseNoExt(srcAudio), baseNoExt(dstAudio)

	var moves []SidecarMove
	for _, ext := range sidecarExts {
		s := filepath.Join(srcDir, srcBase+ext)
		if isRegularFile(s) {
			moves = append(moves, SidecarMove{Src: s, Dst: filepath.Join(dstDir, dstBase+ext)})
		}
	}
	if len(moves) == 0 {
		return nil
	}
	if x == nil {
		x = NewSiblings()
	}
	if x.shared(srcAudio, dstAudio) {
		for i := range moves {
			moves[i].Shared = true
		}
	}
	return moves
}

// mediaExtensions are the files that can own a sidecar beside an audio file of the same
// name: other encodings, and the videos (a music video, a concert) whose subtitles and
// art a folder may hold too.
var mediaExtensions = append(scan.AudioExtensions(),
	".mkv", ".webm", ".mk3d", ".mov", ".asf", ".avi", ".m4v", ".wmv", ".mpg", ".mpeg", ".ts")

// Siblings finds the media files that share an audio file's name in its folder, reading
// each folder once. A run that moves or deletes files tells it (Left, Arrived), so a
// later lookup sees the folder as it then is. Names are compared without regard to case,
// which keeps a sidecar a case-insensitive filesystem would give either file.
type Siblings struct {
	dirs map[string]map[string][]string // folder, then lowercase name stem, then names
}

// NewSiblings returns an empty index.
func NewSiblings() *Siblings { return &Siblings{dirs: map[string]map[string][]string{}} }

func (x *Siblings) folder(dir string) map[string][]string {
	stems, ok := x.dirs[dir]
	if !ok {
		stems = map[string][]string{}
		entries, _ := os.ReadDir(pathx.Long(dir))
		for _, e := range entries {
			if e.Type().IsRegular() && isMedia(e.Name()) {
				k := stemKey(e.Name())
				stems[k] = append(stems[k], e.Name())
			}
		}
		x.dirs[dir] = stems
	}
	return stems
}

// shared reports whether audio's folder holds another media file under its name. The
// file's own entry is none, whether it is still there, renamed on a case-insensitive
// filesystem, or moved within the folder to dst.
func (x *Siblings) shared(audio, dst string) bool {
	dir, base := filepath.Dir(audio), filepath.Base(audio)
	for _, name := range x.folder(dir)[stemKey(base)] {
		p := filepath.Join(dir, name)
		if name != base && p != dst && !(strings.EqualFold(name, base) && fsx.SameFile(p, audio)) {
			return true
		}
	}
	return false
}

// Left tells the index a file has left its folder.
func (x *Siblings) Left(path string) {
	if stems, ok := x.dirs[filepath.Dir(path)]; ok {
		k, name := stemKey(filepath.Base(path)), filepath.Base(path)
		stems[k] = slices.DeleteFunc(stems[k], func(n string) bool { return n == name })
	}
}

// Arrived tells the index a file has come into a folder.
func (x *Siblings) Arrived(path string) {
	if stems, ok := x.dirs[filepath.Dir(path)]; ok && isMedia(path) {
		k, name := stemKey(filepath.Base(path)), filepath.Base(path)
		if !slices.Contains(stems[k], name) {
			stems[k] = append(stems[k], name)
		}
	}
}

func stemKey(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name)))
}

func isMedia(path string) bool {
	return slices.Contains(mediaExtensions, strings.ToLower(filepath.Ext(path)))
}

// CoverMove is one folder cover or companion to carry: copied when the source folder
// stays, or when its audio is going to more than one place; moved when the folder is
// being emptied.
type CoverMove struct {
	Src, Dst string
	Copy     bool
}

// CoverMoves plans the folder covers and companions for a batch of audio moves as a
// whole, after the audio moved, where SidecarMoves plans one file's own companions. Only
// a source folder below o.Root carries anything: the library root holds no album's
// files, and neither does the folder of a file handed over alone (o.Root is that folder).
// A folder left with nothing but junk and companions (o's predicates) whose audio all
// went to one destination sends every companion there, which empties it. Otherwise only
// its covers travel (model.CoverArtNames, matched without regard to case), to every
// destination its audio went to: copied while the folder keeps anything else, and when
// it keeps nothing but covers, copied to all but the last destination, which takes the
// file itself. A destination already holding a file by that name, and a pair whose
// folders are the same place, are skipped.
//
// Two source folders merging into one destination therefore leave the second cover
// behind in its emptied folder, since the first one's already sits there; choosing
// between two covers is not this function's call.
func CoverMoves(moved []SidecarMove, o fsx.PruneOptions) []CoverMove {
	// Destinations per source directory, in first-appearance order so the move lands
	// deterministically.
	dests := map[string][]string{}
	sent := map[string]bool{}
	for _, m := range moved {
		sent[m.Src] = true
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

	// Destinations this plan has already given a file, so two directories merging into
	// one do not both plan a move onto it.
	claimed := map[string]bool{}
	var out []CoverMove
	for _, srcDir := range srcDirs {
		if !pathx.UnderRoot(o.Root, srcDir) || pathx.SamePath(o.Root, srcDir) {
			continue
		}
		// Read from the directory as it is now, after the moves, so "emptied" is the real
		// outcome rather than what the plan intended.
		entries, err := os.ReadDir(pathx.Long(srcDir))
		if err != nil {
			continue
		}
		// A file the batch copied out is gone from the folder as far as its companions go.
		entries = slices.DeleteFunc(entries, func(e fs.DirEntry) bool { return sent[filepath.Join(srcDir, e.Name())] })
		_, companions, other := o.Leftovers(srcDir, entries)
		// A track's own lyrics, cue sheet or captions left here have no audio to follow.
		if slices.ContainsFunc(companions, trackSidecar) {
			other = true
			companions = slices.DeleteFunc(companions, trackSidecar)
		}
		all := !other && len(dests[srcDir]) == 1
		moveLast := all || !other && !slices.ContainsFunc(companions, func(p string) bool { return !isCover(filepath.Base(p)) })
		for _, src := range companions {
			if !all && !isCover(filepath.Base(src)) {
				continue
			}
			// The destinations that still want a copy, decided before the copy-or-move
			// split so the move lands on one that will actually take it.
			var dsts []string
			for _, dstDir := range dests[srcDir] {
				dst := filepath.Join(dstDir, filepath.Base(src))
				if !claimed[strings.ToLower(dst)] && !isRegularFile(dst) {
					dsts = append(dsts, dst)
				}
			}
			for i, dst := range dsts {
				claimed[strings.ToLower(dst)] = true
				out = append(out, CoverMove{Src: src, Dst: dst, Copy: !moveLast || i < len(dsts)-1})
			}
		}
	}
	return out
}

func isCover(name string) bool { return slices.Contains(model.CoverArtNames, strings.ToLower(name)) }

// trackSidecar reports whether path is a kind of file that belongs to one track (lyrics,
// a cue sheet, captions), which travels only renamed with its audio.
func trackSidecar(path string) bool {
	return slices.Contains([]string{".lrc", ".cue", ".srt", ".vtt"}, strings.ToLower(filepath.Ext(path)))
}

// FolderDisposal returns the Dispose and Undo for pruning the folders a batch of moves
// emptied; root gives each moved file's library or staging root. A companion follows the
// audio when everything that left its folder, from the folder itself or from a disc
// folder inside it, went to one destination folder. Any other stays (fsx.ErrKeep), as
// does a track's own lyrics, cue sheet or captions and one whose name the destination
// already holds, and keeps its folder.
func FolderDisposal(moved []SidecarMove, root func(src string) string) (dispose, undo func(string) error) {
	dests := map[string][]string{}
	add := func(dir, dst string) {
		if !slices.ContainsFunc(dests[dir], func(d string) bool { return sameDir(d, dst) }) {
			dests[dir] = append(dests[dir], dst)
		}
	}
	for _, m := range moved {
		dst := filepath.Dir(m.Dst)
		add(filepath.Dir(m.Src), dst)
		if album := identity.AlbumFolder(root(m.Src), m.Src); album != filepath.Dir(m.Src) {
			add(album, dst)
		}
	}
	target := func(p string) (string, bool) {
		d := dests[filepath.Dir(p)]
		if len(d) != 1 || trackSidecar(p) {
			return "", false
		}
		return filepath.Join(d[0], filepath.Base(p)), true
	}
	dispose = func(p string) error {
		q, ok := target(p)
		if !ok {
			return fsx.ErrKeep
		}
		if err := fsx.Move(p, q); err != nil {
			if errors.Is(err, fsx.ErrExist) {
				return fsx.ErrKeep
			}
			return err
		}
		return nil
	}
	undo = func(p string) error {
		if q, ok := target(p); ok {
			return fsx.Move(q, p)
		}
		return nil
	}
	return dispose, undo
}

// applyCoverMoves carries the planned directory covers, reporting how many landed. A
// failure or a destination collision is logged and skipped, the way a sidecar's is.
func (o *Organizer) applyCoverMoves(sp *fsx.Speller, moves []CoverMove) int {
	moved := 0
	for _, m := range moves {
		switch err := sp.MoveOrCopy(m.Src, m.Dst, m.Copy); {
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

var errSidecarExists = errors.New("sidecar destination exists")

// moveSidecar moves (or copies) one sidecar without clobbering an existing destination,
// creating the parent directory and falling back to copy+remove across
// filesystems. A pre-existing destination yields errSidecarExists so the caller
// can leave the source in place rather than lose either copy.
func moveSidecar(sp *fsx.Speller, src, dst string, asCopy bool) error {
	if src == dst {
		return nil
	}
	// fsx.Move is long-path-safe and creates the parent + cross-device fallback; an
	// existing destination becomes errSidecarExists so the caller leaves the source.
	if err := sp.MoveOrCopy(src, dst, asCopy); err != nil {
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

// PruneOptions are the options for pruning the folders below a library root that moves
// have emptied: the scanner's audio set, fsx's junk and companion names, and dispose for
// a removed folder's companions (nil keeps them, and their folder with them).
func PruneOptions(root string, dispose func(string) error) fsx.PruneOptions {
	return fsx.PruneOptions{Root: root, IsAudio: scan.IsAudio, Junk: fsx.IsJunk, Companion: fsx.IsCompanion, Dispose: dispose}
}
