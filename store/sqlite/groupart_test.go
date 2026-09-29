package sqlite_test

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// groupArtFixture seeds one identified release group per title and returns readers
// for their row ids and pids.
func groupArtFixture(t *testing.T, titles ...string) (*sqlite.Store, func(string) int64, func(string) model.PID, func(string, ...any) int) {
	t.Helper()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	for i, title := range titles {
		auxRGTrack(t, st, lib.ID, title, title, auxRGMBID(i))
	}
	id := func(title string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE title=?", title))
	}
	pid := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE title=?", title))
	}
	count := func(q string, args ...any) int { return scalarQueryInt(t, db, q, args...) }
	return st, id, pid, count
}

// fillRGAux holds every auxiliary role of a group.
func fillRGAux(t *testing.T, st *sqlite.Store, pid model.PID) {
	t.Helper()
	for _, role := range model.AuxArtRoles() {
		setRGArt(t, st, pid, role, "aux-"+string(role)+"-"+string(pid))
	}
}

// groupArtQueue walks the group-art queue and returns the halves due for each queued
// group by title: "front", "aux", or "front+aux".
func groupArtQueue(t *testing.T, st *sqlite.Store, opts model.EnrichQueueOptions, slots model.ArtSlots) map[string]string {
	t.Helper()
	targets, err := st.ReleaseGroupsNeedingArt(context.Background(), opts, 0, 100, slots, nil)
	if err != nil {
		t.Fatalf("ReleaseGroupsNeedingArt: %v", err)
	}
	out := map[string]string{}
	for _, tgt := range targets {
		var due []string
		if tgt.FrontDue {
			due = append(due, "front")
		}
		if tgt.AuxDue {
			due = append(due, "aux")
		}
		out[tgt.Name] = strings.Join(due, "+")
	}
	return out
}

// TestReleaseGroupsNeedingArtHalves: the walk asks about a vacant front and the empty
// auxiliary roles as two halves, each due while its own marker allows. The release-group
// identity plays no part, since that pass asks about no vacant front, so a matched, owed
// or missed identity leaves the front to this walk. A group with every slot filled, a
// whole-entity lock, or both halves answered is not walked.
func TestReleaseGroupsNeedingArtHalves(t *testing.T) {
	ctx := context.Background()
	st, id, pid, _ := groupArtFixture(t, "Fresh", "Matched", "Owed", "Missed", "Held", "HeldFull",
		"Whole", "FrontAnswered", "AuxAnswered", "BothAnswered")
	identity := func(title string, matched, owed bool) {
		t.Helper()
		if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
			ReleaseGroupID: id(title), PID: pid(title), Matched: matched, Incomplete: owed,
		}); err != nil {
			t.Fatalf("identity %s: %v", title, err)
		}
	}
	identity("Matched", true, false)
	identity("Owed", true, true)
	identity("Missed", false, false)
	setRGArt(t, st, pid("Held"), model.ArtRoleFront, "held-front")
	setRGArt(t, st, pid("HeldFull"), model.ArtRoleFront, "held-full-front")
	fillRGAux(t, st, pid("HeldFull"))
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid("Whole"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	mark := func(title string, front, aux bool) {
		t.Helper()
		if err := st.ApplyReleaseGroupArtBackfill(ctx, model.ReleaseGroupArtBackfill{
			ReleaseGroupID: id(title), PID: pid(title),
			Front: model.ArtHalf{Asked: front}, Aux: model.ArtHalf{Asked: aux}}); err != nil {
			t.Fatalf("mark %s: %v", title, err)
		}
	}
	mark("FrontAnswered", true, false)
	mark("AuxAnswered", false, true)
	mark("BothAnswered", true, true)

	var run model.EnrichQueueOptions
	both := model.ArtSlots{Front: true, Aux: true}
	for _, c := range []struct {
		slots model.ArtSlots
		want  map[string]string
	}{
		{both, map[string]string{"Fresh": "front+aux", "Matched": "front+aux", "Owed": "front+aux",
			"Missed": "front+aux", "Held": "aux", "FrontAnswered": "aux", "AuxAnswered": "front"}},
		{model.ArtSlots{Front: true}, map[string]string{"Fresh": "front", "Matched": "front", "Owed": "front",
			"Missed": "front", "AuxAnswered": "front"}},
		{model.ArtSlots{Aux: true}, map[string]string{"Fresh": "aux", "Matched": "aux", "Owed": "aux",
			"Missed": "aux", "Held": "aux", "FrontAnswered": "aux"}},
	} {
		if got := groupArtQueue(t, st, run, c.slots); !maps.Equal(got, c.want) {
			t.Errorf("queue for %+v = %v, want %v", c.slots, got, c.want)
		}
	}

	// The count mirrors the queue per slot combination, with the release-group phase in
	// the run or not.
	for _, phases := range [][]model.EnrichPhase{
		{model.EnrichPhaseGroupArt},
		{model.EnrichPhaseReleaseGroup, model.EnrichPhaseGroupArt},
	} {
		base, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{Phases: phases[:len(phases)-1]}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, slots := range []model.ArtSlots{both, {Front: true}, {Aux: true}} {
			n, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{Phases: phases, GroupArt: slots}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if want := len(groupArtQueue(t, st, run, slots)); n-base != want {
				t.Errorf("count for %+v with %v = %d, want the %d queued", slots, phases, n-base, want)
			}
		}
	}
}

