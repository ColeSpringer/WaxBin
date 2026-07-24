package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file covers a playlist as an art entity: a one-rung chain (so every role,
// front included, resolves at the playlist's own level or not at all), the HasArt
// projection on the playlist read model, and the delete/verify/GC lifecycle of a
// playlist cover, which has no merge or orphan hook to lean on.

// newPlaylist creates an empty static playlist.
func newPlaylist(t *testing.T, st *Store, name string) model.PID {
	t.Helper()
	pl, err := st.CreatePlaylist(context.Background(), name, "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create playlist %q: %v", name, err)
	}
	return pl
}

// TestPlaylistArtResolvesOwnLevelOnly is the core of the playlist art entity: a
// member track's cover never answers for the playlist, and the playlist's own front
// and back both resolve at the playlist level with no fallback and no derivation.
func TestPlaylistArtResolvesOwnLevelOnly(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	trackCover, front, back := testPNG(t, 40, 40), testPNG(t, 42, 42), testPNG(t, 43, 43)
	// A member track carrying its own cover: the shape a fallback would leak through.
	item := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", trackCover)
	pl := newPlaylist(t, st, "Mix")
	if err := st.AddPlaylistItems(ctx, pl, []model.PID{item}); err != nil {
		t.Fatalf("add playlist items: %v", err)
	}
	ref := model.EntityRef{Type: model.ArtPlaylist, PID: pl}

	// Nothing set on the playlist itself: the member track's cover (and its album's
	// derived one) must not answer, and there is no ancestor to walk to.
	if _, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("front on a coverless playlist = %v, want CodeNotFound (no member or ancestor fallback)", err)
	}
	// An empty role list, not an error: the playlist exists, it just holds no art.
	if roles, err := st.ArtRoles(ctx, ref); err != nil || len(roles) != 0 {
		t.Fatalf("roles on a coverless playlist = %+v (err %v), want an empty list", roles, err)
	}

	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, front.Data); err != nil {
		t.Fatalf("set playlist front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleBack, back.Data); err != nil {
		t.Fatalf("set playlist back: %v", err)
	}

	fb, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("front resolve: %v", err)
	}
	if fb.SourceHash != front.Hash || fb.Level != model.ArtPlaylist || fb.Derived {
		t.Errorf("front = %+v, want the set cover at level playlist, not derived", fb)
	}
	bb, err := st.ResolveArt(ctx, ref, model.ArtRoleBack, 0)
	if err != nil {
		t.Fatalf("back resolve: %v", err)
	}
	if bb.SourceHash != back.Hash || bb.Level != model.ArtPlaylist {
		t.Errorf("back = %+v, want the set back image at level playlist", bb)
	}

	roles, err := st.ArtRoles(ctx, ref)
	if err != nil {
		t.Fatalf("roles: %v", err)
	}
	if len(roles) != 2 || roles[0].Role != model.ArtRoleBack || roles[1].Role != model.ArtRoleFront {
		t.Fatalf("roles = %+v, want [back front] at the playlist's own level", roles)
	}
	if roles[1].SourceHash != front.Hash || roles[1].Width != 42 {
		t.Errorf("front listing = %+v, want the source's hash and dimensions", roles[1])
	}

	// Clearing the front leaves the back alone, the same per-role independence every
	// other entity has.
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, nil); err != nil {
		t.Fatalf("clear playlist front: %v", err)
	}
	if _, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("cleared front = %v, want CodeNotFound", err)
	}
	if _, err := st.ResolveArt(ctx, ref, model.ArtRoleBack, 0); err != nil {
		t.Errorf("back after clearing front: %v", err)
	}

	// An unknown playlist is a not-found, not a silent empty answer.
	missing := model.EntityRef{Type: model.ArtPlaylist, PID: "nosuch"}
	if _, err := st.ResolveArt(ctx, missing, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("resolve on an unknown playlist = %v, want CodeNotFound", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, "nosuch", model.ArtRoleFront, front.Data); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("set art on an unknown playlist = %v, want CodeNotFound", err)
	}
}

// playlistHasArt reads one playlist's HasArt flag, failing the test on a read error
// rather than dereferencing the nil playlist it comes back with.
func playlistHasArt(t *testing.T, st *Store, pid model.PID) bool {
	t.Helper()
	p, err := st.PlaylistByPID(context.Background(), pid)
	if err != nil {
		t.Fatalf("read playlist %s: %v", pid, err)
	}
	return p.HasArt
}

// TestPlaylistHasArt checks the read-model projection: it follows a set and cleared
// cover on both the single and list reads, and counts only the front role, so an
// auxiliary image does not advertise a cover the playlist does not have.
func TestPlaylistHasArt(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	front, back := testPNG(t, 42, 42), testPNG(t, 43, 43)
	pl := newPlaylist(t, st, "Mix")

	if playlistHasArt(t, st, pl) {
		t.Errorf("HasArt = true on a new playlist, want false")
	}

	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, front.Data); err != nil {
		t.Fatalf("set front: %v", err)
	}
	if !playlistHasArt(t, st, pl) {
		t.Errorf("HasArt = false after setting a front cover, want true")
	}
	pls, err := st.ListPlaylists(ctx, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pls) != 1 || !pls[0].HasArt {
		t.Errorf("list HasArt = %+v, want the cover reported on the list read too", pls)
	}

	// A back image alone is not a cover: the projection is front-scoped.
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, nil); err != nil {
		t.Fatalf("clear front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleBack, back.Data); err != nil {
		t.Fatalf("set back: %v", err)
	}
	if playlistHasArt(t, st, pl) {
		t.Errorf("HasArt = true with only a back image, want false (front role only)")
	}
}

