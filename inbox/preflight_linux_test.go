package inbox

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/internal/testpath"
	"github.com/colespringer/waxbin/model"
)

// TestPreflightPlanOnBtrfs: roots on two subvolumes of one filesystem fill one pool, so
// two imports that each fit it are refused together. The layout is the one fsx's
// TestPoolOnBtrfs describes.
func TestPreflightPlanOnBtrfs(t *testing.T) {
	t.Parallel()
	const env = "WAXBIN_TEST_BTRFS"
	rootsShareAPool(t, testpath.Fixture(t, env, "top", "a"), testpath.Fixture(t, env, "b"))
}

// TestPreflightPlanOnZFS: roots on two datasets of one pool fill it together, a dataset
// under a quota refuses what its pool has room for and the quota does not, and two under
// quotas take between them more than either quota, which the pool's top dataset shows the
// pool has room for. The layout is the one fsx's TestPoolOnZFS describes.
func TestPreflightPlanOnZFS(t *testing.T) {
	t.Parallel()
	const env = "WAXBIN_TEST_ZFS"
	a, b := testpath.Fixture(t, env, "waxbina", "a"), testpath.Fixture(t, env, "waxbina", "b")
	held := testpath.Fixture(t, env, "waxbina", "held")
	rootsShareAPool(t, a, b)

	quota, pool := room(t, held), room(t, a)
	if quota >= pool {
		t.Fatalf("the dataset under a quota has %d bytes of room, the pool %d", quota, pool)
	}
	lib := &model.Library{ID: 3, Root: []byte(held)}
	plan := Plan{Library: lib, Actions: []Action{{
		Src: filepath.Join(a, "x.flac"), Dst: filepath.Join(held, "Album", "x.flac"),
		Size: quota + (pool-quota)/2, Library: lib, Outcome: OutcomeImport,
	}}}
	err := preflightPlan(&plan, osVolumes)
	if err == nil || !strings.Contains(err.Error(), held+" for import") {
		t.Fatalf("a move of %d bytes onto a quota of %d = %v, want it refused under %s", plan.Actions[0].Size, quota, err, held)
	}

	held2 := testpath.Fixture(t, env, "waxbina", "held2")
	staging := t.TempDir()
	share := min(quota, room(t, held2)) * 3 / 4
	if top := room(t, testpath.Fixture(t, env, "waxbina")); 2*share >= top {
		t.Fatalf("the pool has %d bytes of room, too few for two imports of %d; the fixture is too small", top, share)
	}
	lib2 := &model.Library{ID: 4, Root: []byte(held2)}
	split := Plan{Library: lib, Actions: []Action{
		{Src: filepath.Join(staging, "x.flac"), Dst: filepath.Join(held, "x.flac"), Size: share, Library: lib, Outcome: OutcomeImport},
		{Src: filepath.Join(staging, "y.flac"), Dst: filepath.Join(held2, "y.flac"), Size: share, Library: lib2, Outcome: OutcomeImport},
	}}
	if err := preflightPlan(&split, osVolumes); err != nil {
		t.Errorf("%d bytes onto each of two quotas, each with room for it: %v", share, err)
	}

	// waxbinc's top is not mounted, and x and y, under like quotas and empty, report the
	// same room, as two under no quota would; the pool itself says it holds more.
	x, y := testpath.Fixture(t, env, "waxbinc", "x"), testpath.Fixture(t, env, "waxbinc", "y")
	part := room(t, x) * 3 / 4
	lx, ly := &model.Library{ID: 5, Root: []byte(x)}, &model.Library{ID: 6, Root: []byte(y)}
	alike := Plan{Library: lx, Actions: []Action{
		{Src: filepath.Join(staging, "x.flac"), Dst: filepath.Join(x, "x.flac"), Size: part, Library: lx, Outcome: OutcomeImport},
		{Src: filepath.Join(staging, "y.flac"), Dst: filepath.Join(y, "y.flac"), Size: part, Library: ly, Outcome: OutcomeImport},
	}}
	if err := preflightPlan(&alike, osVolumes); err != nil {
		t.Errorf("%d bytes onto each of two like quotas, each with room for it: %v", part, err)
	}
}

// rootsShareAPool checks two roots on one pool against imports from outside it, each of
// 60% of the pool's room: one at a time they fit, together they are refused. Nothing is
// written.
func rootsShareAPool(t *testing.T, a, b string) {
	t.Helper()
	staging := t.TempDir()
	size := room(t, a) / 10 * 6
	la := &model.Library{ID: 1, Root: []byte(a)}
	lb := &model.Library{ID: 2, Root: []byte(b)}
	onto := func(lib *model.Library, name string) Action {
		return Action{Src: filepath.Join(staging, name), Dst: filepath.Join(string(lib.Root), "Album", name),
			Size: size, Library: lib, Outcome: OutcomeImport}
	}
	both := Plan{Library: la, Actions: []Action{onto(la, "x.flac"), onto(lb, "y.flac")}}
	if err := preflightPlan(&both, osVolumes); err == nil || !strings.Contains(err.Error(), "need "+strconv.FormatInt(2*size, 10)+" bytes") {
		t.Errorf("two imports of %d bytes onto one pool = %v, want them refused together", size, err)
	}
	for _, lib := range []*model.Library{la, lb} {
		alone := Plan{Library: lib, Actions: []Action{onto(lib, "x.flac")}}
		if err := preflightPlan(&alone, osVolumes); err != nil {
			t.Errorf("one import of %d bytes onto %s: %v", size, lib.Root, err)
		}
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
