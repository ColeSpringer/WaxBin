package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
)

// TestDetachDurabilityWarning: the caveat tracks whether the file's tags actually lost
// the release ids, not whether --write-back was asked for. A strip that reported
// failures is surfaced as a warning and returns a nil error, which is exactly the case
// where the linkage comes back and the user has to hear about it.
func TestDetachDurabilityWarning(t *testing.T) {
	stripFailed := &waxbin.WriteBackError{
		ItemPID: "i1",
		Failures: []waxbin.WriteBackFailure{{
			Path:   "/lib/one/01.flac",
			Reason: "on-disk tag write-back is unavailable for a file shared by multiple items",
		}},
	}
	for _, tc := range []struct {
		name      string
		writeBack bool
		err       error
		want      string
	}{
		{"no write-back", false, nil, "pass --write-back"},
		{"clean strip", true, nil, ""},
		{"failed strip", true, error(stripFailed), "retag, move, or content change"},
		{"failed strip wrapped", true, errors.Join(errors.New("ctx"), stripFailed), "retag, move, or content change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detachDurabilityWarning(tc.writeBack, tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("warning = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}
