package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// entityStateDeltas counts the synthetic "entity_play_state" change_log rows for one
// entity pid, the observable a value-identical entity star/rating must not move (the
// entityPlayStateWrite changed gate). It counts the synthetic type, not the entity's own
// kind, because that is where the writes emit: a scan/merge emits shared-entity deltas
// under "album"/"artist"/..., which must never be confused with a per-user star.
func entityStateDeltas(t *testing.T, st *Store, pid model.PID) int {
	t.Helper()
	return scalarInt(t, st,
		"SELECT COUNT(*) FROM change_log WHERE entity_type='entity_play_state' AND entity_pid=?", string(pid))
}

// TestEntityStarRatingReadBack stars an album and an artist for a user and reads them
// back through EntityPlayState and StarredEntities, then exercises rating set, clamp, and
// clear. It is the core round-trip the getStarred2 migration import needs.
func TestEntityStarRatingReadBack(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{
		path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One",
		artist: "X", album: "Al", genre: "Rock",
	})
	albumPID := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))
	artistPID := entityPID(t, st, "artist", "X")

	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, albumPID, true, nil); err != nil {
		t.Fatalf("star album: %v", err)
	}
	if err := st.SetEntityStar(ctx, "", model.MergeArtist, artistPID, true, nil); err != nil {
		t.Fatalf("star artist: %v", err)
	}

	// Single read-back: each entity carries its own star, scoped to its kind.
	al, err := st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID)
	if err != nil {
		t.Fatalf("read album state: %v", err)
	}
	if !al.Starred || al.Kind != model.MergeAlbum || al.EntityPID != albumPID || al.StarredAt == 0 {
		t.Fatalf("album state = %+v, want starred album %s with a set time", al, albumPID)
	}
	ar, _ := st.EntityPlayState(ctx, "", model.MergeArtist, artistPID)
	if !ar.Starred || ar.Kind != model.MergeArtist {
		t.Fatalf("artist state = %+v, want starred artist", ar)
	}

	// The starred list is kind-scoped: the album list holds only the album, the artist
	// list only the artist.
	albums, err := st.StarredEntities(ctx, "", model.MergeAlbum)
	if err != nil {
		t.Fatalf("starred albums: %v", err)
	}
	if len(albums) != 1 || albums[0].EntityPID != albumPID {
		t.Fatalf("starred albums = %+v, want just %s", albums, albumPID)
	}
	artists, _ := st.StarredEntities(ctx, "", model.MergeArtist)
	if len(artists) != 1 || artists[0].EntityPID != artistPID {
		t.Fatalf("starred artists = %+v, want just %s", artists, artistPID)
	}

	// Rating set, clamp above 100, then clear.
	r := 80
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, &r, nil); err != nil {
		t.Fatalf("rate album: %v", err)
	}
	if al, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID); !al.HasRating || al.Rating != 80 {
		t.Fatalf("album rating = %+v, want 80", al)
	}
	over := 150
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, &over, nil); err != nil {
		t.Fatalf("rate album over: %v", err)
	}
	if al, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID); al.Rating != 100 {
		t.Errorf("over-100 rating = %d, want clamped to 100", al.Rating)
	}
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, albumPID, nil, nil); err != nil {
		t.Fatalf("clear album rating: %v", err)
	}
	if al, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, albumPID); al.HasRating {
		t.Errorf("cleared rating = %+v, want no rating", al)
	}
	// Clearing the rating left the star intact (the row persists).
	if !al.Starred {
		t.Errorf("clearing the rating dropped the star: %+v", al)
	}

	// An untouched entity reads back zero-valued, not an error.
	genrePID := entityPID(t, st, "genre", "Rock")
	g, err := st.EntityPlayState(ctx, "", model.MergeGenre, genrePID)
	if err != nil {
		t.Fatalf("read untouched genre: %v", err)
	}
	if g.Starred || g.HasRating {
		t.Errorf("untouched genre state = %+v, want zero-valued", g)
	}
}

// TestEntityPlayStateUnknownPID confirms a read for an unknown entity pid is CodeNotFound
// rather than a confident zero-valued "not starred". The read resolves the entity first,
// matching PlayStateFor and EntityCuration, so a typo'd or stale pid surfaces instead of
// masking as unstarred, and a bad entity errors just like a bad user does.
func TestEntityPlayStateUnknownPID(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})

	if _, err := st.EntityPlayState(ctx, "", model.MergeAlbum, model.PID("01JZZNONEXISTENT000000000")); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("unknown album pid: got %v, want CodeNotFound", err)
	}
	// A real but untouched entity still reads back zero-valued (not an error).
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))
	got, err := st.EntityPlayState(ctx, "", model.MergeAlbum, album)
	if err != nil {
		t.Fatalf("untouched album: %v", err)
	}
	if got.Starred || got.HasRating {
		t.Errorf("untouched album state = %+v, want zero-valued", got)
	}
}

