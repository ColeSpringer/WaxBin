//go:build windows

package organize

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPlanFreesADestinationARespelledFileVacates: on a case-insensitive filesystem a file
// renamed by case outside WaxBin still answers to the spelling the catalog holds, so the
// move the plan makes of it frees its name for another move, as Execute will wait for it.
func TestPlanFreesADestinationARespelledFileVacates(t *testing.T) {
	t.Parallel()
	o, st, lib := holdLib(t)
	one := holdTrack(t, st, lib, filepath.Join("A", "Al", "02 - Two.mp3"), "e-1", "One", 1)
	two := holdTrack(t, st, lib, filepath.Join("in", "two.mp3"), "e-2", "Two", 2)
	if err := os.Rename(one.DisplayPath, filepath.Join(lib.RootPath(), "A", "Al", "02 - two.mp3")); err != nil {
		t.Fatal(err)
	}
	if plan := planFor(t, o, lib, nativeProfile, one, two); plan.Pending() != 2 {
		t.Fatalf("plan = %+v, want both moves pending", plan.Actions)
	}
}
