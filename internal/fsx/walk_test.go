package fsx

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/internal/testpath"
)

// walked records what a walk reported: each path with whether it was a folder or a
// link, in order.
type walked struct {
	path string
	dir  bool
	link bool
}

func walkAll(t *testing.T, walk func(string, fs.WalkDirFunc) error, root string) ([]walked, error) {
	t.Helper()
	var out []walked
	err := walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out = append(out, walked{path, d.IsDir(), d.Type()&fs.ModeSymlink != 0})
		return nil
	})
	return out, err
}

// linkedTree lays out real/a/1.mp3, real/b/2.mp3 and a link lib -> real, skipping the
// test where links cannot be made.
func linkedTree(t *testing.T) (real, link string) {
	t.Helper()
	dir := t.TempDir()
	real, link = filepath.Join(dir, "real"), filepath.Join(dir, "lib")
	writeFile(t, filepath.Join(real, "a", "1.mp3"), "one")
	writeFile(t, filepath.Join(real, "b", "2.mp3"), "two")
	testpath.Symlink(t, real, link)
	return real, link
}

// TestWalkRootFollowsALinkedRoot: a root that is a link to a folder is walked as that
// folder, with every path, the root's own entry included, spelled under the root.
func TestWalkRootFollowsALinkedRoot(t *testing.T) {
	t.Parallel()
	real, link := linkedTree(t)
	got, err := walkAll(t, WalkRoot, link)
	if err != nil {
		t.Fatalf("WalkRoot(link): %v", err)
	}
	want, err := walkAll(t, filepath.WalkDir, real)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		rel, _ := filepath.Rel(real, want[i].path)
		want[i].path = filepath.Join(link, rel)
	}
	if !slices.Equal(got, want) {
		t.Errorf("WalkRoot(link) reported\n%v\nwant the folder's entries under the link\n%v", got, want)
	}
	if len(got) == 0 {
		t.Fatal("WalkRoot(link) reported nothing")
	}
	if got[0].path != link || !got[0].dir {
		t.Errorf("first entry = %+v, want the root itself as a folder at %s", got[0], link)
	}
}

// TestWalkRootIsWalkDirOnAFolder: a root that is a plain folder walks exactly as
// filepath.WalkDir does, and a link below the root is reported as a link with nothing
// under it.
func TestWalkRootIsWalkDirOnAFolder(t *testing.T) {
	t.Parallel()
	real, link := linkedTree(t)
	inside := filepath.Join(real, "linked")
	testpath.Symlink(t, filepath.Join(real, "a"), inside)
	got, err := walkAll(t, WalkRoot, real)
	if err != nil {
		t.Fatalf("WalkRoot(real): %v", err)
	}
	want, err := walkAll(t, filepath.WalkDir, real)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("WalkRoot(real) reported\n%v\nwant WalkDir's\n%v", got, want)
	}
	if i := slices.IndexFunc(got, func(w walked) bool { return w.path == inside }); i < 0 || !got[i].link || got[i].dir {
		t.Errorf("the link below the root was reported as %+v, want a link", got)
	}
	if slices.ContainsFunc(got, func(w walked) bool { return w.path == filepath.Join(inside, "1.mp3") }) {
		t.Error("the link below the root was followed")
	}
	// The same holds under the linked root: a link below it is a link, not walked.
	got, err = walkAll(t, WalkRoot, link)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got, func(w walked) bool { return w.path == filepath.Join(link, "linked", "1.mp3") }) {
		t.Error("the link below the linked root was followed")
	}
}

// TestWalkRootReportsADanglingLinkRoot: a link root whose target is not there is
// reported once as the root with its error, the way WalkDir reports a root it cannot
// stat, and the walk returns that error unless the callback waves it off.
func TestWalkRootReportsADanglingLinkRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "lib")
	testpath.Symlink(t, filepath.Join(dir, "gone"), link)
	calls := 0
	err := WalkRoot(link, func(path string, d fs.DirEntry, err error) error {
		calls++
		if path != link || d != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("reported (%s, %v, %v), want the root with a not-exist error", path, d, err)
		}
		return err
	})
	if calls != 1 || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("walk made %d calls and returned %v, want one call and the not-exist error", calls, err)
	}
	if err := WalkRoot(link, func(string, fs.DirEntry, error) error { return fs.SkipAll }); err != nil {
		t.Errorf("a callback returning SkipAll got %v, want nil", err)
	}
}

// TestWalkRootWalksAFileRoot: a root that is a file is reported once as that file, as
// WalkDir reports it, and a link to a file is reported once as the link it is, not
// followed, since only a link to a folder is walked.
func TestWalkRootWalksAFileRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp3")
	writeFile(t, file, "audio")
	got, err := walkAll(t, WalkRoot, file)
	if err != nil || len(got) != 1 || got[0] != (walked{file, false, false}) {
		t.Fatalf("WalkRoot(file) = %v, %v; want the file once", got, err)
	}
	link := filepath.Join(dir, "link.mp3")
	testpath.Symlink(t, file, link)
	if got, err = walkAll(t, WalkRoot, link); err != nil || len(got) != 1 || got[0] != (walked{link, false, true}) {
		t.Fatalf("WalkRoot(link to a file) = %v, %v; want the link once, not followed", got, err)
	}
}
