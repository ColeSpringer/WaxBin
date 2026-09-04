package sqlite

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// entityPID reads an entity's public id by display name (test-only lookup).
func entityPID(t *testing.T, st *Store, table, name string) model.PID {
	t.Helper()
	var pid string
	err := st.read.QueryRowContext(context.Background(),
		"SELECT pid FROM "+table+" WHERE name = ?", name).Scan(&pid)
	if err != nil {
		t.Fatalf("no %s named %q: %v", table, name, err)
	}
	return model.PID(pid)
}

func entityExists(t *testing.T, st *Store, table, name string) bool {
	t.Helper()
	var n int
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table+" WHERE name = ?", name).Scan(&n); err != nil {
		t.Fatalf("count %s %q: %v", table, name, err)
	}
	return n > 0
}

func TestMergeArtists(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two heuristically-distinct artists for the same act (the "The"-strip does not
	// unify these because MatchKey keeps "the").
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Beatles", album: "A1", genre: "Rock", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "The Beatles", album: "A2", genre: "Rock", durationMS: 200,
	})

	survivor := entityPID(t, st, "artist", "The Beatles")
	loser := entityPID(t, st, "artist", "Beatles")

	rep, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rep.Children != 1 { // one track had artist_id = loser
		t.Errorf("children re-pointed = %d, want 1", rep.Children)
	}
	if entityExists(t, st, "artist", "Beatles") {
		t.Error("loser artist still present after merge")
	}
	// Survivor now owns both tracks.
	if got := rollupTrackCount(t, st, "artist_rollup", "artist", "name", "The Beatles"); got != 2 {
		t.Errorf("survivor rollup track_count = %d, want 2", got)
	}
	// The loser's name is preserved as an alias so the old spelling resolves.
	var aliases int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artist_alias al JOIN artist a ON a.id = al.artist_id
		 WHERE a.name = 'The Beatles' AND al.name = 'Beatles'`).Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	if aliases != 1 {
		t.Errorf("loser name alias count = %d, want 1", aliases)
	}
	// Derived data stays consistent (rollups recomputed, no drift).
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("derived inconsistent after merge: %+v (err %v)", r, err)
	}
}

func TestMergeArtistsUnionsMBIDAndEnrichmentMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "Nirvana", album: "A", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Nirvana (US)", album: "B", durationMS: 100,
	})
	survivor := entityPID(t, st, "artist", "Nirvana")
	loser := entityPID(t, st, "artist", "Nirvana (US)")

	// Give the loser an MBID + a matched enrichment marker; the survivor has neither.
	// A direct write seeds the fixture state (the enrichment pass is not under test).
	if _, err := st.write.ExecContext(ctx, "UPDATE artist SET mbid='mbid-x' WHERE name='Nirvana (US)'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at) SELECT 'artist', id, 'musicbrainz', 1, 'mbid-x', 1 FROM artist WHERE name='Nirvana (US)'"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var mbid string
	if err := st.read.QueryRowContext(ctx, "SELECT COALESCE(mbid,'') FROM artist WHERE name='Nirvana'").Scan(&mbid); err != nil {
		t.Fatal(err)
	}
	if mbid != "mbid-x" {
		t.Errorf("survivor mbid = %q, want inherited mbid-x", mbid)
	}
	var marked int
	if err := st.read.QueryRowContext(ctx,
		"SELECT matched FROM entity_enrichment ee JOIN artist a ON a.id=ee.entity_id AND ee.entity_type='artist' WHERE a.name='Nirvana'").Scan(&marked); err != nil {
		t.Fatalf("survivor should inherit enrichment marker: %v", err)
	}
	if marked != 1 {
		t.Errorf("survivor enrichment matched = %d, want 1", marked)
	}
}

// TestMergeAlbumUnionsEnrichmentMarker guards the marker union for albums, which
// only became reachable once the release match started writing album markers.
// Without it, merging two albums strands the loser's entity_enrichment row and the
// survivor reads as never-searched.
func TestMergeAlbumUnionsEnrichmentMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", albumArt: "A", album: "Greatest Hits", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "B", albumArt: "B", album: "Greatest Hits", durationMS: 100,
	})
	// Album match keys embed the folder and artist, so these are two album rows.
	var survivor, loser string
	if err := st.read.QueryRowContext(ctx,
		"SELECT MIN(pid), MAX(pid) FROM album").Scan(&survivor, &loser); err != nil {
		t.Fatal(err)
	}
	if survivor == loser {
		t.Fatalf("want two album rows to merge, got one (%s)", survivor)
	}

	// The loser was searched and matched; the survivor was never looked up.
	if _, err := st.write.ExecContext(ctx,
		`INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		 SELECT 'album', id, 'musicbrainz', 1, 'rel-x', 1 FROM album WHERE pid = ?`, loser); err != nil {
		t.Fatal(err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeAlbum, model.PID(survivor), model.PID(loser)); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var matched int
	if err := st.read.QueryRowContext(ctx,
		`SELECT matched FROM entity_enrichment ee JOIN album al ON al.id = ee.entity_id
		 WHERE ee.entity_type = 'album' AND al.pid = ?`, survivor).Scan(&matched); err != nil {
		t.Fatalf("survivor should inherit the album enrichment marker: %v", err)
	}
	if matched != 1 {
		t.Errorf("survivor album marker matched = %d, want 1", matched)
	}
	var stranded int
	if err := st.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity_enrichment ee
		 WHERE ee.entity_type = 'album' AND NOT EXISTS (SELECT 1 FROM album al WHERE al.id = ee.entity_id)`).
		Scan(&stranded); err != nil {
		t.Fatal(err)
	}
	if stranded != 0 {
		t.Errorf("stranded album markers = %d, want 0", stranded)
	}
}

