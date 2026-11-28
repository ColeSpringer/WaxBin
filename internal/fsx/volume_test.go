package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// TestVolume: two folders of one filesystem are on one volume, a link to a folder is on
// the volume of the folder it reaches, and a path that is not there is on none.
func TestVolume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	va, ok := Volume(a)
	if !ok || va == "" {
		t.Fatalf("Volume(%q) = %q, %v, want a volume", a, va, ok)
	}
	if vb, ok := Volume(b); !ok || vb != va {
		t.Errorf("Volume(%q) = %q, %v, want %q as for the folder beside it", b, vb, ok, va)
	}
	if v, ok := Volume(filepath.Join(dir, "gone")); ok {
		t.Errorf("a missing path is on volume %q", v)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(b, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
	if vl, ok := Volume(link); !ok || vl != va {
		t.Errorf("Volume of a link = %q, %v, want %q as for the folder it reaches", vl, ok, va)
	}
}
