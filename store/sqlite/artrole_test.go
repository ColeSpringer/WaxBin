package sqlite

import (
	"context"
	"database/sql"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file covers the artwork-role surface: independent per-role slots on one
// entity, the front-only fallback chain, the Level/Derived report, and the
// verify/GC treatment of multi-role and episode/podcast art.

func TestArtRolesIndependentSetAndClear(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	front, back := testPNG(t, 40, 40), testPNG(t, 41, 41)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", front)

	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set back: %v", err)
	}

	// Both roles resolve to their own image.
	fb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0)
	if err != nil || fb.SourceHash != front.Hash {
		t.Fatalf("front = %v (err %v), want the scanned cover", fb, err)
	}
	bb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0)
	if err != nil || bb.SourceHash != back.Hash {
		t.Fatalf("back = %v (err %v), want the set back image", bb, err)
	}

	// The listing reports both slots at the item's own level.
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 2 || roles[0].Role != model.ArtRoleBack || roles[1].Role != model.ArtRoleFront {
		t.Fatalf("roles = %+v, want [back front]", roles)
	}
	if roles[1].SourceHash != front.Hash || roles[1].Width != 40 {
		t.Fatalf("front listing = %+v, want the source's hash and dims", roles[1])
	}

	// Clearing one role leaves the other intact.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, nil, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear back: %v", err)
	}
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("cleared back = %v, want CodeNotFound", err)
	}
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0); err != nil {
		t.Fatalf("front after back clear: %v", err)
	}
}

func TestScanPreservesNonFrontRoles(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	coverA, coverB, back := testPNG(t, 40, 40), testPNG(t, 42, 42), testPNG(t, 41, 41)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", coverA)

	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set back: %v", err)
	}
	// A rescan with a DIFFERENT front cover replaces front and must not touch back.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", coverB)

	fb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0)
	if err != nil || fb.SourceHash != coverB.Hash {
		t.Fatalf("front after rescan = %v (err %v), want the new cover", fb, err)
	}
	bb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0)
	if err != nil || bb.SourceHash != back.Hash {
		t.Fatalf("back after rescan = %v (err %v), want the set back preserved", bb, err)
	}
}

// TestChainIgnoresNonFrontRows pins the fix for the latent any-role-answers-front
// bug: an item holding nothing but a back image must not serve it as a front cover; the
// front walk falls through to the album level instead.
func TestChainIgnoresNonFrontRows(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	albumCover, back := testPNG(t, 40, 40), testPNG(t, 41, 41)
	// Track 1 carries the album's only front cover; track 2 gets only a back image.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", albumCover)
	pid2 := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)
	if err := st.SetItemArt(ctx, pid2, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set back: %v", err)
	}

	fb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid2}, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("front resolve: %v", err)
	}
	if fb.SourceHash != albumCover.Hash {
		t.Fatalf("front = %s, want the album fallback %s (the back image must not answer front)", fb.SourceHash, albumCover.Hash)
	}
	if fb.Level != model.ArtAlbum {
		t.Fatalf("level = %s, want album (the answer came from the fallback chain)", fb.Level)
	}
}

// TestNonFrontResolvesOwnLevelOnly verifies a non-front role never looks past
// the requested entity in either direction: a track with no back image reports
// CodeNotFound even when its album carries one, and an album with no back
// reports CodeNotFound even when a member track carries one (the member-derived
// answer is a front-cover mechanism alone).
func TestNonFrontResolvesOwnLevelOnly(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	back := testPNG(t, 41, 41)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil)
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set album back: %v", err)
	}

	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleBack, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("track back = %v, want CodeNotFound (no ancestor inheritance)", err)
	}
	// The album's own back resolves, at its own level.
	bb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: albumPID(t, st)}, model.ArtRoleBack, 0)
	if err != nil || bb.Level != model.ArtAlbum || bb.Derived {
		t.Fatalf("album back = %+v (err %v), want a direct own-level answer", bb, err)
	}

	// The reverse direction: with the album's own back cleared and a back on the
	// member track instead, the album must not derive it.
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRoleBack, nil, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("clear album back: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set track back: %v", err)
	}
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: albumPID(t, st)}, model.ArtRoleBack, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("album back with only a member back = %v, want CodeNotFound (derivation is front-only)", err)
	}
}