func TestMergeGenresDedupsSharedItems(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// One track tagged with both "Hip-Hop" and "Rap": merging Rap into Hip-Hop must
	// not violate the item_genre PK.
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "X", album: "A", genre: "Hip-Hop; Rap", durationMS: 100,
	})
	survivor := entityPID(t, st, "genre", "Hip-Hop")
	loser := entityPID(t, st, "genre", "Rap")

	if _, err := st.MergeEntity(ctx, model.MergeGenre, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if entityExists(t, st, "genre", "Rap") {
		t.Error("loser genre still present")
	}
	// The track still has exactly one genre link (to the survivor).
	var links int
	if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM item_genre").Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Errorf("item_genre links = %d, want 1 after dedup merge", links)
	}
	if got := rollupTrackCount(t, st, "genre_rollup", "genre", "name", "Hip-Hop"); got != 1 {
		t.Errorf("survivor genre rollup = %d, want 1", got)
	}
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("derived inconsistent after genre merge: %+v (err %v)", r, err)
	}
}

func TestMergeAlbumsAcrossReleaseGroups(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// Two albums under two different release groups (distinct album artists).
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "A", albumArt: "A", album: "Greatest Hits", durationMS: 100,
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two",
		artist: "B", albumArt: "B", album: "Greatest Hits", durationMS: 100,
	})
	// Album match keys embed the folder + artist, so these are two album rows.
	var survivor, loser string
	rows, err := st.read.QueryContext(ctx, "SELECT pid FROM album ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	var pids []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(pids) != 2 {
		t.Fatalf("want 2 albums, got %d", len(pids))
	}
	survivor, loser = pids[0], pids[1]

	rep, err := st.MergeEntity(ctx, model.MergeAlbum, model.PID(survivor), model.PID(loser))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rep.Children != 1 {
		t.Errorf("children = %d, want 1 track re-pointed", rep.Children)
	}
	var albums int
	if err := st.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM album").Scan(&albums); err != nil {
		t.Fatal(err)
	}
	if albums != 1 {
		t.Errorf("albums after merge = %d, want 1", albums)
	}
	if r, err := st.VerifyDerived(ctx); err != nil || !r.Consistent() {
		t.Fatalf("derived inconsistent after album merge: %+v (err %v)", r, err)
	}
}

