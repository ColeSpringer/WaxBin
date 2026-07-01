package main

import (
	"strings"
	"testing"

	"github.com/colespringer/waxbin/waxerr"
)

// TestAuditRejectsUnknownCheck ensures a mistyped --check name is rejected before
// the audit runs, rather than silently matching nothing and reporting no issues.
// Validation happens before the catalog is opened, so no database is needed.
func TestAuditRejectsUnknownCheck(t *testing.T) {
	cmd := newAuditCmd(&globals{})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--check", "missing_replay_gain"}) // typo: real name is missing_replaygain
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown check name, got nil")
	}
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("error code = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "unknown check") {
		t.Errorf("error = %q, want it to mention the unknown check", err)
	}
}
