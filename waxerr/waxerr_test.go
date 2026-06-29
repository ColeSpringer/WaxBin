package waxerr_test

import (
	"context"
	"errors"
	"testing"

	"github.com/colespringer/waxbin/waxerr"
)

func TestFromContext(t *testing.T) {
	if got := waxerr.CodeOf(waxerr.FromContext("op", context.Canceled, waxerr.CodeIO)); got != waxerr.CodeCanceled {
		t.Fatalf("canceled -> %s, want canceled", got)
	}
	if got := waxerr.CodeOf(waxerr.FromContext("op", context.DeadlineExceeded, waxerr.CodeIO)); got != waxerr.CodeCanceled {
		t.Fatalf("deadline -> %s, want canceled", got)
	}
	if got := waxerr.CodeOf(waxerr.FromContext("op", errors.New("disk"), waxerr.CodeIO)); got != waxerr.CodeIO {
		t.Fatalf("plain error -> %s, want io (the fallback)", got)
	}
	if err := waxerr.FromContext("op", nil, waxerr.CodeIO); err != nil {
		t.Fatalf("nil cause should stay nil, got %v", err)
	}
}