// artistID looks up an artist's internal id by display name (test-only).
func artistID(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	var id int64
	if err := st.read.QueryRowContext(context.Background(),
		"SELECT id FROM artist WHERE name = ?", name).Scan(&id); err != nil {
		t.Fatalf("artist %q: %v", name, err)
	}
	return id
}

// seedArt attaches a front cover (a distinct source hash) to an entity.
func seedArt(t *testing.T, st *Store, hash, entityType string, entityID int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.write.ExecContext(ctx,
		"INSERT OR IGNORE INTO art_source(hash, format, size, data, created_at) VALUES (?,?,?,?,1)",
		hash, "jpeg", 3, []byte("img")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO art_map(entity_type, entity_id, source_hash, role, source, updated_at) VALUES (?,?,?,'front','tag',1)",
		entityType, entityID, hash); err != nil {
		t.Fatal(err)
	}
}

func artHashes(t *testing.T, st *Store, entityType string, entityID int64) []string {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(),
		"SELECT source_hash FROM art_map WHERE entity_type = ? AND entity_id = ? ORDER BY source_hash",
		entityType, entityID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMergeArtistPreservesSurvivorArt(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "B"})
	survivor, loser := entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")
	sID, lID := artistID(t, st, "The Beatles"), artistID(t, st, "Beatles")

	// Both artists carry a DIFFERENT cover.
	seedArt(t, st, "hashSurv", "artist", sID)
	seedArt(t, st, "hashLose", "artist", lID)

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The survivor keeps ONLY its own cover; the loser's is dropped (not accumulated).
	if got := artHashes(t, st, "artist", sID); len(got) != 1 || got[0] != "hashSurv" {
		t.Errorf("survivor art = %v, want [hashSurv] only", got)
	}
	if got := artHashes(t, st, "artist", lID); len(got) != 0 {
		t.Errorf("loser art = %v, want none", got)
	}
}

func TestMergeArtistInheritsArtWhenSurvivorHasNone(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "B"})
	survivor, loser := entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")
	sID, lID := artistID(t, st, "The Beatles"), artistID(t, st, "Beatles")

	// Only the loser has a cover; the survivor should inherit it.
	seedArt(t, st, "hashLose", "artist", lID)

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := artHashes(t, st, "artist", sID); len(got) != 1 || got[0] != "hashLose" {
		t.Errorf("survivor art = %v, want inherited [hashLose]", got)
	}
}

// seedArtRole is seedArt for a specific role slot.
func seedArtRole(t *testing.T, st *Store, hash, entityType string, entityID int64, role string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.write.ExecContext(ctx,
		"INSERT OR IGNORE INTO art_source(hash, format, size, data, created_at) VALUES (?,?,?,?,1)",
		hash, "jpeg", 3, []byte("img")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO art_map(entity_type, entity_id, source_hash, role, source, updated_at) VALUES (?,?,?,?,'tag',1)",
		entityType, entityID, hash, role); err != nil {
		t.Fatal(err)
	}
}

// TestMergeArtInheritsPerRole verifies the per-role inherit: the survivor keeps
// the roles it fills and gains only the loser's roles it lacks, in one merge.
func TestMergeArtInheritsPerRole(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "B"})
	survivor, loser := entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")
	sID, lID := artistID(t, st, "The Beatles"), artistID(t, st, "Beatles")

	// Survivor: front only. Loser: a competing front plus a background the
	// survivor lacks.
	seedArtRole(t, st, "hashSurvFront", "artist", sID, "front")
	seedArtRole(t, st, "hashLoseFront", "artist", lID, "front")
	seedArtRole(t, st, "hashLoseBg", "artist", lID, "background")

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := artHashes(t, st, "artist", sID); len(got) != 2 || got[0] != "hashLoseBg" || got[1] != "hashSurvFront" {
		t.Errorf("survivor art = %v, want its own front plus the inherited background", got)
	}
	if got := artHashes(t, st, "artist", lID); len(got) != 0 {
		t.Errorf("loser art = %v, want none", got)
	}
}

