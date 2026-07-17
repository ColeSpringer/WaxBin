package pidpath_test

// Integration against a real WaxBin catalog: waxbin (read-write) authors the fixture
// over synthesized WAVs, a pidpath.Cache reads the same catalog, and the tests drive
// the flows the package exists for THROUGH the glue in example_test.go, so the
// documented integration is the one under test rather than a second copy of it.
//
// The ramp fixture is what makes the span assertions mean something: every sample
// value names its own position, so a window that delivered the wrong samples cannot
// pass by delivering the right count.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	binconfig "github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/pidpath"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"

	"github.com/colespringer/waxflow"
	"github.com/colespringer/waxflow/audio"
	"github.com/colespringer/waxflow/codec"
	"github.com/colespringer/waxflow/codec/pcm"
	"github.com/colespringer/waxflow/container"
	"github.com/colespringer/waxflow/container/riff"
	"github.com/colespringer/waxflow/server"
	"github.com/colespringer/waxflow/source"
	flowerr "github.com/colespringer/waxflow/waxerr"
)

// ramp synthesizes a per-channel counter that walks the full range, so sample i of
// channel c is predictable and a span assertion can name the exact samples it wants.
//
// Ported from waxflow/internal/testutil.Ramp's Int branch, which Go's
// import-path-prefix internal rule puts out of reach from this module. Only the Int
// branch, because this fixture is mono 16-bit PCM; a local unexported synth helper is
// the pattern here anyway (decode's tone, analyze's cheapSignal).
func ramp(f audio.Format, frames int) *audio.Buffer {
	b := audio.Get(f, frames)
	b.N = frames
	for c := 0; c < f.Channels; c++ {
		s := b.ChanI(c)
		for i := range s {
			s[i] = rampAtI(f, c, int64(i))
		}
	}
	return b
}

// rampAtI is the closed form of ramp, which lets a test verify any position without
// holding the whole signal.
func rampAtI(f audio.Format, channel int, pos int64) int32 {
	span := int64(1) << f.BitDepth
	lo := int64(-1) << (f.BitDepth - 1)
	return int32(lo + (pos*7+int64(channel)*13)%span)
}

// ripFormat is the fixture's audio format: 44.1 kHz mono 16-bit, the CD rate, where
// one CD frame is exactly 588 samples.
var ripFormat = audio.Format{Rate: 44100, Channels: 1, Layout: audio.DefaultLayout(1), Type: audio.Int, BitDepth: 16}

