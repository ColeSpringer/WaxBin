package waxbin_test

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
