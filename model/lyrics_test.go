package model

import (
	"slices"
	"testing"
)

func TestNormalizeSyncedLines(t *testing.T) {
	t.Parallel()
	line := func(ms int64, text string) SyncedLine { return SyncedLine{TimeMS: ms, Text: text} }

	sorted := []SyncedLine{line(0, "a"), line(10, "b"), line(10, "c"), line(20, "d")}
	if got := NormalizeSyncedLines(sorted); &got[0] != &sorted[0] || len(got) != len(sorted) {
		t.Errorf("lines already in order came back as another slice: %+v", got)
	}

	shuffled := []SyncedLine{line(20, "d"), line(10, "b"), line(0, "a"), line(10, "c")}
	sent := slices.Clone(shuffled)
	if got := NormalizeSyncedLines(shuffled); !slices.Equal(got, sorted) {
		t.Errorf("shuffled lines = %+v, want %+v with the tie in the order sent", got, sorted)
	}
	if !slices.Equal(shuffled, sent) {
		t.Errorf("the caller's slice was reordered: %+v", shuffled)
	}

	// Sorted by the time sent and only then floored, so the lines before the start keep
	// their order in time.
	early := []SyncedLine{line(0, "zero"), line(-200, "late"), line(-1500, "early")}
	sent = slices.Clone(early)
	want := []SyncedLine{line(0, "early"), line(0, "late"), line(0, "zero")}
	if got := NormalizeSyncedLines(early); !slices.Equal(got, want) {
		t.Errorf("lines before the start = %+v, want %+v", got, want)
	}
	if !slices.Equal(early, sent) {
		t.Errorf("the caller's slice was changed: %+v", early)
	}

	// In order already, but one time is before the start.
	floored := []SyncedLine{line(-5, "a"), line(3, "b")}
	if got := NormalizeSyncedLines(floored); !slices.Equal(got, []SyncedLine{line(0, "a"), line(3, "b")}) || floored[0].TimeMS != -5 {
		t.Errorf("an ordered line before the start = %+v (sent %+v), want it at 0 in a copy", got, floored)
	}

	// A time past what an .lrc timestamp can name is held at the last one it can, after
	// the sort, so the lines past it keep their order in time.
	late := []SyncedLine{line(1<<50, "last"), line(5, "a"), line(MaxSyncedLineMS+1, "first past")}
	want = []SyncedLine{line(5, "a"), line(MaxSyncedLineMS, "first past"), line(MaxSyncedLineMS, "last")}
	if got := NormalizeSyncedLines(late); !slices.Equal(got, want) {
		t.Errorf("lines past the ceiling = %+v, want %+v", got, want)
	}
	capped := []SyncedLine{line(5, "a"), line(MaxSyncedLineMS+1, "b")}
	if got := NormalizeSyncedLines(capped); got[1].TimeMS != MaxSyncedLineMS || capped[1].TimeMS != MaxSyncedLineMS+1 {
		t.Errorf("an ordered line past the ceiling = %+v (sent %+v), want it capped in a copy", got, capped)
	}
	atCeiling := []SyncedLine{line(0, "a"), line(MaxSyncedLineMS, "b")}
	if got := NormalizeSyncedLines(atCeiling); &got[0] != &atCeiling[0] {
		t.Errorf("lines within the range came back as another slice: %+v", got)
	}

	if got := NormalizeSyncedLines(nil); got != nil {
		t.Errorf("nil = %+v, want nil", got)
	}
}