// TestStarredEntitiesRecencyOrder verifies the starred list is ordered most-recent
// first by starred_at, the recency contract a getStarred2 serve relies on.
func TestStarredEntitiesRecencyOrder(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "First"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "Second"})
	first := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "First"))
	second := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Second"))

	ns := func(v int64) *int64 { return &v }
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, first, true, ns(100)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, second, true, ns(200)); err != nil {
		t.Fatal(err)
	}
	got, err := st.StarredEntities(ctx, "", model.MergeAlbum)
	if err != nil {
		t.Fatalf("starred: %v", err)
	}
	if len(got) != 2 || got[0].EntityPID != second || got[1].EntityPID != first {
		t.Fatalf("order = %+v, want the later-starred %s before %s", got, second, first)
	}
}

// TestEntityStarAsOfRecordedTime pins the recorded-time (as-of) guard on SetEntityStar,
// the entity twin of TestStarAsOfRecordedTime: a flip not newer than the stored change is
// skipped as a stale replay, a newer one applies and stamps in recorded time, and a
// value-identical call stays a no-op regardless of as-of, none of the skips emitting a
// delta.
func TestEntityStarAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))
	ns := func(v int64) *int64 { return &v }

	// Star recorded at 100: starred_at and the stamp both land in recorded time.
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, true, ns(100)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.EntityPlayState(ctx, "", model.MergeAlbum, album)
	if !got.Starred || got.StarredAt != 100 || got.StarredChangedAt != 100 {
		t.Fatalf("star@100 = %+v, want starred with recorded time 100", got)
	}
	if n := entityStateDeltas(t, st, album); n != 1 {
		t.Fatalf("star@100 emitted %d deltas, want 1", n)
	}

	// Stale replay: an unstar recorded at 50 (not newer than 100) is skipped.
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, false, ns(50)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, album)
	if !got.Starred || got.StarredChangedAt != 100 {
		t.Errorf("stale unstar@50 = %+v, want still starred with stamp 100", got)
	}
	// Equal recorded time is stale too (not strictly newer).
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, false, ns(100)); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, album); !got.Starred {
		t.Errorf("unstar@100 (equal) = %+v, want skipped", got)
	}
	// A value-identical re-star with any as-of stays a no-op preserving the stamp.
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, true, ns(999)); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, album); got.StarredChangedAt != 100 || got.StarredAt != 100 {
		t.Errorf("identical re-star@999 = %+v, want stored stamp 100 preserved", got)
	}
	if n := entityStateDeltas(t, st, album); n != 1 {
		t.Errorf("stale/identical writes emitted deltas (%d total), want just the first", n)
	}

	// A newer recorded time wins: unstar at 200 applies and advances the stamp.
	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, false, ns(200)); err != nil {
		t.Fatal(err)
	}
	got, _ = st.EntityPlayState(ctx, "", model.MergeAlbum, album)
	if got.Starred || got.StarredAt != 0 || got.StarredChangedAt != 200 {
		t.Errorf("unstar@200 = %+v, want cleared with stamp 200", got)
	}
	if n := entityStateDeltas(t, st, album); n != 2 {
		t.Errorf("unstar@200 emitted %d total deltas, want 2", n)
	}
}

// TestEntityRatingAsOfRecordedTime mirrors the star as-of guard for entity ratings.
func TestEntityRatingAsOfRecordedTime(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))
	ns := func(v int64) *int64 { return &v }
	r80, r60 := 80, 60

	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, album, &r80, ns(100)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.EntityPlayState(ctx, "", model.MergeAlbum, album); got.Rating != 80 || got.RatingChangedAt != 100 {
		t.Fatalf("rate@100 = %+v, want 80 with stamp 100", got)
	}
	// Stale: a different value recorded at 50 is skipped.
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, album, &r60, ns(50)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.EntityPlayState(ctx, "", model.MergeAlbum, album); got.Rating != 80 || got.RatingChangedAt != 100 {
		t.Errorf("stale rate@50 = %+v, want unchanged 80/100", got)
	}
	// Newer wins.
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, album, &r60, ns(200)); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.EntityPlayState(ctx, "", model.MergeAlbum, album); got.Rating != 60 || got.RatingChangedAt != 200 {
		t.Errorf("rate@200 = %+v, want 60 with stamp 200", got)
	}
}

