package meta

import (
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxlabel/tag"
)

// TestRoleTagKeysResolve asserts every music contributor role maps to a canonical
// WaxLabel tag key present at the pinned waxlabel version (the prerequisite gate).
func TestRoleTagKeysResolve(t *testing.T) {
	want := map[model.ContributorRole]tag.Key{
		model.RoleComposer:  tag.Composer,
		model.RoleLyricist:  tag.Lyricist,
		model.RoleConductor: tag.Conductor,
		model.RolePerformer: tag.Performer,
		model.RoleRemixer:   tag.Remixer,
		model.RoleProducer:  tag.Producer,
		model.RoleEngineer:  tag.Engineer,
		model.RoleMixer:     tag.Mixer,
		model.RoleArranger:  tag.Arranger,
		model.RoleWriter:    tag.Writer,
		model.RoleDJMixer:   tag.DJMixer,
	}
	if len(want) != 11 {
		t.Fatalf("expected 11 music roles, have %d", len(want))
	}
	for role, key := range want {
		got, ok := RoleTagKey(role)
		if !ok {
			t.Errorf("role %s has no tag key", role)
			continue
		}
		if got != string(key) {
			t.Errorf("role %s -> %q, want %q", role, got, key)
		}
	}
}
