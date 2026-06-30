package waxbin_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

// TestEndToEndSingleFile verifies the core flow from scan to store to query to
// organize and read back.
func TestEndToEndSingleFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))

	lib := openManaged(t, ctx, db, root)

	// SCAN
	scanRes, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanRes.Total.AudioFiles != 1 || scanRes.Total.ItemsCreated != 1 {
		t.Fatalf("scan tally = %+v, want 1 audio / 1 created", scanRes.Total)
	}
	if scanRes.JobPID == "" {
		t.Fatal("scan did not run under a job")
	}

	// QUERY (by substring, through the shared query engine)
	items, err := lib.Query(ctx, query.New(query.EntityItems).
		Where("title", query.OpContains, "Midnight").Build())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("query returned %d items, want 1", len(items))
	}
	got := items[0]
	if got.Title != "Midnight Drive" || got.Artist != "The Foobars" || got.Album != "Night Moves" {
		t.Fatalf("read-back tags wrong: %+v", got)
	}
	if got.TrackNo != 3 {
		t.Fatalf("track no = %d, want 3", got.TrackNo)
	}
	pid := got.PID

	// ORGANIZE (dry run, then apply)
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	if plan.Pending() != 1 {
		t.Fatalf("plan pending = %d, want 1", plan.Pending())
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply organize: %v", err)
	}
	if rep.Moved != 1 || rep.Errored != 0 {
		t.Fatalf("organize report = %+v, want moved 1 errored 0", rep)
	}

	// READ BACK: the catalog reflects the new templated location, and the file
	// is actually there (and gone from its origin).
	wantPath := filepath.Join(root, "The Foobars", "Night Moves", "03 - Midnight Drive.mp3")
	after, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("get after organize: %v", err)
	}
	if after.DisplayPath != wantPath {
		t.Fatalf("path after organize = %q, want %q", after.DisplayPath, wantPath)
	}
	if !fileExists(wantPath) {
		t.Fatalf("organized file missing on disk: %s", wantPath)
	}
	if fileExists(src) {
		t.Fatalf("source file still present after move: %s", src)
	}

	// JOBS: both the scan and organize jobs are recorded and done.
	jobs, err := lib.Jobs(ctx, 10)
	if err != nil {
		t.Fatalf("jobs: %v", err)
	}
	if !hasDoneJob(jobs, "scan") || !hasDoneJob(jobs, "organize") {
		t.Fatalf("expected done scan+organize jobs, got %+v", jobs)
	}

	// CHANGE LOG: mutations were recorded for delta consumers.
	changes, err := lib.Changes(ctx, 0)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) == 0 {
		t.Fatal("expected change_log rows")
	}
}

