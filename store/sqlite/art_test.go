package sqlite

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func testPNG(t *testing.T, w, h int) *model.ArtImage {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 100, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	data := buf.Bytes()
	return &model.ArtImage{Data: data, Format: "png", Width: w, Height: h, Hash: art.Hash(data)}
}

func putWithCover(t *testing.T, st *Store, libID int64, path, essence string, cover *model.ArtImage) model.PID {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, ContentHash: "c" + essence, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T" + essence,
			SortKey: model.SortKey("T" + essence), IdentityKey: "essence:" + essence,
		},
		Track:    model.Track{Artist: "X", Album: "Al", Year: 2000},
		CoverArt: cover,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res.ItemPID
}

func TestResolveArtOriginalAndThumbnail(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 120, 80)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)

	// size 0 -> the original source image.
	orig, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, 0)
	if err != nil {
		t.Fatalf("resolve original: %v", err)
	}
	if orig.Thumbnail || orig.Width != 120 || orig.Height != 80 {
		t.Errorf("original = %+v, want 120x80 non-thumbnail", orig)
	}
	if orig.SourceHash != cover.Hash {
		t.Errorf("source hash = %s, want %s", orig.SourceHash, cover.Hash)
	}

	// size 40 -> a thumbnail scaled to fit (120x80 -> 40x27).
	thumb, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, 40)
	if err != nil {
		t.Fatalf("resolve thumb: %v", err)
	}
	if !thumb.Thumbnail || thumb.Width != 40 {
		t.Errorf("thumb = %+v, want a 40-wide thumbnail", thumb)
	}

	// A second request hits the cache; verify a thumb_cache row exists.
	var n int
	if err := st.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM thumb_cache WHERE source_hash = ? AND size = 40", cover.Hash).Scan(&n); err != nil {
		t.Fatalf("count thumbs: %v", err)
	}
	if n != 1 {
		t.Errorf("thumb_cache rows = %d, want 1 (generated thumbnail cached)", n)
	}
}

func TestResolveArtFallbackChain(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 64, 64)
	// Track 1 carries the cover. Track 2 will find it through album fallback.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	// Track 2 is in the same album but carries no cover of its own.
	pid2 := putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", nil)

	// Track 2 has no direct art, so resolution falls back to the album's art.
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid2}, 0)
	if err != nil {
		t.Fatalf("resolve fallback: %v", err)
	}
	if blob.SourceHash != cover.Hash {
		t.Errorf("fallback resolved to %s, want the album cover %s", blob.SourceHash, cover.Hash)
	}
}

func TestArtAttachedWithoutAudioChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	// First scan: no cover.
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil)
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, 0); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("expected no art initially, got %v", err)
	}
	// Rescan the SAME audio bytes, but now a directory cover image exists. The art
	// must attach even though the audio did not change.
	cover := testPNG(t, 64, 64)
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, 0)
	if err != nil {
		t.Fatalf("art should be present after the cover appeared: %v", err)
	}
	if blob.SourceHash != cover.Hash {
		t.Errorf("resolved %s, want the newly-attached cover %s", blob.SourceHash, cover.Hash)
	}
}

func albumPID(t *testing.T, st *Store) model.PID {
	t.Helper()
	var pid model.PID
	if err := st.read.QueryRowContext(context.Background(), "SELECT pid FROM album LIMIT 1").Scan(&pid); err != nil {
		t.Fatalf("album pid: %v", err)
	}
	return pid
}

func TestAlbumArtPrunedOnCoverChange(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	coverA, coverB, coverC := testPNG(t, 64, 64), testPNG(t, 65, 65), testPNG(t, 66, 66)

	// Two tracks in one album with different covers; the album maps both.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", coverA)
	putWithCover(t, st, lib.ID, "/lib/al/2.flac", "e2", coverB)
	alb := albumPID(t, st)
	got, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: alb}, 0)
	if err != nil {
		t.Fatalf("album art: %v", err)
	}
	if got.SourceHash != coverA.Hash {
		t.Fatalf("album initially resolves to %s, want the first-mapped cover A %s", got.SourceHash, coverA.Hash)
	}

	// Re-cover track 1 (A -> C). Cover A is now backed by no track, so the album
	// must stop resolving to it and A becomes reclaimable.
	putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", coverC)
	got, err = st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: alb}, 0)
	if err != nil {
		t.Fatalf("album art after change: %v", err)
	}
	if got.SourceHash == coverA.Hash {
		t.Errorf("album still resolves to the stale cover A after every track dropped it")
	}
	if got.SourceHash != coverB.Hash && got.SourceHash != coverC.Hash {
		t.Errorf("album resolved to %s, want a current track cover (B %s or C %s)", got.SourceHash, coverB.Hash, coverC.Hash)
	}
	rep, _ := st.VerifyDerived(ctx)
	if rep.OrphanArtSources == 0 {
		t.Errorf("expected cover A to be orphaned after no track references it")
	}
}

