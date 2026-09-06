package waxbin

import (
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
)

// TestEnrichConfigRetryWindow pins where the retry window's default lives. The enrich
// package treats a zero duration as "never retry", so an embedder building its Config
// directly keeps the behaviour it had; the facade is what turns an unset config key into
// the 30 day default, and an explicit 0 back into never.
func TestEnrichConfigRetryWindow(t *testing.T) {
	days := func(n int) *int { return &n }
	cases := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"unset defaults to thirty days", nil, 30 * 24 * time.Hour},
		{"zero never retries", days(0), 0},
		{"a negative value never retries", days(-1), 0},
		{"an explicit window is honoured", days(7), 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := enrichConfig(config.EnrichConfig{RetryMissesAfterDays: tc.in}, nil).RetryMissesAfter
			if got != tc.want {
				t.Errorf("RetryMissesAfter = %v, want %v", got, tc.want)
			}
		})
	}
}
