package sqlite

import (
	"context"
	"testing"

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
		"INSERT INTO art_map(entity_type, entity_id, source_hash, role, priority) VALUES (?,?,?,'front',0)",
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

func TestMergeErrors(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "A",
	})
	pid := entityPID(t, st, "artist", "X")

	if _, err := st.MergeEntity(ctx, model.MergeEntity("bogus"), pid, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("bad type: got %v, want CodeInvalid", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, pid, pid); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("same pid: got %v, want CodeInvalid", err)
	}
	if _, err := st.MergeEntity(ctx, model.MergeArtist, pid, model.PID("nonexistent")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("missing loser: got %v, want CodeNotFound", err)
	}
}