// TestApplyReleaseGroupArtBackfillSettlesEachHalf: each half the walk asked settles its
// own marker, matched when an image for it came back, and a half it did not ask keeps
// whatever stood. A front no provider had stays a miss beside an auxiliary match, and a
// failed half is owed while the other settles.
func TestApplyReleaseGroupArtBackfillSettlesEachHalf(t *testing.T) {
	ctx := context.Background()
	st, id, pid, count := groupArtFixture(t, "FrontMissed", "FrontOnly", "AuxFailed")
	marker := func(title, typ string) string {
		t.Helper()
		if count("SELECT COUNT(*) FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(title)) == 0 {
			return "none"
		}
		if count("SELECT owed FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(title)) == 1 {
			return "owed"
		}
		if count("SELECT matched FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(title)) == 1 {
			return "matched"
		}
		return "miss"
	}
	for _, in := range []model.ReleaseGroupArtBackfill{
		{ReleaseGroupID: id("FrontMissed"), PID: pid("FrontMissed"), AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("fm-back", "mock")},
			Front: model.ArtHalf{Asked: true, Provider: "mock"}, Aux: model.ArtHalf{Asked: true, Provider: "mock"}},
		{ReleaseGroupID: id("FrontOnly"), PID: pid("FrontOnly"), Art: enrichArtImg("fo-front", "mock"), Front: model.ArtHalf{Asked: true, Provider: "mock"}},
		{ReleaseGroupID: id("AuxFailed"), PID: pid("AuxFailed"), Art: enrichArtImg("af-front", "mock"),
			Front: model.ArtHalf{Asked: true, Provider: "mock"}, Aux: model.ArtHalf{Asked: true, Incomplete: true, Provider: "mock"}},
	} {
		if err := st.ApplyReleaseGroupArtBackfill(ctx, in); err != nil {
			t.Fatalf("apply %d: %v", in.ReleaseGroupID, err)
		}
	}
	for _, c := range []struct{ title, front, aux string }{
		{"FrontMissed", "miss", "matched"},
		{"FrontOnly", "matched", "none"},
		{"AuxFailed", "matched", "owed"},
	} {
		if f, a := marker(c.title, "group_front"), marker(c.title, "group_art"); f != c.front || a != c.aux {
			t.Errorf("%s markers: front %s, aux %s; want %s and %s", c.title, f, a, c.front, c.aux)
		}
	}
}

