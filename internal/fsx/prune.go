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

// PruneReport lists the folders a prune removed, deepest first, and the companions it
// handed to Dispose.
type PruneReport struct {
	Removed, Disposed []string
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
// an error to warn with the folder it stopped at, and returns how many folders went.
func PruneAll(dirs []string, opts func(dir string) PruneOptions, warn func(dir string, err error)) int {
	pruned := 0
	seen := map[string]bool{}
	for _, dir := range dirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		rep, err := PruneDirs(dir, opts(dir))
		if err != nil && warn != nil {
			warn(dir, err)
		}
		pruned += len(rep.Removed)
	}
	return pruned
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
		rep.Disposed = append(rep.Disposed, taken...)
		rep.Removed = append(rep.Removed, dir)
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
