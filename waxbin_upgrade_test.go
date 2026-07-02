package waxbin_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
)

// TestFindUpgradesGroupsAltEncodings verifies the quality/upgrade policy groups
// two encodings of one recording and leaves an unrelated track ungrouped, marking
// exactly one keeper per group.
func TestFindUpgradesGroupsAltEncodings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	const rate = 22050
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	transcoded := testaudio.Reencode(orig, 0.85, 42) // same recording, different bytes
	other := testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)

	writeFile(t, filepath.Join(root, "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	writeFile(t, filepath.Join(root, "beta.wav"), testaudio.EncodeWAV16(rate, transcoded))
	writeFile(t, filepath.Join(root, "gamma.wav"), testaudio.EncodeWAV16(rate, other))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := lib.Analyze(ctx, waxbin.AnalyzeOptions{}); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	groups, err := lib.FindUpgrades(ctx)
	if err != nil {
		t.Fatalf("find upgrades: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 alt-encoding group (alpha+beta), got %d: %+v", len(groups), groups)
	}
	g := groups[0]
	if len(g.Members) != 2 {
		t.Fatalf("group should have 2 members, got %d", len(g.Members))
	}

	bestCount := 0
	inGroup := map[model.PID]bool{}
	for _, m := range g.Members {
		inGroup[m.ItemPID] = true
		if m.Best {
			bestCount++
		}
		if m.Codec == "" {
			t.Errorf("member %s has no codec quality read", m.ItemPID)
		}
	}
	if bestCount != 1 {
		t.Errorf("exactly one member should be the keeper, got %d", bestCount)
	}
	alpha := itemPIDByTitle(t, ctx, lib, "alpha")
	beta := itemPIDByTitle(t, ctx, lib, "beta")
	gamma := itemPIDByTitle(t, ctx, lib, "gamma")
	if !inGroup[alpha] || !inGroup[beta] {
		t.Errorf("group should contain alpha and beta, got %+v", g.Members)
	}
	if inGroup[gamma] {
		t.Errorf("gamma (unrelated) must not be grouped")
	}
}
