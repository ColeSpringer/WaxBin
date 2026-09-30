package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestJobsNamesWhatAJobTargeted: a scan of one library shows that library as its
// target in both outputs, and a scan of every library shows none.
func TestJobsNamesWhatAJobTargeted(t *testing.T) {
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