// TestAnalyzeAndGroupAltEncodings verifies the scan/analyze boundary end to end:
// scanning catalogs WAVs without decoding; the analyze pass decodes + fingerprints
// them; and the fingerprint index groups two encodings of one recording while
// excluding an unrelated track.
func TestAnalyzeAndGroupAltEncodings(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	const rate = 22050
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	transcoded := testaudio.Reencode(orig, 0.85, 42) // same recording, different bytes
	other := testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)

	writeFile(t, filepath.Join(root, "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	writeFile(t, filepath.Join(root, "beta.wav"), testaudio.EncodeWAV16(rate, transcoded))
	writeFile(t, filepath.Join(root, "gamma.wav"), testaudio.EncodeWAV16(rate, other))

	lib := openManaged(t, ctx, db, root)

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// Scanning must not have produced fingerprints (it never decodes PCM).
	if rep, _ := lib.Doctor(ctx); rep.FingerprintCount != 0 {
		t.Fatalf("scan produced %d fingerprints; scan must not analyze", rep.FingerprintCount)
	}
	// The derived data (FTS, rollups, sort keys) is consistent right after a scan,
	// since scan refreshes the rollups and maintains the FTS in the write txn.
	if dr, err := lib.VerifyDerived(ctx); err != nil || !dr.Consistent() {
		t.Fatalf("derived data inconsistent after scan: %+v (err %v)", dr, err)
	}

	ares, err := lib.Analyze(ctx)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if ares.Result.Analyzed != 3 || ares.Result.Skipped != 0 || ares.Result.Errored != 0 {
		t.Fatalf("analyze result = %+v, want 3 analyzed / 0 skipped / 0 errored", ares.Result)
	}

	rep, err := lib.Doctor(ctx)
	if err != nil || rep.FingerprintCount != 3 {
		t.Fatalf("after analyze: fingerprints=%d err=%v, want 3", rep.FingerprintCount, err)
	}

	// A second analyze is a no-op: nothing is stale (essence + version unchanged).
	again, err := lib.Analyze(ctx)
	if err != nil {
		t.Fatalf("re-analyze: %v", err)
	}
	if again.Result.Analyzed != 0 {
		t.Fatalf("re-analyze re-did %d files; analysis is essence-keyed and should skip", again.Result.Analyzed)
	}

	alphaPID := itemPIDByTitle(t, ctx, lib, "alpha")
	betaPID := itemPIDByTitle(t, ctx, lib, "beta")
	gammaPID := itemPIDByTitle(t, ctx, lib, "gamma")

	alts, err := lib.FindAltEncodings(ctx, alphaPID)
	if err != nil {
		t.Fatalf("find alt encodings: %v", err)
	}
	matched := map[model.PID]float64{}
	for _, a := range alts {
		matched[a.ItemPID] = a.Similarity
	}
	if _, ok := matched[betaPID]; !ok {
		t.Errorf("beta (a transcode of alpha) was not grouped as an alt encoding; got %+v", alts)
	}
	if _, ok := matched[gammaPID]; ok {
		t.Errorf("gamma (an unrelated track) was wrongly grouped with alpha")
	}
}

// TestAnalyzeMeasuresLoudness verifies the analyze pass computes and stores
// ReplayGain for decodable files (WAV loudness works whether via ffmpeg's
// ebur128 or the pure-Go R128 fallback, so the assertion is host-independent).
func TestAnalyzeMeasuresLoudness(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	const rate = 22050
	writeFile(t, filepath.Join(root, "a.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 5, testaudio.MusicalPartials, 1)))
	writeFile(t, filepath.Join(root, "b.wav"), testaudio.EncodeWAV16(rate, testaudio.RichSignal(rate, 5, testaudio.AltPartials, 2)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	ares, err := lib.Analyze(ctx)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if ares.Result.LoudnessMeasured != 2 {
		t.Fatalf("loudness measured = %d, want 2", ares.Result.LoudnessMeasured)
	}

	rep, _ := lib.Doctor(ctx)
	if rep.LoudnessCount != 2 {
		t.Errorf("doctor loudness count = %d, want 2", rep.LoudnessCount)
	}

	pid := itemPIDByTitle(t, ctx, lib, "a")
	ld, err := lib.Loudness(ctx, pid)
	if err != nil {
		t.Fatalf("read loudness: %v", err)
	}
	// A real measurement has a finite integrated loudness and a sane track peak.
	if ld.IntegratedLUFS >= 0 || ld.TrackPeak <= 0 || ld.TrackPeak > 1.01 {
		t.Errorf("implausible loudness: %+v", ld)
	}

	// The waveform was stored and unpacks to the requested resolution.
	pk, err := lib.Peaks(ctx, pid)
	if err != nil {
		t.Fatalf("read peaks: %v", err)
	}
	if pk.Buckets <= 0 || len(pk.Data) != pk.Buckets*2 {
		t.Errorf("peaks shape wrong: buckets=%d dataLen=%d", pk.Buckets, len(pk.Data))
	}
}

func itemPIDByTitle(t *testing.T, ctx context.Context, lib *waxbin.Library, title string) model.PID {
	t.Helper()
	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, title).Build())
	if err != nil {
		t.Fatalf("query %q: %v", title, err)
	}
	if len(items) != 1 {
		t.Fatalf("query %q returned %d items, want 1", title, len(items))
	}
	return items[0].PID
}

// TestPlaybackAndChangeBus verifies the playback service and the in-process
// change bus wired through the facade: a star/rating round-trips for the default
// user, and a subscriber sees the play_state delta.
func TestPlaybackAndChangeBus(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	item := items[0].PID

	ch, cancel := lib.Subscribe()
	defer cancel()

	pb := lib.Playback()
	if err := pb.SetStar(ctx, "", item, true); err != nil {
		t.Fatalf("star: %v", err)
	}
	r := 90
	if err := pb.SetRating(ctx, "", item, &r); err != nil {
		t.Fatalf("rate: %v", err)
	}
	if err := pb.MarkPlayed(ctx, "", item, true); err != nil {
		t.Fatalf("played: %v", err)
	}

	st, err := pb.State(ctx, "", item)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if !st.Starred || !st.HasRating || st.Rating != 90 || !st.Finished || st.PlayCount != 1 {
		t.Fatalf("round-trip state wrong: %+v", st)
	}

	// The subscriber observed play_state deltas for the item.
	var saw bool
	timeout := time.After(time.Second)
collect:
	for {
		select {
		case c := <-ch:
			if c.EntityType == "play_state" && c.EntityPID == item {
				saw = true
				break collect
			}
		case <-timeout:
			break collect
		}
	}
	if !saw {
		t.Error("subscriber did not observe a play_state delta")
	}
}

// TestAnalyzeAIFFNotErrored verifies an AIFF file (which WaxLabel reports as PCM)
// is decoded via ffmpeg rather than routed to the RIFF/WAVE-only pure-Go decoder
// and erroring. ffmpeg is required to synthesize and decode it, so skip without it.
func TestAnalyzeAIFFNotErrored(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	aiff := filepath.Join(root, "tone.aiff")
	if out, err := exec.Command(bin, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-y", aiff).CombinedOutput(); err != nil {
		t.Skipf("could not synthesize AIFF: %v (%s)", err, out)
	}

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	// The AIFF cataloged with the container-keyed codec, not "pcm".
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if len(items) != 1 || items[0].Codec != "aiff" {
		t.Fatalf("AIFF codec = %v, want one item keyed 'aiff'", items)
	}
	res, err := lib.Analyze(ctx)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if res.Result.Errored != 0 {
		t.Errorf("AIFF analysis errored (%d); it should decode via ffmpeg", res.Result.Errored)
	}
	if res.Result.Analyzed != 1 {
		t.Errorf("AIFF analyzed = %d, want 1 (ffmpeg path)", res.Result.Analyzed)
	}
}

// TestStatsOnFacet verifies the stats summary: structural counts and the
// Facet-built top genres/artists, plus per-user play stats from play_state.
func TestStatsOnFacet(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Distinct audio per file so essence differs (three separate items).
	write := func(name string, s testaudio.MP3Spec, seed byte) {
		s.Audio = testaudio.AudioWithSeed(seed)
		writeFile(t, filepath.Join(root, name), testaudio.BuildMP3FromSpec(s))
	}
	write("a.mp3", testaudio.MP3Spec{Title: "A", Artist: "Alpha", Album: "One", Genre: "Rock"}, 1)
	write("b.mp3", testaudio.MP3Spec{Title: "B", Artist: "Beta", Album: "Two", Genre: "Rock"}, 2)
	write("c.mp3", testaudio.MP3Spec{Title: "C", Artist: "Alpha", Album: "Three", Genre: "Jazz"}, 3)

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Record some plays on the first item.
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Where("title", query.OpIs, "A").Build())
	if len(items) != 1 {
		t.Fatalf("want item A, got %d", len(items))
	}
	pb := lib.Playback()
	for i := 0; i < 3; i++ {
		if err := pb.MarkPlayed(ctx, "", items[0].PID, i == 2); err != nil {
			t.Fatal(err)
		}
	}
	_ = pb.SetStar(ctx, "", items[0].PID, true)

	st, err := lib.Stats(ctx, "", 5)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Items != 3 || st.Artists != 2 || st.Genres != 2 {
		t.Errorf("counts wrong: items=%d artists=%d genres=%d, want 3/2/2", st.Items, st.Artists, st.Genres)
	}
	if len(st.TopGenres) == 0 || st.TopGenres[0].Display != "Rock" || st.TopGenres[0].Count != 2 {
		t.Errorf("top genre = %+v, want Rock with 2", st.TopGenres)
	}
	if len(st.TopArtists) == 0 || st.TopArtists[0].Display != "Alpha" || st.TopArtists[0].Count != 2 {
		t.Errorf("top artist = %+v, want Alpha with 2", st.TopArtists)
	}
	if st.Play.TotalPlays != 3 || st.Play.Finished != 1 || st.Play.Starred != 1 {
		t.Errorf("play stats = %+v, want plays=3 finished=1 starred=1", st.Play)
	}
	if len(st.Play.MostPlayed) == 0 || st.Play.MostPlayed[0].Title != "A" || st.Play.MostPlayed[0].PlayCount != 3 {
		t.Errorf("most played = %+v, want A with 3", st.Play.MostPlayed)
	}
}

// TestDoctorToleratesOlderCatalog verifies doctor can inspect a catalog written
// by an older binary without querying tables added by later migrations.
func TestDoctorToleratesOlderCatalog(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))

	rw := openManaged(t, ctx, db, root)
	if _, err := rw.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a catalog from an older binary: drop the v3 fingerprint tables and
	// roll the recorded schema version back to 2.
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TABLE fingerprint_term",
		"DROP TABLE fingerprint",
		"DELETE FROM schema_migrations WHERE version >= 3",
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = raw.Close()

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open of an older catalog: %v", err)
	}
	defer ro.Close()

	rep, err := ro.Doctor(ctx)
	if err != nil {
		t.Fatalf("doctor on an older catalog should not fail: %v", err)
	}
	if rep.SchemaVersion != 2 {
		t.Errorf("reported schema v%d, want 2 (the catalog's actual version)", rep.SchemaVersion)
	}
	if !rep.NeedsMigration() {
		t.Error("an older catalog should report NeedsMigration")
	}
	if rep.FingerprintCount != 0 {
		t.Errorf("fingerprint count = %d, want 0 (skipped on a pre-v3 catalog)", rep.FingerprintCount)
	}
	if rep.ItemCount != 1 {
		t.Errorf("item count = %d, want 1 (works on the v1 tables)", rep.ItemCount)
	}
}

