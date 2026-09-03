package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
)

// TestAnalyzeDecodesMusepack: Musepack files fingerprint and measure through
// WaxFlow, which is why the scanner picks them up rather than leaving them in the
// analyze pass's retry set.
func TestAnalyzeDecodesMusepack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for _, f := range [][2]string{{"ref-2s-sv7.mpc", "seven.mpc"}, {"ref-2s-sv8-chapters.mpc", "eight.mp+"}} {
		writeFile(t, filepath.Join(root, f[1]), testaudio.Fixture(t, f[0]))
	}
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	ares, err := lib.Analyze(ctx, waxbin.AnalyzeOptions{})
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if r := ares.Result; r.Analyzed != 2 || r.Skipped != 0 || r.Errored != 0 || r.LoudnessMeasured != 2 {
		t.Fatalf("analyze result = %+v, want both files analyzed and measured, none skipped", r)
	}
}
