package main

import (
	"testing"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

func TestParseAsOf(t *testing.T) {
	// Empty means "no recorded time": stamp at server now.
	if got, err := parseAsOf(""); err != nil || got != nil {
		t.Errorf("parseAsOf(\"\") = (%v, %v), want (nil, nil)", got, err)
	}

	// A plausible nanosecond stamp is taken verbatim.
	const ns int64 = 1_770_000_000_000_000_000 // ~2026 in ns
	got, err := parseAsOf("1770000000000000000")
	if err != nil || got == nil || *got != ns {
		t.Errorf("parseAsOf(ns) = (%v, %v), want %d", got, err, ns)
	}

	// RFC3339 resolves to that instant's UnixNano.
	rfc := "2026-01-02T03:04:05Z"
	want, _ := time.Parse(time.RFC3339, rfc)
	got, err = parseAsOf(rfc)
	if err != nil || got == nil || *got != want.UnixNano() {
		t.Errorf("parseAsOf(rfc) = (%v, %v), want %d", got, err, want.UnixNano())
	}

	// A seconds/millis timestamp (the common unit mix-up) is rejected, not silently
	// read as an epoch-adjacent stale time.
	for _, wrong := range []string{"1770000000", "1770000000000", "0", "-5"} {
		if _, err := parseAsOf(wrong); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("parseAsOf(%q) err = %v, want CodeInvalid (too small for ns)", wrong, err)
		}
	}

	// Non-integer, non-RFC3339 garbage is rejected.
	if _, err := parseAsOf("yesterday"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("parseAsOf(garbage) err = %v, want CodeInvalid", err)
	}

	// An RFC3339 date outside the int64-nanosecond range (~1678..2262) is rejected
	// rather than silently overflowing UnixNano to a garbage stamp.
	for _, oob := range []string{"3000-01-01T00:00:00Z", "1000-01-01T00:00:00Z"} {
		if _, err := parseAsOf(oob); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("parseAsOf(%q) err = %v, want CodeInvalid (out of ns range)", oob, err)
		}
	}
}