// TestWriteOwnershipConflict verifies a second writer is refused via the flock.
func TestWriteOwnershipConflict(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	first := openManaged(t, ctx, db, root)
	_ = first

	_, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged}},
	})
	if err == nil {
		t.Fatal("second writer should be refused while the first holds the lock")
	}
	if !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("want CodeConflict, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestReadOnlyRefusesMutations verifies a read-only library cannot mutate but
// can read, and that read-only opens do not contend for the write lock.
func TestReadOnlyRefusesMutations(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Artist", "Album", 1))

	// Seed via a read-write session, then close it.
	rw := openManaged(t, ctx, db, root)
	if _, err := rw.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	items, err := ro.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		t.Fatalf("read-only query: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("read-only query returned %d items, want 1", len(items))
	}

	if _, err := ro.Scan(ctx, waxbin.ScanRequest{}); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("read-only scan: want CodeUnsupported, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestOrganizeLeavesInPlaceLibraryFiles verifies organize only moves files in
// the managed library and never touches an in-place library's files.
func TestOrganizeLeavesInPlaceLibraryFiles(t *testing.T) {
	ctx := context.Background()
	managedRoot := t.TempDir()
	inplaceRoot := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	managedFile := filepath.Join(managedRoot, "m.mp3")
	inplaceFile := filepath.Join(inplaceRoot, "i.mp3")
	// Distinct audio payloads so the two files have distinct essence (and thus
	// are two separate items, not one deduped item).
	audioM := testaudio.DefaultAudio()
	audioI := testaudio.DefaultAudio()
	audioI[10] = 0x55
	writeFile(t, managedFile, testaudio.BuildMP3WithAudio("Managed Song", "M Artist", "M Album", 1, audioM))
	writeFile(t, inplaceFile, testaudio.BuildMP3WithAudio("InPlace Song", "I Artist", "I Album", 2, audioI))

	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: managedRoot, Mode: model.ModeManaged, Profile: "waxbin-native"},
			{Path: inplaceRoot, Mode: model.ModeInPlace},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 1 {
		t.Fatalf("plan should move only the managed file, got %d pending", plan.Pending())
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	if !fileExists(inplaceFile) {
		t.Fatal("in-place library file was moved; ModeInPlace was violated")
	}
	if fileExists(managedFile) {
		t.Fatal("managed file was not moved")
	}
}

// TestScanRelativeSubPath verifies a relative --sub-path is resolved under the
// library root rather than rejected.
func TestScanRelativeSubPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "sub1", "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))
	writeFile(t, filepath.Join(root, "sub2", "b.mp3"), testaudio.BuildMP3("B", "Br", "Bl", 2))

	lib := openManaged(t, ctx, db, root)

	res, err := lib.Scan(ctx, waxbin.ScanRequest{SubPath: "sub1"})
	if err != nil {
		t.Fatalf("scan sub-path: %v", err)
	}
	if res.Total.AudioFiles != 1 {
		t.Fatalf("expected 1 file under sub1, got %d", res.Total.AudioFiles)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(items) != 1 || items[0].Title != "A" {
		t.Fatalf("expected only sub1's track, got %+v", items)
	}
}

// TestOpenRejectsOverlappingRoots verifies embedders get the same root isolation
// as the CLI because Open runs config validation.
func TestOpenRejectsOverlappingRoots(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	_, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: filepath.Join(base, "music"), Mode: model.ModeManaged},
			{Path: filepath.Join(base, "music", "sub"), Mode: model.ModeInPlace}, // nested
		},
	})
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for overlapping roots, got %v (code %s)", err, waxerr.CodeOf(err))
	}
}

