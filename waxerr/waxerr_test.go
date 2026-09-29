package waxerr_test

import (
	"context"
	"errors"
	"fmt"
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

func TestWrapKeepsFirstClass(t *testing.T) {
	inner := waxerr.New(waxerr.CodeNotFound, "store.Get", "no such pid")
	outer := waxerr.Wrap(waxerr.CodeIO, "facade.Get", inner)
	if got := waxerr.CodeOf(outer); got != waxerr.CodeNotFound {
		t.Fatalf("wrapped class = %s, want not_found kept", got)
	}
	if !waxerr.Is(outer, waxerr.CodeNotFound) || waxerr.Is(outer, waxerr.CodeIO) {
		t.Fatal("Is should follow the kept class, not the wrapper's")
	}
	if !errors.Is(outer, inner) || outer.Error() != "facade.Get: store.Get: no such pid" {
		t.Fatalf("wrap lost its cause or op: %v", outer)
	}
	if got := waxerr.CodeOf(waxerr.Wrapf(waxerr.CodeIO, "op", inner, "reading %s", "x")); got != waxerr.CodeNotFound {
		t.Fatalf("Wrapf class = %s, want not_found kept", got)
	}
	if got := waxerr.CodeOf(waxerr.Wrap(waxerr.CodeIO, "op", fmt.Errorf("walk: %w", inner))); got != waxerr.CodeNotFound {
		t.Fatalf("class under a plain wrapper = %s, want not_found found through it", got)
	}
	if got := waxerr.CodeOf(waxerr.Wrap(waxerr.CodeIO, "op", errors.New("disk"))); got != waxerr.CodeIO {
		t.Fatalf("raw cause class = %s, want the wrapper's io", got)
	}
	if waxerr.Wrap(waxerr.CodeIO, "op", nil) != nil || waxerr.Wrapf(waxerr.CodeIO, "op", nil, "x") != nil {
		t.Fatal("wrapping a nil cause should yield nil")
	}
}

func TestFromContextOverridesClassOnCancel(t *testing.T) {
	generic := waxerr.Wrap(waxerr.CodeIO, "store.writeTx", context.Canceled)
	if got := waxerr.CodeOf(waxerr.FromContext("op", generic, waxerr.CodeIO)); got != waxerr.CodeCanceled {
		t.Fatalf("canceled under an io wrap -> %s, want canceled", got)
	}
	kept := waxerr.New(waxerr.CodeConflict, "lease", "busy")
	if got := waxerr.CodeOf(waxerr.FromContext("op", kept, waxerr.CodeIO)); got != waxerr.CodeConflict {
		t.Fatalf("classified non-cancel cause -> %s, want conflict kept", got)
	}
}

func TestClassifyOverridesClass(t *testing.T) {
	inner := waxerr.New(waxerr.CodeNotFound, "cipher", "no such key")
	if got := waxerr.CodeOf(waxerr.Classify(waxerr.CodeInvalid, "op", inner)); got != waxerr.CodeInvalid {
		t.Fatalf("Classify over a classified cause = %s, want invalid", got)
	}
	err := waxerr.Classifyf(waxerr.CodeInvalid, "op", inner, "opening %s", "k")
	if got := waxerr.CodeOf(err); got != waxerr.CodeInvalid || err.Error() != "op: opening k: cipher: no such key" {
		t.Fatalf("Classifyf = %s %q, want invalid with the message and cause", got, err)
	}
	if waxerr.Classify(waxerr.CodeInvalid, "op", nil) != nil || waxerr.Classifyf(waxerr.CodeInvalid, "op", nil, "x") != nil {
		t.Fatal("classifying a nil cause should yield nil")
	}
}
