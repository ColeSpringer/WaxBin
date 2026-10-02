package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestUpgradeShowsAnItemsEncodings: an item holding another encoding is listed as one
// item with each file under it by pid, the form rm --file takes.
func TestUpgradeShowsAnItemsEncodings(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	signal := testaudio.ReferenceSignal(44100, 3*time.Second)
	for name, format := range map[string]string{"song.flac": "flac", "song.mp3": "mp3"} {
		p := filepath.Join(root, format, name)
		writeTestFile(t, p, testaudio.EncodeAs(t, format, "", 44100, signal))
		if _, err := meta.NewWriter().Apply(ctx, p, []meta.TagEdit{
			{Key: "TITLE", Values: []string{"Song"}},
			{Key: "MUSICBRAINZ_TRACKID", Values: []string{"8f1c9b2a-1111-4222-8333-944455556666"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	refs, err := lib.ItemFiles(ctx, mustOnlyItem(t, ctx, lib))
	if err != nil || len(refs) != 2 {
		t.Fatalf("files = %+v (err %v), want two", refs, err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "upgrade"})
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "one item: Song") {
		t.Errorf("output = %q, want the group named as one item", out)
	}
	for _, r := range refs {
		if !strings.Contains(out, "file "+string(r.FilePID)) {
			t.Errorf("output = %q, want file %s listed", out, r.FilePID)
		}
	}
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// mustOnlyItem returns the catalog's single item.
func mustOnlyItem(t *testing.T, ctx context.Context, lib *waxbin.Library) model.PID {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want one", len(items), err)
	}
	return items[0].PID
}
