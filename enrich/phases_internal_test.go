package enrich

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestPhaseKeysMatchTheModelList pins the run's own phase list against the vocabulary
// a phase-scoped force validates against, so adding a phase to one and not the other
// fails here rather than at a user's refused --force-phase.
func TestPhaseKeysMatchTheModelList(t *testing.T) {
	p := &Mock{ProviderName: "everything",
		Caps: CapAuxArt | CapArtistArt | CapCover | CapLyrics | CapFields | CapBookMeta}
	s := New(nil, Config{Contact: "test@example.com", MatchReleases: true, Providers: []Provider{p}}, nil)

	res := &Result{Reach: &model.EnrichScope{}}
	got := s.phases(&runState{}, res, nil)
	want := model.EnrichPhases()
	if len(got) != len(want) {
		t.Fatalf("built %d phases, model names %d", len(got), len(want))
	}
	for i := range got {
		if got[i].key != want[i] {
			t.Errorf("phase %d = %q, want %q", i, got[i].key, want[i])
		}
	}
}
