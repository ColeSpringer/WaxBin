package sqlite_test

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/store/sqlite"
)

const (
	thumbRows = "SELECT COUNT(*) FROM thumb_cache"
	// One row's rung, for the tests that seed exactly one.
	thumbSizes = "SELECT size FROM thumb_cache"
)

// TestSizedResolveReencodesUndisplayableSource is the gap WaxDeck reported. A TIFF
// cover thumbnailed correctly into a small grid tile and came back as raw TIFF at a
// rung above its own size, so one picture drew in one place and not the other.
func TestSizedResolveReencodesUndisplayableSource(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	raw := sizedCover(t, "tiff", 60, 40)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/t.flac", "ess-t", "Tiff", "T",
		stamped(t, raw, model.SourceTag, "", ""))

	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 200)
	if err != nil {
		t.Fatalf("resolve at 200: %v", err)
	}
	if !blob.Thumbnail {
		t.Error("a TIFF source above the box came back as stored; a sized resolve must hand back drawable bytes")
	}
	if blob.Format != "png" {
		t.Errorf("format = %q, want png", blob.Format)
	}
	if blob.Width != 60 || blob.Height != 40 {
		t.Errorf("dimensions = %dx%d, want 60x40 (re-encoded at its own size, never upscaled)", blob.Width, blob.Height)
	}
	if bytes.Equal(blob.Bytes, raw) {
		t.Error("the bytes are the stored TIFF unchanged")
	}

	db := roConn(t, dbPath)
	if n := scalarInt64(t, db, thumbRows); n != 1 {
		t.Errorf("thumb_cache rows = %d, want 1", n)
	}
	if n := scalarInt64(t, db, thumbSizes); n != 256 {
		t.Errorf("cached at size %d, want the rung 200 rounds up to (256)", n)
	}
}

// TestSizedResolveKeepsShortCircuitForDisplayable pins the boundary the change turns
// on rather than one point of it. A displayable source inside the box is still handed
// back as stored, decoding nothing and caching nothing. Every member the test package
// can encode is here: jpeg because it is the common case and the only one whose
// generated output is jpeg rather than png, and bmp because it is the member most
// likely to be argued out of the floor later. WebP is absent because x/image decodes it
// and does not encode it.
func TestSizedResolveKeepsShortCircuitForDisplayable(t *testing.T) {
	ctx := context.Background()
	for _, format := range []string{"jpeg", "png", "gif", "bmp"} {
		t.Run(format, func(t *testing.T) {
			st, dbPath, lib := openStoreAt(t)
			raw := sizedCover(t, format, 40, 40)

			pid := putCoveredTrack(t, st, lib.ID, "/lib/d.flac", "ess-d", "Displayable", "D",
				stamped(t, raw, model.SourceTag, "", ""))

			blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 100)
			if err != nil {
				t.Fatalf("resolve at 100: %v", err)
			}
			if blob.Thumbnail {
				t.Error("a displayable source inside the box was re-encoded; it should be served as stored")
			}
			if blob.Format != format {
				t.Errorf("format = %q, want %q", blob.Format, format)
			}
			if blob.Width != 40 || blob.Height != 40 {
				t.Errorf("dimensions = %dx%d, want 40x40", blob.Width, blob.Height)
			}
			if !bytes.Equal(blob.Bytes, raw) {
				t.Error("the stored bytes were not handed back verbatim")
			}
			if n := scalarInt64(t, roConn(t, dbPath), thumbRows); n != 0 {
				t.Errorf("thumb_cache rows = %d, want 0 (nothing was generated)", n)
			}
		})
	}
}

