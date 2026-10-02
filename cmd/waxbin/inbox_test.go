package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestInboxImportForcesAKind: `inbox import --as book` imports a staged folder's plain
// files as books, each pinned with a kind lock as `import --as` pins one, and --as refuses
// a kind an import cannot force.
func TestInboxImportForcesAKind(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	root, staging := t.TempDir(), t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(staging, "Persuasion", "01.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Title: "Chapter 01", Artist: "Jane Austen",
		Album: "Persuasion", Track: 1, Audio: testaudio.AudioWithSeed(1)}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLIJSON(t, db, root, "inbox", "import", staging, "--as", "episode"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("--as episode = %v, want CodeInvalid", err)
	}
	out, err := runCLIJSON(t, db, root, "inbox", "import", staging, "--as", "book", "--apply")
	if err != nil {
		t.Fatalf("inbox import --as book: %v", err)
	}
	var env struct {
		Data struct {
			Imported int `json:"imported"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Imported != 1 {
		t.Fatalf("report %q (err %v), want one import", out, err)
	}
	ctx := context.Background()
	lib := openCLILib(t, ctx, db, root, true)
	defer lib.Close()
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %d (err %v), want the import", len(books), err)
	}
	rows, err := lib.Provenance(ctx, books[0].PID)
	if err != nil {
		t.Fatal(err)
	}
	locked := false
	for _, r := range rows {
		locked = locked || (r.Field == model.KindLockField && r.Locked)
	}
	if !locked {
		t.Errorf("provenance = %+v, want the forced kind locked", rows)
	}
}
