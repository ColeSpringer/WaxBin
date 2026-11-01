package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
)

// CompanionExts are the extensions of a folder's companion files: images (covers, scans,
// artist pictures), .nfo and .opf metadata, playlists, rip logs and checksums, booklets,
// cue sheets, lyrics and captions.
var CompanionExts = []string{
	".jpg", ".jpeg", ".png", ".webp", ".avif", ".heic", ".heif", ".gif", ".bmp", ".tif", ".tiff",
	".nfo", ".opf", ".m3u", ".m3u8", ".log", ".pdf", ".cue", ".sfv", ".md5", ".ffp", ".accurip",
	".txt", ".json", ".lrc", ".srt", ".vtt",
}

// junkNames are the files and folders a system leaves beside media: Finder's and
// Explorer's view files, a macOS custom folder icon, Synology's and QNAP's thumbnail
// folders, and netatalk's resource forks.
var junkNames = []string{".ds_store", "thumbs.db", "desktop.ini", "icon\r", "@eadir", "@__thumb", ".@__thumb", ".appledouble"}

// IsJunk reports whether path names litter a system leaves in a folder it has shown or
// indexed (junkNames), or an AppleDouble "._" file.
func IsJunk(path string) bool {
	name := filepath.Base(path)
	return strings.HasPrefix(name, "._") || slices.Contains(junkNames, strings.ToLower(name))
}

// IsCompanion reports whether path names a folder companion (CompanionExts).
func IsCompanion(path string) bool {
	return slices.Contains(CompanionExts, strings.ToLower(filepath.Ext(path)))
}

// ErrKeep is what Dispose returns to leave a companion where it is, which keeps its
// folder. It ends a prune quietly.
var ErrKeep = errors.New("fsx: companion kept")

// PruneOptions says what a prune may remove. A nil predicate claims nothing, and a file
// none claims keeps its folder.
type PruneOptions struct {
	// Root bounds the walk: it and every folder above it stay.
	Root string
	// IsAudio, Junk and Companion classify a folder's entries by path. Audio keeps the
	// folder, and junk, files or folders, is deleted with it.
	IsAudio, Junk, Companion func(path string) bool
	// Dispose takes a companion out of a folder about to be removed, into a trash entry
	// or a new folder. When nil a companion keeps its folder.
	Dispose func(path string) error
	// Undo puts back a companion Dispose took when its folder cannot be removed after
	// all. Nil leaves it where Dispose put it.
	Undo func(path string) error
}

// PruneReport lists the folders a prune removed, deepest first.
type PruneReport struct {
	Folders []PrunedDir
}

// PrunedDir is a folder a prune removed, with the junk it deleted from it and the
// companions it handed to Dispose.
type PrunedDir struct {
	Dir              string
	Junk, Companions []string
}

// PruneDirs removes start and then each folder above it that holds nothing but junk and,
// when Dispose is set, companions, up to but not including the root. It stops at the
// first folder holding anything else, at a symlink or a mount point, inside the library
// trash, and quietly at a folder it may not read or remove or that has gained a file. A
// folder's junk goes first, so a junk file that will not go leaves its companions alone,
// and the companions taken from a folder that then stays are put back (Undo). An error
// from Dispose, or an unexpected filesystem error, stops it with that error.
func PruneDirs(start string, o PruneOptions) (PruneReport, error) {
	return pruneDirs(osDirs{}, start, o)
}

// PruneAll prunes each of dirs once, in order, with the options opts gives for it, passes
// an error to warn with the folder it stopped at, and returns the folders that went.
func PruneAll(dirs []string, opts func(dir string) PruneOptions, warn func(dir string, err error)) []PrunedDir {
	return pruneAll(osDirs{}, dirs, opts, warn)
}

func pruneAll(fsys dirFS, dirs []string, opts func(dir string) PruneOptions, warn func(dir string, err error)) []PrunedDir {
	var pruned []PrunedDir
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		rep, err := pruneDirs(fsys, dir, opts(dir))
		if err != nil && warn != nil {
			warn(dir, err)
		}
		pruned = append(pruned, rep.Folders...)
	}
	return pruned
}

// PreviewPrune is PruneAll run over the folders as they will be once the files in removed
// are gone, touching nothing: it lists the folders the prune would remove, with the junk
// it would delete and the companions it would hand to Dispose (each Dispose taken to
// succeed), if nothing changes meanwhile. A removed path takes the entry it resolves to on
// the disk, so a file whose folder lists it under another case or Unicode form than the
// path goes where the disk folds names, as the removal itself would take it.
func PreviewPrune(dirs []string, opts func(dir string) PruneOptions, removed map[string]bool) []PrunedDir {
	fsys := previewDirs{gone: map[string]bool{}, folded: map[string][]string{}}
	for p, gone := range removed {
		p = filepath.Clean(p)
		fsys.gone[p] = gone
		if gone {
			fsys.folded[pathx.CollisionKey(p)] = append(fsys.folded[pathx.CollisionKey(p)], p)
		}
	}
	return pruneAll(fsys, dirs, func(dir string) PruneOptions {
		o := opts(dir)
		if o.Dispose != nil {
			o.Dispose = func(string) error { return nil }
		}
		o.Undo = nil
		return o
	}, nil)
}

// previewDirs is the OS less what is gone: a removal is recorded rather than made. folded
// keys the planned removals by their folded paths.
type previewDirs struct {
	osDirs
	gone   map[string]bool
	folded map[string][]string
}

func (p previewDirs) Lstat(path string) (fs.FileInfo, error) {
	if p.gone[path] {
		return nil, fs.ErrNotExist
	}
	return p.osDirs.Lstat(path)
}