// TestReadOnlyConcurrentWithWriter verifies a read-only consumer can open and
// query while another session holds the write lock (what the read-only CLI
// commands rely on).
func TestReadOnlyConcurrentWithWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("A", "Ar", "Al", 1))

	rw := openManaged(t, ctx, db, root) // holds the write lock for the test
	if _, err := rw.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("seed scan: %v", err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open while write-locked should succeed: %v", err)
	}
	defer ro.Close()

	items, err := ro.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 {
		t.Fatalf("concurrent read-only query: err=%v len=%d", err, len(items))
	}
}

// TestOrganizeMoveFailureRollsBack verifies a colliding destination fails the
// action (reported, not fatal) and leaves the source in place.
func TestOrganizeMoveFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Occupy the destination so the move collides.
	dst := filepath.Join(root, "The Foobars", "Night Moves", "03 - Midnight Drive.mp3")
	writeFile(t, dst, []byte("occupied"))

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Moved != 0 || rep.Errored != 1 {
		t.Fatalf("expected 0 moved / 1 errored on collision, got %+v", rep)
	}
	if !fileExists(src) {
		t.Fatal("source should remain after a failed move")
	}
}

// TestOrganizeRelocatesSidecars verifies that organizing an audio file carries
// its same-basename sidecars (renamed to the new basename) and the directory
// cover art along with it.
func TestOrganizeRelocatesSidecars(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Midnight Drive", "The Foobars", "Night Moves", 3))
	writeFile(t, filepath.Join(root, "song.lrc"), []byte("[00:00.00]lyric"))
	writeFile(t, filepath.Join(root, "cover.jpg"), []byte("jpegdata"))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Moved != 1 {
		t.Fatalf("audio not moved: %+v", rep)
	}
	if rep.SidecarsMoved != 2 {
		t.Fatalf("sidecars moved = %d, want 2 (lrc + cover)", rep.SidecarsMoved)
	}

	dstDir := filepath.Join(root, "The Foobars", "Night Moves")
	if !fileExists(filepath.Join(dstDir, "03 - Midnight Drive.lrc")) {
		t.Error("lyrics sidecar was not renamed and moved with the audio")
	}
	if !fileExists(filepath.Join(dstDir, "cover.jpg")) {
		t.Error("directory cover art was not moved with the audio")
	}
	if fileExists(filepath.Join(root, "song.lrc")) {
		t.Error("lyrics sidecar left behind at source")
	}
}