// TestMergeCarriesRoleArtLocks: an art.<role> lock travels the way the plain "art"
// lock does, on the row move alone rather than through the locked-wins union the other
// curation fields take. A role the survivor does not curate inherits the loser's lock
// beside the image repointArtMap moved into the same empty slot; a role the survivor
// does curate keeps its own answer, where the union would have promoted it to locked
// over a picture the loser never protected.
func TestMergeCarriesRoleArtLocks(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "The Beatles", album: "B"})
	survivor, loser := entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")
	sID, lID := artistID(t, st, "The Beatles"), artistID(t, st, "Beatles")

	// The survivor fills disc and curates it unlocked; the loser fills back and disc
	// and locks both roles.
	seedArtRole(t, st, "hashSurvDisc", "artist", sID, "disc")
	seedRoleLock(t, st, "artist", sID, "art.disc", false)
	seedArtRole(t, st, "hashLoseBack", "artist", lID, "back")
	seedArtRole(t, st, "hashLoseDisc", "artist", lID, "disc")
	seedRoleLock(t, st, "artist", lID, "art.back", true)
	seedRoleLock(t, st, "artist", lID, "art.disc", true)

	if _, err := st.MergeEntity(ctx, model.MergeArtist, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if got := artHashes(t, st, "artist", sID); len(got) != 2 || got[0] != "hashLoseBack" || got[1] != "hashSurvDisc" {
		t.Errorf("survivor art = %v, want the inherited back plus its own disc", got)
	}
	// The back lock came across into the slot the back image landed in.
	if !roleLocked(t, st, "artist", sID, "art.back") {
		t.Error("the loser's art.back lock did not reach the survivor")
	}
	// The disc lock did not, because the survivor curates that role itself. Without
	// the art.% exclusion the union would have promoted this row to locked, pinning
	// the survivor's own disc image under a lock the loser put on a different one.
	if roleLocked(t, st, "artist", sID, "art.disc") {
		t.Error("the loser's art.disc lock was unioned onto the survivor's own disc curation")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_curation WHERE entity_type='artist' AND entity_id=?", lID); n != 0 {
		t.Errorf("%d curation rows outlived the loser, want 0", n)
	}
	assertVerifyClean(t, st)
}

// seedRoleLock writes an entity_curation row for one art field in the given lock state.
func seedRoleLock(t *testing.T, st *Store, entityType string, entityID int64, field string, locked bool) {
	t.Helper()
	if _, err := st.write.ExecContext(context.Background(),
		`INSERT INTO entity_curation(entity_type, entity_id, field, source, locked, updated_at)
		 VALUES (?,?,?,'user',?,1)`, entityType, entityID, field, boolInt(locked)); err != nil {
		t.Fatal(err)
	}
}

func roleLocked(t *testing.T, st *Store, entityType string, entityID int64, field string) bool {
	t.Helper()
	return scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_curation WHERE entity_type=? AND entity_id=? AND field=? AND locked=1",
		entityType, entityID, field) == 1
}

func TestMergePreservesUnrelatedSelfLoop(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Aartist", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "Bartist", album: "B"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/c/3.flac", essence: "e3", content: "c3", title: "Three", artist: "Cartist", album: "C"})
	cID := artistID(t, st, "Cartist")
	// A pre-existing self-loop on an UNRELATED artist (bad enrichment data).
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO artist_relation(src_id, dst_id, kind) VALUES (?,?,'similar')", cID, cID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist,
		entityPID(t, st, "artist", "Aartist"), entityPID(t, st, "artist", "Bartist")); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var n int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM artist_relation WHERE src_id = ? AND dst_id = ?", cID, cID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("unrelated artist's self-loop was destroyed by the merge (count=%d, want 1)", n)
	}
}