func (p previewDirs) Stat(path string) (fs.FileInfo, error) {
	if p.gone[path] {
		return nil, fs.ErrNotExist
	}
	return p.osDirs.Stat(path)
}

func (p previewDirs) ReadDir(path string) ([]fs.DirEntry, error) {
	entries, err := p.osDirs.ReadDir(path)
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Name()] = true
	}
	return slices.DeleteFunc(entries, func(e fs.DirEntry) bool {
		full := filepath.Join(path, e.Name())
		return p.gone[full] || p.resolvesTo(full, listed)
	}), err
}

// resolvesTo reports whether a planned removal names the file at full under another
// spelling: a removed path in its folder, not listed there itself, that the disk resolves
// to the same file (a filesystem folding case or form). A second name listed beside the
// removed one (a hard link) is a file of its own.
func (p previewDirs) resolvesTo(full string, listed map[string]bool) bool {
	for _, r := range p.folded[pathx.CollisionKey(full)] {
		if r == full || filepath.Dir(r) != filepath.Dir(full) || listed[filepath.Base(r)] {
			continue
		}
		a, errA := os.Lstat(pathx.Long(r))
		b, errB := os.Lstat(pathx.Long(full))
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return true
		}
	}
	return false
}

func (p previewDirs) Remove(path string) error {
	p.gone[path] = true
	return nil
}

func (p previewDirs) RemoveAll(path string) error {
	p.gone[path] = true
	return nil
}

func pruneDirs(fsys dirFS, start string, o PruneOptions) (PruneReport, error) {
	var rep PruneReport
	root := filepath.Clean(o.Root)
	for dir := filepath.Clean(start); below(root, dir); dir = filepath.Dir(dir) {
		info, err := fsys.Lstat(dir)
		if err != nil {
			return rep, refused(err)
		}
		// A symlink, a junction or volume mount point on Windows, or not a folder.
		if info.Mode().Type() != fs.ModeDir {
			return rep, nil
		}
		// The root is the folder it names, which a root given as a symlink reaches only by
		// following it.
		lstat := fsys.Lstat
		if pathx.SamePath(root, filepath.Dir(dir)) {
			lstat = fsys.Stat
		}
		parent, err := lstat(filepath.Dir(dir))
		if err != nil {
			return rep, refused(err)
		}
		if d, ok := fsys.Device(dir, info); ok {
			if p, ok := fsys.Device(filepath.Dir(dir), parent); ok && p != d {
				return rep, nil
			}
		}
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			return rep, refused(err)
		}
		junk, companions, other := o.Leftovers(dir, entries)
		if other || len(companions) > 0 && o.Dispose == nil {
			return rep, nil
		}
		for _, j := range junk {
			if err := fsys.RemoveAll(j); err != nil {
				return rep, refused(err)
			}
		}
		var taken []string
		for _, c := range companions {
			if err := o.Dispose(c); err != nil {
				undone := undo(o, taken)
				if errors.Is(err, ErrKeep) {
					return rep, undone
				}
				return rep, errors.Join(err, undone)
			}
			taken = append(taken, c)
		}
		if err := fsys.Remove(dir); err != nil {
			return rep, errors.Join(refused(err), undo(o, taken))
		}
		rep.Folders = append(rep.Folders, PrunedDir{Dir: dir, Junk: junk, Companions: taken})
	}
	return rep, nil
}

// undo puts back what Dispose took, last first.
func undo(o PruneOptions, taken []string) error {
	if o.Undo == nil {
		return nil
	}
	var errs []error
	for _, p := range slices.Backward(taken) {
		errs = append(errs, o.Undo(p))
	}
	return errors.Join(errs...)
}

// Leftovers sorts the entries of dir into the junk and the companions a prune may clear,
// and reports other when one keeps the folder whatever Dispose does: a subfolder, a link,
// audio, or a file no predicate claims.
func (o PruneOptions) Leftovers(dir string, entries []fs.DirEntry) (junk, companions []string, other bool) {
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir() && claims(o.Junk, p):
			junk = append(junk, p)
		case !e.Type().IsRegular():
			other = true
		case claims(o.Junk, p):
			junk = append(junk, p)
		case claims(o.IsAudio, p):
			other = true
		case claims(o.Companion, p):
			companions = append(companions, p)
		default:
			other = true
		}
	}
	return junk, companions, other
}

func claims(pred func(string) bool, path string) bool { return pred != nil && pred(path) }

// below reports whether dir lies under root, not root itself, and outside the library
// trash.
func below(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !model.InTrash(rel)
}

// refused is nil for the failures that end a prune quietly: a folder already gone, one
// the process may not read or change, one a file has reached since it was read, or one
// in use.
func refused(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrExist) || inUse(err) {
		return nil
	}
	return err
}

// dirFS is the filesystem a prune works on: the OS, or a test's.
type dirFS interface {
	Lstat(path string) (fs.FileInfo, error)
	Stat(path string) (fs.FileInfo, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	Remove(path string) error
	RemoveAll(path string) error
	// Device names the filesystem holding path, when the platform says.
	Device(path string, info fs.FileInfo) (uint64, bool)
}

type osDirs struct{}

func (osDirs) Lstat(path string) (fs.FileInfo, error) { return os.Lstat(pathx.Long(path)) }

func (osDirs) Stat(path string) (fs.FileInfo, error) { return os.Stat(pathx.Long(path)) }

func (osDirs) ReadDir(path string) ([]fs.DirEntry, error) { return os.ReadDir(pathx.Long(path)) }

func (osDirs) Remove(path string) error { return os.Remove(pathx.Long(path)) }

func (osDirs) RemoveAll(path string) error { return os.RemoveAll(pathx.Long(path)) }

func (osDirs) Device(_ string, info fs.FileInfo) (uint64, bool) { return device(info) }
