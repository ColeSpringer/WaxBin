package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin/port"
)

// TestManifestCountsWithoutTheBody drives `manifest` through a catalog with a logged
// session and checks the header it prints carries the export's counts and version.
func TestManifestCountsWithoutTheBody(t *testing.T) {
	db, root, pid := creditCLIFixture(t)
	if _, err := runStateCLI(t, db, root, "set", string(pid), "--played", "--session", "240000", "--as-of", "2020-01-02T03:04:05Z"); err != nil {
		t.Fatalf("state set: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "manifest")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	var env struct {
		Data struct {
			Format       string `json:"format"`
			Version      int    `json:"version"`
			Items        int    `json:"items"`
			PlayStates   int    `json:"playStates"`
			PlaySessions int    `json:"playSessions"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("manifest printed %q: %v", out, err)
	}
	if d := env.Data; d.Format != port.ExportFormat || d.Version != port.ExportVersion ||
		d.Items != 1 || d.PlayStates != 1 || d.PlaySessions != 1 {
		t.Errorf("manifest = %+v, want v%d with one item, one play state, one session", d, port.ExportVersion)
	}
}
