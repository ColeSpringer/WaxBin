//go:build windows

package fsx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// TestSpellerRenamesByCase: on a case-insensitive filesystem a move onto the same path in
// another case renames the file, and through a Speller the folder above it, to the new
// spelling; a new file moved into a folder that exists under another spelling gives the
// folder the new one, its other files moving with it.
func TestSpellerRenamesByCase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "author", "A.mp3"), "audio")
	var renamed [][2]string
	sp := NewSpeller(dir, func(from, to string) error {
		renamed = append(renamed, [2]string{from, to})
		return nil
	})
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

// TestSpelledAfterACaseOnlyRename: on a case-insensitive filesystem a path resolves under
// both spellings before and after a case-only rename, and the folders' listings tell the
// two apart.
func TestSpelledAfterACaseOnlyRename(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "author", "a.mp3"), filepath.Join(dir, "Author", "A.mp3")
	writeFile(t, src, "audio")
	if !NewLister().Spelled(dir, src) || NewLister().Spelled(dir, dst) {
		t.Error("before the rename, want the source spelled as listed and the destination not")
	}
	if err := os.Rename(filepath.Join(dir, "author"), filepath.Join(dir, "Author")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "Author", "a.mp3"), dst); err != nil {
		t.Fatal(err)
	}
	if NewLister().Spelled(dir, src) || !NewLister().Spelled(dir, dst) {
		t.Error("after the rename, want the destination spelled as listed and the source not")
	}
}

// TestARenameAcrossVolumesIsCrossDevice: Windows refuses a rename onto another volume with
// ERROR_NOT_SAME_DEVICE, never EXDEV, so Move copies on that answer.
func TestARenameAcrossVolumesIsCrossDevice(t *testing.T) {
	t.Parallel()
	if !crossDevice(&os.LinkError{Op: "rename", Old: `C:\a`, New: `D:\a`, Err: windows.ERROR_NOT_SAME_DEVICE}) {
		t.Error("ERROR_NOT_SAME_DEVICE is not read as a move across filesystems")
	}
}

// TestVolumeTellsDrivesApart: a local volume is named by its GUID, and the folder the
// tests run in is on another volume than the temp folder wherever the two are on two
// drives, as they are on the CI runner.
func TestVolumeTellsDrivesApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	here, ok := Volume(dir)
	if !ok || !strings.HasPrefix(here, `\\?\Volume{`) || !strings.HasSuffix(here, "}") {
		t.Fatalf("Volume(%q) = %q, %v, want the volume's GUID name", dir, here, ok)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.EqualFold(filepath.VolumeName(wd), filepath.VolumeName(dir)) {
		t.Skip("the working folder and the temp folder are on one drive")
	}
	if there, ok := Volume(wd); !ok || there == here {
		t.Errorf("Volume(%q) = %q, %v, the same as the temp folder's on another drive", wd, there, ok)
	}
}

// TestVolumeNamesAShareByItsServer: a folder reached through a share has no volume GUID,
// and is named by the server beside the serial number, so two servers reporting one
// serial are two volumes. The runner's own administrative share stands in for a server,
// where it answers.
func TestVolumeNamesAShareByItsServer(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	drive := filepath.VolumeName(dir)
	if len(drive) != 2 || drive[1] != ':' {
		t.Skipf("the temp folder is not on a lettered drive: %s", dir)
	}
	shared := `\\localhost\` + drive[:1] + `$` + dir[2:]
	if _, err := os.Stat(shared); err != nil {
		t.Skipf("no administrative share here: %v", err)
	}
	sub := filepath.Join(shared, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	id, ok := Volume(shared)
	if !ok {
		t.Fatalf("Volume(%q) names no volume", shared)
	}
	if !strings.HasPrefix(id, `\\?\Volume{`) && !strings.HasPrefix(id, `\\localhost#`) {
		t.Errorf("Volume(%q) = %q, want it to name the server", shared, id)
	}
	if below, ok := Volume(sub); !ok || below != id {
		t.Errorf("Volume(%q) = %q, %v, want %q as for the folder above it", sub, below, ok, id)
	}
}