// TestPlaylistArtNotInheritedThroughReusedRowid pins why DeletePlaylist drops the art
// rows itself instead of leaving them to GCArt. playlist.id is a plain INTEGER PRIMARY
// KEY, so the next playlist created takes the deleted one's rowid; a surviving map row
// would hand it the dead playlist's cover, and GCArt would never reclaim the row
// because the id is live again.
func TestPlaylistArtNotInheritedThroughReusedRowid(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	front := testPNG(t, 42, 42)
	first := newPlaylist(t, st, "Mix")
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, first, model.ArtRoleFront, front.Data); err != nil {
		t.Fatalf("set front: %v", err)
	}
	firstID := scalarInt(t, st, "SELECT id FROM playlist WHERE pid = ?", string(first))
	if err := st.DeletePlaylist(ctx, first); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}

	second, err := st.CreatePlaylist(ctx, "Next", "", model.PlaylistStatic, "", nil)
	if err != nil {
		t.Fatalf("create second playlist: %v", err)
	}
	// The premise of the test: SQLite really did reuse the rowid.
	if got := scalarInt(t, st, "SELECT id FROM playlist WHERE pid = ?", string(second)); got != firstID {
		t.Fatalf("second playlist got id %d, want the deleted playlist's %d reused", got, firstID)
	}
	if playlistHasArt(t, st, second) {
		t.Errorf("a fresh playlist inherited the deleted playlist's cover through the reused rowid")
	}
	ref := model.EntityRef{Type: model.ArtPlaylist, PID: second}
	if _, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("resolve on the fresh playlist = %v, want CodeNotFound", err)
	}
}

// TestPlaylistArtDeleteAndGC pins the lifecycle the polymorphic no-FK art map rests
// on. A playlist is never merged or orphan-GC'd, so DeletePlaylist is the only place
// its art rows are cleaned: while the playlist lives its cover counts as live art on
// both sides of the verify <-> GC lockstep, and once it is deleted both flip together
// and the sources and their thumbnails are reclaimed. Two roles are set because the
// delete is role-agnostic, like the rest of the art GC.
func TestPlaylistArtDeleteAndGC(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	trackCover, front, back := testPNG(t, 40, 40), testPNG(t, 42, 42), testPNG(t, 43, 43)
	// The track's cover is an unrelated live source: GC must leave it alone.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", trackCover)
	pl := newPlaylist(t, st, "Mix")
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleFront, front.Data); err != nil {
		t.Fatalf("set playlist front: %v", err)
	}
	if err := st.SetEntityArt(ctx, model.ArtPlaylist, pl, model.ArtRoleBack, back.Data); err != nil {
		t.Fatalf("set playlist back: %v", err)
	}
	// Resolve a thumbnail so the delete has a cached derivative to cascade.
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtPlaylist, PID: pl}, model.ArtRoleFront, 16); err != nil {
		t.Fatalf("thumbnail resolve: %v", err)
	}

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 0 || rep.OrphanThumbnails != 0 {
		t.Errorf("live playlist cover reported reclaimable: %+v; the live-art arms must cover the playlist slot", rep)
	}
	if sources, thumbs, err := st.GCArt(ctx); err != nil || sources != 0 || thumbs != 0 {
		t.Errorf("GCArt reclaimed %d sources / %d thumbnails (err %v), want 0 for a live playlist cover", sources, thumbs, err)
	}

	if err := st.DeletePlaylist(ctx, pl); err != nil {
		t.Fatalf("delete playlist: %v", err)
	}
	// Every role's map row goes with the playlist, so nothing points at either source.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_map WHERE entity_type = 'playlist'"); n != 0 {
		t.Errorf("%d playlist art_map rows survived the delete, want 0", n)
	}
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify after delete: %v", err)
	}
	if rep.OrphanArtSources != 2 || rep.OrphanThumbnails != 1 {
		t.Errorf("after delete = %+v, want both playlist sources and the thumbnail reported reclaimable", rep)
	}
	sources, thumbs, err := st.GCArt(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if sources != 2 || thumbs != 1 {
		t.Errorf("GCArt reclaimed %d sources / %d thumbnails, want both playlist covers and the thumbnail", sources, thumbs)
	}
	// The track's own cover was never in play; GC left it alone and verify is clean.
	if n := scalarInt(t, st, "SELECT COUNT(*) FROM art_source WHERE hash = ?", trackCover.Hash); n != 1 {
		t.Errorf("the member track's cover was reclaimed too (%d rows), want it kept", n)
	}
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify after GC: %v", err)
	}
	if rep.OrphanArtSources != 0 || rep.OrphanThumbnails != 0 {
		t.Errorf("verify after GC = %+v, want clean", rep)
	}
}