// TestResolveArtLevelDerivedMatrix covers the Level/Derived report: an item's own
// cover, a sibling answered through the album's track-derived cover, and the
// derived -> durable flip once a real album row exists.
func TestResolveArtLevelDerivedMatrix(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	trackCover, albumCover := testPNG(t, 40, 40), testPNG(t, 42, 42)
	pid1 := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", trackCover)
	pid2 := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)

	// Own cover: level=track, not derived.
	b1, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid1}, model.ArtRoleFront, 0)
	if err != nil || b1.Level != model.ArtTrack || b1.Derived {
		t.Fatalf("own cover = %+v (err %v), want level=track derived=false", b1, err)
	}

	// The bare sibling resolves through the album, derived from track 1's cover.
	b2, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid2}, model.ArtRoleFront, 0)
	if err != nil || b2.Level != model.ArtAlbum || !b2.Derived {
		t.Fatalf("sibling fallback = %+v (err %v), want level=album derived=true", b2, err)
	}

	// The album ref itself reports the same derivation.
	alb := albumPID(t, st)
	ba, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: alb}, model.ArtRoleFront, 0)
	if err != nil || ba.Level != model.ArtAlbum || !ba.Derived {
		t.Fatalf("album derived = %+v (err %v), want level=album derived=true", ba, err)
	}

	// A durable album cover flips Derived off (and wins over the track-derived one).
	if err := st.SetEntityArt(ctx, model.ArtAlbum, alb, model.ArtRoleFront, albumCover.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set album front: %v", err)
	}
	ba, err = st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: alb}, model.ArtRoleFront, 0)
	if err != nil || ba.Level != model.ArtAlbum || ba.Derived || ba.SourceHash != albumCover.Hash {
		t.Fatalf("durable album = %+v (err %v), want derived=false and the set cover", ba, err)
	}

	// A thumbnail carries the same level/derived stamp.
	bt, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid2}, model.ArtRoleFront, 20)
	if err != nil || !bt.Thumbnail || bt.Level != model.ArtAlbum || bt.Derived {
		t.Fatalf("thumbnail = %+v (err %v), want level=album derived=true on the scaled blob", bt, err)
	}
}

func TestSetArtUnknownRoleRejected(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	img := testPNG(t, 40, 40)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil)

	if err := st.SetItemArt(ctx, pid, model.ArtRole("portrait"), img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("item art with unknown role = %v, want CodeInvalid", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRole("artist"), img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("entity art with unknown role = %v, want CodeInvalid (the vocabulary is closed now)", err)
	}
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRole("nope"), 0); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("resolve with unknown role = %v, want CodeInvalid", err)
	}

	// The front lock gates only the front slot: with art locked, a back set works.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set+lock front: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Errorf("back set under a front lock = %v, want allowed (the lock guards the scanned slot only)", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("front set under lock = %v, want CodeLocked", err)
	}
}

// TestEntityArtSlotKindMismatchRejected covers the track and episode art slots sharing
// playable_item: setting episode art on a track's pid (or the reverse) would store a
// map row no resolver reads back, since the chain probes the slot the item's kind
// selects, so the mismatch is refused up front.
func TestEntityArtSlotKindMismatchRejected(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	img := testPNG(t, 40, 40)
	track := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil)

	if err := st.SetEntityArt(ctx, model.ArtEpisode, track, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("episode art on a track pid = %v, want CodeInvalid", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type = 'episode'"); n != 0 {
		t.Errorf("%d episode art rows written for a track, want none", n)
	}
	// The reverse, for a consumer calling the entity path with the track slot.
	feed, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/slots"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	ep := episodeByTitle(t, st, feed.PodcastPID, "Alpha").PID
	if err := st.SetEntityArt(ctx, model.ArtTrack, ep, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("track art on an episode pid = %v, want CodeInvalid", err)
	}
	// The matching slot still works: an episode's own cover is a real, resolvable row.
	if err := st.SetEntityArt(ctx, model.ArtEpisode, ep, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("episode art on an episode pid: %v", err)
	}
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtEpisode, PID: ep}, model.ArtRoleFront, 0)
	if err != nil || blob.SourceHash != img.Hash {
		t.Fatalf("episode front = %+v (err %v), want the set cover", blob, err)
	}
}