func TestMergeEntitiesAtomicOnBadLoser(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Surv", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "Los1", album: "B"})
	survivor := entityPID(t, st, "artist", "Surv")
	los1 := entityPID(t, st, "artist", "Los1")

	// A batch with a valid loser followed by a bad PID must roll back entirely.
	_, err := st.MergeEntities(ctx, model.MergeArtist, survivor, []model.PID{los1, "nonexistent"})
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("batch with bad loser: got %v, want CodeNotFound", err)
	}
	if !entityExists(t, st, "artist", "Los1") {
		t.Error("the valid loser was merged even though the batch failed (not atomic)")
	}
}

func TestMergeEmitsPerItemChangeLog(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "LoserSong", artist: "Beatles", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "SurvSong", artist: "The Beatles", album: "B"})
	var itemPID string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE title = 'LoserSong'").Scan(&itemPID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist,
		entityPID(t, st, "artist", "The Beatles"), entityPID(t, st, "artist", "Beatles")); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The re-pointed track's item-to-artist association changed, so a delta-sync
	// consumer must see a per-item update.
	var n int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM change_log WHERE entity_type = 'item' AND op = 'update' AND entity_pid = ?",
		itemPID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("merge emitted no per-item change_log delta for the re-pointed track")
	}
}

func TestMergeReleaseGroupUnionsType(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})
	// The loser release group carries a specific type; the survivor has the default.
	if _, err := st.write.ExecContext(ctx, "UPDATE release_group SET type='compilation' WHERE title='LoseRG'"); err != nil {
		t.Fatal(err)
	}
	var survivorRG, loserRG string
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM release_group WHERE title='SurvRG'").Scan(&survivorRG); err != nil {
		t.Fatal(err)
	}
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM release_group WHERE title='LoseRG'").Scan(&loserRG); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, model.PID(survivorRG), model.PID(loserRG)); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var typ string
	if err := st.read.QueryRowContext(ctx, "SELECT type FROM release_group WHERE pid = ?", survivorRG).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "compilation" {
		t.Errorf("survivor release-group type = %q, want compilation (unioned from the loser)", typ)
	}
}

// TestMergeDropsLoserAuxMarker: a release-group merge drops the loser's aux-art
// backfill marker instead of unioning it (the loser's images have just moved into the
// survivor's empty roles, so its recorded answer no longer describes anything), and
// leaves the survivor's own marker alone.
func TestMergeDropsLoserAuxMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})

	rg := func(title string) (int64, model.PID) {
		t.Helper()
		var id int64
		var pid string
		if err := st.read.QueryRowContext(ctx,
			"SELECT id, pid FROM release_group WHERE title = ?", title).Scan(&id, &pid); err != nil {
			t.Fatalf("no release group titled %q: %v", title, err)
		}
		return id, model.PID(pid)
	}
	survID, survPID := rg("SurvRG")
	loseID, losePID := rg("LoseRG")

	for _, e := range []struct {
		id  int64
		pid model.PID
	}{{survID, survPID}, {loseID, losePID}} {
		if err := st.ApplyReleaseGroupAuxArt(ctx, model.ReleaseGroupAuxArt{ReleaseGroupID: e.id, PID: e.pid}); err != nil {
			t.Fatalf("mark %s: %v", e.pid, err)
		}
	}

	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, survPID, losePID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", loseID); n != 0 {
		t.Errorf("loser aux_art rows = %d, want 0 (a reused rowid would inherit it)", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='aux_art' AND entity_id=?", survID); n != 1 {
		t.Errorf("survivor aux_art rows = %d, want its own kept", n)
	}
	assertVerifyClean(t, st)
}

// seriesFixture puts two books under two series and returns the store and the two
// series pids, survivor first. The series merge cases share it.
func seriesFixture(t *testing.T) (*Store, model.PID, model.PID) {
	t.Helper()
	st, lib := entityFixture(t)
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/One/b1.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Author", series: "Dune", seq: "1",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/Two/b2.m4b", essence: "be2", content: "bc2",
		title: "Book Two", author: "Jane Author", series: "Dune Chronicles", seq: "2",
	})
	return st, entityPID(t, st, "series", "Dune Chronicles"), entityPID(t, st, "series", "Dune")
}

