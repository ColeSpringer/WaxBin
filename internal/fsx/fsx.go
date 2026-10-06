// Package fsx provides the long-path-safe filesystem move/copy primitives shared
// by organize, inbox, and trash. The helpers keep cross-device fallback,
// no-clobber behavior, mode preservation, partial-copy cleanup, and Windows
// extended-length path handling consistent across callers.
package fsx

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/colespringer/waxbin/internal/pathx"
	"golang.org/x/text/unicode/norm"
)

// ErrExist is returned when a move/copy would overwrite an existing destination.
// Callers translate it to their own typed error (a conflict, or a skip).
var ErrExist = errors.New("fsx: destination already exists")

// MoveOrCopy moves src to dst, or copies it (leaving src in place) when asCopy is
// set. It is the importer's primitive (move staged files, or copy to keep them).
func MoveOrCopy(src, dst string, asCopy bool) error {
	if asCopy {
		return Copy(src, dst)
	}
	return Move(src, dst)
}

// Move relocates src to dst, creating dst's parent directory and falling back to a
// copy+remove across filesystems. It refuses to overwrite an existing dst
// (ErrExist) and is long-path-safe on Windows. A dst that is src's own entry under a
// name differing only by case or Unicode form (a rename on a filesystem that matches
// names without regard to either) is respelled in place instead.
func Move(src, dst string) error {
	if err := ensureAbsent(dst); err != nil {
		if errors.Is(err, ErrExist) && spelledAlike(src, dst) {
			return respellEntry(osFolders{}, filepath.Dir(dst), filepath.Base(dst), true)
		}
		return err
	}
	if err := os.MkdirAll(pathx.Long(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	if err := os.Rename(pathx.Long(src), pathx.Long(dst)); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return copyThenRemove(src, dst)
		}
		return err
	}
	return nil
}

