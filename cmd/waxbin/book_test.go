package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBookShowsWhereEachPartStarts: `book` prints where each part starts on the book's
// timeline, and its JSON gives each part's start and the length it advances the book by,
// which sum to the total.
func TestBookShowsWhereEachPartStarts(t *testing.T) {
	t.Parallel()
	db, root, pids := kindCLIFixture(t)
	if _, err := runKindText(t, db, root, string(pids[0]), string(pids[1]), "--to", "book", "--yes"); err != nil {
		t.Fatalf("kind: %v", err)
	}
	out, err := runCLIJSON(t, db, root, "book", string(pids[0]))
	if err != nil {
		t.Fatalf("book --json: %v", err)
	}
	var env struct {
		Data struct {
			Total int64 `json:"totalDurationMs"`
			Parts []struct {
				Path        string `json:"path"`
				StartMS     *int64 `json:"startMs"`
				EffectiveMS *int64 `json:"effectiveMs"`
			} `json:"parts"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || len(env.Data.Parts) != 2 {
		t.Fatalf("book --json printed %s (err %v), want two parts", out, err)
	}
	first, second := env.Data.Parts[0], env.Data.Parts[1]
	if first.StartMS == nil || second.StartMS == nil || first.EffectiveMS == nil || second.EffectiveMS == nil ||
		*first.StartMS != 0 || *second.StartMS != *first.EffectiveMS || *first.EffectiveMS <= 0 ||
		*first.EffectiveMS+*second.EffectiveMS != env.Data.Total {
		t.Errorf("parts = %s, want the second starting where the first ends and the lengths summing to %d", out, env.Data.Total)
	}

	text, err := runCLIText(t, db, root, "book", string(pids[0]))
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	if !strings.Contains(text, first.Path+"  from 0:00:00 (") || !strings.Contains(text, second.Path+"  from "+durationLabel(*second.StartMS)+" (") {
		t.Errorf("book printed %q, want each part's start beside its path", text)
	}
}
