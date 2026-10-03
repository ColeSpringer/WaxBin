package sqlite_test

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// artistArtFixture seeds one artist per name, each backing a track, and returns readers
// for their row ids and pids, and for an integer and a string query.
func artistArtFixture(t *testing.T, names ...string) (*sqlite.Store, func(string) int64, func(string) model.PID,
	func(string, ...any) int, func(string, ...any) string) {
	t.Helper()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	for _, name := range names {
		auxRGTrack(t, st, lib.ID, name, name+" Album", "")
	}
	id := func(name string) int64 {
		return int64(scalarQueryInt(t, db, "SELECT id FROM artist WHERE name=?", name))
	}
	pid := func(name string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM artist WHERE name=?", name))
	}
	count := func(q string, args ...any) int { return scalarQueryInt(t, db, q, args...) }
	str := func(q string, args ...any) string { return scalarQueryStr(t, db, q, args...) }
	return st, id, pid, count, str
}

// setArtistArt stores one artist art role from raw bytes, the way a user's `art set` does.
func setArtistArt(t *testing.T, st *sqlite.Store, pid model.PID, role model.ArtRole, data string) {
	t.Helper()
	if err := st.SetEntityArt(context.Background(), model.ArtArtist, pid, role,
		[]byte(data), "png", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set %s art: %v", role, err)
	}
}

// bothArtistHalves is the slot set of a pass holding a provider serving CapArtistArt.
var bothArtistHalves = model.ArtSlots{Front: true, Aux: true}

// artistArtQueue walks the artist-art queue with the given slots askable and returns the
// halves due for each queued artist by name: "front", "aux", or "front+aux".
func artistArtQueue(t *testing.T, st *sqlite.Store, opts model.EnrichQueueOptions, slots model.ArtSlots) map[string]string {
	t.Helper()
	targets, err := st.ArtistsNeedingArtBackfill(context.Background(), opts, 0, 100, slots, nil)
	if err != nil {
		t.Fatalf("ArtistsNeedingArtBackfill: %v", err)
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

// TestArtistsNeedingArtBackfillHalves: the walk asks about a vacant front and an empty
// background as two halves, each due while its slot is askable and its own marker allows,
// whatever the artist identity recorded. Background is the one auxiliary role an artist
// carries, so an artist holding one has no auxiliary vacancy. An artist with every slot
// filled, a whole-entity lock, or both halves answered is not walked.
func TestArtistsNeedingArtBackfillHalves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, _, _ := artistArtFixture(t, "Fresh", "Identified", "Held", "HeldFull", "Whole",
		"FrontAnswered", "AuxAnswered", "BothAnswered", "BgHeld", "FrontBgHeld")
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{
		ArtistID: id("Identified"), PID: pid("Identified"), Matched: true,
	}); err != nil {
		t.Fatalf("identity: %v", err)
	}
	setArtistArt(t, st, pid("Held"), model.ArtRoleFront, "held-front")
	setArtistArt(t, st, pid("HeldFull"), model.ArtRoleFront, "held-full-front")
	for _, role := range model.AuxArtRoles() {
		setArtistArt(t, st, pid("HeldFull"), role, "held-full-"+string(role))
	}
	setArtistArt(t, st, pid("BgHeld"), model.ArtRoleBackground, "bg-held")
	setArtistArt(t, st, pid("FrontBgHeld"), model.ArtRoleFront, "fbg-front")
	setArtistArt(t, st, pid("FrontBgHeld"), model.ArtRoleBackground, "fbg-bg")
	if _, err := st.SetArtLock(ctx, model.ArtArtist, pid("Whole"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	mark := func(name string, front, aux bool) {
		t.Helper()
		if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id(name), PID: pid(name),
			Front: model.ArtHalf{Asked: front}, Aux: model.ArtHalf{Asked: aux}}); err != nil {
			t.Fatalf("mark %s: %v", name, err)
		}
	}
	mark("FrontAnswered", true, false)
	mark("AuxAnswered", false, true)
	mark("BothAnswered", true, true)

	var run model.EnrichQueueOptions
	for _, c := range []struct {
		slots model.ArtSlots
		want  map[string]string
	}{
		{bothArtistHalves, map[string]string{"Fresh": "front+aux", "Identified": "front+aux", "Held": "aux",
			"FrontAnswered": "aux", "AuxAnswered": "front", "BgHeld": "front"}},
		{model.ArtSlots{Front: true}, map[string]string{"Fresh": "front", "Identified": "front",
			"AuxAnswered": "front", "BgHeld": "front"}},
		{model.ArtSlots{Aux: true}, map[string]string{"Fresh": "aux", "Identified": "aux", "Held": "aux",
			"FrontAnswered": "aux"}},
		{model.ArtSlots{}, map[string]string{}},
	} {
		if got := artistArtQueue(t, st, run, c.slots); !maps.Equal(got, c.want) {
			t.Errorf("queue for %+v = %v, want %v", c.slots, got, c.want)
		}

		// The count mirrors the queue, with the identity phase in the run or not.
		for _, phases := range [][]model.EnrichPhase{
			{model.EnrichPhaseArtistArt},
			{model.EnrichPhaseArtist, model.EnrichPhaseArtistArt},
		} {
			base, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{Phases: phases[:len(phases)-1]}, nil)
			if err != nil {
				t.Fatal(err)
			}
			n, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{Phases: phases, ArtistArt: c.slots}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if n-base != len(c.want) {
				t.Errorf("count with %v for %+v = %d, want the %d queued", phases, c.slots, n-base, len(c.want))
			}
		}
	}
}

