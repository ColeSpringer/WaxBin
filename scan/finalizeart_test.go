package scan

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestFinalizeArtKeepsDeclaredDimensionsForAnExoticPicture: a picture nothing here
// decodes keeps the dimensions its container declared, since the describer has none to
// offer in their place.
func TestFinalizeArtKeepsDeclaredDimensionsForAnExoticPicture(t *testing.T) {
	t.Parallel()
	avif := append([]byte{0, 0, 0, 0x20}, []byte("ftypavif")...)
	img := &model.ArtImage{Data: avif, Format: "avif", Width: 300, Height: 200}
	if recognized, decoded := finalizeArt(img); !recognized || decoded {
		t.Fatalf("finalizeArt = recognized %t, decoded %t; want recognized and not decoded", recognized, decoded)
	}
	if img.Format != "avif" || img.Width != 300 || img.Height != 200 || img.Hash == "" {
		t.Errorf("finalized = %s %dx%d hash %q, want avif 300x200 with a hash", img.Format, img.Width, img.Height, img.Hash)
	}
}

// TestResolveCoverExoticWithDeclaredSizeYieldsToDirCover: an embedded AVIF the container
// sized is still a picture nothing here can draw, so a decodable cover.jpg beside the
// file wins; without one the AVIF is kept, size and all.
func TestResolveCoverExoticWithDeclaredSizeYieldsToDirCover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeJPEG(t, filepath.Join(dir, "cover.jpg"), 64, 64)
	audio := filepath.Join(dir, "song.flac")
	exotic := func() *model.ArtImage {
		return &model.ArtImage{Data: exoticAVIF(), Format: "avif", Width: 600, Height: 600}
	}
	got := resolveCover(audio, exotic(), newArtCache())
	if got == nil || got.Format != "jpeg" || got.Width != 64 {
		t.Fatalf("resolved = %+v, want the 64x64 jpeg directory cover", got)
	}
	alone := resolveCover(filepath.Join(t.TempDir(), "song.flac"), exotic(), newArtCache())
	if alone == nil || alone.Format != "avif" || alone.Width != 600 {
		t.Fatalf("with no directory cover, resolved = %+v, want the 600x600 avif kept", alone)
	}
}
