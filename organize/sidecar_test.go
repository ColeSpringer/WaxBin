package organize

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSidecarMovesCarriesExoticCover confirms a directory cover in an exotic format
// (AVIF/HEIC), now recognized by the scanner, is moved with the album, not left
// behind in the old directory.
func TestSidecarMovesCarriesExoticCover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "cover.avif"), []byte("avifdata"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "cover.webp"), []byte("webpdata"), 0o644); err != nil {
		t.Fatal(err)
	}

	moves := SidecarMoves(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"))

	found := map[string]bool{}
	for _, m := range moves {
		found[filepath.Base(m.Src)] = true
		if filepath.Dir(m.Dst) != dstDir {
			t.Errorf("cover dst dir = %q, want %q", filepath.Dir(m.Dst), dstDir)
		}
	}
	for _, name := range []string{"cover.avif", "cover.webp"} {
		if !found[name] {
			t.Errorf("%s not carried by organize (would be stranded in the old directory)", name)
		}
	}
}

// TestSidecarMovesCarriesMixedCaseCover confirms a mixed-case cover filename (which
// the scanner matches case-insensitively) is also moved by organize, not stranded
// on a case-sensitive filesystem.
func TestSidecarMovesCarriesMixedCaseCover(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "Cover.JPG"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	moves := SidecarMoves(filepath.Join(srcDir, "track.mp3"), filepath.Join(dstDir, "01 - Track.mp3"))
	found := false
	for _, m := range moves {
		if filepath.Base(m.Src) == "Cover.JPG" {
			found = true
			if filepath.Base(m.Dst) != "Cover.JPG" {
				t.Errorf("cover renamed on move: dst=%q, want Cover.JPG (keep name)", filepath.Base(m.Dst))
			}
		}
	}
	if !found {
		t.Error("mixed-case Cover.JPG not carried by organize (stranded on a case-sensitive fs)")
	}
}

// TestSidecarMovesSkipsDirArtSameDir confirms directory art is not moved when the
// audio stays in the same directory (only same-basename companions move).
func TestSidecarMovesSkipsDirArtSameDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cover.avif"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	moves := SidecarMoves(filepath.Join(dir, "a.mp3"), filepath.Join(dir, "01 - a.mp3"))
	for _, m := range moves {
		if filepath.Base(m.Src) == "cover.avif" {
			t.Error("directory cover moved within the same directory (should stay for other tracks)")
		}
	}
}