// TestItemDeleteDropsArtRows pins the art half of deleteItemCascade: an item that is
// deleted because its file re-keyed to a new identity takes its art_map rows with it.
// playable_item.id is a plain INTEGER PRIMARY KEY, so a row left for GCArt could
// resurface on whatever item inherits the rowid.
func TestItemDeleteDropsArtRows(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	old := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	oldID := int64(scalarInt(t, st, "SELECT id FROM playable_item WHERE pid = ?", string(old)))
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type='track' AND entity_id=?", oldID); n != 1 {
		t.Fatalf("scanned cover rows = %d, want 1 before the re-key", n)
	}

	// Rescanning the same path with a different essence re-keys the file to a new
	// identity, orphaning the old item and deleting it.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e2", nil)
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM playable_item WHERE id = ?", oldID); n != 0 {
		t.Fatalf("the re-keyed item survived, so this test no longer covers the delete path")
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type='track' AND entity_id=?", oldID); n != 0 {
		t.Errorf("%d art rows outlived the deleted item, want 0", n)
	}
	if _, _, err := st.GCArt(ctx); err != nil {
		t.Fatalf("gc: %v", err)
	}
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 0 || rep.OrphanThumbnails != 0 {
		t.Errorf("verify after GC = %+v, want clean", rep)
	}
}

// TestRemovePodcastDropsArtRows is the podcast twin of TestItemDeleteDropsArtRows: a
// removed show takes its own feed art and each episode's cover with it, since podcast
// and playable_item rowids are reused the same way.
func TestRemovePodcastDropsArtRows(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	feedArt, epArt := testPNG(t, 40, 40), testPNG(t, 41, 41)
	feed, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/art"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	ep := episodeByTitle(t, st, feed.PodcastPID, "Alpha").PID
	if err := st.SetEntityArt(ctx, model.ArtPodcast, feed.PodcastPID, model.ArtRoleFront, feedArt.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set podcast art: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtEpisode, ep, model.ArtRoleFront, epArt.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set episode art: %v", err)
	}

	if _, err := st.RemovePodcast(ctx, feed.PodcastPID); err != nil {
		t.Fatalf("RemovePodcast: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type IN ('podcast','episode')"); n != 0 {
		t.Errorf("%d podcast/episode art rows outlived the show, want 0", n)
	}
	// The sources are now unreferenced, which is GC's job, and both agree on it.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 2 {
		t.Errorf("orphan sources = %d, want the show's and the episode's covers", rep.OrphanArtSources)
	}
	if sources, _, err := st.GCArt(ctx); err != nil || sources != 2 {
		t.Errorf("GCArt reclaimed %d sources (err %v), want 2", sources, err)
	}
}

