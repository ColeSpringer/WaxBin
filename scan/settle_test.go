package scan

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// settleCounter counts the settles a scan asks for, and cancels the scan after its first
// put when cancel is set.
type settleCounter struct {
	model.Catalog
	settles  int
	deferred bool
	cancel   context.CancelFunc
}

func (c *settleCounter) SettleRollups(ctx context.Context) (model.RollupSettle, error) {
	c.settles++
	return c.Catalog.SettleRollups(ctx)
}

func (c *settleCounter) PutScannedTrack(ctx context.Context, in model.PutScannedTrackInput) (*model.ScanItemResult, error) {
	c.deferred = in.DeferRollups
	res, err := c.Catalog.PutScannedTrack(ctx, in)
	if c.cancel != nil {
		c.cancel()
	}
	return res, err
}

func settledCleanly(t *testing.T, st *sqlite.Store) {
	t.Helper()
	rep, err := st.VerifyDerived(context.Background())
	if err != nil || !rep.Consistent() || rep.RollupsPending != 0 {
		t.Errorf("verify = %+v (err %v), want consistent with nothing pending", rep, err)
	}
}

// TestScanSettlesEveryNPutsAndAtTheEnd: a scan defers its puts' rollups, settles them every
// RollupSettleEvery puts and when it ends, and a scan canceled partway still settles what
// it put.
func TestScanSettlesEveryNPutsAndAtTheEnd(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	for i := range 5 {
		writeMP3(t, filepath.Join(root, fmt.Sprintf("%d.mp3", i)), fmt.Sprintf("T%d", i), byte(i+1))
	}
	counter := &settleCounter{Catalog: st}
	sc := New(counter, meta.NewReader(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	sc.RollupSettleEvery = 2
	if _, err := sc.Scan(context.Background(), Request{Library: lib}, nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !counter.deferred {
		t.Error("the scan's puts did not defer their rollups")
	}
	// Five puts: after the second and the fourth, and at the end.
	if counter.settles != 3 {
		t.Errorf("settles = %d, want 3", counter.settles)
	}
	settledCleanly(t, st)

	writeMP3(t, filepath.Join(root, "late", "a.mp3"), "Late A", 9)
	writeMP3(t, filepath.Join(root, "late", "b.mp3"), "Late B", 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter.settles, counter.cancel = 0, cancel
	sc.RollupSettleEvery = 100
	if _, err := sc.Scan(ctx, Request{Library: lib}, nil); err == nil {
		t.Fatal("the canceled scan reported no error")
	}
	if counter.settles != 1 {
		t.Errorf("settles of the canceled scan = %d, want the one at its end", counter.settles)
	}
	settledCleanly(t, st)
}

// TestASingleFileScanRecomputesInThePut: a scan of one file outside a walk (an import, a
// kind change) has no settle to come, so its put recomputes the rollups itself.
func TestASingleFileScanRecomputesInThePut(t *testing.T) {
	t.Parallel()
	st, lib, _, _, root := fastPathFixture(t)
	path := filepath.Join(root, "one.mp3")
	writeMP3(t, path, "One", 1)
	counter := &settleCounter{Catalog: st}
	sc := New(counter, meta.NewReader(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := sc.ScanFile(context.Background(), lib, path); err != nil {
		t.Fatalf("scan file: %v", err)
	}
	if counter.deferred || counter.settles != 0 {
		t.Errorf("deferred %t, settles %d, want a put that recomputes with no settle", counter.deferred, counter.settles)
	}
	settledCleanly(t, st)
}
