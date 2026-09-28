package proxy

import (
	"errors"
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
