package fsx

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/internal/testpath"
	"golang.org/x/sys/unix"
)

// TestPoolIsTheDeviceWhereNothingIsShared: a filesystem that shares no pool draws on
// its own device, so Pool reads its mount and answers what Volume does.
func TestPoolIsTheDeviceWhereNothingIsShared(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatal(err)
	}
	const btrfsMagic, zfsMagic = 0x9123683e, 0x2fc12fc1
	if m := uint32(st.Type); m == btrfsMagic || m == zfsMagic {
		t.Skip("the temp folder is on a filesystem that shares a pool")
	}
	v, _ := Volume(dir)
	if p, ok := Pool(dir); !ok || p != v {
		t.Errorf("Pool(%q) = %q, %v, want the device %q", dir, p, ok, v)
	}
	if peers := PoolPeers(dir); peers != nil {
		t.Errorf("PoolPeers(%q) = %q, want none where nothing is shared", dir, peers)
	}
	if n, ok := PoolRoom(dir); ok {
		t.Errorf("PoolRoom(%q) = %d, want no pool to ask where nothing is shared", dir, n)
	}
	// Pool would answer Volume through its fallback too, so the mount line is read here
	// on its own.
	var stx unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, dir, 0, unix.STATX_MNT_ID, &stx); err != nil || stx.Mask&unix.STATX_MNT_ID == 0 {
		t.Skipf("no mount id from statx here (%v)", err)
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := mountPool(info, stx.Mnt_id); !ok || p != v {
		t.Errorf("the mount line of %s names %q, %v; want the device %q", dir, p, ok, v)
	}
}

// TestPoolOnBtrfs: subvolumes are devices of their own over one filesystem's space, and
// one mounted on its own draws on that space as well. CI makes the layout
// WAXBIN_TEST_BTRFS names: top (the top level, holding subvolumes a and b) and b (the
// subvolume b mounted again by itself).
func TestPoolOnBtrfs(t *testing.T) {
	t.Parallel()
	const env = "WAXBIN_TEST_BTRFS"
	a, b, alone := testpath.Fixture(t, env, "top", "a"), testpath.Fixture(t, env, "top", "b"), testpath.Fixture(t, env, "b")
	devicesDiffer(t, a, b)
	devicesDiffer(t, a, alone)
	poolsMatch(t, a, b, alone)
	poolsDiffer(t, a, t.TempDir())
	roomsMatch(t, a, alone)
}

// TestPoolOnZFS: datasets are devices of their own over their pool's space, and the pool
// reports that space itself, whether or not its top dataset is mounted. CI makes the
// layout WAXBIN_TEST_ZFS names: waxbina (a pool) with datasets a, b, and held and held2
// (each under a quota); waxbinb (a second pool); and waxbinc, whose top dataset is not
// mounted, with x and y under like quotas.
func TestPoolOnZFS(t *testing.T) {
	t.Parallel()
	const env = "WAXBIN_TEST_ZFS"
	a, b := testpath.Fixture(t, env, "waxbina", "a"), testpath.Fixture(t, env, "waxbina", "b")
	held, other := testpath.Fixture(t, env, "waxbina", "held"), testpath.Fixture(t, env, "waxbinb")
	devicesDiffer(t, a, b)
	devicesDiffer(t, a, held)
	poolsMatch(t, a, b, held)
	poolsDiffer(t, a, other)
	roomsMatch(t, a, b)
	if room(t, held) >= room(t, a) {
		t.Errorf("the dataset under a quota reports %d bytes of room, the pool %d", room(t, held), room(t, a))
	}
	top := testpath.Fixture(t, env, "waxbina")
	if peers := PoolPeers(held); !slices.Contains(peers, top) || slices.Contains(peers, other) {
		t.Errorf("PoolPeers(%q) = %q, want the pool's top dataset %s and nothing of another pool", held, peers, top)
	}
	// statfs counts whole blocks, so a dataset under no quota reports the pool's room to
	// within one.
	reports := func() bool {
		n, ok := PoolRoom(held)
		return ok && n-room(t, a) < 1<<20 && room(t, a)-n < 1<<20
	}
	if !eventually(reports) {
		n, ok := PoolRoom(held)
		t.Errorf("PoolRoom(%q) = %d, %v; want about the %d bytes a dataset under no quota reports", held, n, ok, room(t, a))
	}
	x := testpath.Fixture(t, env, "waxbinc", "x")
	if n, ok := PoolRoom(x); !ok || n <= room(t, x) {
		t.Errorf("PoolRoom(%q) = %d, %v; want more than its quota leaves, %d, though the pool's top is not mounted", x, n, ok, room(t, x))
	}
}

// eventually reports whether check holds within a few tries, a write landing between two
// reads of a pool read past.
func eventually(check func() bool) bool {
	for range 5 {
		if check() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return false
}

func devicesDiffer(t *testing.T, a, b string) {
	t.Helper()
	va, oka := Volume(a)
	vb, okb := Volume(b)
	if !oka || !okb || va == vb {
		t.Errorf("Volume(%q) = %q, %v and Volume(%q) = %q, %v; want two devices", a, va, oka, b, vb, okb)
	}
}

func poolsMatch(t *testing.T, first string, rest ...string) {
	t.Helper()
	want, ok := Pool(first)
	if !ok {
		t.Fatalf("Pool(%q) names no pool", first)
	}
	for _, p := range rest {
		if got, ok := Pool(p); !ok || got != want {
			t.Errorf("Pool(%q) = %q, %v; want %q as for %s", p, got, ok, want, first)
		}
	}
}

func poolsDiffer(t *testing.T, a, b string) {
	t.Helper()
	pa, _ := Pool(a)
	if pb, ok := Pool(b); !ok || pb == pa {
		t.Errorf("Pool(%q) = %q, %v, the pool of %s", b, pb, ok, a)
	}
}

// roomsMatch: two members of an idle pool, neither under a limit of its own, report the
// pool's room alike. A write landing between the two reads (a transaction group syncing)
// is read past, a few times.
func roomsMatch(t *testing.T, a, b string) {
	t.Helper()
	if !eventually(func() bool { return room(t, a) == room(t, b) }) {
		t.Errorf("room under %s is %d, under %s %d; want the pool's room for both", a, room(t, a), b, room(t, b))
	}
}

func room(t *testing.T, dir string) int64 {
	t.Helper()
	n, err := diskfree.Available(dir)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
