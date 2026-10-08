package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRmReasonReachesTheTrash: rm --reason shows the reason in its plan and writes it
// on the trash row `trash list` prints, and a reason the planner refuses is an invalid
// argument.
func TestRmReasonReachesTheTrash(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(filepath.Join(root, "1.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want one", len(items), err)
	}
	pid := string(items[0].PID)
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := runCLIJSON(t, db, root, "rm", pid, "--reason", strings.Repeat("x", 65)); !waxerr.Is(err, waxerr.CodeInvalid) ||
		!strings.Contains(err.Error(), "delete reason") {
		t.Fatalf("rm with an over-long reason = %v, want the planner's CodeInvalid", err)
	}
	if _, err := runCLIJSON(t, db, root, "rm", pid, "--reason", "book-merge", "--permanent"); !waxerr.Is(err, waxerr.CodeInvalid) ||
		!strings.Contains(err.Error(), "delete reason") {
		t.Fatalf("rm --reason --permanent = %v, want the planner's CodeInvalid", err)
	}
	out, err := runCLIJSON(t, db, root, "rm", pid, "--reason", "book-merge")
	if err != nil {
		t.Fatalf("rm dry run: %v (%s)", err, out)
	}
	var plan struct {
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &plan); err != nil || plan.Data.Reason != "book-merge" {
		t.Fatalf("rm plan printed %q (err %v), want reason book-merge", out, err)
	}
	if out, err := runCLIJSON(t, db, root, "rm", pid, "--reason", "book-merge", "--apply"); err != nil {
		t.Fatalf("rm --apply: %v (%s)", err, out)
	}
	out, err = runCLIJSON(t, db, root, "trash", "list")
	if err != nil {
		t.Fatalf("trash list: %v", err)
	}
	var list struct {
		Data []struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Data) != 1 || list.Data[0].Reason != "book-merge" {
		t.Fatalf("trash list printed %q (err %v), want one entry with reason book-merge", out, err)
	}
}