// TestDeleteTrashRestoreRoundTrip verifies a user delete moves the file to the
// trash and archives its item, a re-scan does not resurrect it, and restore
// brings both the file and the item back.
func TestDeleteTrashRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	itemPID := items[0].PID

	// TRASH
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPID}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if plan.Pending() != 1 {
		t.Fatalf("delete plan pending = %d, want 1", plan.Pending())
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	if rep.Trashed != 1 {
		t.Fatalf("delete report = %+v, want trashed 1", rep)
	}
	if fileExists(src) {
		t.Fatal("source file should be gone after trashing")
	}
	got, err := lib.Get(ctx, itemPID)
	if err != nil {
		t.Fatalf("item should be preserved (archived) after trash: %v", err)
	}
	if got.State != model.StateArchived {
		t.Fatalf("item state = %s, want archived", got.State)
	}
	// Trashing the file must keep the derived data consistent (the rollups'
	// duration sums from the now-absent file, so they are recomputed on delete).
	if dr, err := lib.VerifyDerived(ctx); err != nil || !dr.Consistent() {
		t.Fatalf("derived data inconsistent after trash: %+v (err %v)", dr, err)
	}

	entries, err := lib.Trash(ctx, false, 0)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trash list: err=%v len=%d, want 1", err, len(entries))
	}

	// A re-scan must not resurrect the trashed file (the trash dir is skipped).
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("re-scan: %v", err)
	}
	if again, _ := lib.Get(ctx, itemPID); again.State != model.StateArchived {
		t.Fatal("re-scan resurrected a trashed item; the trash dir must be skipped")
	}

	// RESTORE
	if err := lib.RestoreTrash(ctx, entries[0].PID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !fileExists(src) {
		t.Fatal("restore did not return the file to its original path")
	}
	restored, err := lib.Get(ctx, itemPID)
	if err != nil {
		t.Fatalf("get after restore: %v", err)
	}
	if restored.State != model.StatePresent {
		t.Fatalf("item state after restore = %s, want present", restored.State)
	}
	if restored.FilePID == "" {
		t.Fatal("restored item has no backing file")
	}
	if dr, err := lib.VerifyDerived(ctx); err != nil || !dr.Consistent() {
		t.Fatalf("derived data inconsistent after restore: %+v (err %v)", dr, err)
	}
	// The trash entry is now marked restored.
	if active, _ := lib.Trash(ctx, false, 0); len(active) != 0 {
		t.Fatalf("restored entry should leave the active trash empty, got %d", len(active))
	}
}

// TestPruneBypassesTrash verifies pruning deletes from disk immediately, records
// no undo entry, and still preserves the logical item.
func TestPruneBypassesTrash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	itemPID := items[0].PID

	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{itemPID}, model.DeletePrune)
	if err != nil {
		t.Fatalf("plan prune: %v", err)
	}
	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil {
		t.Fatalf("apply prune: %v", err)
	}
	if rep.Deleted != 1 || rep.Trashed != 0 {
		t.Fatalf("prune report = %+v, want deleted 1 trashed 0", rep)
	}
	if rep.ReclaimedBytes <= 0 {
		t.Errorf("prune should report reclaimed bytes, got %d", rep.ReclaimedBytes)
	}
	if fileExists(src) {
		t.Fatal("prune should remove the file from disk")
	}
	if entries, _ := lib.Trash(ctx, false, 0); len(entries) != 0 {
		t.Fatalf("prune must not write a trash entry, got %d", len(entries))
	}
	if got, _ := lib.Get(ctx, itemPID); got.State != model.StateArchived {
		t.Fatal("pruned item should be preserved as archived")
	}
}

