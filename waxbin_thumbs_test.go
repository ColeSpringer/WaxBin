package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/waxerr"
)

// TestThumbPrunePolicyZeroValueIsRefused pins the one thing the pointer budget buys.
// A struct whose zero value emptied the cache would make a forgotten bound look like
// a working call, so the unbounded policy is refused instead of defaulted.
func TestThumbPrunePolicyZeroValueIsRefused(t *testing.T) {
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())

	_, _, err := lib.PruneThumbnails(ctx, waxbin.ThumbPrunePolicy{})
	if waxerr.CodeOf(err) != waxerr.CodeInvalid {
		t.Errorf("err = %v, want a CodeInvalid refusal for a policy with no bound", err)
	}
}

// TestThumbPruneZeroIsABound is the other half, and it has to hold for both bounds
// alike: a field set to zero is a real instruction, not the absence of one. An age
// that read zero as "unbounded" would refuse `--older-than 0d` for naming no bound,
// which is the opposite of what was typed.
func TestThumbPruneZeroIsABound(t *testing.T) {
	ctx := context.Background()
	var zeroAge time.Duration
	var zeroBudget int64
	for name, policy := range map[string]waxbin.ThumbPrunePolicy{
		"budget": {MaxBytes: &zeroBudget},
		"age":    {OlderThan: &zeroAge},
	} {
		lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())
		removed, freed, err := lib.PruneThumbnails(ctx, policy)
		if err != nil {
			t.Errorf("PruneThumbnails with a zero %s: %v", name, err)
			continue
		}
		if removed != 0 || freed != 0 {
			t.Errorf("zero %s removed %d rows/%d bytes from an empty cache, want none", name, removed, freed)
		}
	}
}

// TestThumbCacheStatsOnAnEmptyCatalog pins that the census answers before anything is
// scanned, since "how much is this costing me" is asked of catalogs in every state.
func TestThumbCacheStatsOnAnEmptyCatalog(t *testing.T) {
	ctx := context.Background()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), t.TempDir())

	rep, err := lib.ThumbCacheStats(ctx)
	if err != nil {
		t.Fatalf("ThumbCacheStats: %v", err)
	}
	if rep.Rows != 0 || rep.ArtSources != 0 {
		t.Errorf("report = %+v, want an empty census", rep)
	}
}