// TestApplyArtistArtBackfillSettlesEachHalf: each half the walk asked settles its own
// marker, matched when an image for it came back, and a half it did not ask keeps
// whatever stood. A front no provider had stays a miss beside an auxiliary match, and a
// failed half is owed while the other settles.
func TestApplyArtistArtBackfillSettlesEachHalf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, count, _ := artistArtFixture(t, "FrontMissed", "FrontOnly", "AuxMissed", "AuxFailed")
	marker := func(name, typ string) string {
		t.Helper()
		if count("SELECT COUNT(*) FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(name)) == 0 {
			return "none"
		}
		if count("SELECT owed FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(name)) == 1 {
			return "owed"
		}
		if count("SELECT matched FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(name)) == 1 {
			return "matched"
		}
		return "miss"
	}
	for _, in := range []model.ArtistArtBackfill{
		{ArtistID: id("FrontMissed"), PID: pid("FrontMissed"), AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBackground: enrichArtImg("fm-bg", "mock")},
			Front: model.ArtHalf{Asked: true, Provider: "mock"}, Aux: model.ArtHalf{Asked: true, Provider: "mock"}},
		{ArtistID: id("FrontOnly"), PID: pid("FrontOnly"), Art: enrichArtImg("fo-front", "mock"), Front: model.ArtHalf{Asked: true, Provider: "mock"}},
		{ArtistID: id("AuxMissed"), PID: pid("AuxMissed"), Art: enrichArtImg("am-front", "mock"),
			Front: model.ArtHalf{Asked: true, Provider: "mock"}, Aux: model.ArtHalf{Asked: true, Provider: "mock"}},
		{ArtistID: id("AuxFailed"), PID: pid("AuxFailed"), Art: enrichArtImg("af-front", "mock"),
			Front: model.ArtHalf{Asked: true, Provider: "mock"}, Aux: model.ArtHalf{Asked: true, Incomplete: true, Provider: "mock"}},
	} {
		if err := st.ApplyArtistArtBackfill(ctx, in); err != nil {
			t.Fatalf("apply %d: %v", in.ArtistID, err)
		}
	}
	for _, c := range []struct{ name, front, aux string }{
		{"FrontMissed", "miss", "matched"},
		{"FrontOnly", "matched", "none"},
		{"AuxMissed", "matched", "miss"},
		{"AuxFailed", "matched", "owed"},
	} {
		if f, a := marker(c.name, "artist_front"), marker(c.name, "artist_art"); f != c.front || a != c.aux {
			t.Errorf("%s markers: front %s, aux %s; want %s and %s", c.name, f, a, c.front, c.aux)
		}
	}
	assertStoreVerifyClean(t, st)
}