// TestGCArtMultiRole verifies GC reclaims every role's source once the entity is
// gone, and that VerifyDerived counts live multi-role sources as reachable.
func TestGCArtMultiRole(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	front, back := testPNG(t, 40, 40), testPNG(t, 41, 41)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", front)
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set back: %v", err)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil || rep.OrphanArtSources != 0 {
		t.Fatalf("live multi-role sources reported orphaned: %+v (err %v)", rep, err)
	}

	// The entity vanishes without art cleanup; both roles' sources become orphans.
	if _, err := st.write.ExecContext(ctx, "DELETE FROM playable_item"); err != nil {
		t.Fatalf("delete items: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, "DELETE FROM album"); err != nil {
		t.Fatalf("delete albums: %v", err)
	}
	sources, _, err := st.GCArt(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if sources != 2 {
		t.Errorf("GCArt removed %d sources, want both roles' images (2)", sources)
	}
}

// TestVerifyCountsPodcastArtLive pins the verify <-> GC lockstep fix: a source
// reachable only through a podcast or episode slot is live (GCArt keeps it), so
// VerifyDerived must not count it orphaned.
func TestVerifyCountsPodcastArtLive(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()

	// A minimal podcast row to hang feed art on (the sync machinery is not under
	// test here).
	if _, err := st.write.ExecContext(ctx, `INSERT INTO podcast
		(pid, feed_url, identity_key, title, sort_key, created_at, updated_at)
		VALUES ('pod1','https://x.test/feed','feed:https://x.test/feed','Show','show',1,1)`); err != nil {
		t.Fatalf("insert podcast: %v", err)
	}
	var podID int64
	if err := st.read.QueryRowContext(ctx, "SELECT id FROM podcast WHERE pid='pod1'").Scan(&podID); err != nil {
		t.Fatalf("podcast id: %v", err)
	}
	seedArt(t, st, "hashPodCover", "podcast", podID)

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 0 {
		t.Errorf("podcast-only source counted orphaned (%d); the live-art arms must cover podcast/episode slots", rep.OrphanArtSources)
	}
	// GCArt agrees: nothing to reclaim.
	if sources, _, err := st.GCArt(ctx); err != nil || sources != 0 {
		t.Errorf("GCArt reclaimed %d sources (err %v), want 0 for a live podcast cover", sources, err)
	}
	// Once the show is gone, both verify and GC flip together.
	if _, err := st.write.ExecContext(ctx, "DELETE FROM podcast"); err != nil {
		t.Fatalf("delete podcast: %v", err)
	}
	rep, _ = st.VerifyDerived(ctx)
	if rep.OrphanArtSources != 1 {
		t.Errorf("dead podcast's source not reported orphaned: %+v", rep)
	}
	if sources, _, err := st.GCArt(ctx); err != nil || sources != 1 {
		t.Errorf("GCArt reclaimed %d sources (err %v), want the dead show's cover", sources, err)
	}
}

// A cleared and locked cover is the state `art set --clear` leaves behind, and it
// refuses every later set. It used to be invisible: ArtRoles overlaid Locked while
// iterating the rows that exist, and a coverless entity has none. The lock is the base
// fact now and the artifact the overlay, so the lock reports on its own.
func TestArtRolesReportsCoverlessLock(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	feed, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/coverless-lock"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	ref := model.EntityRef{Type: model.ArtPodcast, PID: feed.PodcastPID}
	img := testPNG(t, 44, 44)

	// Set then clear, the journey that produces the state, keeping the default lock.
	if err := st.SetEntityArt(ctx, model.ArtPodcast, feed.PodcastPID, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); err != nil {
		t.Fatalf("set podcast front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPodcast, feed.PodcastPID, model.ArtRoleFront, nil, "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), true); err != nil {
		t.Fatalf("clear podcast front: %v", err)
	}

	roles, err := st.ArtRoles(ctx, ref)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 1 {
		t.Fatalf("roles on a coverless locked podcast = %+v, want one front entry", roles)
	}
	if roles[0].Role != model.ArtRoleFront || !roles[0].Locked {
		t.Fatalf("entry = %+v, want a locked front", roles[0])
	}
	if roles[0].SourceHash != "" {
		t.Errorf("SourceHash = %q, want empty: the sentinel for a lock with no artifact", roles[0].SourceHash)
	}
	if roles[0].Source != model.SourceUser {
		t.Errorf("Source = %q, want the lock row's recorded user source", roles[0].Source)
	}
	if roles[0].UpdatedAt == 0 {
		t.Error("UpdatedAt = 0; the lock row carries a timestamp and the entry should too")
	}

	if locked, err := st.ArtLocked(ctx, model.ArtPodcast, feed.PodcastPID); err != nil || !locked {
		t.Errorf("ArtLocked = %v (err %v), want true", locked, err)
	}
}

// The synthesized entry must not appear for an entity that simply holds no art, which
// is what keeps "an empty list means nothing stored" true for the unlocked case.
func TestArtRolesEmptyOnCoverlessUnlockedEntity(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	pl := newPlaylist(t, st, "Mix")
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pl})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("roles on a coverless unlocked playlist = %+v, want an empty list", roles)
	}
}

// A locked entity that still holds other roles reports the front lock alongside them,
// in role order (front sorts last).
func TestArtRolesCoverlessLockKeepsRoleOrder(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	pl := newPlaylist(t, st, "Ordered")
	back := testPNG(t, 45, 45)
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleBack, back.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set playlist back: %v", err)
	}
	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, true); err != nil {
		t.Fatalf("lock playlist front: %v", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pl})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 2 || roles[0].Role != model.ArtRoleBack || roles[1].Role != model.ArtRoleFront {
		t.Fatalf("roles = %+v, want [back front]", roles)
	}
	if roles[0].Locked || !roles[1].Locked {
		t.Errorf("locks = back %v / front %v, want false / true", roles[0].Locked, roles[1].Locked)
	}
}

