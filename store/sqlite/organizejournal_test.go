package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// organizeJob records a job of kind in state, as a run would.
func organizeJob(t *testing.T, st *Store, kind, state string) model.PID {
	t.Helper()
	pid := model.NewPID()
	now := nowNS()
	if _, err := st.wdb().ExecContext(context.Background(), `INSERT INTO job(pid, kind, scope, state, owner, started_at, heartbeat_at)
		VALUES (?, ?, 'fs', ?, 'test', ?, ?)`, string(pid), kind, state, now, now); err != nil {
		t.Fatal(err)
	}
	return pid
}

// journal writes a journal row for job in state, made at at.
func journal(t *testing.T, st *Store, job model.PID, kind, state string, at int64) {
	t.Helper()
	if _, err := st.wdb().ExecContext(context.Background(), `INSERT INTO organize_journal(pid, job_pid, src, dst, state, kind, created_at)
		VALUES (?, ?, 'a', 'b', ?, ?, ?)`, string(model.NewPID()), string(job), state, kind, at); err != nil {
		t.Fatal(err)
	}
}

// TestPruneOrganizeJournalTakesWholeJobs: a prune takes a job's moves together or not at
// all, so an undo never finds half a job: one whose newest move is younger than the age,
// one with a move still planned, or one still running keeps every row.
func TestPruneOrganizeJournalTakesWholeJobs(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	old := nowNS() - int64(2*time.Hour)
	aged, spanning := organizeJob(t, st, "organize", "done"), organizeJob(t, st, "organize-undo", "done")
	unsettled, running := organizeJob(t, st, "organize", "crashed"), organizeJob(t, st, "organize", "running")
	journal(t, st, aged, "file", "committed", old)
	journal(t, st, aged, "file", "rolled_back", old)
	journal(t, st, spanning, "file", "committed", old)
	journal(t, st, spanning, "file", "committed", nowNS())
	journal(t, st, unsettled, "file", "committed", old)
	journal(t, st, unsettled, "file", "planned", old)
	journal(t, st, running, "file", "committed", old)
	n, err := st.PruneOrganizeJournal(ctx, int64(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("prune = %d (err %v), want the aged job's two rows", n, err)
	}
	for job, want := range map[model.PID]int{aged: 0, spanning: 2, unsettled: 2, running: 1} {
		if got := scalarInt(t, st, "SELECT COUNT(*) FROM organize_journal WHERE job_pid = ?", string(job)); got != want {
			t.Errorf("rows of %s = %d, want %d", job, got, want)
		}
	}
}

// TestOrganizeHistoryPagesByStart: the history lists organize and undo runs that moved a
// file, newest start first, a page at a time, each with its moves counted by state; a job
// of another kind and a run that only carried companions are not runs to list.
func TestOrganizeHistoryPagesByStart(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	now := nowNS()
	first := organizeJob(t, st, "organize", "done")
	journal(t, st, first, "file", "committed", now)
	scan := organizeJob(t, st, "scan", "done")
	journal(t, st, scan, "file", "committed", now)
	second := organizeJob(t, st, "organize-undo", "done")
	journal(t, st, second, "file", "committed", now)
	journal(t, st, second, "file", "rolled_back", now)
	companions := organizeJob(t, st, "organize", "done")
	journal(t, st, companions, "companion", "committed", now)
	third := organizeJob(t, st, "organize", "crashed")
	journal(t, st, third, "file", "planned", now)

	page, err := st.OrganizeHistory(ctx, "", 2)
	if err != nil || len(page.Batches) != 2 || !page.HasMore || page.Next != second {
		t.Fatalf("first page = %+v (err %v), want the two newest runs and a cursor at %s", page, err, second)
	}
	if b := page.Batches[0]; b.JobPID != third || b.Kind != "organize" || b.State != model.JobCrashed || b.Planned != 1 {
		t.Errorf("newest = %+v, want the crashed run with its planned move", b)
	}
	if b := page.Batches[1]; b.JobPID != second || b.Kind != "organize-undo" || b.Committed != 1 || b.RolledBack != 1 {
		t.Errorf("second = %+v, want the undo with one committed and one rolled back", b)
	}
	page, err = st.OrganizeHistory(ctx, page.Next, 2)
	if err != nil || len(page.Batches) != 1 || page.HasMore || page.Next != "" || page.Batches[0].JobPID != first {
		t.Fatalf("second page = %+v (err %v), want the first run alone and no more", page, err)
	}
	if page, err := st.OrganizeHistory(ctx, "", 0); err != nil || len(page.Batches) != 3 || page.HasMore {
		t.Errorf("a page of the default size = %+v (err %v), want every run", page, err)
	}
}

// TestOrganizeBatchByJobReadsOneRun: one run's moves are read by its job, and a job that
// moved no file is not found.
func TestOrganizeBatchByJobReadsOneRun(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	run := organizeJob(t, st, "organize", "done")
	journal(t, st, run, "file", "committed", nowNS())
	journal(t, st, run, "file", "committed", nowNS())
	journal(t, st, run, "companion", "committed", nowNS())
	b, err := st.OrganizeBatchByJob(ctx, run)
	if err != nil || b.JobPID != run || b.Committed != 2 || b.State != model.JobDone || b.StartedAt == 0 {
		t.Fatalf("batch = %+v (err %v), want the run with its two moves", b, err)
	}
	carried := organizeJob(t, st, "organize", "done")
	journal(t, st, carried, "companion", "committed", nowNS())
	for _, pid := range []model.PID{carried, organizeJob(t, st, "organize", "done"), "NOSUCHJOB"} {
		if _, err := st.OrganizeBatchByJob(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("batch of %s = %v, want CodeNotFound", pid, err)
		}
	}
}

// TestOrganizeHistorySeeksByJob: a page reads the jobs newest first off the pid index and
// each one's journal off organize_journal_job, sorting nothing, however long the history.
func TestOrganizeHistorySeeksByJob(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	for _, narrow := range []string{"ORDER BY pid DESC LIMIT ?", "AND pid < ? ORDER BY pid DESC LIMIT ?"} {
		args := []any{11}
		if len(narrow) > 30 {
			args = []any{"Z", 11}
		}
		plan := queryPlan(t, st, fmt.Sprintf(organizeRuns, narrow), args...)
		if !strings.Contains(plan, "sqlite_autoindex_job_1") || !strings.Contains(plan, "organize_journal_job") || strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("plan for %q:\n%s\nwant the pid index, the journal's job index and no sort", narrow, plan)
		}
	}
}