// rampWAV renders a 16-bit mono ramp as a WAV file; frames vary per fixture so every
// file has a distinct audio essence (WaxBin's move detection matches files by essence
// hash).
func rampWAV(t *testing.T, frames int) []byte {
	t.Helper()
	buf := ramp(ripFormat, frames)
	defer audio.Put(buf)
	enc, err := pcm.NewEncoder(pcm.Config{Encoding: pcm.SignedInt, Bits: 16}, ripFormat)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux := riff.NewMuxer(&out, nil)
	track := container.Track{Codec: codec.PCM, CodecConfig: enc.CodecConfig(), Fmt: ripFormat, Samples: int64(frames), Default: true}
	if err := mux.Begin([]container.Track{track}); err != nil {
		t.Fatal(err)
	}
	emit := func(p codec.Packet) error { return mux.WritePacket(container.Packet{Track: 0, Packet: p}) }
	if err := enc.Encode(buf, emit); err != nil {
		t.Fatal(err)
	}
	trailer, err := enc.Finish(emit)
	if err != nil {
		t.Fatal(err)
	}
	if err := mux.End(trailer); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// buildCatalog writes the named files under a fresh root, opens WaxBin read-write
// beside it, and scans. The returned library handle stays open (the writer a consumer
// coexists with in production). Only .wav files are audio; a .cue rides along as a
// sidecar of the rip it indexes.
func buildCatalog(t *testing.T, files map[string][]byte) (lib *waxbin.Library, root, db string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	db = filepath.Join(t.TempDir(), "catalog.db")
	audioFiles := 0
	for name, data := range files {
		if filepath.Ext(name) == ".wav" {
			audioFiles++
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []binconfig.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("waxbin open: %v", err)
	}
	t.Cleanup(func() { lib.Close() })
	res, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("waxbin scan: %v", err)
	}
	if int(res.Total.AudioFiles) != audioFiles {
		t.Fatalf("scan tally = %+v, want %d audio files", res.Total, audioFiles)
	}
	return lib, root, db
}

// itemPIDs maps each cataloged file's base name onto its item PID. It is for
// whole-file items only: virtual tracks all share one path, so they key by title
// instead (itemPIDsByTitle).
func itemPIDs(t *testing.T, lib *waxbin.Library) map[string]model.PID {
	t.Helper()
	out := make(map[string]model.PID)
	for _, iv := range allItems(t, lib) {
		out[filepath.Base(string(iv.Path))] = iv.PID
	}
	return out
}

func itemPIDsByTitle(t *testing.T, lib *waxbin.Library) map[string]model.PID {
	t.Helper()
	out := make(map[string]model.PID)
	for _, iv := range allItems(t, lib) {
		out[iv.Title] = iv.PID
	}
	return out
}

func allItems(t *testing.T, lib *waxbin.Library) []*model.ItemView {
	t.Helper()
	// This selection has no per-user field, so it is not scoped by user; an empty
	// userPID satisfies the signature without narrowing the results.
	items, err := lib.Query(context.Background(), query.New(query.EntityItems).OrderBy("track", false).Build(), "")
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	return items
}

// newCache opens a poll-disabled Cache over the catalog at db (tests drive Poll
// directly).
func newCache(t *testing.T, db string) *pidpath.Cache {
	t.Helper()
	c, err := pidpath.Open(context.Background(), pidpath.Options{DBPath: db, PollInterval: -1})
	if err != nil {
		t.Fatalf("pidpath.Open: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// serveCache stands up a WaxFlow server in front of the cache, wired through
// example_test.go's pidResolver: the glue under test is the glue documented.
func serveCache(t *testing.T, cache *pidpath.Cache) *httptest.Server {
	t.Helper()
	roots, err := source.OpenRoots(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { roots.Close() })
	srv, err := server.New(server.Config{
		APIKeys:    []string{"test-key"},
		Resolver:   &pidResolver{cache: cache, next: roots},
		PIDSources: true,
		CacheDir:   t.TempDir(),
		Version:    "test",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	t.Cleanup(func() { srv.Close() })
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

// get reads the whole body and closes it before returning, so no connection lingers
// to the end of the test.
func get(t *testing.T, ts *httptest.Server, url string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// decodePCM decodes a streamed response back to samples.
func decodePCM(t *testing.T, raw []byte) *audio.Buffer {
	t.Helper()
	med, err := waxflow.New().OpenStream(container.BytesSource(raw), "")
	if err != nil {
		t.Fatal(err)
	}
	defer med.Close()
	f := med.Info().Default().Fmt
	total := med.Info().Default().Samples
	if total < 0 {
		total = 1 << 22
	}
	out := audio.Get(f, int(total))
	t.Cleanup(func() { audio.Put(out) })
	tmp := audio.Get(f, audio.StandardChunk)
	defer audio.Put(tmp)
	for {
		err := med.ReadChunk(tmp)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		audio.CopyFrames(out, out.N, tmp, 0, tmp.N)
		out.N += tmp.N
	}
	return out
}

func TestResolveAgainstRealCatalog(t *testing.T) {
	ctx := context.Background()
	alpha := rampWAV(t, 8000)
	lib, _, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": alpha,
		"beta.wav":  rampWAV(t, 12000),
	})
	pids := itemPIDs(t, lib)
	cache := newCache(t, db)

	loc, err := cache.Locate(ctx, pids["alpha.wav"])
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if filepath.Base(loc.Path) != "alpha.wav" {
		t.Fatalf("located %q, want alpha.wav", loc.Path)
	}
	if loc.Virtual {
		t.Error("a whole-file track must not read back as virtual")
	}
	if loc.SampleRate != 44100 {
		t.Errorf("sample rate = %d, want 44100 (it rides along with the location)", loc.SampleRate)
	}
	// A whole-file item has no window, so Span asks for no bounds.
	if from, to, err := loc.Span(); from != 0 || to != 0 || err != nil {
		t.Errorf("whole-file Span = (%d, %d, %v), want (0, 0, nil)", from, to, err)
	}
	got, err := os.ReadFile(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, alpha) {
		t.Fatal("the located path does not hold the cataloged bytes")
	}

	if _, err := cache.Locate(ctx, model.NewPID()); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unknown pid = %v, want not-found", err)
	}
}

func TestRenameInvalidatesWithinOnePoll(t *testing.T) {
	ctx := context.Background()
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]
	cache := newCache(t, db)

	loc, err := cache.Locate(ctx, pid)
	if err != nil {
		t.Fatalf("warm Locate: %v", err)
	}
	oldPath := loc.Path

	// The user reorganizes the library: the file moves on disk and a rescan relinks it
	// in the catalog (same essence, new path, same file PID), emitting file-update
	// change rows.
	newPath := filepath.Join(root, "omega.wav")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("rescan: %v", err)
	}

	// One poll must drop the stale cached location.
	if err := cache.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if stale, ok := cache.Cached(pid); ok {
		t.Fatalf("cached location %q survived the poll after a rename", stale.Path)
	}
	loc, err = cache.Locate(ctx, pid)
	if err != nil {
		t.Fatalf("Locate after rename: %v", err)
	}
	if loc.Path != newPath {
		t.Fatalf("located %q after the rename, want %q", loc.Path, newPath)
	}
}

func TestBackgroundPollInvalidates(t *testing.T) {
	ctx := context.Background()
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]

	cache, err := pidpath.Open(ctx, pidpath.Options{DBPath: db, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cache.Close()

	loc, err := cache.Locate(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(loc.Path, filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := cache.Cached(pid); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background poll never dropped the renamed location")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRelocateHealsAStaleLocation drives the self-heal the adapter in example_test.go
// implements: a rename that lands between polls leaves the cached path stale, the
// open fails, and Relocate fixes it without waiting for the next tick.
func TestRelocateHealsAStaleLocation(t *testing.T) {
	ctx := context.Background()
	alpha := rampWAV(t, 8000)
	lib, root, db := buildCatalog(t, map[string][]byte{"alpha.wav": alpha})
	pid := itemPIDs(t, lib)["alpha.wav"]
	cache := newCache(t, db)
	roots, err := source.OpenRoots(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	resolver := &pidResolver{cache: cache, next: roots}

	f, err := resolver.Resolve(ctx, "pid:"+string(pid))
	if err != nil {
		t.Fatalf("warm Resolve: %v", err)
	}
	f.Close()

	// Move the file and relink it, but run no poll: the cache still names the old path.
	if err := os.Rename(filepath.Join(root, "alpha.wav"), filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if stale, ok := cache.Cached(pid); !ok || filepath.Base(stale.Path) != "alpha.wav" {
		t.Fatalf("cache = %+v, want the stale pre-rename path still held", stale)
	}

	f, err = resolver.Resolve(ctx, "pid:"+string(pid))
	if err != nil {
		t.Fatalf("Resolve after an unpolled rename: %v; the ENOENT self-heal is broken", err)
	}
	defer f.Close()
	got := make([]byte, f.Size())
	if _, err := f.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, alpha) {
		t.Fatal("the healed resolve serves the wrong bytes")
	}
	if healed, _ := cache.Cached(pid); filepath.Base(healed.Path) != "omega.wav" {
		t.Fatalf("cache after the heal = %q, want omega.wav", healed.Path)
	}
}

func TestPIDStreamsE2E(t *testing.T) {
	ctx := context.Background()
	lib, root, db := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]
	cache := newCache(t, db)
	ts := serveCache(t, cache)

	// The server advertises pid sources.
	_, body := get(t, ts, ts.URL+"/caps")
	var caps struct {
		Delivery struct {
			PID bool `json:"pid"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(body, &caps); err != nil || !caps.Delivery.PID {
		t.Fatalf("caps delivery.pid = %v (err %v), want true", caps.Delivery.PID, err)
	}

	// pid: probes and streams.
	if resp, _ := get(t, ts, ts.URL+"/probe?src=pid:"+string(pid)); resp.StatusCode != http.StatusOK {
		t.Fatalf("probe = %d", resp.StatusCode)
	}
	resp, body := get(t, ts, ts.URL+"/stream?src=pid:"+string(pid)+"&format=wav")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("stream = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if len(body) < 44 {
		t.Fatalf("stream body = %d bytes", len(body))
	}

	// id= pins bytes for pid refs exactly like path refs: stale is 410.
	resp, body = get(t, ts, ts.URL+"/stream?src=pid:"+string(pid)+"&format=wav&id=1-1")
	var envlp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &envlp); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusGone || envlp.Code != string(flowerr.CodeSourceChanged) {
		t.Fatalf("stale id = %d %q, want 410 source-changed", resp.StatusCode, envlp.Code)
	}

	// A rename changes no bytes, so a URL pinned to the true identity keeps playing
	// across it: the pid re-resolves to the new path and identity (size+mtimeNS,
	// preserved by rename) still matches.
	roots, err := source.OpenRoots(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	f, err := (&pidResolver{cache: cache, next: roots}).Resolve(ctx, "pid:"+string(pid))
	if err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	f.Close()
	if err := os.Rename(filepath.Join(root, "alpha.wav"), filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := cache.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	resp, _ = get(t, ts, ts.URL+"/stream?src=pid:"+string(pid)+"&format=wav&id="+id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pinned stream after rename = %d, want 200 (renames must not kill URLs)", resp.StatusCode)
	}
}

// ripCue indexes a single-file rip at frames 0, 37, and 127.
//
// Both interior boundaries sit on frames not divisible by 3, which is the entire
// point: they do not land on a whole millisecond, so the millisecond path this
// replaced could not name them. Frame 37 is sample 21756 but truncates to 493 ms,
// which is sample 21740, putting 16 samples of track one at the head of track two.
const ripCue = `PERFORMER "Rip Band"
TITLE "The Rip"
FILE "rip.wav" WAVE
  TRACK 01 AUDIO
    TITLE "One"
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    TITLE "Two"
    INDEX 01 00:00:37
  TRACK 03 AUDIO
    TITLE "Three"
    INDEX 01 00:01:52
`

// TestVirtualTrackStreamsItsOwnSamples is the gap this whole change exists to close.
// A CUE rip is scanned into virtual tracks, and each is streamed by pid: plus the
// from=/to= its own Span produces. The assertion is on SAMPLE VALUES, not counts:
// the ramp names positions, so serving the album, or serving the window 15 samples
// off the boundary the disc named, both fail.
func TestVirtualTrackStreamsItsOwnSamples(t *testing.T) {
	ctx := context.Background()
	const totalSamples = 3 * 44100
	lib, _, db := buildCatalog(t, map[string][]byte{
		"rip.wav": rampWAV(t, totalSamples),
		"rip.cue": []byte(ripCue),
	})
	pids := itemPIDsByTitle(t, lib)
	if len(pids) != 3 {
		t.Fatalf("catalog holds %d items, want 3 virtual tracks: %v", len(pids), pids)
	}
	cache := newCache(t, db)
	ts := serveCache(t, cache)

	// One CD frame is 588 samples at 44.1 kHz, exactly.
	want := []struct {
		title    string
		from, to int64
	}{
		{"One", 0, 37 * 588},
		{"Two", 37 * 588, 127 * 588},
		{"Three", 127 * 588, 0}, // the final track's end stays open
	}

	for _, w := range want {
		t.Run(w.title, func(t *testing.T) {
			pid, ok := pids[w.title]
			if !ok {
				t.Fatalf("no virtual track titled %q", w.title)
			}
			loc, err := cache.Locate(ctx, pid)
			if err != nil {
				t.Fatalf("Locate: %v", err)
			}
			if !loc.Virtual {
				t.Fatal("a cue TRACK must read back as a virtual track")
			}
			from, to, err := loc.Span()
			if err != nil {
				t.Fatalf("Span: %v", err)
			}
			if from != w.from || to != w.to {
				t.Fatalf("span = [%d, %d), want [%d, %d)", from, to, w.from, w.to)
			}

			// FLAC at the source rate keeps it bit-exact: no resampler means no state to
			// prime, so the window's sample 0 is the source's sample `from` exactly.
			u, err := streamURL(ctx, cache, ts.URL, pid, "flac")
			if err != nil {
				t.Fatalf("streamURL: %v", err)
			}
			resp, body := get(t, ts, u)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200", u, resp.StatusCode)
			}
			got := decodePCM(t, body)

			end := to
			if end == 0 {
				end = totalSamples
			}
			if wantN := int(end - from); got.N != wantN {
				t.Fatalf("window [%d, %d) delivered %d samples, want %d", from, end, got.N, wantN)
			}
			out := got.ChanI(0)
			for i := range got.N {
				if src := rampAtI(ripFormat, 0, from+int64(i)); out[i] != src {
					t.Fatalf("window [%d, %d) sample %d = %d, want the rip's sample %d (%d): "+
						"these are not this track's bytes", from, end, i, out[i], from+int64(i), src)
				}
			}
		})
	}

	// Consecutive tracks abut at the exact sample: no gap, no overlap, nothing of the
	// neighbor. This is the property a millisecond window cannot hold.
	for i := 0; i+1 < len(want); i++ {
		if want[i].to != want[i+1].from {
			t.Fatalf("track %q ends at sample %d but %q starts at %d; the join opened",
				want[i].title, want[i].to, want[i+1].title, want[i+1].from)
		}
	}
}

// TestNewOverACallerOpenedLibrary covers the constructor the split exists for: an
// embedder passes the library it already holds rather than a DB path for a second
// read-only handle onto the same file. Close must stop the poll and leave the library
// alone, since this Cache did not open it.
func TestNewOverACallerOpenedLibrary(t *testing.T) {
	ctx := context.Background()
	lib, root, _ := buildCatalog(t, map[string][]byte{
		"alpha.wav": rampWAV(t, 8000),
		"beta.wav":  rampWAV(t, 12000),
	})
	pid := itemPIDs(t, lib)["alpha.wav"]

	cache, err := pidpath.New(ctx, lib, pidpath.Options{PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	loc, err := cache.Locate(ctx, pid)
	if err != nil {
		t.Fatalf("Locate: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The library is still the caller's, and still answers.
	if _, err := lib.Get(ctx, pid); err != nil {
		t.Fatalf("Close closed the caller's library: %v", err)
	}

	// And the poll really stopped: a change that lands now is never consumed, so the
	// warm entry stays exactly as Close left it. Twenty-odd ticks would have fired.
	if err := os.Rename(loc.Path, filepath.Join(root, "omega.wav")); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, ok := cache.Cached(pid); !ok {
		t.Fatal("the poll loop outlived Close: it dropped an entry after the cache was closed")
	}

	// New refuses a nil library rather than panicking at the first Locate.
	if _, err := pidpath.New(ctx, nil, pidpath.Options{}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("New(nil) = %v, want an invalid-argument error", err)
	}
}