// TestArtistArtMarkersDropOnNewEvidence: a landed id is evidence for both halves and
// drops both markers, while a fillable front clear opens the front alone and leaves the
// background's answer standing.
func TestArtistArtMarkersDropOnNewEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, count, _ := artistArtFixture(t, "Landed", "Cleared")
	markers := func(name string) int {
		return count(`SELECT COUNT(*) FROM entity_enrichment
			WHERE entity_type IN ('artist_front','artist_art') AND entity_id=?`, id(name))
	}
	mark := func(name string) {
		t.Helper()
		if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id(name), PID: pid(name),
			Front: model.ArtHalf{Asked: true}, Aux: model.ArtHalf{Asked: true}}); err != nil {
			t.Fatalf("mark %s: %v", name, err)
		}
		if n := markers(name); n != 2 {
			t.Fatalf("%s markers = %d, want both halves", name, n)
		}
	}

	mark("Landed")
	if err := st.ApplyArtistEnrichment(ctx, model.ArtistEnrichment{ArtistID: id("Landed"), PID: pid("Landed"),
		Matched: true, MBID: auxRGMBID(1)}); err != nil {
		t.Fatalf("identity: %v", err)
	}
	if n := markers("Landed"); n != 0 {
		t.Errorf("markers after an id landed = %d, want both dropped", n)
	}

	setArtistArt(t, st, pid("Cleared"), model.ArtRoleFront, "front")
	mark("Cleared")
	if err := st.SetEntityArt(ctx, model.ArtArtist, pid("Cleared"), model.ArtRoleFront, nil, "",
		model.Attribution{Source: model.SourceUser}, model.LockUnchanged, false); err != nil {
		t.Fatalf("clear front: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='artist_art' AND entity_id=?`, id("Cleared")); n != 1 ||
		markers("Cleared") != 1 {
		t.Errorf("markers after a fillable front clear = %d, want the front half dropped and the background's kept", markers("Cleared"))
	}
}

// TestArtBackfillOnAVanishedRowidWritesNothing: an artist, a release group or an album can
// be merged away between the queue page and the write, and a marker or an image written
// for the dead rowid would sit on whatever entity inherits it.
func TestArtBackfillOnAVanishedRowidWritesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, _ := openStoreAt(t)
	db := roConn(t, dbPath)
	both := model.ArtHalf{Asked: true}
	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: 424242, PID: model.NewPID(),
		Art:    enrichArtImg("ghost-front", "fanart"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBackground: enrichArtImg("ghost-bg", "fanart")},
		Front:  both, Aux: both}); err != nil {
		t.Fatalf("artist apply: %v", err)
	}
	if err := st.ApplyReleaseGroupArtBackfill(ctx, model.ReleaseGroupArtBackfill{ReleaseGroupID: 424242, PID: model.NewPID(),
		Art: enrichArtImg("ghost-group", "fanart"), Front: both, Aux: both}); err != nil {
		t.Fatalf("group apply: %v", err)
	}
	if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: 424242, PID: model.NewPID(),
		Art:    enrichArtImg("ghost-album", "fanart"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("ghost-back", "fanart")},
		Front:  both, Aux: both}); err != nil {
		t.Fatalf("album apply: %v", err)
	}
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM entity_enrichment WHERE entity_id = 424242"); n != 0 {
		t.Errorf("markers on the vanished rowid = %d, want none", n)
	}
	if n := scalarQueryInt(t, db, "SELECT COUNT(*) FROM art_map WHERE entity_id = 424242"); n != 0 {
		t.Errorf("art rows on the vanished rowid = %d, want none", n)
	}
}

// TestArtHalvesNameTheirOwnProvider: each half's marker names the provider whose answer
// settled that half, and a half nothing answered names none, rather than both halves
// taking whoever supplied the first image.
func TestArtHalvesNameTheirOwnProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, count, str := artistArtFixture(t, "Split", "FrontOnly")
	provider := func(name, typ string) string {
		t.Helper()
		var p string
		if count("SELECT COUNT(*) FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(name)) == 1 {
			p = str("SELECT provider FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id(name))
		}
		return p
	}
	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id("Split"), PID: pid("Split"),
		Art:    enrichArtImg("split-front", "deezer"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBackground: enrichArtImg("split-bg", "fanart")},
		Front:  model.ArtHalf{Asked: true, Provider: "deezer"}, Aux: model.ArtHalf{Asked: true, Provider: "fanart"}}); err != nil {
		t.Fatalf("apply Split: %v", err)
	}
	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id("FrontOnly"), PID: pid("FrontOnly"),
		Art:   enrichArtImg("fo-front", "deezer"),
		Front: model.ArtHalf{Asked: true, Provider: "deezer"}, Aux: model.ArtHalf{Asked: true}}); err != nil {
		t.Fatalf("apply FrontOnly: %v", err)
	}
	for _, c := range []struct{ name, typ, want string }{
		{"Split", "artist_front", "deezer"}, {"Split", "artist_art", "fanart"},
		{"FrontOnly", "artist_front", "deezer"}, {"FrontOnly", "artist_art", "none"},
	} {
		if got := provider(c.name, c.typ); got != c.want {
			t.Errorf("%s %s marker provider = %q, want %q", c.name, c.typ, got, c.want)
		}
	}
}