// TestSizedResolveRungsAboveSourceAgree pins that every rung at or above a source's own
// size answers with the same picture, since Thumbnail never upscales. They are cached
// per rung rather than collapsed onto one entry: see the box passed to thumbnail for why
// the stored dimensions are not trusted to do that collapsing.
func TestSizedResolveRungsAboveSourceAgree(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStoreAt(t)
	raw := sizedCover(t, "tiff", 60, 40)

	pid := putCoveredTrack(t, st, lib.ID, "/lib/c.flac", "ess-c", "Rungs", "C",
		stamped(t, raw, model.SourceTag, "", ""))
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	low, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 200)
	if err != nil {
		t.Fatalf("resolve at 200: %v", err)
	}
	high, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 4000)
	if err != nil {
		t.Fatalf("resolve at 4000: %v", err)
	}
	if !bytes.Equal(low.Bytes, high.Bytes) || low.Format != high.Format ||
		low.Width != high.Width || low.Height != high.Height {
		t.Errorf("rungs above the source disagree: %s %dx%d vs %s %dx%d",
			low.Format, low.Width, low.Height, high.Format, high.Width, high.Height)
	}
	if low.Width != 60 || low.Height != 40 {
		t.Errorf("dimensions = %dx%d, want the source's own 60x40 (never upscaled)", low.Width, low.Height)
	}
}

// TestSizedResolveIgnoresUnderstatedStoredDimensions is why the rung goes through as
// it is rather than clamped to the stored dimensions. storableArt keeps a producer's
// own values and fills only what it left zero, so a row can name a size smaller than
// its bytes really are. Clamping to that figure would answer a large request with a
// small picture and cache it under the small rung, where nothing would ever correct it.
func TestSizedResolveIgnoresUnderstatedStoredDimensions(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	raw := sizedCover(t, "tiff", 800, 800)

	// All four fields filled, so storableArt hands the carrier back without deriving the
	// real dimensions from the bytes.
	pid := putCoveredTrack(t, st, lib.ID, "/lib/u.flac", "ess-u", "Understated", "U",
		&model.ArtImage{
			Data: raw, Hash: "understated", Format: "tiff", Width: 100, Height: 100,
			Attribution: model.Attribution{Source: model.SourceTag},
		})

	db := roConn(t, dbPath)
	if _, _, w, h, _ := storedArt(t, db, "track", itemRowID(t, db, pid)); w != 100 || h != 100 {
		t.Fatalf("stored dimensions = %dx%d, want the understated 100x100 this test is about", w, h)
	}

	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 512)
	if err != nil {
		t.Fatalf("resolve at 512: %v", err)
	}
	if blob.Width != 512 || blob.Height != 512 {
		t.Errorf("dimensions = %dx%d, want 512x512; the rung was clamped to the row's understated size",
			blob.Width, blob.Height)
	}
	if !blob.Thumbnail || blob.Format != "png" {
		t.Errorf("got %s thumbnail=%v, want a generated png", blob.Format, blob.Thumbnail)
	}
}

// TestSizedResolveFallsBackWhenGenerationFails covers a row newly reachable by the
// decoder: an undisplayable format with real dimensions over bytes that do not decode,
// which before this change took the short circuit above its own size and was never
// handed to a generator.
func TestSizedResolveFallsBackWhenGenerationFails(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	junk := []byte("II*\x00 this is not a tiff, only the first bytes of one")

	// All four fields filled, so storableArt hands the carrier straight back rather than
	// deriving 0x0 from bytes no decoder can read.
	pid := putCoveredTrack(t, st, lib.ID, "/lib/j.flac", "ess-j", "Junk", "J",
		&model.ArtImage{
			Data: junk, Hash: "deadbeef", Format: "tiff", Width: 60, Height: 40,
			Attribution: model.Attribution{Source: model.SourceTag},
		})

	// The row has to carry real dimensions, or the resolve exits one branch earlier and
	// the generator is never asked.
	db := roConn(t, dbPath)
	if _, format, w, h, _ := storedArt(t, db, "track", itemRowID(t, db, pid)); format != "tiff" || w != 60 || h != 40 {
		t.Fatalf("stored art = %s %dx%d, want tiff 60x40", format, w, h)
	}

	blob, err := st.ResolveArt(ctx, model.EntityRef{Type: model.ArtTrack, PID: pid}, model.ArtRoleFront, 200)
	if err != nil {
		t.Fatalf("resolve at 200: %v", err)
	}
	if blob.Thumbnail {
		t.Error("a source that cannot be decoded must come back as stored, not marked generated")
	}
	if blob.Width != 60 || blob.Height != 40 {
		t.Errorf("dimensions = %dx%d, want the stored 60x40", blob.Width, blob.Height)
	}
	if blob.Format != "tiff" {
		t.Errorf("format = %q, want tiff", blob.Format)
	}
	if !bytes.Equal(blob.Bytes, junk) {
		t.Error("the stored bytes were not handed back verbatim")
	}
	if n := scalarInt64(t, db, thumbRows); n != 0 {
		t.Errorf("thumb_cache rows = %d, want 0 (nothing was generated)", n)
	}
}

