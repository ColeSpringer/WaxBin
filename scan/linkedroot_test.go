package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestScanFollowsALinkedRoot: a library whose root is a link to a folder is walked as
// that folder, every file spelled under the root as registered, and later scans of the
// whole root and of a sub-path under it read the catalog's rows as the same files.
func TestScanFollowsALinkedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "lib")
	writeMP3(t, filepath.Join(real, "a", "1.mp3"), "One", 1)
	writeMP3(t, filepath.Join(real, "b", "2.mp3"), "Two", 2)
	testpath.Symlink(t, real, link)
	st, lib, sc, cr, _ := fastPathFixtureAt(t, link)
	ctx := context.Background()

	r := scanAll(t, sc, lib, false)
	if r.AudioFiles != 2 || r.ItemsCreated != 2 || r.FilesSeen != 2 || r.Links != 0 {
		t.Fatalf("scan = %+v, want both files under the linked root created", r)
	}
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 2 {
		t.Fatalf("items = %d (err %v), want 2", len(items), err)
	}
	for _, it := range items {
		if !strings.HasPrefix(it.DisplayPath, link+string(filepath.Separator)) {
			t.Errorf("item at %s, want it spelled under the root %s", it.DisplayPath, link)
		}
	}
	if n := countAudio(ctx, link); n != 2 {
		t.Errorf("countAudio(link) = %d, want 2", n)
	}

	reads := cr.reads
	if r = scanAll(t, sc, lib, false); r.Unchanged != 2 || cr.reads != reads {
		t.Errorf("rescan = %+v with %d more reads, want both unchanged and nothing re-read", r, cr.reads-reads)
	}
	r, err = sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(link, "a")}, nil)
	if err != nil || r.AudioFiles != 1 || r.Unchanged != 1 {
		t.Errorf("sub-path scan = %+v (err %v), want the one file under it unchanged", r, err)
	}

	if err := os.Remove(filepath.Join(real, "b", "2.mp3")); err != nil {
		t.Fatal(err)
	}
	if r = scanAll(t, sc, lib, false); r.Missing != 1 || r.Unchanged != 1 {
		t.Errorf("scan after a removal = %+v, want one missing and one unchanged", r)
	}
}

// TestScanCountsLinksBelowTheRootAndFollowsNone: a link below the root, to a folder or
// to a file, is counted and not followed, and a sub-path that reaches its folder through
// such a link walks nothing and says so, since the whole-library walk never catalogs what
// it holds; a single file behind one is refused outright.
func TestScanCountsLinksBelowTheRootAndFollowsNone(t *testing.T) {
	t.Parallel()
	_, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	elsewhere := t.TempDir()
	writeMP3(t, filepath.Join(elsewhere, "folder", "sub", "x.mp3"), "Linked Folder", 1)
	writeMP3(t, filepath.Join(elsewhere, "y.mp3"), "Linked File", 2)
	writeMP3(t, filepath.Join(root, "own", "1.mp3"), "Own", 3)
	testpath.Symlink(t, filepath.Join(elsewhere, "folder"), filepath.Join(root, "linked"))
	testpath.Symlink(t, filepath.Join(elsewhere, "y.mp3"), filepath.Join(root, "y.mp3"))

	r := scanAll(t, sc, lib, false)
	if r.Links != 2 || r.AudioFiles != 1 || r.ItemsCreated != 1 || r.FilesSeen != 1 {
		t.Fatalf("scan = %+v, want two links counted and only the library's own file cataloged", r)
	}
	for _, sub := range []string{
		filepath.Join(root, "linked"),
		filepath.Join(root, "linked", "sub"),
		filepath.Join(root, "linked", "never", "was"),
		filepath.Join("linked", "sub"),
		filepath.Join(root, "y.mp3"),
	} {
		r, err := sc.Scan(ctx, Request{Library: lib, SubPath: sub}, nil)
		if err != nil || !r.SubPathLinked || r.FilesSeen != 0 || r.Links != 0 || r.SubPathGone {
			t.Errorf("scan of %s = %+v (err %v), want nothing walked and the sub-path reported as behind a link", sub, r, err)
		}
	}
	if r, err := sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(root, "own")}, nil); err != nil || r.Unchanged != 1 || r.SubPathLinked {
		t.Errorf("scan of a folder reached through no link = %+v (err %v), want its file unchanged", r, err)
	}
	if _, err := sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(elsewhere, "folder")}, nil); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "as registered") {
		t.Errorf("scan of a folder outside the root: %v, want CodeInvalid saying a sub-path is spelled under the root as registered", err)
	}
	for _, path := range []string{filepath.Join(root, "linked", "sub", "x.mp3"), filepath.Join(root, "y.mp3")} {
		if _, err := sc.ScanFile(ctx, lib, path); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
			t.Errorf("ScanFile(%s) = %v, want CodeInvalid naming the link", path, err)
		}
	}
	if r, err := sc.ScanFile(ctx, lib, filepath.Join(root, "own", "1.mp3")); err != nil || r.Unchanged != 1 {
		t.Errorf("ScanFile of a file reached through no link = %+v (err %v), want it unchanged", r, err)
	}
}

// TestScanOfASubPathBehindALinkReconcilesWhatItHeld: a folder cataloged before it became
// a link holds rows the library no longer sees; a sub-path scan of it marks them missing,
// as the next whole-library scan would, rather than failing.
func TestScanOfASubPathBehindALinkReconcilesWhatItHeld(t *testing.T) {
	t.Parallel()
	_, lib, sc, _, root := fastPathFixture(t)
	ctx := context.Background()
	writeMP3(t, filepath.Join(root, "a", "x.mp3"), "Was Here", 1)
	writeMP3(t, filepath.Join(root, "b", "y.mp3"), "Stays", 2)
	if r := scanAll(t, sc, lib, false); r.ItemsCreated != 2 {
		t.Fatalf("scan = %+v, want two files created", r)
	}
	elsewhere := t.TempDir()
	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(elsewhere, "a")); err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, filepath.Join(elsewhere, "a"), filepath.Join(root, "a"))
	r, err := sc.Scan(ctx, Request{Library: lib, SubPath: filepath.Join(root, "a")}, nil)
	if err != nil || !r.SubPathLinked || r.Missing != 1 {
		t.Fatalf("sub-path scan of the folder now behind a link = %+v (err %v), want its row marked missing", r, err)
	}
	if r = scanAll(t, sc, lib, false); r.Links != 1 || r.Missing != 0 || r.Unchanged != 1 {
		t.Errorf("whole-library scan after = %+v, want the link counted, the other file unchanged and nothing more to reconcile", r)
	}
}
