package waxbin_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestServeProxiedMergeAndUnfold: a proxied merge reports the key it folded, and a
// proxied unfold forgets it, refusing a key no fold holds.
func TestServeProxiedMergeAndUnfold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	sock := testsock.Path(t)
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3WithAudio("One", "Beatles", "One", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(root, "b.mp3"), testaudio.BuildMP3WithAudio("Two", "The Beatles", "Two", 1, testaudio.AudioWithSeed(2)))
	lib := openServed(t, ctx, db, root, sock)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	c := dialWhenReady(t, sock)
	survivor, loser := artistPIDByName(t, ctx, db, "The Beatles"), artistPIDByName(t, ctx, db, "Beatles")
	reports, err := c.Merge(ctx, model.MergeArtist, survivor, []model.PID{loser})
	if err != nil {
		t.Fatalf("proxied merge: %v", err)
	}
	if len(reports) != 1 || len(reports[0].Folds) != 1 || reports[0].Folds[0] != "beatles" {
		t.Fatalf("merge reports = %+v, want the loser's key folded", reports)
	}
	if err := c.UnfoldEntity(ctx, model.MergeArtist, []string{"beatles", "nobody"}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unfold with an unknown key = %v, want CodeNotFound", err)
	}
	if folds, err := lib.EntityFolds(ctx, model.MergeArtist); err != nil || len(folds) != 1 {
		t.Fatalf("folds after a refused unfold = %+v (err %v), want the merge's still there", folds, err)
	}
	if err := c.UnfoldEntity(ctx, model.MergeArtist, []string{"beatles"}); err != nil {
		t.Fatalf("proxied unfold: %v", err)
	}
	if folds, err := lib.EntityFolds(ctx, model.MergeArtist); err != nil || len(folds) != 0 {
		t.Errorf("folds after the unfold = %+v (err %v), want none", folds, err)
	}
}

// TestServeProxiedRunOrganizeUndo: run_organize_undo moves an organize's files back as a
// job in the server, targeted at the organize it undoes, and refuses a job that is no
// organize before starting one.
func TestServeProxiedRunOrganizeUndo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "Inbox", "one.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 1))
	sock := testsock.Path(t)
	lib := openServed(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root, sock)
	c := dialWhenReady(t, sock)
	if _, err := lib.Organize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{}); err != nil {
		t.Fatal(err)
	}
	batches, err := lib.OrganizeHistory(ctx, 1)
	if err != nil || len(batches) != 1 {
		t.Fatalf("history = %+v (err %v)", batches, err)
	}
	jobs, err := lib.Jobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var scanJob model.PID
	for _, j := range jobs {
		if j.Kind == "scan" {
			scanJob = j.PID
		}
	}
	if _, err := c.RunOrganizeUndo(ctx, scanJob); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("undo of a scan = %v, want CodeInvalid", err)
	}
	undo, err := c.RunOrganizeUndo(ctx, batches[0].JobPID)
	if err != nil {
		t.Fatalf("run_organize_undo: %v", err)
	}
	job := waitForJobDone(t, ctx, lib, undo)
	if job.Kind != "organize-undo" || job.TargetType != "job" || job.TargetPID != batches[0].JobPID {
		t.Errorf("job = %s on %s %s, want organize-undo on job %s", job.Kind, job.TargetType, job.TargetPID, batches[0].JobPID)
	}
	var rr organize.RunResult
	if err := json.Unmarshal([]byte(job.Result), &rr); err != nil || rr.Report.Moved != 1 {
		t.Errorf("job result %q (err %v), want one move", job.Result, err)
	}
	if !fileExists(src) {
		t.Error("the file is not back")
	}
}