// TestSizedResolveOnReadOnlyStore proves the fix does not depend on being able to
// persist: a read-only library re-encodes and serves from the in-process cache, and
// writes nothing.
func TestSizedResolveOnReadOnlyStore(t *testing.T) {
	ctx := context.Background()
	st, dbPath, lib := openStoreAt(t)
	raw := sizedCover(t, "tiff", 60, 40)
	pid := putCoveredTrack(t, st, lib.ID, "/lib/r.flac", "ess-r", "ReadOnly", "R",
		stamped(t, raw, model.SourceTag, "", ""))
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: dbPath, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}
	for i := range 2 {
		blob, err := ro.ResolveArt(ctx, ref, model.ArtRoleFront, 200)
		if err != nil {
			t.Fatalf("read-only resolve %d: %v", i, err)
		}
		if !blob.Thumbnail || blob.Format != "png" || blob.Width != 60 || blob.Height != 40 {
			t.Errorf("read-only resolve %d = %s %dx%d thumbnail=%v, want png 60x40 thumbnail=true",
				i, blob.Format, blob.Width, blob.Height, blob.Thumbnail)
		}
	}
	if n := scalarInt64(t, roConn(t, dbPath), thumbRows); n != 0 {
		t.Errorf("thumb_cache rows = %d, want 0; a read-only store must not persist", n)
	}
}

// countingHandler counts records at Warn or above, so a test can assert that a repeated
// resolve did not repeat the work behind the warning.
type countingHandler struct {
	mu sync.Mutex
	n  int
}

func (h *countingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (h *countingHandler) Handle(_ context.Context, _ slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// TestSizedResolveRemembersGenerationFailure is the negative cache seen from the outside.
// A cover with declared dimensions whose bytes no decoder here reads is handed to the
// generator at every rung now that it no longer short-circuits above its own size, so
// without the record a grid scroll re-attempts the same decode and re-emits the same
// warning once per request.
func TestSizedResolveRemembersGenerationFailure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	h := &countingHandler{}
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: dbPath, Owner: "test", Logger: slog.New(h)})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		t.Fatalf("ensure library: %v", err)
	}

	junk := []byte("II*\x00 this is not a tiff, only the first bytes of one")
	pid := putCoveredTrack(t, st, lib.ID, "/lib/j.flac", "ess-j", "Junk", "J",
		&model.ArtImage{
			Data: junk, Hash: "deadbeef", Format: "tiff", Width: 60, Height: 40,
			Attribution: model.Attribution{Source: model.SourceTag},
		})
	ref := model.EntityRef{Type: model.ArtTrack, PID: pid}

	for i := range 3 {
		blob, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 200)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if !bytes.Equal(blob.Bytes, junk) || blob.Thumbnail {
			t.Errorf("resolve %d did not serve the original", i)
		}
	}
	if n := h.count(); n != 1 {
		t.Errorf("generation was attempted %d times across three resolves, want 1; "+
			"the failure is meant to be remembered", n)
	}

	// The record is per source, not per box: art.Thumbnail fails inside image.Decode,
	// before it reads the box, so another rung asking about the same bytes is asking a
	// question already answered.
	if _, err := st.ResolveArt(ctx, ref, model.ArtRoleFront, 300); err != nil {
		t.Fatalf("resolve at another rung: %v", err)
	}
	if n := h.count(); n != 1 {
		t.Errorf("warnings = %d after a second rung, want 1; the failure is a property "+
			"of the bytes, so every rung shares one record", n)
	}
}
