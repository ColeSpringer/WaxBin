package audit

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
)

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAuditOrphanSidecarFindsCompanionOnlyFolders: a folder under a library root holding
// companions and nothing but junk besides is reported, as info, by folder. A folder with
// audio, a file it does not know or a subfolder is not, and neither is the root, a folder
// with no companion, or anything in the trash.
func TestAuditOrphanSidecarFindsCompanionOnlyFolders(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, p := range []string{
		"Leftover/cover.jpg", "Leftover/album.nfo", "Leftover/.DS_Store",
		"Mac/folder.jpg", "Mac/._01 Song.mp3",
		"Scanned/01.flac", "Scanned/Scans/page1.jpg",
		"Discs/CD1/01.flac", "Discs/Artwork/front.jpg",
		"Artist/Gone/cover.jpg", "Artist/Kept/01.mp3",
		"Nas/01.flac", "Nas/@eaDir/01.flac/SYNOPHOTO_THUMB_M.jpg",
		"Album/01.flac", "Album/cover.jpg",
		"Notes/cover.jpg", "Notes/notes.md",
		"Artist/artist.jpg", "Artist/Album/01.mp3",
		"Junk/Thumbs.db",
		"cover.jpg",
		".waxbin-trash/01ENTRY/Artist/cover.jpg",
	} {
		touch(t, filepath.Join(root, filepath.FromSlash(p)))
	}
	bare := t.TempDir()
	touch(t, filepath.Join(bare, "cover.jpg"))
	st := &fakeStore{libs: []*model.Library{
		{PID: "L1", Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace},
		{PID: "L2", Root: []byte(bare), DisplayRoot: bare, Mode: model.ModeManaged},
	}}
	rep, err := New(st, nil, nil, nil).Run(context.Background(), Config{Only: []model.AuditCheck{model.CheckOrphanSidecar}})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range findingsFor(rep, model.CheckOrphanSidecar) {
		if f.Severity != model.SeverityInfo {
			t.Errorf("finding %+v is not info", f)
		}
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	if want := []string{filepath.Join(root, "Artist", "Gone"), filepath.Join(root, "Leftover"), filepath.Join(root, "Mac")}; !slices.Equal(paths, want) {
		t.Fatalf("findings for %v, want %v: an album's scans folder, a NAS's thumbnails and junk are no orphans", paths, want)
	}
}

// TestAuditFolderWalkRunsWithIntegrity: the folder walk reads the disk, so a plain audit
// leaves it out, and --integrity or naming the check runs it.
func TestAuditFolderWalkRunsWithIntegrity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	touch(t, filepath.Join(root, "Leftover", "cover.jpg"))
	st := &fakeStore{libs: []*model.Library{{PID: "L1", Root: []byte(root), DisplayRoot: root, Mode: model.ModeInPlace}}}
	for _, tc := range []struct {
		cfg  Config
		want int
	}{
		{Config{}, 0},
		{Config{Integrity: true}, 1},
		{Config{Only: []model.AuditCheck{model.CheckOrphanSidecar}}, 1},
	} {
		rep, err := New(st, nil, nil, nil).Run(context.Background(), tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(findingsFor(rep, model.CheckOrphanSidecar)); got != tc.want {
			t.Errorf("config %+v: %d orphan findings, want %d", tc.cfg, got, tc.want)
		}
	}
}
