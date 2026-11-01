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
// item once, in time that grows with the library rather than with its square.
func TestIdentitiesScaleWithTheItemsTheyName(t *testing.T) {
	t.Parallel()
	const n = 50_000
	start := time.Now()
	l := &identities{}
	for i := range n {
		pid := model.PID(fmt.Sprintf("01M690PMPJ%016d", i))
		l.note(&model.ScanItemResult{ItemPID: pid, Rekinded: true})
		l.note(&model.ScanItemResult{ItemPID: pid, Rekinded: true})
	}
	var res, total Result
	l.fill(&res)
	total.MergeIdentities(&res)
	total.MergeIdentities(&res)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("naming %d conversions took %v", n, took)
	}
	if len(total.Converted) != n {
		t.Errorf("converted names %d items, want each of %d once", len(total.Converted), n)
	}
}
