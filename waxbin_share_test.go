package waxbin_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"

	_ "modernc.org/sqlite"
)

// exportRefs turns a set of local item PIDs into portable refs through the real facade
// surface: a static playlist, then ExportPlaylistRefs. It is how a host would ship a
// playlist to another catalog.
func exportRefs(t *testing.T, ctx context.Context, lib *waxbin.Library, pids ...model.PID) []model.PortableRef {
	t.Helper()
	pl, err := lib.Playlists().CreateStatic(ctx, "share", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	if err := lib.Playlists().Set(ctx, pl, pids); err != nil {
		t.Fatalf("set playlist: %v", err)
	}
	refs, err := lib.ExportPlaylistRefs(ctx, pl, "")
	if err != nil {
		t.Fatalf("export refs: %v", err)
	}
	return refs
}

// rawExec runs one statement against the catalog file over a separate connection. It
// stands in for enrichment writing a strong id (recording MBID / book ASIN) that the
// scan path does not set from test fixtures, so the strong-id rung can be driven
// end-to-end through the facade.
func rawExec(t *testing.T, db, stmt string, args ...any) {
	t.Helper()
	raw, err := sql.Open("sqlite", "file:"+db+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(), stmt, args...); err != nil {
		t.Fatalf("raw exec %q: %v", stmt, err)
	}
}

func scanLib(t *testing.T, ctx context.Context, lib *waxbin.Library) {
	t.Helper()
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// TestResolveRefEssence covers the exact-bytes rung: a shared rip resolves by essence
// across two catalogs, a bogus essence misses, and when the same bytes back two
// different-kind items the rung is disambiguated by ref.Kind.
func TestResolveRefEssence(t *testing.T) {
	ctx := context.Background()

	// Catalog A holds the source rip; export its portable ref.
	rootA := t.TempDir()
	dbA := filepath.Join(t.TempDir(), "a.db")
	writeFile(t, filepath.Join(rootA, "song.mp3"),
		testaudio.BuildMP3WithAudio("Shared Song", "Shared Artist", "Shared Album", 1, testaudio.AudioWithSeed(1)))
	libA := openManaged(t, ctx, dbA, rootA)
	scanLib(t, ctx, libA)
	aPID := itemPIDByTitle(t, ctx, libA, "Shared Song")
	ref := exportRefs(t, ctx, libA, aPID)[0]
	if ref.Essence == "" {
		t.Fatal("exported ref has no essence")
	}

	// Catalog B holds the same bytes (same essence) plus a decoy.
	rootB := t.TempDir()
	dbB := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(rootB, "copy.mp3"),
		testaudio.BuildMP3WithAudio("Shared Song", "Shared Artist", "Shared Album", 1, testaudio.AudioWithSeed(1)))
	writeFile(t, filepath.Join(rootB, "decoy.mp3"),
		testaudio.BuildMP3WithAudio("Other", "Other", "Other", 1, testaudio.AudioWithSeed(2)))
	libB := openManaged(t, ctx, dbB, rootB)
	scanLib(t, ctx, libB)
	bPID := itemPIDByTitle(t, ctx, libB, "Shared Song")

	item, rung, err := libB.ResolveRef(ctx, ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rung != model.MatchEssence || item == nil || item.PID != bPID {
		t.Fatalf("essence resolve = rung %s pid %v, want essence -> %s", rung, item, bPID)
	}

	// A ref that matches nothing is a clean miss, not an error.
	if item, rung, err := libB.ResolveRef(ctx, model.PortableRef{Essence: "sha256:ghost"}); err != nil || rung != model.MatchNone || item != nil {
		t.Fatalf("ghost essence = rung %s item %v err %v, want none", rung, item, err)
	}

	// Cross-kind: the same audio bytes back both a track and a book in one catalog, so
	// the essence rung has two hits and must disambiguate by ref.Kind.
	rootX := t.TempDir()
	dbX := filepath.Join(t.TempDir(), "x.db")
	writeFile(t, filepath.Join(rootX, "track.mp3"),
		testaudio.BuildMP3WithAudio("Track Kind", "TArtist", "TAlbum", 1, testaudio.AudioWithSeed(9)))
	writeFile(t, filepath.Join(rootX, "book.m4b"),
		testaudio.BuildMP3WithAudio("Book Kind", "BAuthor", "", 1, testaudio.AudioWithSeed(9)))
	libX := openManaged(t, ctx, dbX, rootX)
	scanLib(t, ctx, libX)
	trackPID := itemPIDByTitle(t, ctx, libX, "Track Kind")
	bookPID := itemPIDByTitle(t, ctx, libX, "Book Kind")

	trackRef := exportRefs(t, ctx, libX, trackPID)[0]
	bookRef := exportRefs(t, ctx, libX, bookPID)[0]
	if trackRef.Essence == "" || trackRef.Essence != bookRef.Essence {
		t.Fatalf("track and book should share one essence; got %q vs %q", trackRef.Essence, bookRef.Essence)
	}
	if trackRef.Kind != model.KindTrack || bookRef.Kind != model.KindBook {
		t.Fatalf("exported kinds wrong: track=%s book=%s", trackRef.Kind, bookRef.Kind)
	}

	// Resolving each ref against the same catalog picks the item of the ref's kind.
	if item, rung, err := libX.ResolveRef(ctx, trackRef); err != nil || rung != model.MatchEssence || item.PID != trackPID {
		t.Fatalf("cross-kind track = rung %s pid %v err %v, want the track", rung, item, err)
	}
	if item, rung, err := libX.ResolveRef(ctx, bookRef); err != nil || rung != model.MatchEssence || item.PID != bookPID {
		t.Fatalf("cross-kind book = rung %s pid %v err %v, want the book", rung, item, err)
	}
}

// TestResolveRefFingerprint covers the fingerprint rung: a different encoding of the
// same recording (a transcode with different bytes) resolves by fingerprint, while an
// algorithm mismatch and a too-short fingerprint both fall through cleanly.
func TestResolveRefFingerprint(t *testing.T) {
	ctx := context.Background()
	const rate = 22050
	orig := testaudio.RichSignal(rate, 20, testaudio.MusicalPartials, 1)
	transcoded := testaudio.Reencode(orig, 0.85, 42) // same recording, different bytes/essence
	other := testaudio.RichSignal(rate, 20, testaudio.AltPartials, 7)

	// Catalog A: the original; analyze so its ref carries a fingerprint.
	rootA := t.TempDir()
	dbA := filepath.Join(t.TempDir(), "a.db")
	writeFile(t, filepath.Join(rootA, "alpha.wav"), testaudio.EncodeWAV16(rate, orig))
	libA := openManaged(t, ctx, dbA, rootA)
	scanLib(t, ctx, libA)
	if _, err := libA.Analyze(ctx, waxbin.AnalyzeOptions{}); err != nil {
		t.Fatalf("analyze A: %v", err)
	}
	alphaRef := exportRefs(t, ctx, libA, itemPIDByTitle(t, ctx, libA, "alpha"))[0]
	if len(alphaRef.Fingerprint) == 0 {
		t.Skip("analyze produced no fingerprint (no decoder available); skipping fingerprint rung")
	}

	// Catalog B: the transcode (a fingerprint match) plus an unrelated track.
	rootB := t.TempDir()
	dbB := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(rootB, "beta.wav"), testaudio.EncodeWAV16(rate, transcoded))
	writeFile(t, filepath.Join(rootB, "gamma.wav"), testaudio.EncodeWAV16(rate, other))
	libB := openManaged(t, ctx, dbB, rootB)
	scanLib(t, ctx, libB)
	if _, err := libB.Analyze(ctx, waxbin.AnalyzeOptions{}); err != nil {
		t.Fatalf("analyze B: %v", err)
	}
	betaPID := itemPIDByTitle(t, ctx, libB, "beta")

	// alpha's essence is absent from B, so the ladder falls to the fingerprint rung and
	// verifies the transcode.
	item, rung, err := libB.ResolveRef(ctx, alphaRef)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rung != model.MatchFingerprint || item == nil || item.PID != betaPID {
		t.Fatalf("fingerprint resolve = rung %s pid %v, want fingerprint -> %s (beta)", rung, item, betaPID)
	}

	// An algorithm mismatch scores against no candidates and, with no descriptive tags on
	// a WAV, misses cleanly.
	algoMismatch := alphaRef
	algoMismatch.FingerprintAlgo = 100 // Chromaprint; B stores only pure-Go (algo 1)
	if _, rung, err := libB.ResolveRef(ctx, algoMismatch); err != nil || rung == model.MatchFingerprint {
		t.Fatalf("algo mismatch = rung %s err %v, must not match by fingerprint", rung, err)
	}

	// A short/corrupt fingerprint yields zero terms and skips the rung without a SQL error.
	shortFP := alphaRef
	shortFP.Fingerprint = []byte{1, 2, 3}
	if _, rung, err := libB.ResolveRef(ctx, shortFP); err != nil || rung == model.MatchFingerprint {
		t.Fatalf("short fingerprint = rung %s err %v, must skip cleanly", rung, err)
	}
}

// TestResolveRefDescriptive covers the fuzzy-metadata rung for tracks: album is a
// tie-breaker (not a filter), a duration off by more than the tolerance misses, an empty
// artist skips the rung, and more than one survivor is resolved only by a unique album.
func TestResolveRefDescriptive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(root, "solo.mp3"),
		testaudio.BuildMP3WithAudio("Solo Song", "Solo Artist", "Real Album", 1, testaudio.AudioWithSeed(11)))
	// Two same-artist/same-title tracks on different albums, for the >1-survivor tie-break.
	writeFile(t, filepath.Join(root, "dup1.mp3"),
		testaudio.BuildMP3WithAudio("Dup Song", "Dup Artist", "Album One", 1, testaudio.AudioWithSeed(12)))
	writeFile(t, filepath.Join(root, "dup2.mp3"),
		testaudio.BuildMP3WithAudio("Dup Song", "Dup Artist", "Album Two", 1, testaudio.AudioWithSeed(13)))
	// Two same-artist/same-title tracks, one WITHOUT an album, for the empty-tie-breaker case.
	writeFile(t, filepath.Join(root, "amb1.mp3"),
		testaudio.BuildMP3WithAudio("Amb Song", "Amb Artist", "", 1, testaudio.AudioWithSeed(14)))
	writeFile(t, filepath.Join(root, "amb2.mp3"),
		testaudio.BuildMP3WithAudio("Amb Song", "Amb Artist", "Some Album", 1, testaudio.AudioWithSeed(15)))
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)

	soloPID := itemPIDByTitle(t, ctx, lib, "Solo Song")
	solo, err := lib.Get(ctx, soloPID)
	if err != nil {
		t.Fatalf("get solo: %v", err)
	}

	// Album differs, but a single title survivor still resolves (album is a tie-breaker).
	base := model.PortableRef{Kind: model.KindTrack, Artist: "Solo Artist", Title: "Solo Song", Album: "Some Other Album"}
	if item, rung, err := lib.ResolveRef(ctx, base); err != nil || rung != model.MatchDescriptive || item.PID != soloPID {
		t.Fatalf("album-differs = rung %s item %v err %v, want descriptive -> solo", rung, item, err)
	}

	// A duration far outside the tolerance misses.
	if solo.DurationMS <= 0 {
		t.Fatal("solo item has no duration to gate on")
	}
	offDur := base
	offDur.DurationMS = solo.DurationMS + 100000
	if item, rung, err := lib.ResolveRef(ctx, offDur); err != nil || rung != model.MatchNone || item != nil {
		t.Fatalf("duration off = rung %s item %v err %v, want none", rung, item, err)
	}

	// An empty artist skips the seed entirely.
	noArtist := model.PortableRef{Kind: model.KindTrack, Title: "Solo Song"}
	if _, rung, err := lib.ResolveRef(ctx, noArtist); err != nil || rung != model.MatchNone {
		t.Fatalf("empty artist = rung %s err %v, want none", rung, err)
	}

	// Two survivors: a unique matching album resolves; a non-matching album is ambiguous.
	pick := model.PortableRef{Kind: model.KindTrack, Artist: "Dup Artist", Title: "Dup Song", Album: "Album One"}
	got, rung, err := lib.ResolveRef(ctx, pick)
	if err != nil || rung != model.MatchDescriptive || got == nil || got.Album != "Album One" {
		t.Fatalf("album tie-break = rung %s item %v err %v, want the Album One track", rung, got, err)
	}
	ambiguous := model.PortableRef{Kind: model.KindTrack, Artist: "Dup Artist", Title: "Dup Song", Album: "No Such Album"}
	if item, rung, err := lib.ResolveRef(ctx, ambiguous); err != nil || rung != model.MatchNone || item != nil {
		t.Fatalf("ambiguous album = rung %s item %v err %v, want none", rung, item, err)
	}

	// An empty album tells the tie-break nothing, so with several survivors the result is
	// ambiguous. An item that merely also lacks an album must not win.
	noAlbum := model.PortableRef{Kind: model.KindTrack, Artist: "Amb Artist", Title: "Amb Song"}
	if item, rung, err := lib.ResolveRef(ctx, noAlbum); err != nil || rung != model.MatchNone || item != nil {
		t.Fatalf("empty-album tie-break = rung %s item %v err %v, want none (ambiguous)", rung, item, err)
	}
}

