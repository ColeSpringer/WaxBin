package fsx

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/pathx"
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
// filesystems do. Its keys are slash paths on every OS; the Speller's native paths are
// mapped onto them.
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
		if part == "" {
			continue
		}
		e, ok := f.find(cur, part)
		if !ok {
			return "", false
		}
		cur = path.Join(cur, e)
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
	parent, oldName, newName := path.Dir(rf), path.Base(rf), path.Base(filepath.ToSlash(to))
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
			f.dirs[path.Join(parent, newName)+strings.TrimPrefix(d, rf)] = entries
		}
	}
	return nil
}

// TestSpelledReadsTheFolderListings: a path is spelled as listed when each folder below the
// root lists the next name exactly, whatever spelling the filesystem would resolve; the
// root's own spelling is not judged, and a path outside the root is judged by its last name.
func TestSpelledReadsTheFolderListings(t *testing.T) {
	fs := &foldFS{dirs: map[string][]string{
		"/":                         {"music", "elsewhere"},
		"/music":                    {"Author", "same", "both", "Both"},
		"/music/Author":             {"E\u0301dith"},
		"/music/Author/E\u0301dith": {"x.mp3"},
		"/music/same":               {"x.mp3"},
		"/music/both":               {"x.mp3"},
		"/music/Both":               {"x.mp3"},
		"/elsewhere":                {"x.mp3"},
	}}
	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{"/music", "/music/Author/E\u0301dith/x.mp3", true},
		{"/music", "/music/author/E\u0301dith/x.mp3", false},
		{"/music", "/music/Author/\u00c9dith/x.mp3", false},
		{"/music", "/music/same/x.mp3", true},
		{"/music", "/music/same/X.mp3", false},
		{"/music", "/music/both/x.mp3", true},
		{"/music", "/music/Both/x.mp3", true},
		{"/music", "/music/none/x.mp3", false},
		{"/Music", "/Music/same/x.mp3", true},
		{"/", "/music/same/x.mp3", true},
		{"/", "/Music/same/x.mp3", false},
		{"/music", "/elsewhere/x.mp3", true},
		{"/music", "/elsewhere/X.mp3", false},
	} {
		if got := newLister(fs).Spelled(filepath.FromSlash(tc.root), filepath.FromSlash(tc.path)); got != tc.want {
			t.Errorf("Spelled(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

// TestSpelledOnDisk: a file is spelled as listed, an absent one is not, and nor is another
// spelling of a listed name, whether or not the filesystem would resolve it.
func TestSpelledOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "x.mp3"), "audio")
	for path, want := range map[string]bool{
		filepath.Join(dir, "a", "x.mp3"): true,
		filepath.Join(dir, "a", "y.mp3"): false,
		filepath.Join(dir, "a", "X.mp3"): false,
		filepath.Join(dir, "A", "x.mp3"): false,
	} {
		if got := NewLister().Spelled(dir, path); got != want {
			t.Errorf("Spelled(%q) = %v, want %v", path, got, want)
		}
	}
}

