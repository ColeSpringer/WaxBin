package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/waxerr"
)

// TestMergeFoldsAndUnfold: merge reports the key it folded, `entity folds` lists it (an
// album key quoted, since it holds separators), and `entity unfold` forgets it, taking
// the key as `entity folds` printed it.
func TestMergeFoldsAndUnfold(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, f := range []struct{ dir, artist string }{{"Help", "Beatles"}, {"Help (2)", "The Beatles"}} {
		path := filepath.Join(root, f.dir, "1.mp3")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, testaudio.BuildMP3WithAudio("Help", f.artist, "Help", 1, testaudio.AudioWithSeed(byte(i+1))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	pidOf := func(table, col, name string) string {
		t.Helper()
		var pid string
		if err := rawDB(t, db).QueryRowContext(ctx, "SELECT pid FROM "+table+" WHERE "+col+" = ?", name).Scan(&pid); err != nil {
			t.Fatalf("pid of %s %q: %v", table, name, err)
		}
		return pid
	}

	out, err := runCLIJSON(t, db, root, "merge", "artist", pidOf("artist", "name", "The Beatles"), pidOf("artist", "name", "Beatles"))
	if err != nil {
		t.Fatalf("merge: %v (%s)", err, out)
	}
	var merged struct {
		Data []struct {
			Folds []string `json:"folds"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &merged); err != nil || len(merged.Data) != 1 || len(merged.Data[0].Folds) != 1 || merged.Data[0].Folds[0] != "beatles" {
		t.Fatalf("merge printed %q (err %v), want the folded key", out, err)
	}
	out, err = runCLIJSON(t, db, root, "entity", "folds", "artist")
	if err != nil {
		t.Fatalf("entity folds: %v", err)
	}
	var folds struct {
		Data []struct {
			Key       string `json:"key"`
			EntityPID string `json:"entityPid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &folds); err != nil || len(folds.Data) != 1 || folds.Data[0].Key != "beatles" {
		t.Fatalf("entity folds printed %q (err %v), want the merged key", out, err)
	}
	if _, err := runCLIJSON(t, db, root, "entity", "unfold", "artist", "beatles"); err != nil {
		t.Fatalf("entity unfold: %v", err)
	}
	if _, err := runCLIJSON(t, db, root, "entity", "unfold", "artist", "beatles"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("a second unfold = %v, want CodeNotFound", err)
	}

	// An album key carries separators, so the text listing quotes it and unfold takes the
	// quoted form back.
	if _, err := runCLIJSON(t, db, root, "merge", "album", pidOf("album", "title", "Help"), secondAlbum(t, db)); err != nil {
		t.Fatalf("album merge: %v", err)
	}
	text, err := runCLIText(t, db, root, "entity", "folds", "album")
	if err != nil {
		t.Fatalf("entity folds album: %v", err)
	}
	var quoted string
	for _, line := range strings.Split(text, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], `"`) {
			quoted = line[:strings.Index(line, `" `)+1]
		}
	}
	if _, err := strconv.Unquote(quoted); err != nil {
		t.Fatalf("entity folds album printed %q, want a quoted key", text)
	}
	if _, err := runCLIJSON(t, db, root, "entity", "unfold", "album", quoted); err != nil {
		t.Fatalf("unfold by the quoted key %s: %v", quoted, err)
	}
}

// secondAlbum returns the pid of the album no other album title sorts before, which in
// this fixture is the merge's loser: both are titled Help, so the second by id.
func secondAlbum(t *testing.T, db string) string {
	t.Helper()
	var pid string
	if err := rawDB(t, db).QueryRow("SELECT pid FROM album ORDER BY id DESC LIMIT 1").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// rawDB opens the catalog read-only for a test's own lookups, closed with the test.
func rawDB(t *testing.T, db string) *sql.DB {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// runCLIText executes one command through the root command with text output.
func runCLIText(t *testing.T, db, root string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), err
}
