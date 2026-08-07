package main

import (
	"strings"
	"testing"
)

// TestStateSetFlagValidation checks the contradictory/ineffective flag combinations
// on `state set` are rejected in the command itself, before the catalog is opened
// and its write lock taken. Reaching the errors with an empty globals (no database
// configured) proves the checks run on the early path.
func TestStateSetFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		// star and unstar are mutually exclusive (cobra names both flags in the error).
		{"star and unstar", []string{"set", "01J0X", "--star", "--unstar"}, "unstar"},
		// --as-of with no stamp-writing operation would be silently ignored.
		{"as-of without op", []string{"set", "01J0X", "--played", "--as-of", "1770000000000000000"}, "--as-of applies only"},
		// --unplayed clears finished too, so it contradicts both flags.
		{"played and unplayed", []string{"set", "01J0X", "--played", "--unplayed"}, "unplayed"},
		{"finished and unplayed", []string{"set", "01J0X", "--finished", "--unplayed"}, "unplayed"},
		{"finished and unfinished", []string{"set", "01J0X", "--finished", "--unfinished"}, "unfinished"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newStateCmd(&globals{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("args %v: expected a validation error, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("args %v: error = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}