// SetArtLock is the lock-only mutation SetEntityArt cannot express, and unlocking is
// the way back out of a refused set with no --force in sight.
func TestSetArtLockRoundTrip(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	pl := newPlaylist(t, st, "Locked")
	img := testPNG(t, 46, 46)

	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(true), false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("set under a lock = %v, want CodeLocked", err)
	}
	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if locked, err := st.ArtLocked(ctx, model.ArtPlaylist, pl); err != nil || locked {
		t.Fatalf("ArtLocked after unlock = %v (err %v), want false", locked, err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, img.Data, "", model.Attribution{Source: model.SourceUser}, model.LockOf(false), false); err != nil {
		t.Fatalf("set after unlock (no force): %v", err)
	}
}

// SetArtLock on a track and `lock <pid> art` write the same row, since --type defaults
// to track. The overlap is deliberate, so it is pinned rather than left to be found.
func TestSetArtLockSharesTheItemArtLockRow(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40))

	if err := st.SetArtLock(ctx, model.ArtTrack, pid, true); err != nil {
		t.Fatalf("art lock: %v", err)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE field='art' AND locked=1"); n != 1 {
		t.Fatalf("%d locked art provenance rows, want exactly 1", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_curation WHERE field='art'"); n != 0 {
		t.Errorf("%d entity_curation art rows for a track, want none", n)
	}
	// The item-side reader agrees, and so does the unlock verb on the other side.
	rows, err := st.FieldProvenance(ctx, pid)
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	var sawLockedArt bool
	for _, r := range rows {
		if r.Field == "art" && r.Locked {
			sawLockedArt = true
		}
	}
	if !sawLockedArt {
		t.Errorf("provenance = %+v, want a locked art row", rows)
	}
	if err := st.UnlockField(ctx, pid, "art"); err != nil {
		t.Fatalf("unlock field: %v", err)
	}
	if locked, err := st.ArtLocked(ctx, model.ArtTrack, pid); err != nil || locked {
		t.Errorf("ArtLocked after `unlock <pid> art` = %v (err %v), want false", locked, err)
	}
	// And the row goes with the lock. An art row carries no value, so an unlocked one
	// is inert, and the source it kept ("user", from the lock writer) would report a
	// cover attribution that belongs to no cover.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE field='art'"); n != 0 {
		t.Errorf("%d art provenance rows survived the unlock, want none", n)
	}
}

// SetArtLock is idempotent, the way LockField/UnlockField already are. Without the
// guard, unlocking a never-locked entity writes nothing and still publishes an update,
// sending every ChangesSince tailer to re-fetch for no change.
func TestSetArtLockEmitsNoDeltaWhenAlreadyInState(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	pl := newPlaylist(t, st, "Quiet")

	seq := latestSeq(t, st)
	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, false); err != nil {
		t.Fatalf("unlock a never-locked playlist: %v", err)
	}
	if got := latestSeq(t, st); got != seq {
		t.Errorf("redundant unlock advanced the change log %d -> %d", seq, got)
	}

	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	locked := latestSeq(t, st)
	if locked == seq {
		t.Fatal("a real lock emitted no delta")
	}
	if err := st.SetArtLock(ctx, model.ArtPlaylist, pl, true); err != nil {
		t.Fatalf("re-lock: %v", err)
	}
	if got := latestSeq(t, st); got != locked {
		t.Errorf("redundant lock advanced the change log %d -> %d", locked, got)
	}
}

// TestArtSetCarriesTheCallersAttribution: an embedder that downloaded the cover itself
// is recorded as having downloaded it, where every curation write used to be stamped as
// a hand-set cover on its way into the store.
func TestArtSetCarriesTheCallersAttribution(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40))
	const url = "https://itunes.example/cover.png"

	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, testPNG(t, 42, 42).Data, "",
		model.Attribution{Source: model.SourceEnrichment, Provider: "itunes", SourceURL: url},
		model.LockOf(true), false); err != nil {
		t.Fatalf("set stamped cover: %v", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 1 || roles[0].Source != model.SourceEnrichment ||
		roles[0].Provider != "itunes" || roles[0].SourceURL != url {
		t.Fatalf("front slot = %+v, want the caller's enrichment attribution", roles)
	}

	// A set that names no origin is still a user set.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, testPNG(t, 43, 43).Data, "",
		model.Attribution{}, model.LockOf(true), true); err != nil {
		t.Fatalf("set unstamped cover: %v", err)
	}
	roles, err = st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if roles[0].Source != model.SourceUser || roles[0].Provider != "" {
		t.Errorf("front slot = %+v, want a user cover with no provider", roles[0])
	}
}

