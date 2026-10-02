package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMoveOntoAnotherSpellingOfItself: a destination that is the source under another
// spelling of its path (here a symlinked directory, the way a case-insensitive filesystem
// reads a case-only rename) is the file already in place, not an occupied destination.
func TestMoveOntoAnotherSpellingOfItself(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real", "A.mp3"), "audio")
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := Move(filepath.Join(dir, "link", "A.mp3"), filepath.Join(dir, "real", "A.mp3")); err != nil {
		t.Fatalf("Move: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "real"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "A.mp3" {
		t.Fatalf("real/ holds %v (err %v), want only A.mp3", entries, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "real", "A.mp3")); err != nil || string(b) != "audio" {
		t.Errorf("content = %q (err %v), want it intact", b, err)
	}
}

// TestMoveRefusesAnOccupiedDestination: another file at the destination is never
// overwritten.
func TestMoveRefusesAnOccupiedDestination(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.mp3"), "one")
	writeFile(t, filepath.Join(dir, "b.mp3"), "two")
	if err := Move(filepath.Join(dir, "a.mp3"), filepath.Join(dir, "b.mp3")); !errors.Is(err, ErrExist) {
		t.Fatalf("Move onto another file = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b.mp3")); string(b) != "two" {
		t.Errorf("destination = %q, want it untouched", b)
	}
}

// TestMoveRefusesAHardLink: a hard link at the destination is the same file under another
// entry, so a move onto it is refused rather than taken for the file being in place.
func TestMoveRefusesAHardLink(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "A.mp3"), "audio")
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{filepath.Join(dir, "b", "A.mp3"), filepath.Join(dir, "a", "B.mp3")} {
		if err := os.Link(filepath.Join(dir, "a", "A.mp3"), dst); err != nil {
			t.Skipf("hard link: %v", err)
		}
		if err := Move(filepath.Join(dir, "a", "A.mp3"), dst); !errors.Is(err, ErrExist) {
			t.Errorf("Move onto the hard link %s = %v, want ErrExist", dst, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "A.mp3")); err != nil {
		t.Errorf("source after the refusals: %v, want it in place", err)
	}
}

// TestMoveRefusesAHardLinkSpelledAlike: two hard links in one folder whose names differ
// only by case are two entries, so a move of one onto the other is refused and both stay.
func TestMoveRefusesAHardLinkSpelledAlike(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "A.mp3"), "audio")
	if err := os.Link(filepath.Join(dir, "A.mp3"), filepath.Join(dir, "a.mp3")); err != nil {
		t.Skipf("hard link spelled alike: %v (a filesystem that folds case)", err)
	}
	if err := Move(filepath.Join(dir, "A.mp3"), filepath.Join(dir, "a.mp3")); !errors.Is(err, ErrExist) {
		t.Fatalf("Move onto the other link = %v, want ErrExist", err)
	}
	for _, name := range []string{"A.mp3", "a.mp3"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s after the refusal: %v, want it kept", name, err)
		}
	}
}

