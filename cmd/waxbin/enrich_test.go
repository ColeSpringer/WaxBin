package main

import (
	"strings"
	"testing"
)

// TestEnrichScopeFlagValidation ensures the scope flag-shape errors fire in the
// command itself, before a server is dialed or the catalog opened (and its
// write lock taken); the facade re-validates for embedders and the proxy.
// Validation happens with no database configured, so reaching it proves the
// early path.
func TestEnrichScopeFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"both scopes", []string{"--item", "01J0X", "--entity", "artist:01J0Y"}, "not both"},
		{"malformed entity", []string{"--entity", "artistonly"}, "wants type:pid"},
		{"non-enrichable entity type", []string{"--entity", "genre:01J0Y"}, "non-enrichable entity type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newEnrichCmd(&globals{})
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
