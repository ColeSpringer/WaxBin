package proxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/waxerr"
)

// TestWireErrorKeepsTheErrorsText: an error rebuilt on the client reads as it did on the
// server, whichever way it was built: the op once, and a wrapped cause kept.
func TestWireErrorKeepsTheErrorsText(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"a message", waxerr.New(waxerr.CodeNotFound, "store.Get", "no such pid")},
		{"a wrapped cause", waxerr.Wrap(waxerr.CodeIO, "store.Apply", errors.New("disk full"))},
		{"a message and a cause", waxerr.Wrapf(waxerr.CodeIO, "netsafe.Do", errors.New("connection refused"), "fetching %s", "https://example.org")},
		{"an op alone", waxerr.New(waxerr.CodeInternal, "store.Close", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fromWireError(toWireError(tc.err))
			if got.Error() != tc.err.Error() {
				t.Errorf("rebuilt error = %q, want %q", got.Error(), tc.err.Error())
			}
			if waxerr.CodeOf(got) != waxerr.CodeOf(tc.err) {
				t.Errorf("rebuilt code = %s, want %s", waxerr.CodeOf(got), waxerr.CodeOf(tc.err))
			}
		})
	}
}

// TestWireErrorCarriesTheReason: a reason crosses the wire beside the code, a frame from a
// peer that sends none decodes as before, and an error with no reason sends no field.
func TestWireErrorCarriesTheReason(t *testing.T) {
	t.Parallel()
	err := waxerr.New(waxerr.CodeConflict, "jobs.Run", "another job holds scope fs-mutate").WithReason("lease_held")
	raw, jerr := json.Marshal(toWireError(err))
	if jerr != nil {
		t.Fatal(jerr)
	}
	var we wireError
	if jerr := json.Unmarshal(raw, &we); jerr != nil {
		t.Fatal(jerr)
	}
	got := fromWireError(&we)
	if waxerr.ReasonOf(got) != "lease_held" || !waxerr.Is(got, waxerr.CodeConflict) || got.Error() != err.Error() {
		t.Errorf("rebuilt = %s %q %q, want conflict lease_held %q", waxerr.CodeOf(got), waxerr.ReasonOf(got), got, err)
	}

	var old wireError
	if jerr := json.Unmarshal([]byte(`{"code":"conflict","op":"jobs.Run","msg":"busy"}`), &old); jerr != nil {
		t.Fatal(jerr)
	}
	if got := fromWireError(&old); waxerr.ReasonOf(got) != "" || !waxerr.Is(got, waxerr.CodeConflict) || got.Error() != "jobs.Run: busy" {
		t.Errorf("a frame without the field = %s %q %q, want conflict with no reason", waxerr.CodeOf(got), waxerr.ReasonOf(got), got)
	}

	plain, jerr := json.Marshal(toWireError(waxerr.New(waxerr.CodeIO, "op", "disk")))
	if jerr != nil || strings.Contains(string(plain), "reason") {
		t.Errorf("an unreasoned error = %s (err %v), want no reason field", plain, jerr)
	}
}
