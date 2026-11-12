package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestRespelledPIDHint: a pid typed in lower case finds nothing, and the hint gives the
// spelling that would. It speaks only on a not-found, and only of an argument that is a
// pid once upper-cased.
func TestRespelledPIDHint(t *testing.T) {
	t.Parallel()
	pid := string(model.NewPID())
	lower := strings.ToLower(pid)
	notFound := waxerr.New(waxerr.CodeNotFound, "store.ItemByPID", "no such item: "+lower)
	want := "a pid is written in upper case: " + pid

	for _, tc := range []struct {
		name string
		err  error
		args []string
		want string
	}{
		{"a positional pid", notFound, []string{"show", lower}, want},
		{"a flag's value", notFound, []string{"query", "--user", lower}, want},
		{"a flag=value", notFound, []string{"query", "--user=" + lower}, want},
		{"the catalog's spelling", notFound, []string{"show", pid}, ""},
		{"an argument that is no pid", notFound, []string{"show", "some-title"}, ""},
		{"another failure", errors.New("disk full"), []string{"show", lower}, ""},
		{"an invalid argument", waxerr.New(waxerr.CodeInvalid, "op", "bad"), []string{"show", lower}, ""},
	} {
		if got := respelledPIDHint(tc.err, tc.args); got != tc.want {
			t.Errorf("%s: hint = %q, want %q", tc.name, got, tc.want)
		}
	}
}
