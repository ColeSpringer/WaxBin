package main

import (
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"0d", 0},
		{"36h", 36 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		got, err := parseAge("trash empty", tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
	// A negative retention age has no meaning on either caller: it would purge
	// everything newer than a moment in the future, which is everything.
	for _, in := range []string{"", "d", "x d", "thirty days", "1.5x", "-30d", "-1h"} {
		if _, err := parseAge("trash empty", in); err == nil {
			t.Errorf("parseAge(%q) should fail", in)
		}
	}
}
