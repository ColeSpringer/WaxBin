//go:build unix

package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVolumeTellsFilesystemsApart: where the kernel refuses a rename between two folders
// as crossing filesystems, they are on two volumes. /dev/shm is one wherever it is
// mounted apart from the temp folder.
func TestVolumeTellsFilesystemsApart(t *testing.T) {
	t.Parallel()
	const other = "/dev/shm"
	dir := t.TempDir()
	src := filepath.Join(dir, "probe")
	if err := os.WriteFile(src, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(other, "waxbin-volume-test-"+filepath.Base(dir))
	err := os.Rename(src, dst)
	if err == nil {
		os.Remove(dst)
	}
	if !crossDevice(err) {
		t.Skipf("%s is not another filesystem here: %v", other, err)
	}
	here, _ := Volume(dir)
	there, ok := Volume(other)
	if !ok || here == there {
		t.Errorf("Volume(%q) = %q, %v, the same as the temp folder's %q", other, there, ok, here)
	}
}