// putArt persists a track with an explicit album, content hash, and cover, for art
// tests that need to control album membership and retags.
func putArt(t *testing.T, st *Store, libID int64, path, essence, content, album string, cover *model.ArtImage) model.PID {
	t.Helper()
	in := model.PutScannedTrackInput{
		LibraryID: libID,
		File: model.File{
			Path: []byte(path), DisplayPath: path, RelPath: []byte(filepath.Base(path)),
			Kind: model.FileAudio, ContentHash: content, EssenceHash: essence, ScanState: model.ScanIndexed,
		},
		Item: model.PlayableItem{
			Kind: model.KindTrack, State: model.StatePresent, Title: "T" + essence,
			SortKey: model.SortKey("T" + essence), IdentityKey: "essence:" + essence,
		},
		Track:    model.Track{Artist: "X", Album: album, Year: 2000},
		CoverArt: cover,
	}
	res, err := st.PutScannedTrack(context.Background(), in)
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return res.ItemPID
}

func TestAlbumArtNotStaleAfterTrackDeparts(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	coverA, coverB := testPNG(t, 64, 64), testPNG(t, 65, 65)

	// Album X holds T1 (cover A, first/lowest rowid) and T2 (cover B).
	putArt(t, st, lib.ID, "/lib/x/1.flac", "e1", "c1", "X", coverA)
	putArt(t, st, lib.ID, "/lib/x/2.flac", "e2", "c2", "X", coverB)
	var xPID model.PID
	if err := st.read.QueryRowContext(ctx, "SELECT pid FROM album WHERE title = 'X'").Scan(&xPID); err != nil {
		t.Fatalf("album X pid: %v", err)
	}
	if got, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: xPID}, 0); err != nil || got.SourceHash != coverA.Hash {
		t.Fatalf("album X initially = %v (err %v), want cover A", got, err)
	}

	// Retag T1 into a different album Y (content changed, cover unchanged). Album X
	// now contains only T2, so it must stop resolving to T1's cover A.
	putArt(t, st, lib.ID, "/lib/x/1.flac", "e1", "c1b", "Y", coverA)
	got, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtAlbum, PID: xPID}, 0)
	if err != nil {
		t.Fatalf("album X after departure: %v", err)
	}
	if got.SourceHash == coverA.Hash {
		t.Errorf("album X still serves departed track's cover A (stale)")
	}
	if got.SourceHash != coverB.Hash {
		t.Errorf("album X = %s, want T2's cover B %s", got.SourceHash, coverB.Hash)
	}
}

func TestVerifyConsistentIgnoresReclaimableArt(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	coverA, coverB := testPNG(t, 64, 64), testPNG(t, 65, 65)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", coverA)
	// Swap the cover (same audio); A becomes an orphaned, reclaimable source.
	putArt(t, st, lib.ID, "/lib/al/1.flac", "e1", "ce1", "Al", coverB)
	_ = pid

	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources == 0 {
		t.Fatal("expected the swapped-out cover to be an orphaned source")
	}
	// Reclaimable garbage must NOT fail the consistency check (only real derived-data
	// corruption should).
	if !rep.Consistent() {
		t.Errorf("orphaned art made the catalog report inconsistent: %+v", rep)
	}
	if !rep.Reclaimable() {
		t.Error("orphaned art should be reported as reclaimable")
	}
}

func TestResolveArtNotFound(t *testing.T) {
	st, lib := entityFixture(t)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", nil) // no art anywhere
	_, err := st.ResolveArt(context.Background(), model.EntityRef{Type: model.ArtTrack, PID: pid}, 0)
	if !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("err = %v, want CodeNotFound when no art exists", err)
	}
}

func TestGCArtRemovesOrphans(t *testing.T) {
	st, lib := entityFixture(t)
	ctx := context.Background()
	cover := testPNG(t, 50, 50)
	pid := putWithCover(t, st, lib.ID, "/lib/al/1.flac", "e1", cover)
	// Generate a thumbnail so a thumb_cache row also exists for the source.
	if _, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, 25); err != nil {
		t.Fatalf("thumb: %v", err)
	}

	// A clean catalog has no orphans.
	rep, err := st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 0 || rep.OrphanThumbnails != 0 {
		t.Fatalf("clean catalog reports orphans: %+v", rep)
	}

	// Simulate the backing entities vanishing without art cleanup (white-box: this
	// is what a future delete primitive must not leave behind).
	if _, err := st.write.ExecContext(ctx, "DELETE FROM playable_item"); err != nil {
		t.Fatalf("delete items: %v", err)
	}
	if _, err := st.write.ExecContext(ctx, "DELETE FROM album"); err != nil {
		t.Fatalf("delete albums: %v", err)
	}

	// The source and its thumbnail are now orphaned, so verify reports them.
	rep, err = st.VerifyDerived(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.OrphanArtSources != 1 || rep.OrphanThumbnails != 1 {
		t.Errorf("orphan counts = %d sources / %d thumbs, want 1/1", rep.OrphanArtSources, rep.OrphanThumbnails)
	}

	// GCArt then reclaims them.
	sources, thumbs, err := st.GCArt(ctx)
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if sources != 1 || thumbs != 1 {
		t.Errorf("GCArt removed %d sources / %d thumbs, want 1/1", sources, thumbs)
	}
	// Only the art dimensions are GCArt's concern; the FTS/rollup drift here is an
	// artifact of the white-box entity deletion, not something art GC repairs.
	rep, _ = st.VerifyDerived(ctx)
	if rep.OrphanArtSources != 0 || rep.OrphanThumbnails != 0 {
		t.Errorf("after GC, art orphans remain: %d sources / %d thumbs", rep.OrphanArtSources, rep.OrphanThumbnails)
	}
}