// TestEntityStarIdempotentReimportNoDelta is the entityPlayStateWrite changed gate: an
// idempotent re-star or re-rate (what a getStarred2 re-import sends for every already
// starred row) writes no change_log delta, so a re-import stays change-log silent.
func TestEntityStarIdempotentReimportNoDelta(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))

	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, true, nil); err != nil {
		t.Fatal(err)
	}
	r := 70
	if err := st.SetEntityRating(ctx, "", model.MergeAlbum, album, &r, nil); err != nil {
		t.Fatal(err)
	}
	afterFirst := entityStateDeltas(t, st, album)

	// Re-send the identical star and rating twice: no new deltas.
	for i := 0; i < 2; i++ {
		if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, true, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.SetEntityRating(ctx, "", model.MergeAlbum, album, &r, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := entityStateDeltas(t, st, album); n != afterFirst {
		t.Errorf("idempotent re-import moved the delta count %d -> %d, want unchanged", afterFirst, n)
	}
}

// TestEntityStateMergeConflict drives repointEntityPlayState: two users each starred both
// the survivor and the loser release group. After the merge each user keeps exactly one
// row, on the survivor, and it is the survivor's own (survivor-wins by position, not
// recorded-time-wins), with the loser's rows gone. release_group is used deliberately:
// its entity_type filter in the reads is load-bearing because a rowid is not unique
// across entity tables.
func TestEntityStateMergeConflict(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})
	surv := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "SurvRG"))
	lose := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "LoseRG"))
	survID := scalarInt(t, st, "SELECT id FROM release_group WHERE title = ?", "SurvRG")

	bob, err := st.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ns := func(v int64) *int64 { return &v }
	// Each user stars the survivor at 100 and the loser at a newer 200: if the merge
	// picked by recorded time it would keep the loser's 200, so a surviving stamp of 100
	// proves survivor-wins-by-position.
	for _, u := range []model.PID{"", bob.PID} {
		if err := st.SetEntityStar(ctx, u, model.MergeReleaseGroup, surv, true, ns(100)); err != nil {
			t.Fatal(err)
		}
		if err := st.SetEntityStar(ctx, u, model.MergeReleaseGroup, lose, true, ns(200)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, surv, lose); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Every entity_play_state row now points at the survivor; two rows total (one per user).
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_play_state WHERE entity_type='release_group'"); n != 2 {
		t.Fatalf("release_group play-state rows after merge = %d, want 2", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_play_state WHERE entity_type='release_group' AND entity_id <> ?", survID); n != 0 {
		t.Fatalf("play-state rows off the survivor after merge = %d, want 0", n)
	}
	// Each user keeps the survivor's own stamp (100), not the loser's newer 200.
	for _, u := range []model.PID{"", bob.PID} {
		got, err := st.StarredEntities(ctx, u, model.MergeReleaseGroup)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].EntityPID != surv || got[0].StarredChangedAt != 100 {
			t.Fatalf("user %q starred = %+v, want just the survivor with stamp 100", u, got)
		}
	}
}

// TestEntityStateMergeUnionsDisjoint pins the non-lossy half of the merge fold: when a
// user rated the survivor and starred the loser (disjoint state, no field conflict), the
// merged survivor row carries both, the same way repointArtMap/repointEntityCuration keep
// a role or field only the loser held. A pure survivor-wins-by-row merge would drop the
// star.
func TestEntityStateMergeUnionsDisjoint(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})
	surv := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "SurvRG"))
	lose := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "LoseRG"))

	// Survivor is rated (no star); loser is starred (no rating). No field conflicts.
	r := 80
	if err := st.SetEntityRating(ctx, "", model.MergeReleaseGroup, surv, &r, nil); err != nil {
		t.Fatal(err)
	}
	ns := func(v int64) *int64 { return &v }
	if err := st.SetEntityStar(ctx, "", model.MergeReleaseGroup, lose, true, ns(200)); err != nil {
		t.Fatal(err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, surv, lose); err != nil {
		t.Fatalf("merge: %v", err)
	}

	// One row survives, on the survivor, carrying both the rating and the inherited star.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_play_state WHERE entity_type='release_group'"); n != 1 {
		t.Fatalf("play-state rows after merge = %d, want 1", n)
	}
	got, err := st.EntityPlayState(ctx, "", model.MergeReleaseGroup, surv)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasRating || got.Rating != 80 {
		t.Errorf("merged rating = %+v, want the survivor's 80 kept", got)
	}
	if !got.Starred || got.StarredChangedAt != 200 {
		t.Errorf("merged star = %+v, want the loser's star (stamp 200) inherited, not dropped", got)
	}
}