// TestArtWriteLeavesAnUnreadLockAlone covers the two halves of the same hazard: a clear
// that only meant to remove the picture keeps the pin, and a forced set stops rewriting
// the lock it forced past. Before this, preserving a lock meant reading it first and
// passing it back, which is the interleave two administrators lose a decision to.
func TestArtWriteLeavesAnUnreadLockAlone(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40))

	if err := st.SetArtLock(ctx, model.ArtTrack, pid, true); err != nil {
		t.Fatalf("lock: %v", err)
	}
	// A clear that says nothing about the lock leaves it standing.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, nil, "", model.Attribution{}, model.LockUnchanged, true); err != nil {
		t.Fatalf("clear keeping the lock: %v", err)
	}
	if locked, err := st.ArtLocked(ctx, model.ArtTrack, pid); err != nil || !locked {
		t.Fatalf("locked after a keep-lock clear = %v (err %v), want true", locked, err)
	}
	// So does a forced set: force skips the lock check for that one write and nothing
	// more.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, testPNG(t, 44, 44).Data, "",
		model.Attribution{}, model.LockUnchanged, true); err != nil {
		t.Fatalf("forced set: %v", err)
	}
	if locked, err := st.ArtLocked(ctx, model.ArtTrack, pid); err != nil || !locked {
		t.Fatalf("locked after a forced set = %v (err %v), want the lock still standing", locked, err)
	}
	// An explicit unlock still releases it.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, testPNG(t, 45, 45).Data, "",
		model.Attribution{}, model.LockOff, true); err != nil {
		t.Fatalf("unlocking set: %v", err)
	}
	if locked, err := st.ArtLocked(ctx, model.ArtTrack, pid); err != nil || locked {
		t.Errorf("locked after an explicit unlock = %v (err %v), want false", locked, err)
	}
}

