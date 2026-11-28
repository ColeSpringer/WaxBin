package fsx

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/internal/testpath"
)

// TestPoolOnAPFS: the volumes of an APFS container are devices of their own over the
// container's space. CI makes the layout WAXBIN_TEST_APFS names: a and b, two volumes of
// one container on a disk image.
func TestPoolOnAPFS(t *testing.T) {
	t.Parallel()
	const env = "WAXBIN_TEST_APFS"
	a, b := testpath.Fixture(t, env, "a"), testpath.Fixture(t, env, "b")
	va, _ := Volume(a)
	if vb, ok := Volume(b); !ok || va == vb {
		t.Errorf("Volume(%q) = %q, %v, the device of %s", b, vb, ok, a)
	}
	pa, ok := Pool(a)
	if !ok {
		t.Fatalf("Pool(%q) names no pool", a)
	}
	if pb, ok := Pool(b); !ok || pb != pa {
		t.Errorf("Pool(%q) = %q, %v; want %q as for %s", b, pb, ok, pa, a)
	}
	if pt, ok := Pool(t.TempDir()); !ok || pt == pa {
		t.Errorf("the temp folder's pool is %q, %v, that of the disk image", pt, ok)
	}
	// The mount table spells mount points with every link resolved.
	if real, err := filepath.EvalSymlinks(b); err != nil || !slices.Contains(PoolPeers(a), real) {
		t.Errorf("PoolPeers(%q) = %q, want the container's other volume %s (%v)", a, PoolPeers(a), real, err)
	}
	// The container reports its own free space, the room a volume under no quota shows
	// (statfs counts whole blocks).
	if n, ok := PoolRoom(a); !ok || n-room(t, a) > 1<<20 || room(t, a)-n > 1<<20 {
		t.Errorf("PoolRoom(%q) = %d, %v; want about the %d bytes its volume reports", a, n, ok, room(t, a))
	}
	// A write landing between the two reads is read past, a few times.
	var ra, rb int64
	for range 5 {
		var errA, errB error
		ra, errA = diskfree.Available(a)
		rb, errB = diskfree.Available(b)
		if errA != nil || errB != nil {
			t.Fatal(errA, errB)
		}
		if ra == rb {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Errorf("room under %s is %d, under %s %d; want the container's room for both", a, ra, b, rb)
}

func room(t *testing.T, dir string) int64 {
	t.Helper()
	n, err := diskfree.Available(dir)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