// TestResolveRefBook covers the book path: essence exact, the strong-id rung (an
// enrichment-set ASIN), and the descriptive rung (author + title).
func TestResolveRefBook(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(root, "hobbit.m4b"),
		testaudio.BuildMP3WithAudio("The Hobbit", "Tolkien", "", 1, testaudio.AudioWithSeed(21)))
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	bookPID := itemPIDByTitle(t, ctx, lib, "The Hobbit")
	if got, err := lib.Get(ctx, bookPID); err != nil || got.Kind != model.KindBook {
		t.Fatalf("expected a book item, got kind %v (err %v)", got.Kind, err)
	}

	// Essence exact (a second catalog holding the same bytes).
	rootB := t.TempDir()
	dbB := filepath.Join(t.TempDir(), "b2.db")
	writeFile(t, filepath.Join(rootB, "copy.m4b"),
		testaudio.BuildMP3WithAudio("The Hobbit", "Tolkien", "", 1, testaudio.AudioWithSeed(21)))
	libB := openManaged(t, ctx, dbB, rootB)
	scanLib(t, ctx, libB)
	bookRef := exportRefs(t, ctx, lib, bookPID)[0]
	if item, rung, err := libB.ResolveRef(ctx, bookRef); err != nil || rung != model.MatchEssence {
		t.Fatalf("book essence = rung %s err %v, want essence (item %v)", rung, err, item)
	}

	// Strong-id: an ASIN set on the book resolves it, even against a non-matching essence.
	rawExec(t, db, "UPDATE book SET asin = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)", "B0STRONG", string(bookPID))
	strongID := model.PortableRef{Kind: model.KindBook, ASIN: "b0strong", Essence: "sha256:nomatch"}
	if item, rung, err := lib.ResolveRef(ctx, strongID); err != nil || rung != model.MatchStrongID || item.PID != bookPID {
		t.Fatalf("book strong-id = rung %s item %v err %v, want strongId -> the book", rung, item, err)
	}

	// Descriptive: author + title, no id, no comparable essence.
	descriptive := model.PortableRef{Kind: model.KindBook, Artist: "Tolkien", Title: "The Hobbit", Essence: "sha256:nomatch"}
	if item, rung, err := lib.ResolveRef(ctx, descriptive); err != nil || rung != model.MatchDescriptive || item.PID != bookPID {
		t.Fatalf("book descriptive = rung %s item %v err %v, want descriptive -> the book", rung, item, err)
	}
}