// TestSpellerRenamesBackWhenTheCatalogRefuses: a folder rename the catalog cannot follow is
// undone and the move fails, with the folder left unchecked so a later move tries again.
func TestSpellerRenamesBackWhenTheCatalogRefuses(t *testing.T) {
	fs := &foldFS{dirs: map[string][]string{"/": {"music"}, "/music": {"author"}, "/music/author": {}}}
	var renamed [][2]string
	refuse := true
	sp := newSpeller("/music", fs, func(from, to string) error {
		if refuse {
			refuse = false
			return errors.New("catalog refused")
		}
		renamed = append(renamed, [2]string{filepath.ToSlash(from), filepath.ToSlash(to)})
		return nil
	})
	if err := sp.Respell("/music/Author/Book"); err == nil || err.Error() != "catalog refused" {
		t.Fatalf("Respell = %v, want the catalog's error", err)
	}
	if got := fs.dirs["/music"]; !slices.Equal(got, []string{"author"}) || len(renamed) != 0 {
		t.Fatalf("after the refusal: folders = %q, renamed = %q; want author kept and nothing reported", got, renamed)
	}
	if err := sp.Respell("/music/Author/Book"); err != nil {
		t.Fatalf("Respell again: %v", err)
	}
	if got := fs.dirs["/music"]; !slices.Equal(got, []string{"Author"}) || !slices.Equal(renamed, [][2]string{{"/music/author", "/music/Author"}}) {
		t.Errorf("after the retry: folders = %q, renamed = %q; want Author, reported once", got, renamed)
	}
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
		sp := newSpeller("/Music", fs, func(from, to string) error {
			renamed = append(renamed, [2]string{filepath.ToSlash(from), filepath.ToSlash(to)})
			return nil
		})
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

// TestOccupant: what stands at a move's destination is its exact entry, an entry named
// apart only by case, or a hard link of the source, and never the source's own entry under
// another spelling, even when that entry is listed before a sibling named like it.
func TestOccupant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "Song.mp3"), "src")
	writeFile(t, filepath.Join(dir, "b", "Taken.mp3"), "other")
	src := filepath.Join(dir, "a", "Song.mp3")
	ls := NewLister()
	for _, c := range []struct {
		dst, want string
		exact     bool
	}{
		{filepath.Join(dir, "b", "Free.mp3"), "", false},
		{filepath.Join(dir, "c", "Free.mp3"), "", false},
		{filepath.Join(dir, "b", "Taken.mp3"), filepath.Join(dir, "b", "Taken.mp3"), true},
		{filepath.Join(dir, "b", "TAKEN.mp3"), filepath.Join(dir, "b", "Taken.mp3"), false},
		{filepath.Join(dir, "a", "SONG.mp3"), "", false},
	} {
		got, exact, err := ls.Occupant(src, c.dst)
		if err != nil || got != c.want || exact != c.exact {
			t.Errorf("Occupant(%s) = %q, %v, %v; want %q, %v", c.dst, got, exact, err, c.want, c.exact)
		}
	}

	if err := os.Link(src, filepath.Join(dir, "a", "link.mp3")); err != nil {
		t.Skipf("hard link: %v", err)
	}
	if got, exact, _ := NewLister().Occupant(src, filepath.Join(dir, "a", "link.mp3")); got != filepath.Join(dir, "a", "link.mp3") || !exact {
		t.Errorf("Occupant of a hard link = %q, %v; want the link, exact", got, exact)
	}
	writeFile(t, filepath.Join(dir, "a", "song.mp3"), "sibling")
	if b, _ := os.ReadFile(src); string(b) != "src" {
		t.Skip("this filesystem folds case")
	}
	if got, exact, _ := NewLister().Occupant(src, filepath.Join(dir, "a", "SONG.mp3")); got != filepath.Join(dir, "a", "song.mp3") || exact {
		t.Errorf("Occupant beside the source's own entry = %q, %v; want the sibling song.mp3, not exact", got, exact)
	}
}

// BenchmarkOccupantFlatFolder looks for occupants of a new name for each of 4000 files in
// one folder, the work a re-layout of a flat folder makes Hold do.
func BenchmarkOccupantFlatFolder(b *testing.B) {
	dir := b.TempDir()
	const n = 4000
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, "track "+strconv.Itoa(i)+".mp3"), nil, 0o644); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for range b.N {
		ls := NewLister()
		for i := range n {
			if _, _, err := ls.Occupant(filepath.Join(dir, "track "+strconv.Itoa(i)+".mp3"), filepath.Join(dir, "Song "+strconv.Itoa(i)+".mp3")); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// TestFoldNameReadsNamesAsSameNameDoes: two names share pathx.FoldName exactly when
// sameName takes them for one name, through the case folds that are not plain
// lower-casing (the Greek final sigma, the Kelvin sign) and the two Unicode forms of an
// accent, so the listing index and the plan's keys agree with the moves.
func TestFoldNameReadsNamesAsSameNameDoes(t *testing.T) {
	t.Parallel()
	names := []string{"Song.mp3", "SONG.mp3", "song.MP3", "ΟΔΟΣ", "οδος", "οδοσ", "Kelvin", "kelvin",
		"Beyoncé", "Beyoncé", "BEYONCÉ", "Beyonce", "straße", "STRASSE", "a", "b"}
	for _, a := range names {
		for _, b := range names {
			if got, want := pathx.FoldName(a) == pathx.FoldName(b), sameName(a, b); got != want {
				t.Errorf("fold keys of %q and %q equal %v, sameName says %v", a, b, got, want)
			}
		}
	}
}
