package sqlite_test

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

// albumArtQueue walks the album-art queue and returns the halves due for each queued
// album by title: "front", "aux", or "front+aux".
func albumArtQueue(t *testing.T, st *sqlite.Store, opts model.EnrichQueueOptions, slots model.ArtSlots) map[string]string {
	t.Helper()
	targets, err := st.AlbumsNeedingArt(context.Background(), opts, 0, 100, slots, nil)
	if err != nil {
		t.Fatalf("AlbumsNeedingArt: %v", err)
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

// TestAlbumsNeedingArtHalves: the walk asks about a vacant front and the empty auxiliary
// roles as two halves, each due while its slot is askable and its own marker allows. A
// front a member track's cover answers is held; an album with no identifier, a
// whole-entity lock, or both halves answered is not walked.
func TestAlbumsNeedingArtHalves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	barcodes := map[string]string{"Fresh": "0075992739429", "FrontAnswered": "5099902154251",
		"AuxAnswered": "0724382955528", "BothAnswered": "0724383024124", "WholeLocked": "0724383024131"}
	for title, barcode := range barcodes {
		editionTrack(t, st, lib.ID, "ess-"+title, title, 1, model.Track{Barcode: barcode})
	}
	editionTrackWithCover(t, st, lib.ID, "ess-held", "HeldFront", 1, model.Track{Barcode: "0724383024148"}, pngFixture())
	editionTrack(t, st, lib.ID, "ess-plain", "Unidentified", 1, model.Track{})
	pid := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title=?", title))
	}
	if _, err := st.SetArtLock(ctx, model.ArtAlbum, pid("WholeLocked"), model.ArtRoleFront, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	mark := func(title string, front, aux bool) {
		t.Helper()
		if err := st.ApplyAlbumArtBackfill(ctx, model.AlbumArtBackfill{AlbumID: albumIDByTitle(t, db, title), PID: pid(title),
			Front: model.ArtHalf{Asked: front}, Aux: model.ArtHalf{Asked: aux}}); err != nil {
			t.Fatalf("mark %s: %v", title, err)
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
		{model.ArtSlots{Front: true, Aux: true}, map[string]string{"Fresh": "front+aux", "FrontAnswered": "aux",
			"AuxAnswered": "front", "HeldFront": "aux"}},
		{model.ArtSlots{Front: true}, map[string]string{"Fresh": "front", "AuxAnswered": "front"}},
		{model.ArtSlots{Aux: true}, map[string]string{"Fresh": "aux", "FrontAnswered": "aux", "HeldFront": "aux"}},
		{model.ArtSlots{}, map[string]string{}},
	} {
		if got := albumArtQueue(t, st, run, c.slots); !maps.Equal(got, c.want) {
			t.Errorf("queue for %+v = %v, want %v", c.slots, got, c.want)
		}
		n, err := st.CountEntitiesNeedingEnrichment(ctx, run, model.EnrichCountOptions{
			Phases: []model.EnrichPhase{model.EnrichPhaseAlbumArt}, AlbumArt: c.slots}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n != len(c.want) {
			t.Errorf("count for %+v = %d, want the %d queued", c.slots, n, len(c.want))
		}
	}
}

// TestApplyAlbumArtBackfillSettlesEachHalf: each half the walk asked settles its own
// marker. A front missed beside an auxiliary match stays a miss, a failed auxiliary half
// beside a landed front is owed, and a front answered with the group's picture is matched
// only when a row was actually copied.
func TestApplyAlbumArtBackfillSettlesEachHalf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	db := roConn(t, dbPath)
	titles := map[string]string{"FrontMissed": "0075992739429", "FrontOnly": "5099902154251",
		"AuxMissed": "0724383024148", "AuxFailed": "0724382955528", "Copied": "0724383024124",
		"CopyStale": "0724383024131"}
	for title, barcode := range titles {
		editionTrack(t, st, lib.ID, "ess-"+title, title, 1, model.Track{Barcode: barcode})
	}
	rgID := int64(scalarQueryInt(t, db, "SELECT id FROM release_group WHERE mbid = ?", relTestRGMBID))
	rgPID := model.PID(scalarQueryStr(t, db, "SELECT pid FROM release_group WHERE mbid = ?", relTestRGMBID))
	if err := st.ApplyReleaseGroupEnrichment(ctx, model.ReleaseGroupEnrichment{
		ReleaseGroupID: rgID, PID: rgPID, Matched: true, MBID: relTestRGMBID,
		Art: enrichArtImg("group-front", "coverartarchive"),
	}); err != nil {
		t.Fatalf("group front: %v", err)
	}
	id := func(title string) int64 { return albumIDByTitle(t, db, title) }
	pid := func(title string) model.PID {
		return model.PID(scalarQueryStr(t, db, "SELECT pid FROM album WHERE title=?", title))
	}
	marker := func(title, typ string) string {
		t.Helper()
		q := func(col string) int {
			return scalarQueryInt(t, db, "SELECT COALESCE((SELECT "+col+" FROM entity_enrichment WHERE entity_type=? AND entity_id=?), -1)",
				typ, id(title))
		}
		switch {
		case q("owed") == -1:
			return "none"
		case q("owed") == 1:
			return "owed"
		case q("matched") == 1:
			return "matched"
		}
		return "miss"
	}
	for _, in := range []model.AlbumArtBackfill{
		{AlbumID: id("FrontMissed"), PID: pid("FrontMissed"), AuxArt: map[model.ArtRole]*model.ArtImage{model.ArtRoleBack: enrichArtImg("fm-back", "fanart")},
			Front: model.ArtHalf{Asked: true, Provider: "fanart"}, Aux: model.ArtHalf{Asked: true, Provider: "fanart"}},
		{AlbumID: id("FrontOnly"), PID: pid("FrontOnly"), Art: enrichArtImg("fo-front", "coverartarchive"), Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"}},
		{AlbumID: id("AuxMissed"), PID: pid("AuxMissed"), Art: enrichArtImg("am-front", "coverartarchive"),
			Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"}, Aux: model.ArtHalf{Asked: true, Provider: "coverartarchive"}},
		{AlbumID: id("AuxFailed"), PID: pid("AuxFailed"), Art: enrichArtImg("af-front", "coverartarchive"),
			Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"}, Aux: model.ArtHalf{Asked: true, Incomplete: true, Provider: "coverartarchive"}},
		{AlbumID: id("Copied"), PID: pid("Copied"), FrontFromGroup: true, GroupFrontHash: "group-front", Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"}},
		{AlbumID: id("CopyStale"), PID: pid("CopyStale"), FrontFromGroup: true, GroupFrontHash: "stale", Front: model.ArtHalf{Asked: true, Provider: "coverartarchive"}},
	} {
		if err := st.ApplyAlbumArtBackfill(ctx, in); err != nil {
			t.Fatalf("apply %d: %v", in.AlbumID, err)
		}
	}
	for _, c := range []struct{ title, front, aux string }{
		{"FrontMissed", "miss", "matched"},
		{"FrontOnly", "matched", "none"},
		{"AuxMissed", "matched", "miss"},
		{"AuxFailed", "matched", "owed"},
		{"Copied", "matched", "none"},
		{"CopyStale", "miss", "none"},
	} {
		if f, a := marker(c.title, "album_front"), marker(c.title, "album_art"); f != c.front || a != c.aux {
			t.Errorf("%s markers: front %s, aux %s; want %s and %s", c.title, f, a, c.front, c.aux)
		}
	}
	assertStoreVerifyClean(t, st)
}
