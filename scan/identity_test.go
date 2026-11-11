package scan

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// TestIdentitiesSettleWhereEachItemEnded: an item folded into one that is folded in turn
// names the last, an item absorbed in the end is neither converted nor created, a track a
// rip minted from the whole-file item the same read absorbed names where that item went,
// and a rebuild's stamp renames the item wherever the log names it.
func TestIdentitiesSettleWhereEachItemEnded(t *testing.T) {
	t.Parallel()
	l := &identities{}
	l.note(&model.ScanItemResult{ItemPID: "B", Rekinded: true, Absorbed: map[model.PID]model.PID{"A": "B"}})
	l.note(&model.ScanItemResult{ItemPID: "N", ItemCreated: true, Minted: map[model.PID]model.PID{"N": "S"}})
	l.note(&model.ScanItemResult{ItemPID: "C", Absorbed: map[model.PID]model.PID{"B": "C", "N": "C"}})
	l.note(&model.ScanItemResult{ItemPID: "F", Rekinded: true, Minted: map[model.PID]model.PID{"M": "F"}})
	l.note(&model.ScanItemResult{Absorbed: map[model.PID]model.PID{"W": "O"}, Minted: map[model.PID]model.PID{"T2": "W"}})
	l.repid("F", "STAMP")
	var res Result
	l.fill(&res)
	if !slices.Equal(res.Converted, []model.PID{"STAMP"}) {
		t.Errorf("converted = %v, want only the item still standing, under its stamp", res.Converted)
	}
	if want := map[model.PID]model.PID{"A": "C", "B": "C", "N": "C", "W": "O"}; !maps.Equal(res.Absorbed, want) {
		t.Errorf("absorbed = %v, want %v", res.Absorbed, want)
	}
	if want := map[model.PID]model.PID{"M": "STAMP", "T2": "O"}; !maps.Equal(res.Created, want) {
		t.Errorf("created = %v, want %v", res.Created, want)
	}
	total := Result{Absorbed: map[model.PID]model.PID{"C": "D"}}
	total.MergeIdentities(&res)
	if total.Absorbed["A"] != "D" || total.Absorbed["C"] != "D" {
		t.Errorf("merged absorbed = %v, want every chain ending at D", total.Absorbed)
	}
}

// TestIdentitiesScaleWithTheItemsTheyName: a scan that converts a whole library names each
// item once, in time that grows with the library rather than with its square. The time is
// held against what filing the same pids in a map takes, timed before and after it, not
// against a fixed budget: a loaded machine and the race detector slow both alike, while a
// naming that looked through the items before it would cost thousands of times as much.
func TestIdentitiesScaleWithTheItemsTheyName(t *testing.T) {
	t.Parallel()
	const n, allowed = 100_000, 40
	pidOf := func(i int) model.PID { return model.PID(fmt.Sprintf("01M690PMPJ%016d", i)) }
	filing := func() time.Duration {
		start := time.Now()
		seen := map[model.PID]bool{}
		var pids []model.PID
		for i := range n {
			if pid := pidOf(i); !seen[pid] {
				seen[pid] = true
				pids = append(pids, pid)
			}
		}
		if len(pids) != n {
			t.Fatalf("filed %d pids, want %d", len(pids), n)
		}
		return time.Since(start)
	}
	before := filing()
	start := time.Now()
	l := &identities{}
	for i := range n {
		pid := pidOf(i)
		l.note(&model.ScanItemResult{ItemPID: pid, Rekinded: true})
		l.note(&model.ScanItemResult{ItemPID: pid, Rekinded: true})
	}
	var res, total Result
	l.fill(&res)
	total.MergeIdentities(&res)
	total.MergeIdentities(&res)
	took := time.Since(start)
	if filed := max(before, filing()); took > allowed*filed {
		t.Errorf("naming %d conversions took %v, over %d times the %v filing their pids took", n, took, allowed, filed)
	}
	if len(total.Converted) != n {
		t.Errorf("converted names %d items, want each of %d once", len(total.Converted), n)
	}
}
