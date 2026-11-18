package scan

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// TestResolveCoverSaysWhetherItLooked separates a finding of no cover from a lookup that
// could not finish. A folder with no cover file in it is a finding. A cover file that is
// there and gives no picture is not, whether it is empty, cut short or will not read:
// that is a cover caught mid-copy as much as a broken one.
func TestResolveCoverSaysWhetherItLooked(t *testing.T) {
	t.Parallel()
	audio := func(dir string) string { return filepath.Join(dir, "song.mp3") }

	bare := t.TempDir()
	if got, looked := resolveCover(audio(bare), nil, newArtCache()); got != nil || !looked {
		t.Errorf("a folder with no cover = %v (looked %v), want a finding of none", got, looked)
	}

	for name, bytes := range map[string][]byte{"empty": nil, "cut short": []byte("\xff\xd8\xff")} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, looked := resolveCover(audio(dir), nil, newArtCache()); got != nil || looked {
			t.Errorf("a cover file that is %s = %v (looked %v), want no finding", name, got, looked)
		}
	}

	gone := filepath.Join(t.TempDir(), "missing")
	if got, looked := resolveCover(audio(gone), nil, newArtCache()); got != nil || looked {
		t.Errorf("a folder that will not list = %v (looked %v), want no finding", got, looked)
	}

	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("file permissions cannot make a cover unreadable here")
	}
	shut := t.TempDir()
	cover := filepath.Join(shut, "cover.jpg")
	writeJPEG(t, cover, 64, 64)
	if err := os.Chmod(cover, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cover, 0o644) })
	if got, looked := resolveCover(audio(shut), nil, newArtCache()); got != nil || looked {
		t.Errorf("a cover that will not read = %v (looked %v), want no finding", got, looked)
	}
	// A lower-priority cover that does read is the cover all the same.
	writeJPEG(t, filepath.Join(shut, "folder.jpg"), 32, 32)
	if got, looked := resolveCover(audio(shut), nil, newArtCache()); got == nil || got.Width != 32 || !looked {
		t.Errorf("with a readable folder.jpg beside it = %v (looked %v), want that picture", got, looked)
	}
}

// coverPuts records what each track put said about its cover.
type coverPuts struct {
	model.Catalog
	noCover []bool
}

func (c *coverPuts) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	c.noCover = append(c.noCover, in.NoCover && in.CoverArt == nil)
	return c.Catalog.PutScannedTrack(ctx, in)
}

// TestACoverYetToArriveIsNoFinding: a read of a file whose folder cover follows it, as an
// import's does, says nothing about the cover, where the same read otherwise says the
// file has none.
func TestACoverYetToArriveIsNoFinding(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	path := filepath.Join(root, "song.mp3")
	writeMP3(t, path, "Song", 1)
	puts := &coverPuts{Catalog: st}
	sc := New(puts, meta.NewReader(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{CoverPending: true}); err != nil {
		t.Fatalf("scan with the cover pending: %v", err)
	}
	if _, _, err := sc.ScanFileWith(ctx, lib, path, FileOptions{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(puts.noCover) != 2 || puts.noCover[0] || !puts.noCover[1] {
		t.Errorf("no-cover findings = %v, want none while the cover is pending and one after", puts.noCover)
	}
}

// TestAFileWhoseTagsWereNotReadIsNoFinding: a container the reader has no parser for
// comes back with no tags read, so its carrying no picture says nothing about a cover
// the catalog took from its tags before.
func TestAFileWhoseTagsWereNotReadIsNoFinding(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	path := filepath.Join(root, "song.mp3")
	writeMP3(t, path, "Song", 1)
	puts := &coverPuts{Catalog: st}
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, _, err := New(puts, unparsedReader{}, quiet).ScanFileWith(ctx, lib, path, FileOptions{}); err != nil {
		t.Fatalf("scan with no parser: %v", err)
	}
	if len(puts.noCover) != 1 || puts.noCover[0] {
		t.Errorf("no-cover findings = %v, want none from a file whose tags were not read", puts.noCover)
	}
}
