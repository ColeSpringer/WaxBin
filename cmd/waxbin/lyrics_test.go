package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// runLyricsCLI executes one lyrics command and returns both streams.
func runLyricsCLI(t *testing.T, db, root string, asJSON bool, args ...string) (string, string, error) {
	t.Helper()
	cmd := newRootCmd(&globals{})
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	lead := []string{"--db", db, "--root", root + ":managed:waxbin-native"}
	if asJSON {
		lead = append(lead, "--json")
	}
	cmd.SetArgs(append(append(lead, "lyrics"), args...))
	err := cmd.ExecuteContext(context.Background())
	return stdout.String(), stderr.String(), err
}

// cliLyricLines reads an item's stored synced lines through `lyrics --json`.
func cliLyricLines(t *testing.T, db, root string, pid model.PID) []syncedLineView {
	t.Helper()
	out, _, err := runLyricsCLI(t, db, root, true, string(pid))
	if err != nil {
		t.Fatalf("lyrics %s: %v", pid, err)
	}
	var env struct {
		Data lyricsView `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("lyrics printed %q: %v", out, err)
	}
	return env.Data.Lines
}

func writeLRC(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "song.lrc")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLyricsSetWarnsOfTheLinesItLeftOut: a file with timed and untimed lines stores the
// timed ones, warns on stderr and reports which lines it left out; a file whose every line
// is timed warns of nothing.
func TestLyricsSetWarnsOfTheLinesItLeftOut(t *testing.T) {
	t.Parallel()
	db, root, p := playlistCLIFixture(t)
	type reply struct {
		Data struct {
			DroppedLines []int `json:"droppedLines"`
		} `json:"data"`
	}
	partly := writeLRC(t, "[00:01.00]one\nno time here\n[00:02.00]two\nnor here\n")
	out, stderr, err := runLyricsCLI(t, db, root, true, "set", string(p["One"]), "--file", partly)
	if err != nil {
		t.Fatalf("lyrics set: %v", err)
	}
	var got reply
	if err := json.Unmarshal([]byte(out), &got); err != nil || !slices.Equal(got.Data.DroppedLines, []int{2, 4}) {
		t.Errorf("lyrics set printed %q (err %v), want droppedLines 2 and 4", out, err)
	}
	if want := "warning: " + partly + ": 2 line(s) had no usable timestamp (first at line 2); 2 timed lyric(s) parsed\n"; stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if lines := cliLyricLines(t, db, root, p["One"]); !slices.Equal(lines, []syncedLineView{{1000, "one"}, {2000, "two"}}) {
		t.Errorf("stored lines = %+v, want the two timed ones", lines)
	}

	clean := writeLRC(t, "[00:01.00]one\n[00:02.00]two\n")
	out, stderr, err = runLyricsCLI(t, db, root, true, "set", string(p["Two"]), "--file", clean)
	if err != nil || stderr != "" || strings.Contains(out, "droppedLines") {
		t.Errorf("a clean file printed %q and warned %q (err %v), want no warning and no droppedLines", out, stderr, err)
	}

	// A file with no timed line is plain text, every line of it kept.
	const words = "one\ntwo\n"
	out, stderr, err = runLyricsCLI(t, db, root, true, "set", string(p["Three"]), "--file", writeLRC(t, words))
	if err != nil || stderr != "" || strings.Contains(out, "droppedLines") {
		t.Errorf("a plain file printed %q and warned %q (err %v), want no warning and no droppedLines", out, stderr, err)
	}
	out, _, err = runLyricsCLI(t, db, root, true, string(p["Three"]))
	var plain struct {
		Data lyricsView `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &plain) != nil || plain.Data.Synced || plain.Data.Unsynced != words {
		t.Errorf("a plain file is stored as %q (err %v), want its text whole and unsynchronized", out, err)
	}
}

// TestLyricsPrintWhatLyricsSetReads: what `lyrics A` prints, fed to `lyrics set B`, stores
// every line with no warning, a line whose text holds a line break printed as one line.
func TestLyricsPrintWhatLyricsSetReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, root, p := playlistCLIFixture(t)
	lib := openCLILib(t, ctx, db, root, false)
	sent := []model.SyncedLine{{TimeMS: 0, Text: "first"}, {TimeMS: 61500, Text: "two\nlines"}, {TimeMS: 3723004, Text: "past an hour"}}
	if err := lib.SetLyrics(ctx, p["One"], &model.Lyrics{Synced: sent}, model.LockUnchanged, false); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	printed, _, err := runLyricsCLI(t, db, root, false, string(p["One"]))
	if err != nil || strings.Count(printed, "\n") != len(sent) {
		t.Fatalf("lyrics printed %q (err %v), want one line per lyric line", printed, err)
	}
	_, stderr, err := runLyricsCLI(t, db, root, false, "set", string(p["Two"]), "--file", writeLRC(t, printed))
	if err != nil || stderr != "" {
		t.Fatalf("lyrics set of the printed lines: err %v, stderr %q, want no warning", err, stderr)
	}
	want := []syncedLineView{{0, "first"}, {61500, "two lines"}, {3723004, "past an hour"}}
	if lines := cliLyricLines(t, db, root, p["Two"]); !slices.Equal(lines, want) {
		t.Errorf("lines read back = %+v, want %+v", lines, want)
	}

	// A lone line past the last time an .lrc can name is stored at that time, so it prints
	// as a line the next set reads as timed, not as text to keep whole.
	lib = openCLILib(t, ctx, db, root, false)
	far := []model.SyncedLine{{TimeMS: 126000000000, Text: "past the ceiling"}}
	if err := lib.SetLyrics(ctx, p["One"], &model.Lyrics{Synced: far}, model.LockUnchanged, false); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	printed, _, err = runLyricsCLI(t, db, root, false, string(p["One"]))
	if err != nil {
		t.Fatal(err)
	}
	_, stderr, err = runLyricsCLI(t, db, root, false, "set", string(p["Three"]), "--file", writeLRC(t, printed))
	if err != nil || stderr != "" {
		t.Fatalf("lyrics set of %q: err %v, stderr %q, want no warning", printed, err, stderr)
	}
	if lines := cliLyricLines(t, db, root, p["Three"]); !slices.Equal(lines, []syncedLineView{{model.MaxSyncedLineMS, "past the ceiling"}}) {
		t.Errorf("%q read back as %+v, want the line at the last time", printed, lines)
	}
}
