//go:build windows

package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSpellerRenamesByCase: on a case-insensitive filesystem a move onto the same path in
// another case renames the file, and through a Speller the folder above it, to the new
// spelling; a new file moved into a folder that exists under another spelling gives the
// folder the new one, its other files moving with it.
func TestSpellerRenamesByCase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "author", "A.mp3"), "audio")
	var renamed [][2]string
	sp := NewSpeller(dir, func(from, to string) { renamed = append(renamed, [2]string{from, to}) })
	if err := sp.Move(filepath.Join(dir, "author", "A.mp3"), filepath.Join(dir, "Author", "a.mp3")); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{"Author"}) {
		t.Errorf("folders = %v, want only Author", got)
	}
	if got := names(t, filepath.Join(dir, "Author")); !slices.Equal(got, []string{"a.mp3"}) {
		t.Errorf("files = %v, want only a.mp3", got)
	}

	writeFile(t, filepath.Join(dir, "tolkien", "old.mp3"), "old")
	staged := filepath.Join(t.TempDir(), "new.mp3")
	writeFile(t, staged, "new")
	if err := sp.Move(staged, filepath.Join(dir, "Tolkien", "new.mp3")); err != nil {
		t.Fatalf("Move into a folder spelled otherwise: %v", err)
	}
	if got := names(t, dir); !slices.Contains(got, "Tolkien") || slices.Contains(got, "tolkien") {
		t.Errorf("folders = %v, want Tolkien respelled", got)
	}
	if got := names(t, filepath.Join(dir, "Tolkien")); !slices.Equal(got, []string{"new.mp3", "old.mp3"}) {
		t.Errorf("Tolkien holds %v, want both files", got)
	}
	want := [][2]string{{filepath.Join(dir, "author"), filepath.Join(dir, "Author")}, {filepath.Join(dir, "tolkien"), filepath.Join(dir, "Tolkien")}}
	if !slices.Equal(renamed, want) {
		t.Errorf("renamed = %q, want %q", renamed, want)
	}
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
