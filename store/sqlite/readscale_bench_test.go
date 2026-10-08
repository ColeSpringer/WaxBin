package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// BenchmarkReadScaling measures how catalog reads scale with the readers running at once,
// over a 20k-track catalog and a 16-connection read pool:
//
//	go test ./store/sqlite -run '^$' -bench ReadScaling -cpu 1,2,4,8 -count 10 -mutexprofile m.out
//
// RunParallel runs one reader per CPU and ns/op is the wall time per read, so reads that
// scale divide it by the CPU count. An ns/op that stays flat as -cpu grows means the
// readers queue on something they share; go tool pprof -top m.out names the lock, and
// benchstat compares two runs. The Writes case reads beside a writer committing every
// two milliseconds.
func BenchmarkReadScaling(b *testing.B) {
	seed, err := readScaleSeed()
	if err != nil {
		b.Fatalf("build the read-scaling seed: %v", err)
	}
	pick := func(i int64) model.PID { return seed.pids[int(i)%len(seed.pids)] }
	reads := []struct {
		name   string
		writes bool
		read   func(ctx context.Context, st *Store, i int64) error
	}{
		{"ItemByPID", false, func(ctx context.Context, st *Store, i int64) error {
			_, err := st.ItemByPID(ctx, pick(i))
			return err
		}},
		{"Search", false, func(ctx context.Context, st *Store, i int64) error {
			_, err := st.Search(ctx, fmt.Sprintf("track %d", i%1000), read.SearchOptions{Limit: 20})
			return err
		}},
		{"BrowseAlpha", false, func(ctx context.Context, st *Store, _ int64) error {
			_, err := st.BrowsePage(ctx, read.ListAlphabetical, read.BrowseOptions{Limit: 50})
			return err
		}},
		{"BrowseRecent", false, func(ctx context.Context, st *Store, _ int64) error {
			_, err := st.BrowsePage(ctx, read.ListRecentlyAdded, read.BrowseOptions{Limit: 50})
			return err
		}},
		{"ItemByPIDWrites", true, func(ctx context.Context, st *Store, i int64) error {
			_, err := st.ItemByPID(ctx, pick(i))
			return err
		}},
	}
	for _, rc := range reads {
		b.Run(rc.name, func(b *testing.B) {
			ctx := context.Background()
			path := filepath.Join(b.TempDir(), "c.db")
			if err := os.WriteFile(path, seed.catalog, 0o600); err != nil {
				b.Fatal(err)
			}
			st, err := Open(ctx, OpenOptions{Path: path, Owner: "bench", ReadPoolSize: 16})
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			if rc.writes {
				stop := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(1)
				go func() {
					defer wg.Done()
					tick := time.NewTicker(2 * time.Millisecond)
					defer tick.Stop()
					for n := int64(0); ; n++ {
						select {
						case <-stop:
							return
						case <-tick.C:
							if err := st.SetProgress(ctx, "", pick(n*7), n%600_000, nil); err != nil {
								b.Error(err)
								return
							}
						}
					}
				}()
				defer func() { close(stop); wg.Wait() }()
			}
			var next atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if err := rc.read(ctx, st, next.Add(1)); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// readScaleCatalog is the read-scaling catalog: its bytes and its items' pids.
type readScaleCatalog struct {
	catalog []byte
	pids    []model.PID
}

// readScaleSeed builds the 20k-track catalog once per test binary, through the same
// tracks benchInsert puts; each benchmark opens a copy.
var readScaleSeed = sync.OnceValues(func() (readScaleCatalog, error) {
	const n = 20_000
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "waxbin-readscale")
	if err != nil {
		return readScaleCatalog{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "seed.db")
	st, err := Open(ctx, OpenOptions{Path: path, Owner: "bench"})
	if err != nil {
		return readScaleCatalog{}, err
	}
	defer func() { _ = st.Close() }()
	lib, err := st.EnsureLibrary(ctx, &model.Library{
		Root: []byte("/lib"), DisplayRoot: "/lib", Mode: model.ModeManaged, Profile: "waxbin-native",
	})
	if err != nil {
		return readScaleCatalog{}, err
	}
	out := readScaleCatalog{pids: make([]model.PID, 0, n)}
	for i := range n {
		res, err := st.PutScannedTrack(ctx, benchTrack(lib.ID, i))
		if err != nil {
			return readScaleCatalog{}, err
		}
		out.pids = append(out.pids, res.ItemPID)
	}
	// Close checkpoints the WAL, so the one file carries the whole catalog.
	if err := st.Close(); err != nil {
		return readScaleCatalog{}, err
	}
	out.catalog, err = os.ReadFile(path)
	return out, err
})