// TestApplyReleaseGroupArtBackfillFillsAVacantFront: the backfill's front fills a group
// that has none, is fill-when-empty against a front set since the queue page, and
// answers to the art lock; the entity delta rides on a write.
func TestApplyReleaseGroupArtBackfillFillsAVacantFront(t *testing.T) {
	ctx := context.Background()
	st, id, pid, count := groupArtFixture(t, "Vacant", "Held", "Locked")
	setRGArt(t, st, pid("Held"), model.ArtRoleFront, "user-front")
	if _, err := st.SetArtLock(ctx, model.ArtReleaseGroup, pid("Locked"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	front := func(title string) int {
		return count(`SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=?
			AND role='front' AND source='enrichment'`, id(title))
	}
	for _, c := range []struct {
		title  string
		fronts int
		deltas int
	}{{"Vacant", 1, 1}, {"Held", 0, 0}, {"Locked", 0, 0}} {
		before, err := st.LatestChangeSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.ApplyReleaseGroupArtBackfill(ctx, model.ReleaseGroupArtBackfill{
			ReleaseGroupID: id(c.title), PID: pid(c.title), Art: enrichArtImg("backfill-"+c.title, "coverartarchive"), Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"},
		}); err != nil {
			t.Fatalf("apply %s: %v", c.title, err)
		}
		after, err := st.LatestChangeSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := front(c.title); got != c.fronts {
			t.Errorf("%s: enrichment fronts = %d, want %d", c.title, got, c.fronts)
		}
		if got := int(after - before); got != c.deltas {
			t.Errorf("%s: deltas = %d, want %d", c.title, got, c.deltas)
		}
		if n := count(`SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='group_front'
			AND entity_id=? AND matched=1`, id(c.title)); n != 1 {
			t.Errorf("%s: matched front markers = %d, want 1", c.title, n)
		}
	}
	if n := count(`SELECT COUNT(*) FROM art_map WHERE entity_type='release_group' AND entity_id=?
		AND role='front' AND source='user'`, id("Held")); n != 1 {
		t.Error("the backfill replaced a front set by hand")
	}
	assertStoreVerifyClean(t, st)
}

// TestGroupArtMarkerClearsOnAFrontClear: clearing a front with the slot left fillable
// opens a vacancy the markers say was already asked about and drops them; a clear that
// locks the slot opens nothing.
func TestGroupArtMarkerClearsOnAFrontClear(t *testing.T) {
	ctx := context.Background()
	st, id, pid, count := groupArtFixture(t, "FrontCleared")
	markers := func() int {
		return count(`SELECT COUNT(*) FROM entity_enrichment
			WHERE entity_type IN ('group_art','group_front') AND entity_id=?`, id("FrontCleared"))
	}
	mark := func() {
		t.Helper()
		if err := st.ApplyReleaseGroupArtBackfill(ctx, model.ReleaseGroupArtBackfill{
			ReleaseGroupID: id("FrontCleared"), PID: pid("FrontCleared"),
			Front: model.ArtHalf{Asked: true}, Aux: model.ArtHalf{Asked: true}}); err != nil {
			t.Fatalf("mark: %v", err)
		}
	}

	setRGArt(t, st, pid("FrontCleared"), model.ArtRoleFront, "front")
	mark()
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid("FrontCleared"), model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear front: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='group_art' AND entity_id=?`, id("FrontCleared")); n != 1 ||
		markers() != 1 {
		t.Errorf("markers after a fillable front clear = %d, want the front half dropped and the auxiliary one kept", markers())
	}

	setRGArt(t, st, pid("FrontCleared"), model.ArtRoleFront, "front-again")
	mark()
	if err := st.SetEntityArt(ctx, model.ArtReleaseGroup, pid("FrontCleared"), model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockOn, false); err != nil {
		t.Fatalf("clear and lock front: %v", err)
	}
	if n := markers(); n != 2 {
		t.Errorf("markers after a front clear that locked the slot = %d, want both kept", n)
	}
}
