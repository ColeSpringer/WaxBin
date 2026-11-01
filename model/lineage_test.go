package model_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestSettleIdentitiesNamesWhereEachItemEnded: an absorbed item names the end of its
// chain, a created item names where its source ended, a cycle ends, and an item absorbed
// in the end is neither converted nor created.
func TestSettleIdentitiesNamesWhereEachItemEnded(t *testing.T) {
	t.Parallel()
	absorbed := map[model.PID]model.PID{"A": "B", "B": "C", "W": "O", "O": "P", "X": "Y", "Y": "X"}
	created := map[model.PID]model.PID{"N": "W", "M": "S", "B": "S"}
	converted := model.SettleIdentities([]model.PID{"B", "K"}, absorbed, created)
	if !slices.Equal(converted, []model.PID{"K"}) {
		t.Errorf("converted = %v, want only K, B having been absorbed", converted)
	}
	for p, want := range map[model.PID]model.PID{"A": "C", "B": "C", "W": "P", "O": "P"} {
		if absorbed[p] != want {
			t.Errorf("absorbed[%s] = %s, want %s", p, absorbed[p], want)
		}
	}
	if absorbed["X"] == "" || absorbed["Y"] == "" {
		t.Errorf("a cycle lost its entries: %v", absorbed)
	}
	if want := map[model.PID]model.PID{"N": "P", "M": "S"}; !maps.Equal(created, want) {
		t.Errorf("created = %v, want %v", created, want)
	}
}
