package sqlite

import (
	"context"
	"testing"
	"time"
)

// TestPruneOrganizeJournalTakesWholeJobs: a prune takes a job's moves together or not at
// all, so an undo never finds half a job: one whose newest move is younger than the age,
// or one with a move still planned, keeps every row.
func TestPruneOrganizeJournalTakesWholeJobs(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	old := nowNS() - int64(2*time.Hour)
	rows := []struct {
		pid, job, state string
		at              int64
	}{
		{"j1", "aged", "committed", old}, {"j2", "aged", "rolled_back", old},
		{"j3", "spanning", "committed", old}, {"j4", "spanning", "committed", nowNS()},
		{"j5", "unsettled", "committed", old}, {"j6", "unsettled", "planned", old},
	}
	for _, r := range rows {
		if _, err := st.write.ExecContext(ctx, `INSERT INTO organize_journal(pid, job_pid, src, dst, state, created_at)
			VALUES (?, ?, 'a', 'b', ?, ?)`, r.pid, r.job, r.state, r.at); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.PruneOrganizeJournal(ctx, int64(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("prune = %d (err %v), want the aged job's two rows", n, err)
	}
	for job, want := range map[string]int{"aged": 0, "spanning": 2, "unsettled": 2} {
		if got := scalarInt(t, st, "SELECT COUNT(*) FROM organize_journal WHERE job_pid = ?", job); got != want {
			t.Errorf("rows of %s = %d, want %d", job, got, want)
		}
	}
}
