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
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// playlistCLIFixture scans three tracks into a catalog and returns their pids by title.
func playlistCLIFixture(t *testing.T) (string, string, map[string]model.PID) {
	t.Helper()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestPlaylistDescription: `playlist create --description` and `playlist import
// --description` store a description, `playlist set-description` replaces and clears it,
// the JSON views carry it, `playlist show` prints it under its heading, and
// `smartplaylist create --nsp` takes the document's comment unless --description says
// otherwise.
func TestPlaylistDescription(t *testing.T) {
	t.Parallel()
	db, root, _ := playlistCLIFixture(t)
	described := func(pl, want string) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, "playlist", "show", pl)
		if err != nil {
			t.Fatalf("show %s: %v", pl, err)
		}
		var shown struct {
			Data struct {
				Playlist struct {
					Description *string `json:"description"`
				} `json:"playlist"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &shown); err != nil || shown.Data.Playlist.Description == nil ||
			*shown.Data.Playlist.Description != want {
			t.Errorf("show %s printed %s (err %v), want description %q", pl, out, err, want)
		}
	}

	out, _, err := runPlaylistCLI(t, db, root, "create", "Mix", "--description", "For the drive")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	pl := strings.TrimSpace(out)
	described(pl, "For the drive")
	if shown, _, err := runPlaylistCLI(t, db, root, "show", pl); err != nil ||
		!strings.HasPrefix(shown, "Mix (static, private) - 0 items\nFor the drive\n") {
		t.Errorf("show printed %q (err %v), want the description under the heading", shown, err)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "set-description", pl, "Rewritten"); err != nil {
		t.Fatalf("set-description: %v", err)
	}
	described(pl, "Rewritten")
	if _, _, err := runPlaylistCLI(t, db, root, "set-description", pl, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	described(pl, "")
	if _, _, err := runPlaylistCLI(t, db, root, "set-description", pl, "a\tb"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("set-description with a tab = %v, want CodeInvalid", err)
	}

	doc := filepath.Join(t.TempDir(), "mix.m3u8")
	if err := os.WriteFile(doc, []byte(filepath.Join(root, "Two.mp3")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runPlaylistCLI(t, db, root, "import", "Imported", "--file", doc, "--description", "From a file"); err != nil {
		t.Fatalf("import: %v", err)
	}
	nsp := filepath.Join(t.TempDir(), "mix.nsp")
	if err := os.WriteFile(nsp, []byte(`{"comment":"Road trip","all":[{"is":{"artist":"Artist"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	smart := func(args ...string) string {
		t.Helper()
		cmd := newRootCmd(&globals{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "smartplaylist", "create"}, args...))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("smartplaylist create %v: %v", args, err)
		}
		return strings.TrimSpace(stdout.String())
	}
	described(smart("From nsp", "--nsp", nsp), "Road trip")
	described(smart("Overridden", "--nsp", nsp, "--description", "Mine"), "Mine")
	described(smart("Cleared", "--nsp", nsp, "--description", ""), "")

	out, err = runCLIJSON(t, db, root, "playlist", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var listed struct {
		Data []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatalf("list printed %q: %v", out, err)
	}
	got := map[string]string{}
	for _, p := range listed.Data {
		got[p.Name] = p.Description
	}
	want := map[string]string{"Mix": "", "Imported": "From a file", "From nsp": "Road trip", "Overridden": "Mine", "Cleared": ""}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("listed descriptions = %v, want %v", got, want)
	}
}

// TestPlaylistExportNamesTheEntriesLeftOut: an episode with no download has no path to
// write, so `playlist export` leaves it out, says so on stderr beside the document, and
// names it in the JSON next to the count written.
func TestPlaylistExportNamesTheEntriesLeftOut(t *testing.T) {
	t.Parallel()
	db, root, p := playlistCLIFixture(t)
	ctx := context.Background()
	lib := openCLILib(t, ctx, db, root, false)
	show, err := lib.Podcasts().AddManual(ctx, "Show", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	ep, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", EnclosureURL: "https://example.invalid/ep.mp3"}, true)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	pl, err := lib.Playlists().CreateStatic(ctx, "Mixed", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := lib.Playlists().Set(ctx, pl, []model.PID{p["One"], ep.EpisodePID, p["Two"]}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	doc, stderr, err := runPlaylistCLI(t, db, root, "export", string(pl))
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if strings.Count(doc, "#EXTINF") != 2 || strings.Contains(doc, "\n\n") || !strings.Contains(stderr, string(ep.EpisodePID)) {
		t.Errorf("export printed %q and %q, want two entries and the episode named on stderr", doc, stderr)
	}
	out, err := runCLIJSON(t, db, root, "playlist", "export", string(pl))
	if err != nil {
		t.Fatalf("export --json: %v", err)
	}
	var env struct {
		Data struct {
			Written int         `json:"written"`
			Skipped []model.PID `json:"skipped"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Written != 2 ||
		len(env.Data.Skipped) != 1 || env.Data.Skipped[0] != ep.EpisodePID {
		t.Errorf("export --json printed %s (err %v), want 2 written and the episode skipped", out, err)
	}
}

// TestPlaylistSetDescriptionEchoesTheStoredText: the JSON reply names the description as
// the playlist stores it, trimmed and with its line breaks as line feeds, not as typed.
func TestPlaylistSetDescriptionEchoesTheStoredText(t *testing.T) {
	t.Parallel()
	db, root, p := playlistCLIFixture(t)
	pl := cliPlaylist(t, db, root, p["One"])
	out, err := runCLIJSON(t, db, root, "playlist", "set-description", pl, "  For the drive\r\nSide one  ")
	if err != nil {
		t.Fatalf("set-description: %v", err)
	}
	var env struct {
		Data struct {
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Data.Description != "For the drive\nSide one" {
		t.Errorf("set-description printed %s (err %v), want the stored text", out, err)
	}
}

// runSmartCLI executes one smartplaylist command in text mode and returns both streams.
func runSmartCLI(t *testing.T, db, root string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(append([]string{"--db", db, "--root", root + ":managed:waxbin-native", "smartplaylist"}, args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// TestNSPCommentNoteOnlyWhereTheCommentIsTaken: a comment no description can hold is noted
// when a create would have taken it, and not when --description stands in its place or
// on set-rule, which never reads it.
func TestNSPCommentNoteOnlyWhereTheCommentIsTaken(t *testing.T) {
	t.Parallel()
	db, root, _ := playlistCLIFixture(t)
	nsp := filepath.Join(t.TempDir(), "tabbed.nsp")
	if err := os.WriteFile(nsp, []byte(`{"comment":"a\tb","all":[{"is":{"artist":"Artist"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runSmartCLI(t, db, root, "create", "Taken", "--nsp", nsp)
	if err != nil || !strings.Contains(stderr, "comment") {
		t.Fatalf("create = %q, stderr %q (err %v), want the comment noted", out, stderr, err)
	}
	pl := strings.TrimSpace(out)
	if _, stderr, err := runSmartCLI(t, db, root, "create", "Given", "--nsp", nsp, "--description", "Mine"); err != nil || stderr != "" {
		t.Errorf("create with --description: stderr %q (err %v), want no note", stderr, err)
	}
	if _, stderr, err := runSmartCLI(t, db, root, "set-rule", pl, "--nsp", nsp); err != nil || stderr != "" {
		t.Errorf("set-rule: stderr %q (err %v), want no note", stderr, err)
	}

	// A refused document lists its gaps, and the note only where a create would have
	// taken the comment.
	refused := filepath.Join(t.TempDir(), "refused.nsp")
	if err := os.WriteFile(refused, []byte(`{"comment":"a\tb","all":[{"is":{"bitrate":320}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		note bool
	}{
		{[]string{"create", "Refused", "--nsp", refused}, true},
		{[]string{"create", "Refused", "--nsp", refused, "--description", "Mine"}, false},
		{[]string{"set-rule", pl, "--nsp", refused}, false},
	} {
		_, stderr, err := runSmartCLI(t, db, root, c.args...)
		if err == nil || !strings.Contains(stderr, "bitrate") || strings.Contains(stderr, "comment") != c.note {
			t.Errorf("%v: stderr %q (err %v), want the gap and the note %v", c.args, stderr, err, c.note)
		}
	}
}

// TestExportNSPCarriesTheDescription: `smartplaylist export-nsp` writes the playlist's
// name and description into the document, so importing it again gives them back.
func TestExportNSPCarriesTheDescription(t *testing.T) {
	t.Parallel()
	db, root, _ := playlistCLIFixture(t)
	nsp := filepath.Join(t.TempDir(), "road.nsp")
	if err := os.WriteFile(nsp, []byte(`{"comment":"Road trip","all":[{"is":{"artist":"Artist"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSmartCLI(t, db, root, "create", "Road", "--nsp", nsp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	doc, _, err := runSmartCLI(t, db, root, "export-nsp", strings.TrimSpace(out))
	if err != nil {
		t.Fatalf("export-nsp: %v", err)
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(doc), &top); err != nil || top["name"] != "Road" || top["comment"] != "Road trip" {
		t.Errorf("export-nsp printed %s (err %v), want the name and the comment", doc, err)
	}
}

// TestPlaylistJSONListsAreArraysWhenEmpty: an export that skipped nothing and an import
// that matched everything print their lists as empty arrays, as edit does, never null.
func TestPlaylistJSONListsAreArraysWhenEmpty(t *testing.T) {
	t.Parallel()
	db, root, p := playlistCLIFixture(t)
	pl := cliPlaylist(t, db, root, p["One"])
	field := func(out, name string) string {
		t.Helper()
		var env struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("printed %s: %v", out, err)
		}
		return string(env.Data[name])
	}
	out, err := runCLIJSON(t, db, root, "playlist", "export", pl)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if got := field(out, "skipped"); got != "[]" {
		t.Errorf("export skipped = %s, want []", got)
	}
	doc := filepath.Join(t.TempDir(), "mix.m3u8")
	if err := os.WriteFile(doc, []byte(filepath.Join(root, "Two.mp3")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err = runCLIJSON(t, db, root, "playlist", "import", "Again", "--file", doc); err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := field(out, "unmatchedPaths"); got != "[]" {
		t.Errorf("import unmatchedPaths = %s, want []", got)
	}
}
