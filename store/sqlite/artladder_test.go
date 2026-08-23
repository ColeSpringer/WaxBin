package sqlite_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
)

// TestSizedResolveCollapsesNearbyBoxesOntoOneRung is the waste the ladder exists to
// stop. A client sizing to a layout box asks at whatever width that box happens to
// have, and every one of those widths used to mint its own cache row. They round to
// one rung now, so a resized window costs one derivative rather than one per width.
func TestSizedResolveCollapsesNearbyBoxesOntoOneRung(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/l.flac", "ess-l", "Ladder", "L",
		stamped(t, sizedCoverPNG(t, 400, 300), model.SourceTag, "", ""))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	first, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 130)
	if err != nil {
		t.Fatalf("resolve at 130: %v", err)
	}
	for _, box := range []int{130, 175, 191, 192} {
		blob, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, box)
		if err != nil {
			t.Fatalf("resolve at %d: %v", box, err)
		}
		if blob.Box != 192 {
			t.Errorf("resolve at %d reported box %d, want the rung 192", box, blob.Box)
		}
		if blob.Width != 192 || blob.Height != 144 {
			t.Errorf("resolve at %d = %dx%d, want 192x144", box, blob.Width, blob.Height)
		}
		if !bytes.Equal(blob.Bytes, first.Bytes) {
			t.Errorf("resolve at %d returned different bytes than the first request on the same rung", box)
		}
	}

	db := roConn(t, dbPath)
	if n := scalarInt64(t, db, thumbRows); n != 1 {
		t.Errorf("thumb_cache rows = %d, want 1 for four requests on one rung", n)
	}
	if n := scalarInt64(t, db, thumbSizes); n != 192 {
		t.Errorf("cached at size %d, want the rung 192 rather than a requested width", n)
	}
}

// TestSizedResolveSeparatesDistinctRungs is the other half of the boundary: rounding
// up must not fold genuinely different sizes together, or a grid tile and a hero image
// would fight over one cache entry.
func TestSizedResolveSeparatesDistinctRungs(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/s.flac", "ess-s", "Split", "S",
		stamped(t, sizedCoverPNG(t, 400, 300), model.SourceTag, "", ""))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	// 191 rounds down the ladder to 192 and 193 climbs to 256, so these neighbours are
	// on either side of a rung boundary.
	for _, c := range []struct{ box, want int }{{191, 192}, {193, 256}} {
		box, want := c.box, c.want
		blob, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, box)
		if err != nil {
			t.Fatalf("resolve at %d: %v", box, err)
		}
		if blob.Box != want {
			t.Errorf("resolve at %d reported box %d, want %d", box, blob.Box, want)
		}
	}
	if n := scalarInt64(t, roConn(t, dbPath), thumbRows); n != 2 {
		t.Errorf("thumb_cache rows = %d, want 2, one per rung", n)
	}
}

// TestResolveAboveTheLadderIsServedAsAsked pins what a box past the top rung gets. The
// ladder bounds layout-sized traffic; a request larger than any rung is bespoke, so it is
// answered at the size asked for rather than clamped down to 2048, which would silently
// hand a caller budgeting for a full-size image a downscale it never asked for.
//
// The source is deliberately longer than the top rung, so the rung request really
// resamples. It is a long thin strip rather than a square so the fixture stays cheap:
// fitDimensions measures the longest side, which is all this turns on.
func TestResolveAboveTheLadderIsServedAsAsked(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)

	rungs := art.Rungs()
	top := rungs[len(rungs)-1]
	pid := putCoveredTrack(t, st, lib.ID, "/lib/w.flac", "ess-w", "Wide", "W",
		stamped(t, sizedCoverPNG(t, top+352, 40), model.SourceTag, "", ""))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	atTop, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, top)
	if err != nil {
		t.Fatalf("resolve at the top rung: %v", err)
	}
	if !atTop.Thumbnail || atTop.Width != top {
		t.Errorf("at the top rung = %dx%d thumbnail=%v, want a generated %d-wide image",
			atTop.Width, atTop.Height, atTop.Thumbnail, top)
	}

	// Past the ladder and above the source: served as stored, exactly as it was before
	// the ladder existed, rather than resampled down to the top rung.
	above, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, top*2)
	if err != nil {
		t.Fatalf("resolve above the ladder: %v", err)
	}
	if above.Box != top*2 {
		t.Errorf("box = %d, want the request %d passed through unrounded", above.Box, top*2)
	}
	if above.Thumbnail || above.Width != top+352 {
		t.Errorf("above the ladder = %dx%d thumbnail=%v, want the stored %d-wide source",
			above.Width, above.Height, above.Thumbnail, top+352)
	}
	if n := scalarInt64(t, roConn(t, dbPath), thumbSizes); n != int64(top) {
		t.Errorf("cached rungs = %d, want only the top rung %d", n, top)
	}
}

// TestResolveReportsRungOnEveryAnswer covers the two answers that hand back stored
// bytes rather than generated ones. A sizeless resolve asked for no rung and reports
// none; a source small enough to serve as stored still answers a rung, and the client
// needs to know which one so it can key its own cache off it. Thumbnail is what
// separates the two, not Box.
func TestResolveReportsRungOnEveryAnswer(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStoreAt(t)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/r.flac", "ess-r", "Rung", "R",
		stamped(t, sizedCoverPNG(t, 40, 40), model.SourceTag, "", ""))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	sizeless, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 0)
	if err != nil {
		t.Fatalf("sizeless resolve: %v", err)
	}
	if sizeless.Box != 0 {
		t.Errorf("sizeless resolve reported box %d, want 0", sizeless.Box)
	}

	fits, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 100)
	if err != nil {
		t.Fatalf("resolve at 100: %v", err)
	}
	if fits.Thumbnail {
		t.Error("a displayable source inside the rung was re-encoded")
	}
	if fits.Box != 128 {
		t.Errorf("box = %d, want the rung 128 even though the stored source answered", fits.Box)
	}
}
