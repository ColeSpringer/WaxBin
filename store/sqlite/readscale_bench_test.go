package sqlite

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	msqlite "modernc.org/sqlite"
	"modernc.org/sqlite/pcache"
)

// BenchmarkReadScaling measures how catalog reads scale with the readers running at once,
// over a 20k-track catalog and a 16-connection read pool:
//
//	go test ./store/sqlite -run '^$' -bench ReadScaling -cpu 1,2,4,8,16 -count 3 -mutexprofile m.out
//
// RunParallel runs one reader per CPU and ns/op is the wall time per read, so reads that
// scale divide it by the CPU count. An ns/op that stays flat as -cpu grows means the
// readers queue on something they share; go tool pprof -top m.out names the lock, and
// benchstat compares two runs. The Writes case reads beside a writer committing every
// two milliseconds.
//
// Measured on 32 logical CPUs (2026-10-30, the median of three interleaved runs at
// -benchtime 2s), before the statement cache held the search, batch item and entity
// reads, with Search after it on the line below; the other reads were cached already and
// moved only within noise:
//
//	read             1        2        4        8        16
//	ItemByPID        26.2µs   12.0µs   6.3µs    4.2µs    4.4µs
//	Search           4.81ms   2.25ms   1.13ms   848µs    828µs
//	Search, cached   4.00ms   1.91ms   947µs    672µs    610µs
//	BrowseAlpha      623µs    309µs    147µs    85µs     64µs
//	BrowseRecent     576µs    291µs    141µs    85µs     64µs
//	ItemByPIDWrites  28.1µs   14.9µs   7.3µs    6.5µs    9.7µs
//
// Point reads and search stop scaling near eight readers while the browse pages go on to
// sixteen, and a point read beside the writer is slower at sixteen readers than at four.
// At eight readers the mutex profile puts 38% of the lock delay in modernc's allocator
// (libc's Xmalloc, Xfree and Xrealloc share one lock for the process, and every text bind
// allocates), 24% in the Go runtime's scheduler and GC locks, 18% in database/sql's pool
// and 12% in SQLite's mutexes, a third of that the pcache1 LRU lock a fetch takes for a
// page held in the page cache. A page read through the 256 MiB mmap bypasses that lock,
// so it bites while a writer keeps pages in the WAL. Preparing, 15% before the cache
// held search, is 1.4%. modernc's per-connection page cache (-pcache registers
// pcache.New) left ItemByPIDWrites unchanged at eight and sixteen readers (+0% and +1%)
// and made BrowseAlpha 67% and 98% slower there at the same RSS, so the store registers
// none. What remains is the dependency's own locking.
func BenchmarkReadScaling(b *testing.B) {
	if *usePageCache {
		if err := msqlite.RegisterPageCache(pcache.New()); err != nil {
			b.Fatalf("register the page cache (run this benchmark alone): %v", err)
		}
	}
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
			b.StopTimer()
			if kb := vmRSSKB(); kb > 0 {
				b.ReportMetric(float64(kb), "rss-KB")
			}
		})
	}
}

// usePageCache registers modernc's per-connection page cache before BenchmarkReadScaling
// opens a catalog, for comparing it with SQLite's own: go test -bench ReadScaling -pcache.
var usePageCache = flag.Bool("pcache", false, "run BenchmarkReadScaling over modernc's per-connection page cache")

// vmRSSKB is the process's resident set in KiB, 0 where /proc does not say.
func vmRSSKB() int64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
			return kb
		}
	}
	return 0
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
