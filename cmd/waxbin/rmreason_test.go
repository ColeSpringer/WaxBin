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
	"github.com/colespringer/waxbin/trash"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// TestRmReasonReachesTheTrash: rm --reason shows the reason in its plan and writes it
// on the trash row `trash list` prints, and a reason the planner refuses is an invalid
// argument.
func TestRmReasonReachesTheTrash(t *testing.T) {
	t.Parallel()
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

// TestDeleteReportNamesItsSkips: an applied delete names each action it skipped, with its
// code and reason, and the JSON carries the plan's three skip counts beside them.
func TestDeleteReportNamesItsSkips(t *testing.T) {
	t.Parallel()
	plan := &trash.Plan{Mode: model.DeleteTrash}
	rep := &trash.Report{Skipped: 1, SkippedPodcast: 2, SkippedReadOnly: 3, SkippedRipTracks: 4,
		Skips: []trash.Skip{{FilePID: "FILEA", Src: "/lib/a.mp3", Code: trash.SkipReadOnly, Reason: model.ReasonLibraryReadOnly}}}
	render := func(asJSON bool) string {
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := emitDeleteReport(cmd, &globals{jsonOut: asJSON}, plan, rep); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	if text := render(false); !strings.Contains(text, "skip /lib/a.mp3 [read-only] (library is read-only)") {
		t.Errorf("report text lacks the skip:\n%s", text)
	}
	var env struct {
		Data struct {
			Skips            []trash.Skip `json:"skips"`
			SkippedPodcast   int          `json:"skippedPodcast"`
			SkippedReadOnly  int          `json:"skippedReadOnly"`
			SkippedRipTracks int          `json:"skippedRipTracks"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(render(true)), &env); err != nil || len(env.Data.Skips) != 1 || env.Data.Skips[0] != rep.Skips[0] ||
		env.Data.SkippedPodcast != 2 || env.Data.SkippedReadOnly != 3 || env.Data.SkippedRipTracks != 4 {
		t.Errorf("report json = %+v (err %v)", env.Data, err)
	}
}