// TestResolveRefStrongIDTrack drives the track recording-MBID rung end-to-end through the
// facade, with the id injected as enrichment would set it.
func TestResolveRefStrongIDTrack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(root, "song.mp3"),
		testaudio.BuildMP3WithAudio("Recording", "Some Artist", "Some Album", 1, testaudio.AudioWithSeed(31)))
	lib := openManaged(t, ctx, db, root)
	scanLib(t, ctx, lib)
	pid := itemPIDByTitle(t, ctx, lib, "Recording")

	rawExec(t, db, "UPDATE track SET mbid = ? WHERE item_id = (SELECT id FROM playable_item WHERE pid = ?)", "REC-XYZ", string(pid))
	// A non-matching essence forces the ladder to the strong-id rung; MBID is case-folded.
	ref := model.PortableRef{Kind: model.KindTrack, MBID: "rec-xyz", Essence: "sha256:nomatch"}
	item, rung, err := lib.ResolveRef(ctx, ref)
	if err != nil || rung != model.MatchStrongID || item == nil || item.PID != pid {
		t.Fatalf("track strong-id = rung %s item %v err %v, want strongId -> the track", rung, item, err)
	}
}

// TestPlaylistRefRoundTrip exports a mixed-kind playlist and resolves it in another
// catalog, checking that order and per-entry rung survive a present/absent mix.
func TestPlaylistRefRoundTrip(t *testing.T) {
	ctx := context.Background()

	// Source catalog: a track and a book.
	rootA := t.TempDir()
	dbA := filepath.Join(t.TempDir(), "a.db")
	writeFile(t, filepath.Join(rootA, "song.mp3"),
		testaudio.BuildMP3WithAudio("A Song", "An Artist", "An Album", 1, testaudio.AudioWithSeed(41)))
	writeFile(t, filepath.Join(rootA, "book.m4b"),
		testaudio.BuildMP3WithAudio("A Book", "An Author", "", 1, testaudio.AudioWithSeed(42)))
	libA := openManaged(t, ctx, dbA, rootA)
	scanLib(t, ctx, libA)
	songPID := itemPIDByTitle(t, ctx, libA, "A Song")
	bookPID := itemPIDByTitle(t, ctx, libA, "A Book")
	refs := exportRefs(t, ctx, libA, songPID, bookPID)
	if len(refs) != 2 {
		t.Fatalf("exported %d refs, want 2 in playlist order", len(refs))
	}
	if refs[0].Kind != model.KindTrack || refs[1].Kind != model.KindBook {
		t.Fatalf("playlist order not preserved: %s, %s", refs[0].Kind, refs[1].Kind)
	}

	// Destination catalog: the same song (present) but NOT the book (absent).
	rootB := t.TempDir()
	dbB := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(rootB, "song.mp3"),
		testaudio.BuildMP3WithAudio("A Song", "An Artist", "An Album", 1, testaudio.AudioWithSeed(41)))
	libB := openManaged(t, ctx, dbB, rootB)
	scanLib(t, ctx, libB)
	wantSong := itemPIDByTitle(t, ctx, libB, "A Song")

	res, err := libB.ResolvePlaylistRefs(ctx, refs)
	if err != nil {
		t.Fatalf("resolve playlist: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("resolved %d entries, want 2 (order preserved)", len(res))
	}
	if res[0].Rung == model.MatchNone || res[0].PID != wantSong {
		t.Fatalf("entry 0 = rung %s pid %s, want the present song %s", res[0].Rung, res[0].PID, wantSong)
	}
	if res[1].Rung != model.MatchNone || res[1].PID != "" {
		t.Fatalf("entry 1 = rung %s pid %s, want a miss (book absent)", res[1].Rung, res[1].PID)
	}
}

