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
	t.Parallel()
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
		Data struct {
			Folds []struct {
				Key        string `json:"key"`
				Name       string `json:"name"`
				EntityPID  string `json:"entityPid"`
				EntityName string `json:"entityName"`
			} `json:"folds"`
			HasMore bool `json:"hasMore"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &folds); err != nil || len(folds.Data.Folds) != 1 || folds.Data.HasMore {
		t.Fatalf("entity folds printed %q (err %v), want one fold", out, err)
	}
	if f := folds.Data.Folds[0]; f.Key != "beatles" || f.Name != "Beatles" || f.EntityName != "The Beatles" {
		t.Fatalf("fold = %+v, want the key beatles read from Beatles, on The Beatles", f)
	}
	if text, err := runCLIText(t, db, root, "entity", "folds", "artist", "--limit", "1"); err != nil ||
		!strings.Contains(text, "beatles  Beatles") {
		t.Fatalf("entity folds text = %q (err %v), want the key beside the spelling it was read from", text, err)
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

// TestEntityUnfoldReResolveReadsWhatTheFoldKept: `entity unfold --re-resolve` forgets the
// fold and reads again the file it kept, naming it, so the file's item is filed under the
// artist it states.
func TestEntityUnfoldReResolveReadsWhatTheFoldKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, artist := range []string{"Beatles", "The Beatles"} {
		path := filepath.Join(root, strconv.Itoa(i)+".mp3")
		if err := os.WriteFile(path, testaudio.BuildMP3WithAudio("Song "+strconv.Itoa(i), artist, "Album "+strconv.Itoa(i), 1,
			testaudio.AudioWithSeed(byte(i+1))), 0o644); err != nil {
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
	pidOf := func(name string) string {
		t.Helper()
		var pid string
		if err := rawDB(t, db).QueryRowContext(ctx, "SELECT pid FROM artist WHERE name = ?", name).Scan(&pid); err != nil {
			t.Fatalf("pid of %q: %v", name, err)
		}
		return pid
	}
	if out, err := runCLIJSON(t, db, root, "merge", "artist", pidOf("The Beatles"), pidOf("Beatles")); err != nil {
		t.Fatalf("merge: %v (%s)", err, out)
	}
	out, err := runCLIJSON(t, db, root, "entity", "unfold", "artist", "beatles", "--re-resolve")
	if err != nil {
		t.Fatalf("entity unfold --re-resolve: %v (%s)", err, out)
	}
	var rep struct {
		Data struct {
			Unfolded []string `json:"unfolded"`
			Reread   []string `json:"reread"`
			Skipped  []string `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || len(rep.Data.Unfolded) != 1 || len(rep.Data.Reread) != 2 || rep.Data.Skipped == nil {
		t.Fatalf("entity unfold --re-resolve printed %s (err %v), want the key unfolded, both items read again and an empty skip list", out, err)
	}
	if n := len(pidOf("Beatles")); n == 0 {
		t.Error("no artist Beatles after the re-resolve")
	}
}