// TestCoverlessLockReportsTheWritesAttribution: a cleared and locked cover has no
// art_map row, so ArtRoles synthesizes the front entry from the lock row alone. That row
// used to record an invented "user" whatever the write said.
func TestCoverlessLockReportsTheWritesAttribution(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40))
	al := albumPID(t, st)

	attr := model.Attribution{Source: model.SourceEnrichment, Provider: "itunes"}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, nil, "", attr, model.LockOn, false); err != nil {
		t.Fatalf("clear and lock item cover: %v", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil {
		t.Fatalf("item roles: %v", err)
	}
	if len(roles) != 1 || !roles[0].Locked || roles[0].SourceHash != "" ||
		roles[0].Source != model.SourceEnrichment || roles[0].Provider != "itunes" {
		t.Fatalf("item front = %+v, want a coverless lock attributed to itunes", roles)
	}

	// The entity-scoped table answers the same way.
	if err := st.SetEntityArt(ctx, model.ArtAlbum, al, model.ArtRoleFront, nil, "", attr, model.LockOn, false); err != nil {
		t.Fatalf("clear and lock album cover: %v", err)
	}
	roles, err = st.ArtRoles(ctx, model.EntityRef{Type: model.ArtAlbum, PID: al})
	if err != nil {
		t.Fatalf("album roles: %v", err)
	}
	var front *model.ArtRoleInfo
	for i := range roles {
		if roles[i].Role == model.ArtRoleFront {
			front = &roles[i]
		}
	}
	if front == nil || !front.Locked || front.Source != model.SourceEnrichment || front.Provider != "itunes" {
		t.Errorf("album front = %+v, want a coverless lock attributed to itunes", roles)
	}
}

// TestArtProvenanceAgreesWithResolveArt walks the same chain matrix ResolveArt is
// pinned on and checks the metadata-only read answers identically at every rung: the
// level, the derivation, the address, the stored source's format and dimensions, and
// the attribution. The two reads share one chain walk precisely so this cannot drift,
// and the assertion is what keeps it that way.
func TestArtProvenanceAgreesWithResolveArt(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	trackCover, albumCover, backCover := testPNG(t, 40, 40), testPNG(t, 42, 42), testPNG(t, 44, 44)
	pid1 := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", trackCover)
	pid2 := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)
	alb := albumPID(t, st)
	if err := st.SetItemArt(ctx, pid1, model.ArtRoleBack, backCover.Data, "",
		model.Attribution{Source: model.SourceEnrichment, Provider: "itunes"}, model.LockOff, false); err != nil {
		t.Fatalf("set back: %v", err)
	}

	agrees := func(what string, ref model.EntityRef, role model.ArtRole) {
		t.Helper()
		blob, berr := st.ResolveArt(ctx, ref, role, 0)
		prov, perr := st.ArtProvenance(ctx, ref, role)
		if berr != nil || perr != nil {
			t.Fatalf("%s: resolve err %v, provenance err %v", what, berr, perr)
		}
		if prov.Level != blob.Level || prov.Derived != blob.Derived || prov.SourceHash != blob.SourceHash {
			t.Errorf("%s: chain answer %+v, want %s/%v/%s", what, prov, blob.Level, blob.Derived, blob.SourceHash)
		}
		if prov.Format != blob.Format || prov.Width != blob.Width || prov.Height != blob.Height {
			t.Errorf("%s: source shape = %s %dx%d, want %s %dx%d", what,
				prov.Format, prov.Width, prov.Height, blob.Format, blob.Width, blob.Height)
		}
		if prov.Size != len(blob.Bytes) {
			t.Errorf("%s: size = %d, want %d", what, prov.Size, len(blob.Bytes))
		}
		if prov.Attribution != blob.Attribution || prov.UpdatedAt != blob.UpdatedAt {
			t.Errorf("%s: attribution = %+v/%d, want %+v/%d", what,
				prov.Attribution, prov.UpdatedAt, blob.Attribution, blob.UpdatedAt)
		}
	}

	agrees("own cover", model.EntityRef{Type: model.ArtTrack, PID: pid1}, model.ArtRoleFront)
	agrees("empty role default", model.EntityRef{Type: model.ArtTrack, PID: pid1}, "")
	agrees("non-front role", model.EntityRef{Type: model.ArtTrack, PID: pid1}, model.ArtRoleBack)
	agrees("sibling through the album", model.EntityRef{Type: model.ArtTrack, PID: pid2}, model.ArtRoleFront)
	agrees("derived album", model.EntityRef{Type: model.ArtAlbum, PID: alb}, model.ArtRoleFront)

	if err := st.SetEntityArt(ctx, model.ArtAlbum, alb, model.ArtRoleFront, albumCover.Data, "",
		model.Attribution{}, model.LockOff, false); err != nil {
		t.Fatalf("set album front: %v", err)
	}
	agrees("durable album", model.EntityRef{Type: model.ArtAlbum, PID: alb}, model.ArtRoleFront)

	// And the same refusals, so a caller can swap one read for the other.
	for _, tc := range []struct {
		what string
		ref  model.EntityRef
		role model.ArtRole
		code waxerr.Code
	}{
		{"missing role", model.EntityRef{Type: model.ArtTrack, PID: pid2}, model.ArtRoleBack, waxerr.CodeNotFound},
		{"unknown role", model.EntityRef{Type: model.ArtTrack, PID: pid1}, model.ArtRole("nope"), waxerr.CodeInvalid},
		{"unknown entity type", model.EntityRef{Type: model.ArtEntity("mixtape"), PID: pid1}, model.ArtRoleFront, waxerr.CodeInvalid},
		{"unknown pid", model.EntityRef{Type: model.ArtTrack, PID: "nope"}, model.ArtRoleFront, waxerr.CodeNotFound},
	} {
		_, berr := st.ResolveArt(ctx, tc.ref, tc.role, 0)
		_, perr := st.ArtProvenance(ctx, tc.ref, tc.role)
		if !waxerr.Is(berr, tc.code) || !waxerr.Is(perr, tc.code) {
			t.Errorf("%s: resolve = %v, provenance = %v, want %s from both", tc.what, berr, perr, tc.code)
		}
	}
}

