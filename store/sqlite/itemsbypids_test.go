package sqlite_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// putItems scans n distinct tracks and returns their item pids in creation order.
func putItems(t *testing.T, ctx context.Context, st interface {
	PutScannedTrack(context.Context, model.PutScannedTrackInput) (*model.ScanItemResult, error)
}, libID int64, n int) []model.PID {
	t.Helper()
	pids := make([]model.PID, n)
	for i := 0; i < n; i++ {
		s := fmt.Sprintf("%04d", i)
		r, err := st.PutScannedTrack(ctx,
			input(libID, "/lib/"+s+".mp3", "sha256:E"+s, "sha256:C"+s, "T"+s))
		if err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		pids[i] = r.ItemPID
	}
	return pids
}

func pidSlice(views []*model.ItemView) []model.PID {
	out := make([]model.PID, len(views))
	for i, v := range views {
		out[i] = v.PID
	}
	return out
}

func equalPIDs(a, b []model.PID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestItemsByPIDsPreservesOrderAndSkipsMissing(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	pids := putItems(t, ctx, st, lib.ID, 5)

	missing := model.NewPID()
	// A deliberately non-creation order, with a missing pid interleaved.
	req := []model.PID{pids[2], missing, pids[0], pids[4], pids[1], pids[3]}
	want := []model.PID{pids[2], pids[0], pids[4], pids[1], pids[3]}

	got, err := st.ItemsByPIDs(ctx, req)
	if err != nil {
		t.Fatalf("ItemsByPIDs: %v", err)
	}
	if g := pidSlice(got); !equalPIDs(g, want) {
		t.Fatalf("order = %v, want %v (missing pid must be skipped, input order preserved)", g, want)
	}
}

func TestItemsByPIDsDedupsAndHandlesEmpty(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	pids := putItems(t, ctx, st, lib.ID, 2)

	// Duplicates of both present and missing pids collapse: each distinct present
	// pid appears once at its first position, missing pids never appear.
	missing := model.NewPID()
	got, err := st.ItemsByPIDs(ctx, []model.PID{pids[0], pids[0], missing, pids[1], pids[0], missing})
	if err != nil {
		t.Fatalf("ItemsByPIDs: %v", err)
	}
	if g := pidSlice(got); !equalPIDs(g, []model.PID{pids[0], pids[1]}) {
		t.Fatalf("dedup = %v, want each distinct present pid once at its first position", g)
	}

	empty, err := st.ItemsByPIDs(ctx, nil)
	if err != nil || empty != nil {
		t.Fatalf("empty input = (%v, %v), want (nil, nil)", empty, err)
	}
}

// TestItemsByPIDsChunkBoundary crosses the idBatchSize (500) IN-clause boundary:
// a request far larger than one chunk (real items scattered among many missing
// pids) must span multiple SELECTs yet still return every real item exactly once,
// in request order.
func TestItemsByPIDsChunkBoundary(t *testing.T) {
	ctx := context.Background()
	st, lib := openTestStore(t)
	real := putItems(t, ctx, st, lib.ID, 6)

	// Build a 1300-element request so the lookup spans three 500-pid chunks
	// ([0,500), [500,1000), [1000,1300)), scattering the six real pids across all
	// three: chunk 0 (index 3), chunk 1 (501, 700, 900), chunk 2 (1150, 1250).
	req := make([]model.PID, 0, 1300)
	realAt := map[int]int{3: 0, 501: 1, 700: 2, 900: 3, 1150: 4, 1250: 5}
	for i := 0; i < 1300; i++ {
		if ri, ok := realAt[i]; ok {
			req = append(req, real[ri])
			continue
		}
		req = append(req, model.NewPID())
	}
	want := []model.PID{real[0], real[1], real[2], real[3], real[4], real[5]}

	got, err := st.ItemsByPIDs(ctx, req)
	if err != nil {
		t.Fatalf("ItemsByPIDs: %v", err)
	}
	if g := pidSlice(got); !equalPIDs(g, want) {
		t.Fatalf("across chunk boundary got %v, want %v", g, want)
	}
}
