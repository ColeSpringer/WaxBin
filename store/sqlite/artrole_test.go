package sqlite

import (
	"context"
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

	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, false, false); err != nil {
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
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, nil, false, false); err != nil {
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

	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, false, false); err != nil {
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
	if err := st.SetItemArt(ctx, pid2, model.ArtRoleBack, back.Data, false, false); err != nil {
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
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRoleBack, back.Data); err != nil {
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
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRoleBack, nil); err != nil {
		t.Fatalf("clear album back: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, false, false); err != nil {
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
	if err := st.SetEntityArt(ctx, model.ArtAlbum, alb, model.ArtRoleFront, albumCover.Data); err != nil {
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

	if err := st.SetItemArt(ctx, pid, model.ArtRole("portrait"), img.Data, false, false); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("item art with unknown role = %v, want CodeInvalid", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtAlbum, albumPID(t, st), model.ArtRole("artist"), img.Data); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("entity art with unknown role = %v, want CodeInvalid (the vocabulary is closed now)", err)
	}
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRole("nope"), 0); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("resolve with unknown role = %v, want CodeInvalid", err)
	}

	// The front lock gates only the front slot: with art locked, a back set works.
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, img.Data, true, false); err != nil {
		t.Fatalf("set+lock front: %v", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, img.Data, false, false); err != nil {
		t.Errorf("back set under a front lock = %v, want allowed (the lock guards the scanned slot only)", err)
	}
	if err := st.SetItemArt(ctx, pid, model.ArtRoleFront, img.Data, false, false); !waxerr.Is(err, waxerr.CodeLocked) {
		t.Errorf("front set under lock = %v, want CodeLocked", err)
	}
}

// TestGCArtMultiRole verifies GC reclaims every role's source once the entity is
// gone, and that VerifyDerived counts live multi-role sources as reachable.
func TestGCArtMultiRole(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	front, back := testPNG(t, 40, 40), testPNG(t, 41, 41)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", front)
	if err := st.SetItemArt(ctx, pid, model.ArtRoleBack, back.Data, false, false); err != nil {
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