// TestMergeSeries collapses one series onto another: the loser's books re-point, the
// loser row goes, and the moved book gets its own item delta.
func TestMergeSeries(t *testing.T) {
	st, survivor, loser := seriesFixture(t)
	ctx := context.Background()
	survivorID := entityIDByCol(t, st, "series", "name", "Dune Chronicles")

	rep, err := st.MergeEntity(ctx, model.MergeSeries, survivor, loser)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if rep.Children != 1 {
		t.Errorf("children re-pointed = %d, want 1", rep.Children)
	}
	if entityExists(t, st, "series", "Dune") {
		t.Error("loser series still present after merge")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM book WHERE series_id=?", survivorID); n != 2 {
		t.Errorf("survivor holds %d books, want 2", n)
	}
	itemPID := scalarStr(t, st, "SELECT pid FROM playable_item WHERE title='Book One'")
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE entity_type='item' AND op='update' AND entity_pid=?",
		itemPID); n == 0 {
		t.Error("merge emitted no per-item delta for the moved book")
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE entity_type='series' AND op='delete' AND entity_pid=?",
		string(loser)); n != 1 {
		t.Errorf("loser delete deltas = %d, want 1", n)
	}
	assertVerifyClean(t, st)
}

// TestMergeSeriesRebuildsBookFTS: the search row carries the series name in its album
// column and book has no denormalized series column, so a moved book stays searchable
// under the dead series unless the merge reindexes it. `db verify` counts rows rather
// than content, so nothing else catches the drift.
func TestMergeSeriesRebuildsBookFTS(t *testing.T) {
	st, survivor, loser := seriesFixture(t)
	ctx := context.Background()

	if _, err := st.MergeEntity(ctx, model.MergeSeries, survivor, loser); err != nil {
		t.Fatalf("merge: %v", err)
	}
	itemID := scalarInt(t, st, "SELECT id FROM playable_item WHERE title='Book One'")
	if got := scalarStr(t, st, "SELECT album FROM search_fts WHERE rowid=?", itemID); got != "Dune Chronicles" {
		t.Errorf("moved book search series = %q, want Dune Chronicles", got)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM search_fts WHERE album='Dune'"); n != 0 {
		t.Errorf("%d search rows still indexed under the dead series, want 0", n)
	}
	assertVerifyClean(t, st)
}

// TestMergeDropsLoserOrphanCandidate: orphan_candidate is keyed by rowid and SQLite
// reuses rowids, so a candidate row left behind by a merged-away loser would hand the
// next entity to take that id a pre-aged first_seen and an immediate sweep.
func TestMergeDropsLoserOrphanCandidate(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})
	loseRGID := entityIDByCol(t, st, "release_group", "title", "LoseRG")
	survRG := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title='SurvRG'"))
	loseRG := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title='LoseRG'"))

	// Emptying the loser group's album leaves it childless, so the next sweep records
	// it as a candidate; the grace window keeps it alive rather than deleting it.
	survAlbum := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='SurvRG'"))
	loseAlbum := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title='LoseRG'"))
	if _, err := st.MergeEntity(ctx, model.MergeAlbum, survAlbum, loseAlbum); err != nil {
		t.Fatalf("album merge: %v", err)
	}
	if _, err := st.GCOrphans(ctx, int64(time.Hour)); err != nil {
		t.Fatalf("gc orphans: %v", err)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM orphan_candidate WHERE entity_type='release_group' AND entity_id=?",
		loseRGID); n != 1 {
		t.Fatalf("candidate rows before the merge = %d, want 1", n)
	}

	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, survRG, loseRG); err != nil {
		t.Fatalf("release-group merge: %v", err)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM orphan_candidate WHERE entity_type='release_group' AND entity_id=?",
		loseRGID); n != 0 {
		t.Errorf("candidate rows after the merge = %d, want 0 (a reused rowid would inherit it)", n)
	}
	assertVerifyClean(t, st)
}