// TestInboxImportAndDedup verifies importing a staging folder places and catalogs
// the file under the profile, records an import batch, and skips a duplicate.
func TestInboxImportAndDedup(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	inboxDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	staged := filepath.Join(inboxDir, "incoming.mp3")
	writeFile(t, staged, testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)

	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan import: %v", err)
	}
	if plan.Importable() != 1 {
		t.Fatalf("import plan importable = %d, want 1", plan.Importable())
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if rep.Imported != 1 || rep.Duplicates != 0 {
		t.Fatalf("import report = %+v, want imported 1", rep)
	}

	// The file landed under the profile and is cataloged; the staged copy is gone.
	wantPath := filepath.Join(root, "Artist", "Album", "01 - Song.mp3")
	if !fileExists(wantPath) {
		t.Fatalf("imported file not at %s", wantPath)
	}
	if fileExists(staged) {
		t.Fatal("staged file should be moved out of the inbox")
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if len(items) != 1 || items[0].Title != "Song" {
		t.Fatalf("imported item not cataloged: %+v", items)
	}

	// An import batch was recorded.
	batches, err := lib.ImportBatches(ctx, 10)
	if err != nil || len(batches) != 1 || batches[0].Imported != 1 {
		t.Fatalf("import batch not recorded: err=%v batches=%+v", err, batches)
	}

	// Re-staging the same audio is detected as a duplicate and not imported again.
	writeFile(t, filepath.Join(inboxDir, "again.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	plan2, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan import 2: %v", err)
	}
	if plan2.Importable() != 0 {
		t.Fatalf("duplicate should not be importable, got %d importable", plan2.Importable())
	}
	rep2, err := lib.ApplyImport(ctx, plan2)
	if err != nil {
		t.Fatalf("apply import 2: %v", err)
	}
	if rep2.Duplicates != 1 || rep2.Imported != 0 {
		t.Fatalf("second import = %+v, want 1 duplicate 0 imported", rep2)
	}
}

// TestBackupRedactAndRestore verifies a full backup contains the secret table, a
// redacted backup does not, and a restored backup re-opens with its catalog and
// secrets intact.
func TestBackupRedactAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := lib.SetSecret(ctx, "musicbrainz", "token-123"); err != nil {
		t.Fatalf("set secret: %v", err)
	}

	plain := filepath.Join(t.TempDir(), "plain.db")
	redacted := filepath.Join(t.TempDir(), "redacted.db")
	if err := lib.Backup(ctx, plain, false); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if err := lib.Backup(ctx, redacted, true); err != nil {
		t.Fatalf("redacted backup: %v", err)
	}
	if n := secretCount(t, plain); n != 1 {
		t.Fatalf("plain backup secret count = %d, want 1", n)
	}
	if n := secretCount(t, redacted); n != 0 {
		t.Fatalf("redacted backup secret count = %d, want 0", n)
	}

	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Restore the full backup to a fresh path and re-open read-write.
	restored := filepath.Join(t.TempDir(), "restored.db")
	if err := port.Restore(ctx, plain, restored, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	rlib, err := waxbin.Open(ctx, waxbin.Options{DBPath: restored})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer rlib.Close()
	items, err := rlib.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 {
		t.Fatalf("restored query: err=%v len=%d, want 1", err, len(items))
	}
	if sec, err := rlib.GetSecret(ctx, "musicbrainz"); err != nil || sec != "token-123" {
		t.Fatalf("restored secret = %q (err %v), want token-123", sec, err)
	}

	// Portable relocate: re-point the single library at a new root and confirm the
	// cataloged paths follow.
	newRoot := t.TempDir()
	libs, _ := rlib.Libraries(ctx)
	if err := rlib.RelocateRoot(ctx, libs[0].PID, newRoot); err != nil {
		t.Fatalf("relocate: %v", err)
	}
	moved, _ := rlib.Query(ctx, query.New(query.EntityItems).Build())
	if len(moved) != 1 || !strings.HasPrefix(moved[0].DisplayPath, newRoot) {
		t.Fatalf("relocate did not re-point file paths: %q (want prefix %q)", moved[0].DisplayPath, newRoot)
	}
}

// TestLogicalExport verifies the JSON export carries metadata and user state, a
// versioned manifest, and never secrets.
func TestLogicalExport(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	writeFile(t, filepath.Join(root, "a.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, _ := lib.Query(ctx, query.New(query.EntityItems).Build())
	if err := lib.Playback().SetStar(ctx, "", items[0].PID, true); err != nil {
		t.Fatalf("star: %v", err)
	}
	if err := lib.SetSecret(ctx, "musicbrainz", "token-xyz"); err != nil {
		t.Fatalf("secret: %v", err)
	}

	var buf bytes.Buffer
	man, err := lib.Export(ctx, &buf)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if man.Format != port.ExportFormat || man.Items != 1 || man.PlayStates < 1 {
		t.Fatalf("manifest = %+v, want 1 item / >=1 play state", man)
	}
	if strings.Contains(buf.String(), "token-xyz") {
		t.Fatal("logical export must never contain secrets")
	}

	snap, err := port.ReadSnapshot(&buf)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if len(snap.Items) != 1 || snap.Items[0].Title != "Song" {
		t.Fatalf("exported item wrong: %+v", snap.Items)
	}
	if len(snap.PlayState) < 1 || !snap.PlayState[0].Starred {
		t.Fatalf("exported play state should carry the star: %+v", snap.PlayState)
	}
}

// TestRestoreReplacesAtomically verifies a forced restore over an existing
// catalog swaps in the backup content, leaves no temp file, and that a no-force
// restore is refused without disturbing the target.
func TestRestoreReplacesAtomically(t *testing.T) {
	ctx := context.Background()

	// Backup A: a catalog whose single item is titled "FromBackup".
	rootA := t.TempDir()
	dbA := filepath.Join(t.TempDir(), "a.db")
	writeFile(t, filepath.Join(rootA, "a.mp3"), testaudio.BuildMP3("FromBackup", "Artist", "Album", 1))
	libA := openManaged(t, ctx, dbA, rootA)
	if _, err := libA.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan A: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := libA.Backup(ctx, backup, false); err != nil {
		t.Fatalf("backup: %v", err)
	}
	_ = libA.Close()

	// Target: a different existing catalog (item titled "Original").
	rootB := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.db")
	writeFile(t, filepath.Join(rootB, "b.mp3"), testaudio.BuildMP3("Original", "Artist", "Album", 2))
	libB := openManaged(t, ctx, target, rootB)
	if _, err := libB.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan B: %v", err)
	}
	_ = libB.Close()

	// No-force restore over an existing catalog is refused, target untouched.
	if err := port.Restore(ctx, backup, target, false); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("no-force restore over existing: want CodeConflict, got %v", err)
	}

	// Forced restore swaps in the backup content.
	if err := port.Restore(ctx, backup, target, true); err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	if fileExists(target + ".restore-tmp") {
		t.Fatal("restore left its temp file behind")
	}
	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: target, ReadOnly: true})
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer ro.Close()
	items, err := ro.Query(ctx, query.New(query.EntityItems).Build())
	if err != nil || len(items) != 1 || items[0].Title != "FromBackup" {
		t.Fatalf("restored content wrong: err=%v items=%+v", err, items)
	}
}

// TestInboxQuarantinesCaseCollision verifies that importing a file whose
// destination differs only by case from an already-cataloged file is quarantined
// (it would coexist on Linux but collide on a case-insensitive filesystem).
func TestInboxQuarantinesCaseCollision(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	inboxDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	lib := openManaged(t, ctx, db, root)

	// Import "Song" first; it lands at Artist/Album/01 - Song.mp3 and is cataloged.
	writeFile(t, filepath.Join(inboxDir, "a.mp3"),
		testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	planA, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan A: %v", err)
	}
	if _, err := lib.ApplyImport(ctx, planA); err != nil {
		t.Fatalf("apply A: %v", err)
	}

	// Stage "song" (distinct essence, so it is not an essence duplicate) which
	// renders to 01 - song.mp3, a case collision with the cataloged 01 - Song.mp3.
	writeFile(t, filepath.Join(inboxDir, "b.mp3"),
		testaudio.BuildMP3WithAudio("song", "Artist", "Album", 1, testaudio.AudioWithSeed(2)))
	planB, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan B: %v", err)
	}
	if planB.Importable() != 0 {
		t.Fatalf("case-colliding file should not be importable, got %d importable", planB.Importable())
	}
	rep, err := lib.ApplyImport(ctx, planB)
	if err != nil {
		t.Fatalf("apply B: %v", err)
	}
	if rep.Quarantined != 1 || rep.Imported != 0 {
		t.Fatalf("import B = %+v, want quarantined 1 imported 0", rep)
	}
}