// TestImportAcquiredAlreadyPresent covers the receive-path signal: an already-present
// essence is reported under DupSkip AND under DupAllow (independent of policy), and the
// action's Outcome still conveys skipped-vs-imported.
func TestImportAcquiredAlreadyPresent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "b.db")
	lib := openManaged(t, ctx, db, root)

	bytesFor := testaudio.BuildMP3WithAudio("Dup Track", "Dup Artist", "Dup Album", 1, testaudio.AudioWithSeed(51))

	// First import: a fresh file, not yet present.
	first := filepath.Join(acq, "one.mp3")
	writeFile(t, first, bytesFor)
	r1, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: first}, model.KindTrack, waxbin.AcquiredMeta{})
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if r1.AlreadyPresent {
		t.Fatalf("first import should not be already-present: %+v", r1)
	}
	if _, err := lib.ApplyImport(ctx, r1.Plan); err != nil {
		t.Fatalf("apply first: %v", err)
	}
	presentPID := itemPIDByTitle(t, ctx, lib, "Dup Track")

	// Second import (DupSkip): a copy of the same bytes -> already present, skipped.
	second := filepath.Join(t.TempDir(), "two.mp3")
	writeFile(t, second, bytesFor)
	r2, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: second}, model.KindTrack, waxbin.AcquiredMeta{DupPolicy: model.DupSkip})
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if !r2.AlreadyPresent || r2.AlreadyPresentPID != presentPID {
		t.Fatalf("DupSkip re-import = present %v pid %s, want present -> %s", r2.AlreadyPresent, r2.AlreadyPresentPID, presentPID)
	}
	if r2.Plan.Importable() != 0 {
		t.Fatalf("DupSkip should not import the duplicate, got %d importable", r2.Plan.Importable())
	}

	// Third import (DupAllow): the SAME audio (duplicate essence) but different tags, so it
	// renders to a fresh destination and imports. It is still flagged already-present,
	// because §D resolves by essence independent of DupPolicy (the fix: not gated on the
	// action's Duplicate outcome, which only fires under DupSkip).
	third := filepath.Join(t.TempDir(), "three.mp3")
	writeFile(t, third, testaudio.BuildMP3WithAudio("Dup Track Alt", "Dup Artist", "Alt Album", 2, testaudio.AudioWithSeed(51)))
	r3, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: third}, model.KindTrack, waxbin.AcquiredMeta{DupPolicy: model.DupAllow})
	if err != nil {
		t.Fatalf("third import: %v", err)
	}
	if !r3.AlreadyPresent || r3.AlreadyPresentPID != presentPID {
		t.Fatalf("DupAllow re-import = present %v pid %s, want present -> %s (the §D fix)", r3.AlreadyPresent, r3.AlreadyPresentPID, presentPID)
	}
	if r3.Plan.Importable() != 1 {
		t.Fatalf("DupAllow should import the duplicate anyway, got %d importable", r3.Plan.Importable())
	}
}

