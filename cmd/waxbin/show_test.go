package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestShowSaysWhatArchivedAnItem: show names the archive reason beside the state, and its
// JSON carries archiveReason only while the item is archived.
func TestShowSaysWhatArchivedAnItem(t *testing.T) {
	t.Parallel()
	db, root, pid := creditCLIFixture(t)
	reason := func() (string, bool) {
		t.Helper()
		out, err := runCLIJSON(t, db, root, "show", string(pid))
		if err != nil {
			t.Fatalf("show --json: %v (%s)", err, out)
		}
		var env struct {
			Data struct {
				Item map[string]any `json:"item"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("show --json printed %q: %v", out, err)
		}
		r, ok := env.Data.Item["archiveReason"]
		s, _ := r.(string)
		return s, ok
	}
	if r, ok := reason(); ok {
		t.Errorf("a present item's JSON carries archiveReason %q", r)
	}
	if out, err := runCLIJSON(t, db, root, "rm", string(pid), "--apply"); err != nil {
		t.Fatalf("rm: %v (%s)", err, out)
	}
	if r, _ := reason(); r != "trash" {
		t.Errorf("archiveReason = %q, want trash", r)
	}
	out, err := runCLIText(t, db, root, "show", string(pid))
	if err != nil {
		t.Fatalf("show: %v (%s)", err, out)
	}
	if !strings.Contains(out, "kind/state:   track / archived (trash)\n") {
		t.Errorf("show printed\n%s\nwant the state line to name the reason", out)
	}
}