// TestArtistArtKeepsOnlyTheRolesAnArtistCarries: a role an artist does not carry is
// neither stored nor counted toward the auxiliary half, so a caller handing the store a
// back image for an artist leaves the background half a miss.
func TestArtistArtKeepsOnlyTheRolesAnArtistCarries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, count, _ := artistArtFixture(t, "Stray")
	if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id("Stray"), PID: pid("Stray"),
		AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("stray-back", "fanart")},
		Aux:    model.ArtHalf{Asked: true, Provider: "fanart"}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if n := count("SELECT COUNT(*) FROM art_map WHERE entity_type='artist' AND entity_id=?", id("Stray")); n != 0 {
		t.Errorf("artist art rows = %d, want none for a role an artist does not carry", n)
	}
	if n := count("SELECT matched FROM entity_enrichment WHERE entity_type='artist_art' AND entity_id=?", id("Stray")); n != 0 {
		t.Error("the background half was matched by a back image")
	}
}

// TestAClearDropsOnlyItsOwnHalf: a cleared or unlocked role opens the one half it belongs
// to, so the other half's answer stands; a role the entity does not carry opens nothing,
// and releasing the whole-entity lock opens both.
func TestAClearDropsOnlyItsOwnHalf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, id, pid, count, _ := artistArtFixture(t, "Cleared")
	halves := func() string {
		t.Helper()
		var got []string
		for _, typ := range []string{"artist_front", "artist_art"} {
			if count("SELECT COUNT(*) FROM entity_enrichment WHERE entity_type=? AND entity_id=?", typ, id("Cleared")) == 1 {
				got = append(got, typ)
			}
		}
		return strings.Join(got, "+")
	}
	mark := func() {
		t.Helper()
		if err := st.ApplyArtistArtBackfill(ctx, model.ArtistArtBackfill{ArtistID: id("Cleared"), PID: pid("Cleared"),
			Front: model.ArtHalf{Asked: true}, Aux: model.ArtHalf{Asked: true}}); err != nil {
			t.Fatalf("mark: %v", err)
		}
	}
	user := model.Attribution{Source: model.SourceUser}
	clear := func(role model.ArtRole) {
		t.Helper()
		if err := st.SetEntityArt(ctx, model.ArtArtist, pid("Cleared"), role, nil, "", user, model.LockUnchanged, false); err != nil {
			t.Fatalf("clear %s: %v", role, err)
		}
	}
	setArtistArt(t, st, pid("Cleared"), model.ArtRoleBackground, "bg")
	setArtistArt(t, st, pid("Cleared"), model.ArtRoleBack, "back")
	mark()
	clear(model.ArtRoleBack)
	if got := halves(); got != "artist_front+artist_art" {
		t.Errorf("after clearing a role an artist does not carry: %q, want both halves kept", got)
	}
	clear(model.ArtRoleBackground)
	if got := halves(); got != "artist_front" {
		t.Errorf("after clearing the background: %q, want the front half kept", got)
	}

	mark()
	if _, err := st.SetArtLock(ctx, model.ArtArtist, pid("Cleared"), model.ArtRoleBackground, true); err != nil {
		t.Fatalf("lock background: %v", err)
	}
	if _, err := st.SetArtLock(ctx, model.ArtArtist, pid("Cleared"), model.ArtRoleBackground, false); err != nil {
		t.Fatalf("unlock background: %v", err)
	}
	if got := halves(); got != "artist_front" {
		t.Errorf("after unlocking the background: %q, want the front half kept", got)
	}

	mark()
	if _, err := st.SetArtLock(ctx, model.ArtArtist, pid("Cleared"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock art: %v", err)
	}
	if _, err := st.SetArtLock(ctx, model.ArtArtist, pid("Cleared"), model.ArtRoleFront, false); err != nil {
		t.Fatalf("unlock art: %v", err)
	}
	if got := halves(); got != "" {
		t.Errorf("after releasing the whole lock: %q, want both halves dropped", got)
	}
}
