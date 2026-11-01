package waxbin

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/internal/testcatalog"
)

// OpenSeeded is Open on a seeded catalog where a read-write open would create one, so a
// test does not pay for building the schema; both halves of the root tests open through
// it. A read-only open, or one a test expects to be refused, calls Open itself, since
// whether a catalog comes to exist is then part of what it checks.
func OpenSeeded(tb testing.TB, ctx context.Context, opts Options) (*Library, error) {
	tb.Helper()
	if !opts.ReadOnly {
		testcatalog.Seed(tb, opts.DBPath)
	}
	return Open(ctx, opts)
}
