package waxbin_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestReadOnlyOpenSkipsTheLinkOverlapCheck: a read-only open registers no root and can
// neither scan nor organize, so it is not refused over a root reaching another through
// a link, and it pays no stat of the roots for it.
func TestReadOnlyOpenSkipsTheLinkOverlapCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	writeFile(t, filepath.Join(real, ".keep"), nil)
	testpath.Symlink(t, real, link)
	db := filepath.Join(t.TempDir(), "catalog.db")
	rw := openManaged(t, ctx, db, real)
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true,
		Roots: []config.Root{{Path: real, Mode: model.ModeManaged}, {Path: link, Mode: model.ModeManaged}}})
	if err != nil {
		t.Fatalf("read-only open with two roots reaching one folder: %v, want it allowed", err)
	}
	_ = ro.Close()
}

// TestRestoreRefusesAFolderThatBecameALink: a trash entry whose folder is now reached
// through a link below the root is refused before the file moves, since the rescan that
// would re-link it refuses such a sub-path; the entry stays restorable.
func TestRestoreRefusesAFolderThatBecameALink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Artist", "song.mp3")
	writeFile(t, path, testaudio.BuildMP3("Song", "Artist", "Album", 1))
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want 1", len(items), err)
	}
	deleteFiles(t, lib, items[0].FilePID)
	// The delete pruned the emptied folder; a link to a folder elsewhere takes its place.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(elsewhere, "Artist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "Artist")); err != nil {
		t.Fatal(err)
	}
	testpath.Symlink(t, filepath.Join(elsewhere, "Artist"), filepath.Join(root, "Artist"))
	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash = %+v (err %v), want one entry", entries, err)
	}
	err = lib.RestoreTrash(ctx, entries[0].PID)
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
		t.Fatalf("restore into a folder reached through a link = %v, want CodeInvalid naming the link", err)
	}
	if fileExists(filepath.Join(elsewhere, "Artist", "song.mp3")) || !fileExists(string(entries[0].TrashPath)) {
		t.Error("the file moved out of the trash although the restore was refused")
	}
	if again, err := lib.Trash(ctx, false, 0); err != nil || len(again) != 1 {
		t.Errorf("trash after the refusal = %+v (err %v), want the entry still there", again, err)
	}
}

// TestOpenRefusesAConfiguredRootOverlappingAStoredOne: a configured root that overlaps a
// root added at runtime and stored, as spelled or through a link, is refused at a
// read-write open naming both, as two configured roots overlapping are; a configured
// root that is a stored row is that row.
func TestOpenRefusesAConfiguredRootOverlappingAStoredOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	writeFile(t, filepath.Join(a, ".keep"), nil)
	writeFile(t, filepath.Join(b, "sub", ".keep"), nil)
	link := filepath.Join(base, "link")
	testpath.Symlink(t, b, link)
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openManaged(t, ctx, db, a)
	if _, err := lib.AddRoot(ctx, config.Root{Path: b, Mode: model.ModeManaged}); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	open := func(roots ...string) error {
		opts := waxbin.Options{DBPath: db}
		for _, r := range roots {
			opts.Roots = append(opts.Roots, config.Root{Path: r, Mode: model.ModeManaged, Profile: "waxbin-native"})
		}
		// Not seeded: reopens the catalog openManaged seeded above.
		l, err := waxbin.Open(ctx, opts)
		if err == nil {
			err = l.Close()
		}
		return err
	}
	if err := open(a, filepath.Join(b, "sub")); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), b) {
		t.Errorf("open with a configured root inside a stored one = %v, want CodeInvalid naming the stored root", err)
	}
	if err := open(a, link); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("open with a configured link to a stored root's folder = %v, want CodeInvalid naming the link", err)
	}
	if err := open(a, b); err != nil {
		t.Errorf("open with the stored root configured = %v, want it to be the same row", err)
	}
	if err := open(a); err != nil {
		t.Errorf("open leaving the stored root to the catalog = %v, want it allowed", err)
	}
	// A library relocated onto a link to its own folder, with the configuration still
	// naming the folder: the two are one folder, and the open says so.
	rw := openManaged(t, ctx, db, a)
	libs, err := rw.Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range libs {
		if l.DisplayRoot == b {
			if err := rw.RelocateRoot(ctx, l.PID, link); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := open(a, b); !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("open configuring the folder a relocated library reaches through its link = %v, want CodeInvalid naming the link", err)
	}
}

// TestLinkOverlapIsSeenBeforeTheFolderExists: a podcast dir not made yet under a link to
// a root, and an absent root that is a dangling link whose text reaches a root, are
// refused where they will be rather than compared as spelled.
func TestLinkOverlapIsSeenBeforeTheFolderExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := t.TempDir()
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	writeFile(t, filepath.Join(real, ".keep"), nil)
	testpath.Symlink(t, real, link)
	l, err := waxbin.OpenSeeded(t, ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots:    []config.Root{{Path: real, Mode: model.ModeManaged}},
		Podcasts: config.PodcastConfig{Dir: filepath.Join(link, "podcasts")}})
	if err == nil {
		_ = l.Close()
	}
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("open with a podcast dir not made yet under a link to a root = %v, want CodeInvalid naming the link", err)
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), real)
	dangling := filepath.Join(base, "later")
	testpath.Symlink(t, filepath.Join(real, "later"), dangling)
	_, err = lib.AddRoot(ctx, config.Root{Path: dangling, Mode: model.ModeManaged}, waxbin.AllowAbsent())
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), "through a link") {
		t.Errorf("AddRoot of a dangling link whose text reaches a root = %v, want CodeInvalid naming the link", err)
	}
}
