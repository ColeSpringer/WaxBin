package enrich

import (
	"slices"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// TestPhaseKeysMatchTheModelList pins the run's own phase list against the vocabulary
// a phase-scoped force validates against, so adding a phase to one and not the other
// fails here rather than at a user's refused --force-phase.
func TestPhaseKeysMatchTheModelList(t *testing.T) {
	p := &Mock{ProviderName: "everything",
		Caps: CapAuxArt | CapArtistArt | CapCover | CapLyrics | CapFields | CapBookMeta}
	s := New(nil, Config{Contact: "test@example.com", MatchReleases: true, Providers: []Provider{p}}, nil)

	got := s.phaseKeys(s.providerList())
	want := model.EnrichPhases()
	if len(got) != len(want) {
		t.Fatalf("built %d phases, model names %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("phase %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCheckPhasesNamesTheRung: a forced phase this install cannot run is refused naming
// the rung it needs a provider for, since a provider serving auxiliary art for artists
// alone would not unlock it. Cover art is off, since the archive serves the group rung.
func TestCheckPhasesNamesTheRung(t *testing.T) {
	artistAux := &Mock{ProviderName: "fanart", Caps: CapAuxArt,
		CapsAt: map[TargetType]Capability{TargetArtist: CapAuxArt}}
	s := New(nil, Config{Contact: "test@example.com", Providers: []Provider{artistAux}}, nil)
	err := s.CheckPhases([]model.EnrichPhase{model.EnrichPhaseGroupArt})
	if err == nil {
		t.Fatal("group-art with no release-group art provider was accepted")
	}
	if !strings.Contains(err.Error(), "a cover or auxiliary art for a release group") {
		t.Errorf("refusal = %q, want it to name the release-group rung", err)
	}
}

// TestPhasesReportsTheBuiltList: Phases is the list a run would build now, read through
// the current provider list, and Enabled and the forced-phase check agree with it.
func TestPhasesReportsTheBuiltList(t *testing.T) {
	all := &Mock{ProviderName: "everything",
		Caps: CapAuxArt | CapArtistArt | CapCover | CapLyrics | CapFields | CapBookMeta}
	full := New(nil, Config{Contact: "test@example.com", MatchReleases: true, Providers: []Provider{all}}, nil)
	if got, want := full.Phases(), model.EnrichPhases(); !slices.Equal(got, want) {
		t.Errorf("full install phases = %v, want %v", got, want)
	}

	lyricsListed := true
	lyrics := &Mock{ProviderName: "lyrics", Caps: CapLyrics}
	contactless := New(nil, Config{
		Providers: []Provider{lyrics},
		ProviderList: func(fixed []Provider) []Provider {
			if lyricsListed {
				return fixed
			}
			return nil
		},
	}, nil)
	if got := contactless.Phases(); !slices.Equal(got, []model.EnrichPhase{model.EnrichPhaseLyrics}) {
		t.Errorf("contact-less lyrics install phases = %v, want [lyrics]", got)
	}
	if !contactless.Enabled() {
		t.Error("Enabled = false with a lyrics phase built")
	}
	lyricsListed = false
	if got := contactless.Phases(); len(got) != 0 {
		t.Errorf("phases after the list dropped lyrics = %v, want none", got)
	}
	if contactless.Enabled() {
		t.Error("Enabled = true with nothing left to build")
	}

	artistCovers := &Mock{ProviderName: "artistcovers", Caps: CapCover,
		CapsAt: map[TargetType]Capability{TargetArtist: CapCover}}
	artistOnly := New(nil, Config{Providers: []Provider{artistCovers}}, nil)
	if got := artistOnly.Phases(); len(got) != 0 {
		t.Errorf("artist-only cover install phases = %v, want none", got)
	}
	if artistOnly.Enabled() {
		t.Error("Enabled = true for a cover provider serving no phase's rung")
	}
	if err := artistOnly.CheckPhases([]model.EnrichPhase{model.EnrichPhaseAlbumArt}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("CheckPhases(album-art) = %v, want CodeUnsupported", err)
	}
}
