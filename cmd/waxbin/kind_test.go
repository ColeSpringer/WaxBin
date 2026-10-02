package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

// kindCLIFixture writes two chapters of one book into a fresh root, scans them as tracks
// and returns the catalog, the root and the chapters' pids.
func kindCLIFixture(t *testing.T) (string, string, []model.PID) {
	t.Helper()
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for n := 1; n <= 2; n++ {
		path := filepath.Join(root, "Persuasion", fmt.Sprintf("%02d.mp3", n))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
			Title: fmt.Sprintf("Chapter %02d", n), Artist: "Jane Austen", Album: "Persuasion", Track: n,
			Audio: testaudio.AudioWithSeed(byte(n)),
		}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	var pids []model.PID
	for n := 1; n <= 2; n++ {
		items, err := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, fmt.Sprintf("Chapter %02d", n)).Build(), "")
		if err != nil || len(items) != 1 {
			t.Fatalf("query: %d items (err %v)", len(items), err)
		}
		pids = append(pids, items[0].PID)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return db, root, pids
}

// runKindText runs the kind command with text output.
func runKindText(t *testing.T, db, root string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "kind"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}

// TestKindCommandMergesIntoABook: `kind` turns the named tracks into the book their tags
// name once --yes confirms more than one item, as edit asks, and says which item it kept
// and which it absorbed, in text and in JSON.
func TestKindCommandMergesIntoABook(t *testing.T) {
	db, root, pids := kindCLIFixture(t)
	out, err := runKindText(t, db, root, string(pids[0]), string(pids[1]), "--to", "book")
	if err != nil || !strings.Contains(out, "--yes") {
		t.Fatalf("two pids without --yes = %q (err %v), want the preview gate", out, err)
	}
	out, err = runKindText(t, db, root, string(pids[0]), string(pids[1]), "--to", "book", "--yes")
	if err != nil {
		t.Fatalf("kind: %v", err)
	}
	for _, want := range []string{"converted " + string(pids[0]), "absorbed  " + string(pids[1]) + " -> " + string(pids[0])} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q, want a line %q", out, want)
		}
	}

	out, err = runCLIJSON(t, db, root, "kind", string(pids[0]), "--to", "track", "--force")
	if err != nil {
		t.Fatalf("kind --json: %v", err)
	}
	var env struct {
		Data waxbin.KindReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if len(env.Data.Converted) != 1 || env.Data.Converted[0] != pids[0] || len(env.Data.Created) != 1 {
		t.Errorf("report = %+v, want the book split back with one track created", env.Data)
	}
}

// TestKindCommandSelects: `kind` takes edit's selection flags, previews a selection and
// changes it only with --yes.
func TestKindCommandSelects(t *testing.T) {
	db, root, pids := kindCLIFixture(t)
	out, err := runKindText(t, db, root, "--album", "Persuasion", "--to", "book", "--dry-run")
	if err != nil {
		t.Fatalf("kind --dry-run: %v", err)
	}
	if !strings.Contains(out, "2 item(s) would change") {
		t.Errorf("dry run = %q, want the two chapters previewed", out)
	}
	if out, err = runKindText(t, db, root, "--album", "Persuasion", "--to", "book"); err != nil || !strings.Contains(out, "--yes") {
		t.Errorf("selection without --yes = %q (err %v), want the preview gate", out, err)
	}
	if out, err = runKindText(t, db, root, "--album", "Persuasion", "--to", "book", "--yes"); err != nil {
		t.Fatalf("kind --yes: %v", err)
	}
	if !strings.Contains(out, "converted "+string(pids[0])) && !strings.Contains(out, "converted "+string(pids[1])) {
		t.Errorf("output %q, want a chapter converted", out)
	}
	if _, err := runKindText(t, db, root, string(pids[0]), "--to", "episode"); err == nil {
		t.Error("kind --to episode succeeded, want it refused")
	}
}

// TestKindCommandRunsInTheServer: with a server holding the catalog, `kind` submits the
// change as a job there and prints the report the job recorded.
func TestKindCommandRunsInTheServer(t *testing.T) {
	db, root, pids := kindCLIFixture(t)
	ctx := context.Background()
	lib := serveKindCatalog(t, db, root)
	out, err := runKindText(t, db, root, string(pids[0]), "--to", "book")
	if err != nil {
		t.Fatalf("kind through the server: %v", err)
	}
	if !strings.Contains(out, "converted "+string(pids[0])) {
		t.Errorf("output %q, want the chapter converted", out)
	}
	if v, err := lib.Get(ctx, pids[0]); err != nil || v.Kind != model.KindBook {
		t.Errorf("server's item = %+v (err %v), want the book", v, err)
	}
	jobs, err := lib.Jobs(ctx, 5)
	if err != nil || len(jobs) == 0 || jobs[0].Kind != "set-kind" || jobs[0].Owner == "" {
		t.Errorf("jobs = %+v (err %v), want the server's set-kind job", jobs, err)
	}
}

// TestKindCommandReportsAPartialChange: a file that fails partway still leaves the change
// it made on screen before the error, run directly or through a server.
func TestKindCommandReportsAPartialChange(t *testing.T) {
	for _, served := range []bool{false, true} {
		db, root, pids := kindCLIFixture(t)
		two := filepath.Join(root, "Persuasion", "02.mp3")
		if err := os.Remove(two); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(two, 0o755); err != nil {
			t.Fatal(err)
		}
		if served {
			serveKindCatalog(t, db, root)
		}
		out, err := runKindText(t, db, root, string(pids[0]), string(pids[1]), "--to", "book", "--yes")
		if err == nil {
			t.Fatalf("served %v: kind succeeded, want the unreadable chapter to fail it", served)
		}
		if !strings.Contains(out, "converted "+string(pids[0])) {
			t.Errorf("served %v: output %q, want chapter 1's conversion reported before the error", served, out)
		}
	}
}

// serveKindCatalog serves the fixture's catalog until the test ends.
func serveKindCatalog(t *testing.T, db, root string) *waxbin.Library {
	t.Helper()
	return serveCatalog(t, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
}
