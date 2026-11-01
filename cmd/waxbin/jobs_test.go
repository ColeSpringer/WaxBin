package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestJobsNamesWhatAJobTargeted: a scan of one library shows that library as its
// target in both outputs, and a scan of every library shows none.
func TestJobsNamesWhatAJobTargeted(t *testing.T) {
	t.Parallel()
	db, root := filepath.Join(t.TempDir(), "catalog.db"), t.TempDir()
	if _, err := runCLIJSON(t, db, root, "scan"); err != nil {
		t.Fatalf("scan: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "library", "list")
	if err != nil {
		t.Fatalf("library list: %v", err)
	}
	var libs struct {
		Data []struct {
			PID string `json:"pid"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &libs); err != nil || len(libs.Data) != 1 {
		t.Fatalf("library list printed %q (err %v), want one library", out, err)
	}
	pid := libs.Data[0].PID
	if _, err := runCLIJSON(t, db, root, "scan", "--library", pid); err != nil {
		t.Fatalf("scoped scan: %v", err)
	}

	out, err = runCLIJSON(t, db, root, "jobs")
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	var jobs struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &jobs); err != nil || len(jobs.Data) != 2 {
		t.Fatalf("jobs printed %q (err %v), want two", out, err)
	}
	if j := jobs.Data[0]; j["targetType"] != "library" || j["targetPid"] != pid {
		t.Errorf("scoped scan job = %v, want library:%s", j, pid)
	}
	if j := jobs.Data[1]; j["targetType"] != nil || j["targetPid"] != nil {
		t.Errorf("whole scan job = %v, want no target keys", j)
	}

	cmd := newRootCmd(&globals{})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "--root", root + ":managed:waxbin-native", "jobs"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("jobs text: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "TARGET") || !strings.Contains(lines[1], "library:"+pid) ||
		strings.Contains(lines[2], "library:") {
		t.Errorf("jobs text =\n%s\nwant a TARGET column naming library:%s on the scoped scan alone", stdout.String(), pid)
	}
}

// TestJobErrorExitsWithTheJobsClass: a tailed job that failed answers with the class and
// reason its row kept, so the CLI exits as the run itself would have; a row with none
// (a crashed job) reads as internal.
func TestJobErrorExitsWithTheJobsClass(t *testing.T) {
	t.Parallel()
	err := jobError(&model.Job{Kind: "set-kind", State: model.JobFailed, Error: "item is locked",
		ErrorCode: waxerr.CodeLocked, ErrorReason: "kind_locked"})
	if !waxerr.Is(err, waxerr.CodeLocked) || waxerr.ReasonOf(err) != "kind_locked" || !strings.Contains(err.Error(), "item is locked") {
		t.Errorf("failed job error = %v (reason %q), want locked with its reason", err, waxerr.ReasonOf(err))
	}
	if got, want := exitCodeFor(err), exitCodeFor(waxerr.New(waxerr.CodeLocked, "x", "y")); got != want {
		t.Errorf("exit = %d, want %d, the locked class's", got, want)
	}
	if err := jobError(&model.Job{Kind: "scan", State: model.JobCrashed, Error: "reclaimed: owner not live"}); !waxerr.Is(err, waxerr.CodeInternal) {
		t.Errorf("crashed job error = %v, want internal", err)
	}
	if err := jobError(&model.Job{Kind: "scan", State: model.JobCanceled}); !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Errorf("canceled job error = %v, want canceled", err)
	}
}

// TestJobsJSONCarriesTheErrorClass: the job listing's JSON names a failed job's class and
// reason beside its message.
func TestJobsJSONCarriesTheErrorClass(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(jobViews([]*model.Job{{PID: "J", Kind: "set-kind", State: model.JobFailed, Error: "locked",
		ErrorCode: waxerr.CodeLocked, ErrorReason: "kind_locked"}}))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"errorCode":"locked"`) || !strings.Contains(s, `"errorReason":"kind_locked"`) {
		t.Errorf("jobs json = %s, want the error class and reason", s)
	}
}
