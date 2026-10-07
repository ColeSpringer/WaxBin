//go:build windows

package scan

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"golang.org/x/sys/windows"
)

// TestScanRefusesTheTrashSpelledAsWindowsResolvesIt: Windows reaches a folder through
// its short name and through its name with a trailing dot, neither of which names the
// trash as typed, so the sub-path is judged by the spelling the disk resolves it to.
func TestScanRefusesTheTrashSpelledAsWindowsResolvesIt(t *testing.T) {
	t.Parallel()
	_, lib, sc, _, root := fastPathFixture(t)
	writeMP3(t, filepath.Join(root, model.TrashDirName, "x", "1.mp3"), "Trashed", 1)
	long, err := windows.UTF16PtrFromString(filepath.Join(root, model.TrashDirName))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	short := windows.UTF16ToString(buf[:n]) // the long name itself where short names are off
	for _, sub := range []string{
		filepath.Join(short, "x"),
		filepath.Join(root, model.TrashDirName+".", "x"),
	} {
		_, err := sc.Scan(context.Background(), Request{Library: lib, SubPath: sub}, nil)
		if !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("scan of %s: %v, want CodeInvalid", sub, err)
		}
	}
}
