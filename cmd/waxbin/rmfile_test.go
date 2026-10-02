package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// TestRmFileDeletesOneCopy: rm --file takes one backing file by pid, so deleting a copy
// trashes only it and leaves its item on the original.
func TestRmFileDeletesOneCopy(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for _, p := range []struct{ dir, title string }{{"a", "Original"}, {"b", "Retagged"}} {
		path := filepath.Join(root, p.dir, "1.mp3")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, testaudio.BuildMP3(p.title, "Artist", "Album", 1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want one", len(items), err)
	}
	item := items[0].PID
	refs, err := lib.ItemFiles(ctx, item)
	if err != nil || len(refs) != 2 {
		t.Fatalf("files = %+v (err %v), want two", refs, err)
	}
	var copyPID model.PID
	for _, r := range refs {
		if r.Role == "alternate" {
			copyPID = r.FilePID
		}
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCLIJSON(t, db, root, "rm", "--file", string(copyPID), "--apply")
	if err != nil {
		t.Fatalf("rm --file: %v (%s)", err, out)
	}
	var rep struct {
		Data struct {
			Trashed int `json:"trashed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Data.Trashed != 1 {
		t.Fatalf("rm --file printed %q (err %v), want one trashed", out, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b", "1.mp3")); !os.IsNotExist(err) {
		t.Errorf("the copy is still on disk (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a", "1.mp3")); err != nil {
		t.Errorf("the original was touched: %v", err)
	}

	lib = openCLILib(t, ctx, db, root, true)
	defer lib.Close()
	it, err := lib.Get(ctx, item)
	if err != nil || it.State != model.StatePresent || it.Title != "Original" {
		t.Errorf("item = %+v (err %v), want present Original", it, err)
	}
}