// Copy copies src to a fresh dst (ErrExist if it exists), creating dst's parent,
// fsync'ing, and removing the partial copy on any error. Long-path-safe.
func Copy(src, dst string) error {
	if err := ensureAbsent(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(pathx.Long(filepath.Dir(dst)), 0o755); err != nil {
		return err
	}
	return copyContents(src, dst)
}

// ensureAbsent returns ErrExist if dst is present, nil if it is absent, or the
// stat error otherwise.
func ensureAbsent(dst string) error {
	_, err := os.Lstat(pathx.Long(dst))
	if err == nil {
		return ErrExist
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// sameName reports whether two names are one name to a filesystem that matches names
// without regard to case or Unicode normalization form. NTFS folds case, APFS folds case
// and form, and the fold here covers both so a rename between two such spellings reads
// alike wherever it runs.
func sameName(a, b string) bool {
	return strings.EqualFold(norm.NFC.String(a), norm.NFC.String(b))
}

// spelledAlike reports whether dst is src's own entry under another spelling of its name.
// A hard link names the same file through an entry of its own, which a move must not take
// for the file being in place: that is two entries listed in the folder.
func spelledAlike(src, dst string) bool {
	sb, db := filepath.Base(src), filepath.Base(dst)
	if !sameName(sb, db) || !sameFile(os.Lstat, src, dst) || !sameFile(os.Stat, filepath.Dir(src), filepath.Dir(dst)) {
		return false
	}
	if sb == db {
		return true
	}
	names, err := osFolders{}.ReadDir(filepath.Dir(dst))
	return err == nil && !(slices.Contains(names, sb) && slices.Contains(names, db))
}

// SameFile reports whether two paths reach one file, following links.
func SameFile(a, b string) bool { return sameFile(os.Stat, a, b) }

func sameFile(stat func(string) (os.FileInfo, error), a, b string) bool {
	ai, err := stat(pathx.Long(a))
	if err != nil {
		return false
	}
	bi, err := stat(pathx.Long(b))
	return err == nil && os.SameFile(ai, bi)
}

// A Lister reports whether paths are spelled as their folders list them, reading each
// folder once. NTFS resolves a path spelled in another case than its entries, and APFS
// one in another case or Unicode form, so a stat alone cannot tell a cataloged file that
// moved to another spelling of its name from one still in place.
type Lister struct {
	fs    folders
	lists map[string][]string
}

// NewLister returns a Lister over the OS filesystem.
func NewLister() *Lister { return newLister(osFolders{}) }

func newLister(fs folders) *Lister { return &Lister{fs: fs, lists: map[string][]string{}} }

// Spelled reports whether path is listed under exactly its own spelling at each name
// below root; a path outside root is judged by its last name alone, and one whose folders
// cannot be read is not spelled so.
func (l *Lister) Spelled(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	sep := string(filepath.Separator)
	cur, rel := root, ""
	if r, err := filepath.Rel(root, path); err != nil || r == "." || r == ".." || strings.HasPrefix(r, ".."+sep) {
		cur, rel = filepath.Dir(path), filepath.Base(path)
	} else {
		rel = r
	}
	for _, name := range strings.Split(rel, sep) {
		names, ok := l.lists[cur]
		if !ok {
			var err error
			if names, err = l.fs.ReadDir(cur); err != nil {
				return false
			}
			l.lists[cur] = names
		}
		if !slices.Contains(names, name) {
			return false
		}
		cur = filepath.Join(cur, name)
	}
	return true
}

// respellEntry gives the entry of dir named like name that spelling: by a direct rename
// where the filesystem honours one between two spellings of a name, else through a free
// temporary name. A file's temporary name keeps its extension.
func respellEntry(fs folders, dir, name string, file bool) error {
	names, err := fs.ReadDir(dir)
	if err != nil {
		return err
	}
	if slices.Contains(names, name) {
		return nil
	}
	i := slices.IndexFunc(names, func(e string) bool { return sameName(e, name) })
	if i < 0 {
		return nil
	}
	from, to := filepath.Join(dir, names[i]), filepath.Join(dir, name)
	if err := fs.Rename(from, to); err == nil {
		if names, err := fs.ReadDir(dir); err == nil && slices.Contains(names, name) {
			return nil
		}
	}
	ext := ""
	if file {
		ext = filepath.Ext(name)
	}
	tmp, err := freeName(fs, dir, ext)
	if err != nil {
		return err
	}
	if err := fs.Rename(from, tmp); err != nil {
		return err
	}
	if err := fs.Rename(tmp, to); err != nil {
		_ = fs.Rename(tmp, from)
		return err
	}
	return nil
}

// FreeName returns an unused short name in dir ending in ext, for a file parked there
// for a moment.
func FreeName(dir, ext string) (string, error) { return freeName(osFolders{}, dir, ext) }

func freeName(fs folders, dir, ext string) (string, error) {
	var b [4]byte
	for range 8 {
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		name := filepath.Join(dir, "waxbin-move-"+hex.EncodeToString(b[:])+ext)
		if fs.Lstat(name) != nil {
			return name, nil
		}
	}
	return "", ErrExist
}

// folders is the filesystem a respell works on: the OS, or a test's.
type folders interface {
	Lstat(path string) error
	ReadDir(path string) ([]string, error)
	Rename(from, to string) error
}

type osFolders struct{}

func (osFolders) Lstat(path string) error {
	_, err := os.Lstat(pathx.Long(path))
	return err
}

func (osFolders) ReadDir(path string) ([]string, error) {
	entries, err := os.ReadDir(pathx.Long(path))
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, err
}

func (osFolders) Rename(from, to string) error { return os.Rename(pathx.Long(from), pathx.Long(to)) }

// A Speller gives the folders below a root the spellings of the paths it is handed. A
// filesystem that matches names without regard to case or Unicode form takes a path
// spelled otherwise for the folder as it stands, so a move into "J.R.R. Tolkien" lands in
// "j.r.r. tolkien" and keeps that spelling; the Speller renames such a folder first. It
// never touches the root or a folder above it, remembers the folders it has checked, and
// reports each rename to renamed, whose files have moved with it. An error from renamed
// has the folder renamed back and fails the move, so disk and catalog never disagree.
type Speller struct {
	root    string
	fs      folders
	renamed func(from, to string) error
	checked map[string]bool
}

// NewSpeller returns a Speller for the folders below root.
func NewSpeller(root string, renamed func(from, to string) error) *Speller {
	return newSpeller(root, osFolders{}, renamed)
}

func newSpeller(root string, fs folders, renamed func(from, to string) error) *Speller {
	return &Speller{root: filepath.Clean(root), fs: fs, renamed: renamed, checked: map[string]bool{}}
}

// Respell gives each folder of dir below the root that exists under another spelling the
// one dir names. A folder the filesystem cannot find under dir's spelling is absent, and
// one it finds while listing that spelling is spelled so already (a case-sensitive
// filesystem), so only a folder found under a spelling not listed is renamed.
func (sp *Speller) Respell(dir string) error {
	rel, err := filepath.Rel(sp.root, filepath.Clean(dir))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	cur := sp.root
	for _, name := range strings.Split(rel, string(filepath.Separator)) {
		next := filepath.Join(cur, name)
		if !sp.checked[next] {
			if sp.fs.Lstat(next) != nil {
				return nil
			}
			names, err := sp.fs.ReadDir(cur)
			if err != nil {
				return err
			}
			if i := slices.IndexFunc(names, func(e string) bool { return sameName(e, name) }); i >= 0 && !slices.Contains(names, name) {
				old := filepath.Join(cur, names[i])
				if err := respellEntry(sp.fs, cur, name, false); err != nil {
					return err
				}
				if sp.renamed != nil {
					if err := sp.renamed(old, next); err != nil {
						_ = respellEntry(sp.fs, cur, names[i], false)
						return err
					}
				}
			}
			sp.checked[next] = true
		}
		cur = next
	}
	return nil
}

// Move is Move with dst's folders respelled first.
func (sp *Speller) Move(src, dst string) error {
	if err := sp.Respell(filepath.Dir(dst)); err != nil {
		return err
	}
	return Move(src, dst)
}

// MoveOrCopy is MoveOrCopy with dst's folders respelled first.
func (sp *Speller) MoveOrCopy(src, dst string, asCopy bool) error {
	if err := sp.Respell(filepath.Dir(dst)); err != nil {
		return err
	}
	return MoveOrCopy(src, dst, asCopy)
}

func copyThenRemove(src, dst string) error {
	if err := copyContents(src, dst); err != nil {
		return err
	}
	return os.Remove(pathx.Long(src))
}

// copyContents copies src to dst with O_EXCL (so it never clobbers even if a racing
// writer beat the ensureAbsent check), preserves the source mode, fsyncs, and
// removes a partial dst on any failure so a failed move leaves no stray bytes.
func copyContents(src, dst string) error {
	in, err := os.Open(pathx.Long(src))
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(pathx.Long(dst), os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = out.Close()
			_ = os.Remove(pathx.Long(dst))
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}
