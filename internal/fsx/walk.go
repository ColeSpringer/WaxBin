package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/colespringer/waxbin/internal/pathx"
)

// IsLink reports whether a stat (os.Lstat) describes a link the engine does not follow
// below a root: a symbolic link on every platform, and a junction on Windows.
func IsLink(info fs.FileInfo) bool { return isLink(info) }

// EntryIsLink is IsLink for a folder listing's entry.
func EntryIsLink(d fs.DirEntry) bool {
	if d.Type()&fs.ModeSymlink != 0 {
		return true
	}
	if d.Type()&fs.ModeIrregular == 0 {
		return false
	}
	info, err := d.Info()
	return err == nil && isLink(info)
}

// WalkRoot is filepath.WalkDir over root, except that a root which is itself a link to
// a folder is walked as that folder, with every path it reports, the root's own entry
// included, spelled under root: a library registered at a link is cataloged under the
// path it was registered at. A link below the root, or a link root that reaches a file,
// is reported as a link and not followed, as WalkDir reports it, and a link root that
// reaches nothing (gone, or a loop) is reported as fn(root, nil, err), as WalkDir
// reports a root it cannot stat.
//
// A link to a folder is walked from root with a separator appended, which makes the
// stat of the root follow it on every platform (an lstat resolves a final link that a
// separator follows, a junction included), so no link target is read here; the entries
// below come back clean, and only the root's own entry is respelled.
func WalkRoot(root string, fn fs.WalkDirFunc) error {
	info, err := os.Lstat(root)
	if err != nil || !isLink(info) {
		return filepath.WalkDir(root, fn)
	}
	target, err := os.Stat(root)
	if err != nil {
		if err := fn(root, nil, err); err != fs.SkipDir && err != fs.SkipAll {
			return err
		}
		return nil
	}
	if !target.IsDir() {
		return filepath.WalkDir(root, fn)
	}
	start := root + string(filepath.Separator)
	return filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if path == start {
			path = root
		}
		return fn(path, d, err)
	})
}

// LinkBelow names the first link on the way down from root to sub, the root itself
// excluded, or reports that there is none, which a sub outside the root never has. A
// component that cannot be statted ends the look: one that is not there is a gone
// sub-path, and a walk reports any other.
func LinkBelow(root, sub string) (string, bool) {
	rel, err := filepath.Rel(root, sub)
	if err != nil || rel == "." || !pathx.UnderRoot(root, sub) {
		return "", false
	}
	path := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", false
		}
		if isLink(info) {
			return path, true
		}
	}
	return "", false
}

var errTooManyLinks = errors.New("too many links")

// ResolveLinks is the folder path reaches with every link in it followed (IsLink), for
// comparing two paths by the folders they reach. A path not all there is resolved as far
// as the disk holds it, a dangling link followed by what it says, and the rest carried as
// spelled, so a folder not made yet (an inbox, a podcast dir, a root mounted later) is
// compared where it will be. A component that cannot be read for another reason, or a
// loop, is an error.
func ResolveLinks(path string) (string, error) {
	path = filepath.Clean(path)
	for hops := 0; ; hops++ {
		if hops > 40 {
			return "", &fs.PathError{Op: "resolve", Path: path, Err: errTooManyLinks}
		}
		vol := filepath.VolumeName(path)
		cur := vol + string(filepath.Separator)
		parts := strings.Split(strings.Trim(path[len(vol):], string(filepath.Separator)), string(filepath.Separator))
		if parts[0] == "" {
			return cur, nil
		}
		relinked := false
		for i, part := range parts {
			next := filepath.Join(cur, part)
			info, err := os.Lstat(next)
			switch {
			case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
				return filepath.Join(append([]string{cur}, parts[i:]...)...), nil
			case err != nil:
				return "", err
			case isLink(info):
				target, err := os.Readlink(next)
				if err != nil {
					return "", err
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(cur, target)
				}
				path = filepath.Join(append([]string{target}, parts[i+1:]...)...)
				relinked = true
			default:
				cur = next
			}
			if relinked {
				break
			}
		}
		if !relinked {
			return cur, nil
		}
	}
}