// TestSameName: names match without regard to case or Unicode normalization form, the way
// a case-insensitive filesystem that stores either form reads them.
func TestSameName(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"J.R.R. Tolkien", "j.r.r. tolkien", true}, {"\u00c9dith", "E\u0301dith", true},
		{"\u00e9dith", "E\u0301DITH", true}, {"Edith", "\u00c9dith", false}, {"a.mp3", "b.mp3", false},
	} {
		if got := sameName(c.a, c.b); got != c.want {
			t.Errorf("sameName(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestFreeNameIsShortAndKeepsTheExtension: a temporary name is short whatever the name it
// stands in for, so it fits where that name fit, and keeps an audio extension so a file
// a crash leaves under it is still audio to a scan.
func TestFreeNameIsShortAndKeepsTheExtension(t *testing.T) {
	dir := t.TempDir()
	name, err := FreeName(dir, ".m4b")
	if err != nil || filepath.Dir(name) != dir || filepath.Ext(name) != ".m4b" || len(filepath.Base(name)) > 40 {
		t.Errorf("FreeName = %q (err %v), want a short free .m4b name in %s", name, err, dir)
	}
	if _, err := os.Lstat(name); !os.IsNotExist(err) {
		t.Errorf("FreeName gave %s, which exists", name)
	}
}

// foldFS is a filesystem that matches names without regard to case or Unicode form, as
// NTFS and APFS do, and keeps the spelling a name was created or last renamed with.
// ignoreRename makes a rename between two spellings of one name a silent no-op, as some
// filesystems do.
type foldFS struct {
	dirs         map[string][]string // a folder's entries by their stored spelling
	ignoreRename bool
}

func (f *foldFS) find(dir, name string) (string, bool) {
	for _, e := range f.dirs[dir] {
		if sameName(e, name) {
			return e, true
		}
	}
	return "", false
}

// resolve maps a path to the stored spelling of each of its parts below "/".
func (f *foldFS) resolve(p string) (string, bool) {
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(filepath.ToSlash(p), "/"), "/") {
		e, ok := f.find(cur, part)
		if !ok {
			return "", false
		}
		cur = filepath.Join(cur, e)
	}
	return cur, true
}

func (f *foldFS) Lstat(p string) error {
	if _, ok := f.resolve(p); !ok {
		return os.ErrNotExist
	}
	return nil
}

func (f *foldFS) ReadDir(p string) ([]string, error) {
	r, ok := f.resolve(p)
	if !ok {
		return nil, os.ErrNotExist
	}
	return slices.Clone(f.dirs[r]), nil
}

func (f *foldFS) Rename(from, to string) error {
	rf, ok := f.resolve(from)
	if !ok {
		return os.ErrNotExist
	}
	parent, oldName, newName := filepath.Dir(rf), filepath.Base(rf), filepath.Base(to)
	if e, taken := f.find(parent, newName); taken && e != oldName {
		return os.ErrExist
	}
	if f.ignoreRename && sameName(oldName, newName) {
		return nil
	}
	f.dirs[parent] = slices.DeleteFunc(f.dirs[parent], func(e string) bool { return e == oldName })
	f.dirs[parent] = append(f.dirs[parent], newName)
	for d, entries := range f.dirs {
		if d == rf || strings.HasPrefix(d, rf+"/") {
			delete(f.dirs, d)
			f.dirs[filepath.Join(parent, newName)+strings.TrimPrefix(d, rf)] = entries
		}
	}
	return nil
}

// TestSpellerRespellsFoldersBelowItsRoot: a folder a path names in another spelling is
// renamed to it, directly or through a temporary name where the filesystem ignores the
// direct rename; the root and the folders above it, absent folders and a folder already
// spelled so are left alone, and each rename is reported.
func TestSpellerRespellsFoldersBelowItsRoot(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		fs := &foldFS{ignoreRename: ignore, dirs: map[string][]string{
			"/":                                 {"music"},
			"/music":                            {"j.r.r. tolkien", "Other"},
			"/music/j.r.r. tolkien":             {"E\u0301dith"},
			"/music/j.r.r. tolkien/E\u0301dith": {},
			"/music/Other":                      {},
		}}
		var renamed [][2]string
		sp := newSpeller("/Music", fs, func(from, to string) { renamed = append(renamed, [2]string{from, to}) })
		if err := sp.Respell("/Music/J.R.R. Tolkien/\u00c9dith/Book"); err != nil {
			t.Fatalf("Respell (ignore %v): %v", ignore, err)
		}
		if err := sp.Respell("/Music/Other/Sub"); err != nil {
			t.Fatalf("Respell: %v", err)
		}
		want := [][2]string{
			{"/Music/j.r.r. tolkien", "/Music/J.R.R. Tolkien"},
			{"/Music/J.R.R. Tolkien/E\u0301dith", "/Music/J.R.R. Tolkien/\u00c9dith"},
		}
		if !slices.Equal(renamed, want) {
			t.Errorf("ignore %v: renamed = %q, want %q", ignore, renamed, want)
		}
		if got := fs.dirs["/"]; !slices.Equal(got, []string{"music"}) {
			t.Errorf("ignore %v: root's own spelling changed to %q", ignore, got)
		}
		if got := fs.dirs["/music/J.R.R. Tolkien"]; !slices.Equal(got, []string{"\u00c9dith"}) {
			t.Errorf("ignore %v: entries = %q, want the new spellings", ignore, got)
		}
	}
}
