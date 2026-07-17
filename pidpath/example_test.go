package pidpath_test

// The WaxFlow half of the pid: integration, kept here rather than in the package.
//
// pidpath names no transcoder. The forty-odd lines that do are a consequence of
// choosing WaxFlow, and belong to whoever chose it. They live as Examples so
// `go build ./...` and `go vet ./...` stop them rotting, and so waxbin_e2e_test.go
// can drive this exact code against a real catalog: the documented integration is
// then the one under test rather than a second copy of it.
//
// Read them cold. If the adapter and the URL builder are not obviously what a
// consumer would write unaided, the split is in the wrong place.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/pidpath"
	"github.com/colespringer/waxbin/waxerr"

	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	flowerr "github.com/colespringer/waxflow/waxerr"
)

// pidResolver serves pid:<ULID> references out of a WaxBin catalog and delegates
// every other reference to next (the configured roots).
//
// It resolves a pid to the whole file, deliberately: source.File is a byte blob
// (ReadAt + Size), a window is unrepresentable there by design, and "pid:X" names the
// file. A virtual track's window rides on the request instead, as from=/to=, which
// streamURL builds.
type pidResolver struct {
	cache *pidpath.Cache
	next  source.Resolver
}

func (r *pidResolver) Resolve(ctx context.Context, ref string) (*source.File, error) {
	id, ok := strings.CutPrefix(ref, "pid:")
	if !ok {
		return r.next.Resolve(ctx, ref)
	}
	pid := model.PID(id)
	if !pid.Valid() {
		return nil, flowerr.New(flowerr.CodeInvalidRequest, fmt.Sprintf("source: pid %q is not a ULID", id))
	}
	loc, err := r.cache.Locate(ctx, pid)
	if err != nil {
		return nil, mapErr(pid, err)
	}
	f, err := source.OpenLocal(ref, loc.Path, loc.Path)
	if err == nil || flowerr.CodeOf(err) != flowerr.CodeNotFound {
		return f, err
	}
	// The cached path is gone: a rename landed between polls. Relocate drops the stale
	// entry and re-asks the catalog, which heals it without waiting for the next tick.
	// One retry is enough: a second failure is a real missing file.
	loc, err = r.cache.Relocate(ctx, pid)
	if err != nil {
		return nil, mapErr(pid, err)
	}
	return source.OpenLocal(ref, loc.Path, loc.Path)
}

// mapErr translates WaxBin's error vocabulary into WaxFlow's, so it stops at this
// boundary. It is the outbound mirror of waxbin/decode's inbound mapErr.
func mapErr(pid model.PID, err error) error {
	switch waxerr.CodeOf(err) {
	case waxerr.CodeNotFound:
		return flowerr.New(flowerr.CodeNotFound, fmt.Sprintf("source: no catalog item %s", pid))
	case waxerr.CodeInvalid:
		return flowerr.Wrap(flowerr.CodeInvalidRequest, "source: catalog lookup", err)
	case waxerr.CodeCanceled:
		return flowerr.Wrap(flowerr.CodeCanceled, "source: catalog lookup", err)
	default:
		return flowerr.Wrap(flowerr.CodeCatalogUnavailable, "source: catalog lookup", err)
	}
}

// streamURL builds the /stream URL that plays one catalog item: the pid as the
// source, plus the sample window when the item is a virtual track carved out of a
// shared album rip.
//
// Span is what keeps this branchless. A whole-file item reports (0, 0), and an
// omitted bound is exactly what "the whole file" and "to the end" both mean to the
// server, so neither case needs a Virtual check here.
func streamURL(ctx context.Context, cache *pidpath.Cache, base string, pid model.PID, format string) (string, error) {
	loc, err := cache.Locate(ctx, pid)
	if err != nil {
		return "", err
	}
	from, to, err := loc.Span()
	if err != nil {
		return "", err
	}
	q := url.Values{"src": {"pid:" + pid.String()}}
	if format != "" {
		q.Set("format", format)
	}
	if from > 0 {
		q.Set("from", strconv.FormatInt(from, 10))
	}
	if to > 0 {
		q.Set("to", strconv.FormatInt(to, 10))
	}
	return base + "/stream?" + q.Encode(), nil
}

// Serving a WaxBin catalog by PID: a source.Resolver turns "pid:<ULID>" into the
// item's file and delegates every other reference to the roots.
func ExampleNew() {
	ctx := context.Background()

	// New takes the library you already hold. With only a path, Open takes that
	// instead and opens its own read-only handle.
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: "catalog.db", ReadOnly: true})
	if err != nil {
		log.Fatal(err)
	}
	defer lib.Close()

	cache, err := pidpath.New(ctx, lib, pidpath.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	roots, err := source.OpenRoots(nil, 0)
	if err != nil {
		log.Fatal(err)
	}
	defer roots.Close()

	srv, err := server.New(server.Config{
		Resolver:   &pidResolver{cache: cache, next: roots},
		PIDSources: true,
		CacheDir:   "/var/cache/waxflow",
		APIKeys:    []string{"..."},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	log.Fatal(http.ListenAndServe(":8080", srv))
}

// Playing one item: Locate it, then let Span put the window on the request. A
// whole-file item and a virtual track build the same way.
func ExampleCache_Locate() {
	ctx := context.Background()
	var cache *pidpath.Cache // from New or Open

	u, err := streamURL(ctx, cache, "http://localhost:8080", model.PID("01J0..."), "wav")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(u)
}

// A virtual track is one window of a shared album rip, so its URL carries the window
// as sample offsets. Span converts the stored CD frames exactly: 375 frames at
// 44.1 kHz is sample 220500, and an open end is omitted rather than guessed.
func ExampleLocation_Span() {
	whole := pidpath.Location{Path: "/lib/track.flac", SampleRate: 44100}
	from, to, err := whole.Span()
	fmt.Println(from, to, err)

	track2 := pidpath.Location{
		Path: "/lib/rip.flac", SampleRate: 44100,
		Virtual: true, StartFrames: 375, EndFrames: 14647,
	}
	from, to, err = track2.Span()
	fmt.Println(from, to, err)

	last := pidpath.Location{
		Path: "/lib/rip.flac", SampleRate: 44100,
		Virtual: true, StartFrames: 14647, EndFrames: 0,
	}
	from, to, err = last.Span()
	fmt.Println(from, to, err)
	// Output:
	// 0 0 <nil>
	// 220500 8612436 <nil>
	// 8612436 0 <nil>
}
