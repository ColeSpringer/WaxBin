//go:build unix

package fsx

import (
	"os"
	"syscall"
	"testing"
)

// TestARenameAcrossFilesystemsIsCrossDevice: a rename onto another filesystem answers
// EXDEV, on which Move copies.
func TestARenameAcrossFilesystemsIsCrossDevice(t *testing.T) {
	t.Parallel()
	if !crossDevice(&os.LinkError{Op: "rename", Old: "/a", New: "/mnt/a", Err: syscall.EXDEV}) {
		t.Error("EXDEV is not read as a move across filesystems")
	}
}
