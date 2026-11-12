package pidpath_test

// The WaxFlow half of the pid: integration, kept here rather than in the package.
//
// pidpath names no transcoder. The glue that does is a consequence of choosing
// WaxFlow, and belongs to whoever chose it. It lives as Examples so
// `go build ./...` and `go vet ./...` stop them rotting, and so waxbin_e2e_test.go
// can drive this exact code against a real catalog: the documented integration is
// then the one under test rather than a second copy of it.
//
// There are two ways in. pidResolver is for a build that links WaxFlow's server and
// hands it a resolver. lookupHandler is for a stock WaxFlow daemon, which asks a host
// over HTTP what a pid names (its pidLookupURL setting) and opens the answer under
// its own roots.
//
// Read them cold. If the adapters and the URL builder are not obviously what a
// consumer would write unaided, the split is in the wrong place.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

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
	// One retry is enough: a second failure is a real missing file. A library that is
	// gone altogether is Relocate's own error, which mapErr answers as a wait.
	loc, err = r.cache.Relocate(ctx, pid)
	if err != nil {
		return nil, mapErr(pid, err)
	}
	return source.OpenLocal(ref, loc.Path, loc.Path)
}

// mapErr translates WaxBin's error vocabulary into WaxFlow's, so it stops at this
// boundary. It is the outbound mirror of waxbin/decode's inbound mapErr.
func mapErr(pid model.PID, err error) error {
	if errors.Is(err, pidpath.ErrLibraryDown) {
		// The server answers this code as retryable, and a job holds until the drive
		// is back.
		return flowerr.New(flowerr.CodeRootUnavailable, fmt.Sprintf("source: the library holding %s is unavailable", pid))
	}
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

// lookupHandler answers a stock WaxFlow daemon's pid lookups: GET /<ULID> returns
// {"ref": "<root>/<path>"}, the item's file as a reference under one of the daemon's
// roots. The daemon names each root after the library's pid, so the reference comes
// from the Location alone and nothing here has to follow a relocated library. Like
// pidResolver it names the whole file, and a virtual track's window rides on the
// request.
//
// The statuses are the daemon's contract: 404 is no such item, 400 an id it should
// not have asked about, and 401 or any 5xx this host being down for now, which the
// daemon retries. No Cache-Control goes out, so the daemon asks every time and the
// cache here is the only one a rename has to reach.
type lookupHandler struct {
	cache *pidpath.Cache
	// key is the daemon's pidLookupKey, the bearer token it sends; empty takes any caller.
	key string
	// local says this host reads the library at the catalog's own paths. A path gone
	// from its disk is then resolved again before it is answered, as pidResolver does
	// on a failed open. Without it nothing here touches the library, and a rename is
	// followed at the cache's next poll.
	local bool
}

func (h *lookupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.key != "" {
		sent, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(sent), []byte(h.key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	pid := model.PID(strings.TrimPrefix(r.URL.Path, "/"))
	if r.Method != http.MethodGet || !pid.Valid() {
		http.Error(w, "not a pid", http.StatusBadRequest)
		return
	}
	loc, err := h.cache.Locate(r.Context(), pid)
	if err == nil && h.local && loc.Path != "" {
		if _, serr := os.Stat(loc.Path); errors.Is(serr, fs.ErrNotExist) {
			// A library that is down keeps the reference it had: the daemon reads a
			// mount of its own and answers for what it finds there.
			if fresh, rerr := h.cache.Relocate(r.Context(), pid); !errors.Is(rerr, pidpath.ErrLibraryDown) {
				loc, err = fresh, rerr
			}
		}
	}
	switch {
	case waxerr.Is(err, waxerr.CodeNotFound), err == nil && (loc.RelPath == "" || !utf8.ValidString(loc.RelPath)):
		// There is nothing to name for an item with no file (an episode not downloaded
		// yet), and no way to name a file whose name is not UTF-8, which JSON cannot hold.
		http.Error(w, "no such item", http.StatusNotFound)
	case err != nil:
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"ref": loc.LibraryPID.String() + "/" + filepath.ToSlash(loc.RelPath),
		})
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

// Answering a stock WaxFlow daemon from the process that holds the catalog. The
// daemon needs one root per library, named by the library's pid, and its
// pidLookupURL and pidLookupKey pointed here:
//
//	roots:        [{"name": "01J0...", "path": "/mnt/music"}]
//	pidLookupURL: http://127.0.0.1:8090/pid
//	pidLookupKey: ...
//
// Its path for a root is its own mount of the library, which need not be the path
// the catalog knows it by. A host that does read the library at the catalog's paths
// sets local on the handler.
func ExampleOpen() {
	ctx := context.Background()

	cache, err := pidpath.Open(ctx, pidpath.Options{DBPath: "catalog.db"})
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	mux := http.NewServeMux()
	mux.Handle("/pid/", http.StripPrefix("/pid", &lookupHandler{cache: cache, key: "..."}))
	log.Fatal(http.ListenAndServe("127.0.0.1:8090", mux))
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