// secretCount reads the number of rows in a backup's secret table.
func secretCount(t *testing.T, dbPath string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow("SELECT COUNT(*) FROM secret").Scan(&n); err != nil {
		t.Fatalf("count secrets in %s: %v", dbPath, err)
	}
	return n
}

// TestInboxImportCarriesSidecars verifies an import brings a file's sidecars
// (same-basename lyrics renamed to the new basename, plus directory cover art)
// into the managed tree alongside the audio, so importing does not leave them
// behind.
func TestInboxImportCarriesSidecars(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	inboxDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	writeFile(t, filepath.Join(inboxDir, "track.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	writeFile(t, filepath.Join(inboxDir, "track.lrc"), []byte("[00:00.00]hi"))
	writeFile(t, filepath.Join(inboxDir, "cover.jpg"), []byte("img"))

	lib := openManaged(t, ctx, db, root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan import: %v", err)
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if rep.Imported != 1 || rep.Sidecars != 2 {
		t.Fatalf("import report = %+v, want imported 1 sidecars 2", rep)
	}

	dstDir := filepath.Join(root, "Artist", "Album")
	if !fileExists(filepath.Join(dstDir, "01 - Song.lrc")) {
		t.Error("lyrics sidecar not carried/renamed into the managed tree")
	}
	if !fileExists(filepath.Join(dstDir, "cover.jpg")) {
		t.Error("directory cover art not carried into the managed tree")
	}
	if fileExists(filepath.Join(inboxDir, "track.lrc")) {
		t.Error("lyrics sidecar left behind in the inbox")
	}
}

// TestInboxSkipsWithinBatchDuplicate verifies that under the skip policy, two
// staged copies of the same recording (same audio essence, different tags, hence
// different destinations) do not both import. The second is a duplicate of the
// first within the same batch, not just of the prior catalog.
func TestInboxSkipsWithinBatchDuplicate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	inboxDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	// Same default audio (so identical essence), different titles (so different
	// rendered destinations).
	writeFile(t, filepath.Join(inboxDir, "a.mp3"), testaudio.BuildMP3("Song A", "Artist", "Album", 1))
	writeFile(t, filepath.Join(inboxDir, "b.mp3"), testaudio.BuildMP3("Song B", "Artist", "Album", 2))

	lib := openManaged(t, ctx, db, root)
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir, DupPolicy: model.DupSkip})
	if err != nil {
		t.Fatalf("plan import: %v", err)
	}
	if plan.Importable() != 1 {
		t.Fatalf("within-batch duplicate not detected: %d importable, want 1", plan.Importable())
	}
	rep, err := lib.ApplyImport(ctx, plan)
	if err != nil {
		t.Fatalf("apply import: %v", err)
	}
	if rep.Imported != 1 || rep.Duplicates != 1 {
		t.Fatalf("import report = %+v, want imported 1 duplicate 1", rep)
	}
}

