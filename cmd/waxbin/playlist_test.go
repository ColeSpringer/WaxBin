package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// playlistCLIFixture scans three tracks into a catalog and returns their pids by title.
func playlistCLIFixture(t *testing.T) (string, string, map[string]model.PID) {
	t.Helper()
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	for i, title := range []string{"One", "Two", "Three"} {
		if err := os.WriteFile(filepath.Join(root, title+".mp3"),
			testaudio.BuildMP3WithAudio(title, "Artist", "Album", i+1, testaudio.AudioWithSeed(byte(80+i))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 3 {
		t.Fatalf("query: %d items (err %v)", len(items), err)
	}
	pids := map[string]model.PID{}
	for _, it := range items {
		pids[it.Title] = it.PID
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return db, root, pids
}

// runPlaylistCLI executes one playlist command in text mode and returns both streams.
func runPlaylistCLI(t *testing.T, db, root string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "playlist"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// cliPlaylist creates a static playlist holding members in order and returns its pid.
func cliPlaylist(t *testing.T, db, root string, members ...model.PID) string {
	t.Helper()
	out, _, err := runPlaylistCLI(t, db, root, "create", "Mix")
	if err != nil {
		t.Fatalf("playlist create: %v", err)
	}
	pl := strings.TrimSpace(out)
	args := []string{"add", pl}
	for _, m := range members {
		args = append(args, string(m))
	}
	if _, _, err := runPlaylistCLI(t, db, root, args...); err != nil {
		t.Fatalf("playlist add: %v", err)
	}
	return pl
}

// showRows reads `playlist show` back as INDEX TITLE pairs, header excluded.
func showRows(t *testing.T, db, root, pl string) []string {
	t.Helper()
	out, _, err := runPlaylistCLI(t, db, root, "show", pl)
	if err != nil {
		t.Fatalf("playlist show: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 || !strings.HasPrefix(strings.TrimSpace(lines[1]), "INDEX") {
		t.Fatalf("playlist show printed %q, want a header starting with INDEX", out)
	}
	var rows []string
	for _, l := range lines[2:] {
		f := strings.Fields(l)
		rows = append(rows, f[0]+" "+f[2])
	}
	return rows
}

// TestPlaylistShowPrintsIndexes: `playlist show` numbers each entry with the index the
// by-index removal takes, a duplicate included.
func TestPlaylistShowPrintsIndexes(t *testing.T) {
	db, root, p := playlistCLIFixture(t)
	pl := cliPlaylist(t, db, root, p["One"], p["Two"], p["One"])
	if got := strings.Join(showRows(t, db, root, pl), ", "); got != "0 One, 1 Two, 2 One" {
		t.Errorf("show = %s, want 0 One, 1 Two, 2 One", got)
	}
}

// TestPlaylistRemoveByIndexes: several --index values name the listing as it stood, the
// old --position flag is gone, and an item pid beside --index is the entry the caller
// expects there.
func TestPlaylistRemoveByIndexes(t *testing.T) {
	db, root, p := playlistCLIFixture(t)
	pl := cliPlaylist(t, db, root, p["One"], p["Two"], p["Three"], p["One"], p["Two"])

	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl, "--index", "1", "--index", "3"); err != nil {
		t.Fatalf("remove --index 1 --index 3: %v", err)
	}
	if got := strings.Join(showRows(t, db, root, pl), ", "); got != "0 One, 1 Three, 2 Two" {
		t.Fatalf("after removing 1 and 3 = %s, want 0 One, 1 Three, 2 Two", got)
	}

	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl, "--position", "2"); err == nil {
		t.Fatal("remove --position 2 succeeded, want the flag refused")
	}
	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl, "--index", "2"); err != nil {
		t.Fatalf("remove --index 2: %v", err)
	}
	if got := strings.Join(showRows(t, db, root, pl), ", "); got != "0 One, 1 Three" {
		t.Fatalf("after removing index 2 = %s, want 0 One, 1 Three", got)
	}

	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl, string(p["One"]), "--index", "1"); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("remove One at index 1 (Three) = %v, want CodeConflict", err)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl, string(p["Three"]), "--index", "1"); err != nil {
		t.Fatalf("remove Three at index 1: %v", err)
	}
	if got := strings.Join(showRows(t, db, root, pl), ", "); got != "0 One" {
		t.Fatalf("after the guarded removal = %s, want 0 One", got)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "remove", pl); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("remove with neither an item nor an index = %v, want CodeInvalid", err)
	}
}

// TestPlaylistSetOwner: `playlist set-owner` moves a playlist to another user, and the
// JSON views name the owner's pid.
func TestPlaylistSetOwner(t *testing.T) {
	db, root, p := playlistCLIFixture(t)
	pl := cliPlaylist(t, db, root, p["One"])
	out, err := runCLIJSON(t, db, root, "user", "add", "bob")
	if err != nil {
		t.Fatalf("user add: %v", err)
	}
	var created struct {
		Data []struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || len(created.Data) != 1 {
		t.Fatalf("user add printed %q (err %v)", out, err)
	}
	bob := created.Data[0].PID

	if _, _, err := runPlaylistCLI(t, db, root, "set-owner", pl, bob); err != nil {
		t.Fatalf("set-owner: %v", err)
	}
	out, err = runCLIJSON(t, db, root, "playlist", "show", pl)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	var shown struct {
		Data struct {
			Playlist struct {
				Owner    string `json:"owner"`
				OwnerPID string `json:"ownerPid"`
			} `json:"playlist"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatalf("show printed %q: %v", out, err)
	}
	if shown.Data.Playlist.OwnerPID != bob || shown.Data.Playlist.Owner != "bob" {
		t.Errorf("owner = %+v, want bob and his pid", shown.Data.Playlist)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "set-owner", pl, "nobody"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("set-owner to an unknown user = %v, want CodeNotFound", err)
	}
}

// TestPlaylistImportDryRun: --dry-run prints what each entry would match and creates
// nothing.
func TestPlaylistImportDryRun(t *testing.T) {
	db, root, _ := playlistCLIFixture(t)
	doc := filepath.Join(t.TempDir(), "mix.m3u8")
	if err := os.WriteFile(doc, []byte("#EXTM3U\n"+filepath.Join(root, "Two.mp3")+"\nmissing.mp3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runPlaylistCLI(t, db, root, "import", "Mix", "--file", doc, "--dry-run")
	if err != nil {
		t.Fatalf("import --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "Two") || !strings.Contains(stdout, "missing.mp3") || !strings.Contains(stdout, "matched 1, merged 0, unmatched 1") {
		t.Errorf("dry run printed %q, want each entry and the counts", stdout)
	}
	// A preview fails where the import would, before it looks anything up.
	if _, _, err := runPlaylistCLI(t, db, root, "import", "Mix", "--file", doc, "--dry-run", "--visibility", "bogus"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("dry run with an unknown visibility = %v, want CodeInvalid", err)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "import", "Mix", "--file", doc, "--dry-run", "--user", "nobody"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("dry run for an unknown owner = %v, want CodeNotFound", err)
	}
	out, err := runCLIJSON(t, db, root, "playlist", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Data) != 0 {
		t.Errorf("playlists after a dry run = %s (err %v), want none", out, err)
	}
}
