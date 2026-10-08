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

// TestOrganizeUndoAndHistory: `organize history` lists the organize with its moves, `organize
// undo` moves its files back and reports it as an organize report, and `db vacuum
// --journal-days 0` prunes the settled journal, after which there is nothing to undo.
func TestOrganizeUndoAndHistory(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "Inbox", "one.mp3")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := openCLILib(t, ctx, db, root, false)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCLIJSON(t, db, root, "organize", "history")
	if err != nil {
		t.Fatalf("organize history: %v", err)
	}
	var history struct {
		Data []struct {
			JobPID    string `json:"jobPid"`
			Kind      string `json:"kind"`
			Committed int    `json:"committed"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &history); err != nil || len(history.Data) != 1 ||
		history.Data[0].Kind != "organize" || history.Data[0].Committed != 1 {
		t.Fatalf("organize history printed %q (err %v), want the organize with one move", out, err)
	}
	job := history.Data[0].JobPID
	text, err := runCLIText(t, db, root, "organize", "history")
	if err != nil || !strings.Contains(text, job) {
		t.Fatalf("organize history text = %q (err %v), want the job listed", text, err)
	}

	out, err = runCLIJSON(t, db, root, "organize", "undo", job)
	if err != nil {
		t.Fatalf("organize undo: %v (%s)", err, out)
	}
	var rep struct {
		Data struct {
			Moved int `json:"moved"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Data.Moved != 1 {
		t.Fatalf("organize undo printed %q (err %v), want one move", out, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the file is not back: %v", err)
	}

	// More days than a duration holds keeps everything, as any age past the journal's would.
	out, err = runCLIJSON(t, db, root, "db", "vacuum", "--journal-days", "1000000")
	var vac struct {
		Data struct {
			JournalPruned int `json:"journalPruned"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal([]byte(out), &vac) != nil || vac.Data.JournalPruned != 0 {
		t.Fatalf("db vacuum --journal-days 1000000 printed %q (err %v), want nothing pruned", out, err)
	}
	changes := func() int {
		t.Helper()
		l := openCLILib(t, ctx, db, root, false)
		defer l.Close()
		got, err := l.Changes(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	before := changes()
	if _, err := runCLIJSON(t, db, root, "db", "vacuum", "--prune-changelog", "1", "--journal-days", "-5"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("db vacuum --journal-days -5 = %v, want a usage error", err)
	}
	if after := changes(); after != before || before < 2 {
		t.Errorf("change log rows around the refused vacuum = %d, then %d; want them untouched", before, after)
	}
	if _, err := runCLIJSON(t, db, root, "db", "vacuum", "--journal-days", "0"); err != nil {
		t.Fatalf("db vacuum --journal-days 0: %v", err)
	}
	if _, err := runCLIJSON(t, db, root, "organize", "undo", job); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("undo after the journal was pruned = %v, want CodeNotFound", err)
	}
	if _, err := runCLIJSON(t, db, root, "organize", "undo"); err == nil {
		t.Error("organize undo with no job pid ran, want a usage error")
	}
}