// TestResolveRefReadOnly verifies all three facade methods work on a read-only Library
// (pure reads, no change_log writes).
func TestResolveRefReadOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "b.db")
	writeFile(t, filepath.Join(root, "song.mp3"),
		testaudio.BuildMP3WithAudio("RO Song", "RO Artist", "RO Album", 1, testaudio.AudioWithSeed(61)))

	rw := openManaged(t, ctx, db, root)
	scanLib(t, ctx, rw)
	pid := itemPIDByTitle(t, ctx, rw, "RO Song")
	pl, err := rw.Playlists().CreateStatic(ctx, "ro", "", "")
	if err != nil {
		t.Fatalf("create playlist: %v", err)
	}
	if err := rw.Playlists().Set(ctx, pl, []model.PID{pid}); err != nil {
		t.Fatalf("set playlist: %v", err)
	}
	ref := exportRefs(t, ctx, rw, pid)[0]
	if err := rw.Close(); err != nil {
		t.Fatalf("close rw: %v", err)
	}

	ro, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })
	if !ro.ReadOnly() {
		t.Fatal("library should report read-only")
	}

	// Export.
	roRefs, err := ro.ExportPlaylistRefs(ctx, pl, "")
	if err != nil || len(roRefs) != 1 {
		t.Fatalf("read-only export = %d refs err %v, want 1", len(roRefs), err)
	}
	// Resolve one.
	if item, rung, err := ro.ResolveRef(ctx, ref); err != nil || rung != model.MatchEssence || item.PID != pid {
		t.Fatalf("read-only resolve = rung %s item %v err %v, want essence -> the song", rung, item, err)
	}
	// Resolve a batch.
	if res, err := ro.ResolvePlaylistRefs(ctx, roRefs); err != nil || len(res) != 1 || res[0].PID != pid {
		t.Fatalf("read-only batch resolve = %+v err %v, want the song", res, err)
	}
}
