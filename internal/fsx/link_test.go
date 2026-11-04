package fsx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/internal/testpath"
)

// TestIsLinkSeesASymbolicLink: a symbolic link to a folder or a file is a link, and a
// folder or a file is not, by the stat and by the folder listing's entry alike.
func TestIsLinkSeesASymbolicLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "real", "a.mp3"), "audio")
	testpath.Symlink(t, filepath.Join(dir, "real"), filepath.Join(dir, "dirlink"))
	testpath.Symlink(t, filepath.Join(dir, "real", "a.mp3"), filepath.Join(dir, "filelink"))
	want := map[string]bool{"real": false, "dirlink": true, "filelink": true}
	for name, isLink := range want {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if IsLink(info) != isLink {
			t.Errorf("IsLink(%s) = %t, want %t", name, !isLink, isLink)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if isLink, ok := want[e.Name()]; ok && EntryIsLink(e) != isLink {
			t.Errorf("EntryIsLink(%s) = %t, want %t", e.Name(), !isLink, isLink)
		}
	}
}

// TestResolveLinksCarriesWhatIsNotThere: the folders a path reaches are resolved as far
// as the disk holds them, a dangling link is followed by what it says, and the rest of
// the path is carried as spelled, so a folder not made yet is compared where it will be.
func TestResolveLinksCarriesWhatIsNotThere(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(real, "sub", "a.mp3"), "audio")
	realNow, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, real, filepath.Join(dir, "link"))
	testpath.Symlink(t, "real", filepath.Join(dir, "rel"))
	testpath.Symlink(t, filepath.Join(dir, "link"), filepath.Join(dir, "chain"))
	testpath.Symlink(t, filepath.Join(real, "later"), filepath.Join(dir, "dangling"))
	testpath.Symlink(t, filepath.Join(dir, "dangling"), filepath.Join(dir, "dangling-chain"))
	testpath.Symlink(t, filepath.Join(dir, "loop-b"), filepath.Join(dir, "loop-a"))
	testpath.Symlink(t, filepath.Join(dir, "loop-a"), filepath.Join(dir, "loop-b"))
	for _, tc := range []struct{ name, path, want string }{
		{"a folder", real, realNow},
		{"a link", filepath.Join(dir, "link"), realNow},
		{"a relative link", filepath.Join(dir, "rel"), realNow},
		{"a chain of links", filepath.Join(dir, "chain"), realNow},
		{"a folder below a link", filepath.Join(dir, "link", "sub"), filepath.Join(realNow, "sub")},
		{"a folder not there yet below a link", filepath.Join(dir, "link", "new", "deeper"), filepath.Join(realNow, "new", "deeper")},
		{"a dangling link", filepath.Join(dir, "dangling"), filepath.Join(realNow, "later")},
		{"a folder below a dangling link", filepath.Join(dir, "dangling", "x"), filepath.Join(realNow, "later", "x")},
		{"a chain through a dangling link", filepath.Join(dir, "dangling-chain"), filepath.Join(realNow, "later")},
		{"a path through a file", filepath.Join(real, "sub", "a.mp3", "x"), filepath.Join(realNow, "sub", "a.mp3", "x")},
	} {
		got, err := ResolveLinks(tc.path)
		if err != nil || !pathx.SamePath(got, tc.want) {
			t.Errorf("%s: ResolveLinks(%s) = %q, %v; want %q", tc.name, tc.path, got, err, tc.want)
		}
	}
	if got, err := ResolveLinks(filepath.Join(dir, "loop-a")); err == nil {
		t.Errorf("a loop resolved to %q, want an error", got)
	}
}

// TestPlainPathReadsExtendedLengthSpellings: an extended-length or device spelling of a
// path on a drive or a share is read as the plain path, and any other path is left as it
// is.
func TestPlainPathReadsExtendedLengthSpellings(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`\\?\C:\Music`:           `C:\Music`,
		`\\.\c:\Music`:           `c:\Music`,
		`\\?\C:`:                 `C:`,
		`\\?\UNC\host\share\x`:   `\\host\share\x`,
		`\\.\unc\host\share`:     `\\host\share`,
		`\\?\Volume{1}\Music`:    `\\?\Volume{1}\Music`,
		`\\host\share\x`:         `\\host\share\x`,
		`C:\Music`:               `C:\Music`,
		`/music`:                 `/music`,
		`\\?\`:                   `\\?\`,
		`\\?\UNC`:                `\\?\UNC`,
		`\\?\C`:                  `\\?\C`,
		`\\?x\C:\Music`:          `\\?x\C:\Music`,
		`\\?\C:\Music\..\Books.`: `C:\Music\..\Books.`,
	} {
		if got := plainPath(in); got != want {
			t.Errorf("plainPath(%s) = %s, want %s", in, got, want)
		}
	}
}

// TestCheckAndResolveShareOneProbe: a check and a resolution of one root share one stat,
// so a root that does not answer holds one goroutine however it is asked, and the two
// answer their callers from it.
func TestCheckAndResolveShareOneProbe(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var stats, evals atomic.Int32
		release := make(chan struct{})
		c := RootChecker{
			Stat: func(string) error { stats.Add(1); <-release; return nil },
			Eval: func(p string) (string, error) { evals.Add(1); return p + "/resolved", nil },
		}
		root := "/probe/" + t.Name()
		errs := make(chan error, 2)
		go func() { errs <- c.Check(context.Background(), root) }()
		go func() { _, err := c.Resolve(context.Background(), root); errs <- err }()
		synctest.Wait()
		if n := stats.Load(); n != 1 {
			t.Fatalf("stat ran %d times for a check and a resolution at once, want 1", n)
		}
		for range 2 {
			if err := <-errs; !errors.Is(err, ErrUnverified) {
				t.Errorf("err = %v, want ErrUnverified from the held probe", err)
			}
		}
		close(release)
		synctest.Wait()
		if got, err := c.Resolve(context.Background(), root); err != nil || got != root+"/resolved" {
			t.Errorf("a later resolution = %q, %v; want the eval's answer", got, err)
		}
		if n := evals.Load(); n != 2 {
			t.Errorf("eval ran %d times, want 2 (once per probe)", n)
		}
	})
}

// TestLinkBelowStaysUnderTheRoot: a path outside the root has no link below it, whatever
// the folders above the root are.
func TestLinkBelowStaysUnderTheRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	writeFile(t, filepath.Join(real, "root", "a", ".keep"), "")
	testpath.Symlink(t, real, filepath.Join(dir, "link"))
	root := filepath.Join(dir, "link", "root")
	if link, ok := LinkBelow(root, filepath.Join(dir, "elsewhere", "x")); ok {
		t.Errorf("a path outside the root reported the link %s", link)
	}
	if link, ok := LinkBelow(root, filepath.Join(root, "a")); ok {
		t.Errorf("a folder below the root reported the link %s", link)
	}
}