func openManaged(t *testing.T, ctx context.Context, db, root string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots:  []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
	})
	if err != nil {
		t.Fatalf("open library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func hasDoneJob(jobs []*model.Job, kind string) bool {
	for _, j := range jobs {
		if j.Kind == kind && j.State == model.JobDone {
			return true
		}
	}
	return false
}

// TestEndToEndAudiobook drives the full pipeline for an audiobook: a .m4b file
// (valid MP3 bytes, so WaxLabel parses it and the extension classifies it as a
// book) is scanned, read back as a book with chapters, laid out by the audiobook
// template, found by search, and leaves the derived data consistent.
func TestEndToEndAudiobook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	// Album is the book title; the album artist is the author. The .m4b extension
	// is the audiobook signal.
	src := filepath.Join(root, "incoming", "the-hobbit.m4b")
	writeFile(t, src, testaudio.BuildMP3("The Hobbit", "J.R.R. Tolkien", "The Hobbit", 1))

	lib := openManaged(t, ctx, db, root)

	scanRes, err := lib.Scan(ctx, waxbin.ScanRequest{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if scanRes.Total.AudioFiles != 1 || scanRes.Total.ItemsCreated != 1 {
		t.Fatalf("scan tally = %+v, want 1 audio / 1 created", scanRes.Total)
	}

	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
	if err != nil {
		t.Fatalf("query books: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("kind=book query returned %d, want 1", len(books))
	}
	bookPID := books[0].PID
	if books[0].Kind != model.KindBook || books[0].Title != "The Hobbit" || books[0].Artist != "J.R.R. Tolkien" {
		t.Fatalf("book view wrong: %+v", books[0])
	}

	// Full detail: author contributor and a synthesized whole-file chapter.
	d, err := lib.Book(ctx, bookPID)
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if len(d.Authors) != 1 || d.Authors[0] != "J.R.R. Tolkien" {
		t.Errorf("authors = %v, want [J.R.R. Tolkien]", d.Authors)
	}
	if len(d.Chapters) != 1 {
		t.Errorf("chapters = %d, want 1 (synthesized whole-file)", len(d.Chapters))
	}

	// Chapter-level resume: a recorded position resolves to a chapter.
	if err := lib.Playback().Checkpoint(ctx, "", bookPID, 10); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	st, ch, err := lib.BookResume(ctx, "", bookPID)
	if err != nil {
		t.Fatalf("BookResume: %v", err)
	}
	if st.PositionMS != 10 || ch == nil {
		t.Errorf("resume = pos %d chapter %v, want pos 10 and a chapter", st.PositionMS, ch)
	}

	// Organize lays the book out under the audiobook template (author/book/file).
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	var bookAction bool
	for _, a := range plan.Actions {
		if a.ItemPID == bookPID {
			bookAction = true
			want := filepath.Join("j.r.r. tolkien", "The Hobbit", "The Hobbit.m4b")
			if a.RelDst != want {
				t.Errorf("audiobook dst = %q, want %q", a.RelDst, want)
			}
		}
	}
	if !bookAction {
		t.Error("organize plan did not include the book")
	}

	// Search surfaces the book in its own group.
	res, err := lib.Search(ctx, "hobbit", read.SearchOptions{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Books) != 1 || res.Books[0].PID != bookPID {
		t.Fatalf("search books = %+v, want the hobbit", res.Books)
	}

	if rep, err := lib.VerifyDerived(ctx); err != nil {
		t.Fatalf("verify: %v", err)
	} else if !rep.Consistent() {
		t.Errorf("derived data inconsistent after audiobook flow: %+v", rep)
	}
}

// TestEndToEndMultiFileAudiobookOrganize verifies organize moves EVERY part of a
// multi-file audiobook into one book folder rather than relocating only the
// representative file and splitting the book.
func TestEndToEndMultiFileAudiobookOrganize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	// Two .m4b parts of one book (same album + album-artist groups them), with
	// distinct audio so they are two real files.
	writeFile(t, filepath.Join(root, "in", "p1.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Sanderson", "Mistborn", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "in", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Sanderson", "Mistborn", 2, testaudio.AudioWithSeed(0x55)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}

	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1 (two parts grouped)", len(books))
	}

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 2 {
		t.Fatalf("planned moves = %d, want 2 (every part)", plan.Pending())
	}
	rep, err := lib.ApplyOrganize(ctx, plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if rep.Moved != 2 {
		t.Fatalf("moved = %d, want 2", rep.Moved)
	}

	// Both parts now live together in the author/book folder; none left behind.
	inFolder, _ := filepath.Glob(filepath.Join(root, "sanderson", "Mistborn", "*.m4b"))
	if len(inFolder) != 2 {
		t.Errorf("parts in book folder = %v, want 2 co-located", inFolder)
	}
	leftover, _ := filepath.Glob(filepath.Join(root, "in", "*.m4b"))
	if len(leftover) != 0 {
		t.Errorf("parts left behind in incoming: %v", leftover)
	}
}

// TestMultiFileBookSameBasenameOrganize verifies the multi-file organize fix: two
// parts that share a basename across source folders are renamed to unique,
// reading-ordered names in the book folder rather than colliding (which would skip
// all but one and split the book).
func TestMultiFileBookSameBasenameOrganize(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	// Same basename "track.m4b" in two different source folders; same album+author
	// groups them into one book; distinct audio makes them two real files.
	writeFile(t, filepath.Join(root, "a", "track.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Auth", "OneBook", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "b", "track.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Auth", "OneBook", 2, testaudio.AudioWithSeed(0x77)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, _ := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1 grouped book", len(books))
	}

	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), "waxbin-native")
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.Pending() != 2 {
		t.Fatalf("planned moves = %d, want 2 (no basename collision)", plan.Pending())
	}
	if _, err := lib.ApplyOrganize(ctx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	parts, _ := filepath.Glob(filepath.Join(root, "auth", "OneBook", "*.m4b"))
	if len(parts) != 2 {
		t.Errorf("parts in book folder = %v, want 2 uniquely-named", parts)
	}
}

// TestTrashExpandsMultiFileBook verifies deleting a multi-file audiobook plans a
// removal for EVERY part, not just the representative primary (which would strand
// the rest on disk).
func TestTrashExpandsMultiFileBook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")

	writeFile(t, filepath.Join(root, "a", "p1.m4b"),
		testaudio.BuildMP3WithAudio("Part 1", "Auth", "OneBook", 1, testaudio.DefaultAudio()))
	writeFile(t, filepath.Join(root, "a", "p2.m4b"),
		testaudio.BuildMP3WithAudio("Part 2", "Auth", "OneBook", 2, testaudio.AudioWithSeed(0x91)))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	books, _ := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1", len(books))
	}

	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{books[0].PID}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("plan delete: %v", err)
	}
	if plan.Pending() != 2 {
		t.Fatalf("planned deletions = %d, want 2 (both parts)", plan.Pending())
	}
	if _, err := lib.ApplyDelete(ctx, plan); err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	// Both parts are gone from their original location (moved to the trash).
	left, _ := filepath.Glob(filepath.Join(root, "a", "*.m4b"))
	if len(left) != 0 {
		t.Errorf("parts left on disk after delete: %v", left)
	}
}