// TestMergeUnhandledTypeRefusesBeforeDeleting: both switches inside the merge carry a
// default arm, so widening Valid without also adding a re-point and an affected-item
// query refuses rather than deleting a loser with nothing moved off it. It calls
// mergeEntityTx directly because the public entry point's own Valid gate catches an
// unknown type long before either switch sees it.
func TestMergeUnhandledTypeRefusesBeforeDeleting(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "Surv", album: "A"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "Lose", album: "B"})
	survivor := entityPID(t, st, "artist", "Surv")
	loser := entityPID(t, st, "artist", "Lose")

	err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := mergeEntityTx(ctx, tx, model.MergeEntity("bogus"), "artist", survivor, loser)
		return err
	})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("unhandled merge type: got %v, want CodeInvalid", err)
	}
	if !entityExists(t, st, "artist", "Lose") {
		t.Error("a merge that re-pointed nothing still deleted the loser")
	}
	assertVerifyClean(t, st)
}

func TestMergeErrors(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "A",
	})
	putBook(t, st, lib.ID, bookSpec{
		path: "/lib/Author/One/b1.m4b", essence: "be1", content: "bc1",
		title: "Book One", author: "Jane Author", series: "Saga", seq: "1",
	})
	pid := entityPID(t, st, "artist", "X")
	seriesPID := entityPID(t, st, "series", "Saga")

	if _, err := st.MergeEntity(ctx, model.MergeEntity("bogus"), pid, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad type: got %v, want CodeInvalid", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, pid, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("same pid: got %v, want CodeInvalid", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, pid, model.PID("nonexistent")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("missing loser: got %v, want CodeNotFound", err)
	}
	// series clears the type gate now, so an unknown loser refuses as not-found rather
	// than as an unknown entity type.
	if _, err := st.MergeEntity(ctx, model.MergeSeries, seriesPID, model.PID("nonexistent")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("missing series loser: got %v, want CodeNotFound", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeSeries, seriesPID, seriesPID); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("same series pid: got %v, want CodeInvalid", err)
	}
}

// TestMergeAlbumDropsFieldsMarker: the album fields walk's marker is keyed by the album's
// own rowid under its own entity_type, so the merge's marker union never reaches it. It
// records an answer about the loser, and album rowids are reused, so it goes with the
// loser rather than being inherited.
func TestMergeAlbumDropsFieldsMarker(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Surv/01.flac", essence: "e1", content: "c1", title: "One",
		artist: "Band", album: "SurvAl",
	})
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/Band/Lose/01.flac", essence: "e2", content: "c2", title: "Two",
		artist: "Band", album: "LoseAl",
	})
	al := func(title string) (int64, model.PID) {
		t.Helper()
		var id int64
		var pid string
		if err := st.read.QueryRowContext(ctx,
			"SELECT id, pid FROM album WHERE title = ?", title).Scan(&id, &pid); err != nil {
			t.Fatalf("no album titled %q: %v", title, err)
		}
		return id, model.PID(pid)
	}
	survID, survPID := al("SurvAl")
	loseID, losePID := al("LoseAl")
	for _, e := range []struct {
		id  int64
		pid model.PID
	}{{survID, survPID}, {loseID, losePID}} {
		if err := st.ApplyAlbumFields(ctx, model.AlbumFieldsEnrichment{AlbumID: e.id, PID: e.pid}); err != nil {
			t.Fatalf("mark %s: %v", e.pid, err)
		}
	}

	if _, err := st.MergeEntity(ctx, model.MergeAlbum, survPID, losePID); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='fields_album' AND entity_id=?", loseID); n != 0 {
		t.Errorf("loser fields_album rows = %d, want 0 (a reused rowid would inherit it)", n)
	}
	if n := scalarInt(t, st,
		"SELECT COUNT(*) FROM entity_enrichment WHERE entity_type='fields_album' AND entity_id=?", survID); n != 1 {
		t.Errorf("survivor fields_album rows = %d, want its own kept", n)
	}
	assertVerifyClean(t, st)
}
