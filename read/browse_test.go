package read

import (
	"testing"

	"github.com/colespringer/waxbin/model"
)

func TestDiscoveryListValid(t *testing.T) {
	for _, d := range DiscoveryLists() {
		if !d.Valid() {
			t.Errorf("DiscoveryLists() returned an invalid value %q", d)
		}
	}
	if DiscoveryList("nonsense").Valid() {
		t.Error("an unknown discovery list reported valid")
	}
}

func TestDiscoveryListPerUserAndKinds(t *testing.T) {
	perUser := map[DiscoveryList]bool{
		ListMostPlayed: true, ListRecentlyPlayed: true, ListStarred: true, ListInProgress: true,
	}
	for _, d := range DiscoveryLists() {
		if got := d.PerUser(); got != perUser[d] {
			t.Errorf("%q PerUser() = %v, want %v", d, got, perUser[d])
		}
	}
	// Only the two kind-scoped lists report kinds; in-progress spans all of them,
	// since a half-read audiobook belongs there as much as a half-heard episode.
	kinds := map[DiscoveryList][]model.Kind{
		ListNewest:         {model.KindTrack, model.KindBook},
		ListRecentEpisodes: {model.KindEpisode},
	}
	for _, d := range DiscoveryLists() {
		got, want := d.Kinds(), kinds[d]
		if len(got) != len(want) {
			t.Errorf("%q Kinds() = %v, want %v", d, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q Kinds() = %v, want %v", d, got, want)
				break
			}
		}
	}
}