// TestEntityStateMergeKeepsClearedRating guards the pair-wise fold's use of *_changed_at
// (not the value) as the "has a pair" test: a survivor who rated then cleared (rating
// NULL, rating_changed_at set) must keep that cleared state, never inherit the loser's
// stale rating. A naive per-column COALESCE(rating, loser.rating) would resurrect it.
func TestEntityStateMergeKeepsClearedRating(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "A", albumArt: "A", album: "SurvRG"})
	putTrack(t, st, lib.ID, trackSpec{path: "/lib/b/2.flac", essence: "e2", content: "c2", title: "Two", artist: "B", albumArt: "B", album: "LoseRG"})
	surv := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "SurvRG"))
	lose := model.PID(scalarStr(t, st, "SELECT pid FROM release_group WHERE title = ?", "LoseRG"))

	// Survivor: rate then clear, leaving rating NULL with rating_changed_at set. Loser: 90.
	r50, r90 := 50, 90
	if err := st.SetEntityRating(ctx, "", model.MergeReleaseGroup, surv, &r50, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntityRating(ctx, "", model.MergeReleaseGroup, surv, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntityRating(ctx, "", model.MergeReleaseGroup, lose, &r90, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := st.MergeEntity(ctx, model.MergeReleaseGroup, surv, lose); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := st.EntityPlayState(ctx, "", model.MergeReleaseGroup, surv)
	if got.HasRating {
		t.Errorf("merged rating = %+v, want the survivor's cleared rating preserved (not the loser's 90)", got)
	}
}

// TestEntityStateOrphanLockstep is the invariant the polymorphic-no-FK design rests on:
// a starred album entity whose tracks are all deleted is swept by GCOrphans, and its
// entity_play_state row goes with it, leaving derived data consistent.
func TestEntityStateOrphanLockstep(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	r := putTrack(t, st, lib.ID, trackSpec{path: "/lib/a/1.flac", essence: "e1", content: "c1", title: "One", artist: "X", album: "Al"})
	album := model.PID(scalarStr(t, st, "SELECT pid FROM album WHERE title = ?", "Al"))

	if err := st.SetEntityStar(ctx, "", model.MergeAlbum, album, true, nil); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_play_state"); n != 1 {
		t.Fatalf("entity_play_state rows = %d, want 1", n)
	}

	// Delete the backing item through the cascade helper (which also clears its FTS
	// row, as the real prune path does), orphaning the album, then sweep with no grace
	// window. A raw DELETE would leave a stale search_fts row and fail the verify below
	// for a reason unrelated to entity play-state.
	itemID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(r.ItemPID)))
	if err := st.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := deleteItemCascade(ctx, tx, itemID)
		return err
	}); err != nil {
		t.Fatalf("delete item: %v", err)
	}
	if _, err := st.GCOrphans(ctx, 0); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM album"); n != 0 {
		t.Fatalf("album rows after sweep = %d, want 0", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_play_state"); n != 0 {
		t.Errorf("entity_play_state rows after orphan sweep = %d, want 0 (the star went with the album)", n)
	}
	if rep, err := st.VerifyDerived(ctx); err != nil || !rep.Consistent() {
		t.Fatalf("derived inconsistent after orphan sweep: %+v (err %v)", rep, err)
	}
}

// TestEntityStateSeriesOrphanNoOp confirms the entity_play_state delete in
// deleteOrphanEntity is a harmless no-op for series, which is in orphanKinds but is never
// written by the entity play-state path. It guards against someone later "fixing" the
// series arm by adding it to the write path.
func TestEntityStateSeriesOrphanNoOp(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	// A series with no book is an orphan; it carries no entity_play_state row.
	pid := model.NewPID()
	if _, err := st.write.ExecContext(ctx,
		"INSERT INTO series(pid, name, sort_key, match_key) VALUES (?,?,?,?)",
		string(pid), "Orphan Series", model.SortKey("Orphan Series"), "orphan-series"); err != nil {
		t.Fatalf("insert series: %v", err)
	}
	rep, err := st.GCOrphans(ctx, 0)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if rep.Series != 1 {
		t.Fatalf("series swept = %d, want 1", rep.Series)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM series"); n != 0 {
		t.Errorf("series rows after sweep = %d, want 0", n)
	}
}