// TestArtProvenanceReportsTheStoredSourceNotAThumbnail: the dimensions describe the
// image the store holds, which is what separates this read from a sized resolve.
func TestArtProvenanceReportsTheStoredSourceNotAThumbnail(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 400, 300))

	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	thumb, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 100)
	if err != nil || !thumb.Thumbnail || thumb.Width != 100 {
		t.Fatalf("sized resolve = %+v (err %v), want a 100px thumbnail", thumb, err)
	}
	prov, err := st.ArtProvenance(ctx, ref, model.ArtRoleFront)
	if err != nil {
		t.Fatalf("ArtProvenance: %v", err)
	}
	if prov.Width != 400 || prov.Height != 300 {
		t.Errorf("provenance dims = %dx%d, want the stored source's 400x300", prov.Width, prov.Height)
	}
}

// TestArtWriteRefusesAnUnstorableAttribution covers the gap a clear used to slip
// through: with no image there is nothing for the art_map writer to check, so the
// attribution reached the lock row unvalidated and stored a source no vocabulary
// contains. The refusal is CodeInvalid, not the CodeIO a transaction would wrap it in,
// because a caller mistake is not a disk failure.
func TestArtWriteRefusesAnUnstorableAttribution(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", testPNG(t, 40, 40))
	al := albumPID(t, st)
	img := testPNG(t, 41, 41)

	for _, tc := range []struct {
		what string
		attr model.Attribution
	}{
		{"unknown source", model.Attribution{Source: "totally-bogus"}},
		{"enrichment with no provider", model.Attribution{Source: model.SourceEnrichment}},
		{"provider on a tag source", model.Attribution{Source: model.SourceTag, Provider: "itunes"}},
		{"organize, which never makes a picture", model.Attribution{Source: model.SourceOrganize}},
	} {
		// A clear and a set are refused alike.
		if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, nil, "", tc.attr, model.LockOn, true); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("item clear with %s = %v, want CodeInvalid", tc.what, err)
		}
		if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, img.Data, "", tc.attr, model.LockOn, true); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("item set with %s = %v, want CodeInvalid", tc.what, err)
		}
		if err := st.SetEntityArt(ctx, model.ArtAlbum, al, model.ArtRoleFront, nil, "", tc.attr, model.LockOn, true); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("entity clear with %s = %v, want CodeInvalid", tc.what, err)
		}
	}
	// None of it reached either lock table.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM field_provenance WHERE field='art'"); n != 0 {
		t.Errorf("%d art provenance rows after refused writes, want 0", n)
	}
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM entity_curation WHERE field='art'"); n != 0 {
		t.Errorf("%d entity art rows after refused writes, want 0", n)
	}
	// And the cover the fixture came with is untouched.
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 0); err != nil {
		t.Errorf("front art after refused writes: %v", err)
	}
}

// TestAutomaticAttachRefusesAnUnstorableAttribution: the same-hash branch re-attributes
// an existing mapping in place without going through the role writer, so an unpaired
// cover could blank a correct provider there. Both branches run the same check.
func TestAutomaticAttachRefusesAnUnstorableAttribution(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 40, 40)
	cover.Attribution = model.Attribution{Source: model.SourceEnrichment, Provider: "coverartarchive"}
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)

	// The same bytes from a provider that forgot its name.
	same := testPNG(t, 40, 40)
	same.Attribution = model.Attribution{Source: model.SourceEnrichment}
	if err := rescanCover(ctx, st, pid, same); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("same-hash re-attribution with no provider = %v, want CodeInvalid", err)
	}
	roles, err := st.ArtRoles(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid})
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 1 || roles[0].Provider != "coverartarchive" {
		t.Errorf("front slot = %+v, want the provider it had", roles)
	}
}

// rescanCover re-attaches a cover to an existing item the way a rescan would.
func rescanCover(ctx context.Context, st *Store, pid model.PID, img *model.ArtImage) error {
	return st.writeTx(ctx, func(tx *sql.Tx) error {
		var itemID int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid=?", string(pid)).Scan(&itemID); err != nil {
			return err
		}
		_, err := attachArtTxChanged(ctx, tx, itemID, img)
		return err
	})
}
