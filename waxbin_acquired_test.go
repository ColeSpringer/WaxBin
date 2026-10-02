package waxbin_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// openMediaTyped opens a library with separate music and audiobook managed roots
// plus a podcast download dir.
func openMediaTyped(t *testing.T, ctx context.Context, db, musicRoot, bookRoot, podDir string) *waxbin.Library {
	t.Helper()
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: musicRoot, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
			{Path: bookRoot, Mode: model.ModeManaged, Media: model.MediaAudiobook, Profile: "waxbin-native"},
		},
		Podcasts: config.PodcastConfig{Dir: podDir},
	})
	if err != nil {
		t.Fatalf("open media-typed library: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

// TestImportAcquiredRoutesByKind verifies ImportAcquired routes a track to the
// music-typed root and a book to the audiobook-typed root, records source
// provenance, and surfaces it on the read side.
func TestImportAcquiredRoutesByKind(t *testing.T) {
	ctx := context.Background()
	musicRoot := t.TempDir()
	bookRoot := t.TempDir()
	podDir := t.TempDir()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openMediaTyped(t, ctx, db, musicRoot, bookRoot, podDir)

	// A track routes to the music root.
	trackFile := filepath.Join(acq, "song.mp3")
	writeFile(t, trackFile, testaudio.BuildMP3WithAudio("Acq Song", "Acq Artist", "Acq Album", 1, testaudio.AudioWithSeed(1)))
	tr, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: trackFile}, model.KindTrack, waxbin.AcquiredMeta{
		SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=1",
	})
	if err != nil {
		t.Fatalf("ImportAcquired track: %v", err)
	}
	if tr.Plan == nil || tr.Plan.Importable() != 1 {
		t.Fatalf("track plan not a single import: %+v", tr.Plan)
	}
	if rep, err := lib.ApplyImport(ctx, tr.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("apply track import: rep=%+v err=%v", rep, err)
	}

	// A file forced as a book routes to the audiobook root and is cataloged as a book,
	// even though its tags look like an ordinary MP3 track.
	bookFile := filepath.Join(acq, "chapter.mp3")
	writeFile(t, bookFile, testaudio.BuildMP3WithAudio("Chapter One", "Tolkien", "The Hobbit", 1, testaudio.AudioWithSeed(2)))
	bk, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: bookFile}, model.KindBook, waxbin.AcquiredMeta{
		SourceType: model.SourceManual,
	})
	if err != nil {
		t.Fatalf("ImportAcquired book: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, bk.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("apply book import: rep=%+v err=%v", rep, err)
	}

	// The track reads back under the music root, sourced youtube.
	tracks, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build(), "")
	if err != nil {
		t.Fatalf("query tracks: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("want 1 track, got %d", len(tracks))
	}
	if !strings.HasPrefix(tracks[0].DisplayPath, musicRoot) {
		t.Errorf("track landed at %q, want under the music root %q", tracks[0].DisplayPath, musicRoot)
	}
	if tracks[0].Source != model.SourceYouTube {
		t.Errorf("track source = %q, want youtube", tracks[0].Source)
	}

	// The book reads back under the audiobook root, sourced manual, kind book.
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil {
		t.Fatalf("query books: %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("want 1 book, got %d", len(books))
	}
	if !strings.HasPrefix(books[0].DisplayPath, bookRoot) {
		t.Errorf("book landed at %q, want under the audiobook root %q", books[0].DisplayPath, bookRoot)
	}
	if books[0].Source != model.SourceManual {
		t.Errorf("book source = %q, want manual", books[0].Source)
	}

	// The acquisition provenance is queryable by source and readable per item.
	yt, err := lib.Query(ctx, query.New(query.EntityItems).Where("source", query.OpIs, "youtube").Build(), "")
	if err != nil || len(yt) != 1 || yt[0].PID != tracks[0].PID {
		t.Fatalf("source=youtube filter = %d items (err %v), want the acquired track", len(yt), err)
	}
	acqRow, err := lib.Acquisition(ctx, tracks[0].PID)
	if err != nil {
		t.Fatalf("Acquisition: %v", err)
	}
	if acqRow.SourceType != model.SourceYouTube || acqRow.SourceURL != "https://y/watch?v=1" {
		t.Errorf("acquisition = %+v", acqRow)
	}
}

// TestImportAmbiguousRouteQuarantines verifies a folder import quarantines a file
// whose kind cannot route to a single managed root (two music roots), rather than
// silently placing it in the first one.
func TestImportAmbiguousRouteQuarantines(t *testing.T) {
	ctx := context.Background()
	music1 := t.TempDir()
	music2 := t.TempDir()
	inboxDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib, err := waxbin.Open(ctx, waxbin.Options{
		DBPath: db,
		Roots: []config.Root{
			{Path: music1, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
			{Path: music2, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	writeFile(t, filepath.Join(inboxDir, "song.mp3"), testaudio.BuildMP3("Song", "Artist", "Album", 1))
	plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: inboxDir})
	if err != nil {
		t.Fatalf("plan import: %v", err)
	}
	if plan.Importable() != 0 {
		t.Fatalf("ambiguous track should quarantine, got %d importable", plan.Importable())
	}
	if fileExists(filepath.Join(music1, "Artist", "Album", "01 - Song.mp3")) {
		t.Error("ambiguous track was placed in music1 instead of being quarantined")
	}
}

// TestImportAcquiredEpisode verifies an acquired episode is ingested into the
// internal podcast library under a manual show, pinned and downloaded.
func TestImportAcquiredEpisode(t *testing.T) {
	ctx := context.Background()
	musicRoot := t.TempDir()
	bookRoot := t.TempDir()
	podDir := t.TempDir()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openMediaTyped(t, ctx, db, musicRoot, bookRoot, podDir)

	epFile := filepath.Join(acq, "ep.mp3")
	writeFile(t, epFile, testaudio.BuildMP3WithAudio("Bonus Ep", "Host", "Show", 1, testaudio.AudioWithSeed(3)))
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: epFile}, model.KindEpisode, waxbin.AcquiredMeta{
		ShowTitle: "My Acquired Show", SourceType: model.SourceManual, Title: "Bonus Ep",
	})
	if err != nil {
		t.Fatalf("ImportAcquired episode: %v", err)
	}
	if res.EpisodePID == "" || res.Path == "" {
		t.Fatalf("episode not ingested with a file: %+v", res)
	}
	if !strings.HasPrefix(res.Path, podDir) {
		t.Errorf("episode file at %q, want under the podcast dir %q", res.Path, podDir)
	}

	ep, err := lib.Podcasts().Episode(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if !ep.Episode.Downloaded || !ep.Episode.Pinned {
		t.Fatalf("acquired episode = %+v, want downloaded and pinned", ep.Episode)
	}
	// The show is a manual show.
	pod, err := lib.Podcasts().Get(ctx, ep.Episode.PodcastPID)
	if err != nil {
		t.Fatalf("Get show: %v", err)
	}
	if pod.SourceType != model.SourceManual || pod.Title != "My Acquired Show" {
		t.Fatalf("acquired show = %+v", pod)
	}
	// The episode's origin provenance is recorded and readable.
	if acqRow, err := lib.Acquisition(ctx, res.EpisodePID); err != nil || acqRow.SourceType != model.SourceManual {
		t.Fatalf("episode acquisition = %+v err=%v", acqRow, err)
	}
}

// tagAcquisition stamps the three acquisition tags onto a file, so a scan derives an
// origin row from the file's own evidence the way a mis-tagged rip does.
func tagAcquisition(t *testing.T, path, url, id string) {
	t.Helper()
	if _, err := meta.NewWriter().Apply(context.Background(), path, []meta.TagEdit{
		{Key: "SOURCE_URL", Values: []string{url}},
		{Key: "SOURCE_ID", Values: []string{id}},
	}); err != nil {
		t.Fatalf("stamping acquisition tags on %s: %v", path, err)
	}
}

// TestSetAcquisitionCorrectsAWrongOrigin is the facade half of what WaxDeck asked for:
// a mis-tagged rip reads as acquired from wherever its SOURCE_URL pointed, and this is
// the verb that says otherwise.
func TestSetAcquisitionCorrectsAWrongOrigin(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1))
	tagAcquisition(t, src, "https://wrong.test/x", "wrong-1")

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Ripped")
	if a, err := lib.Acquisition(ctx, pid); err != nil || a.SourceURL != "https://wrong.test/x" {
		t.Fatalf("scan did not derive the wrong origin first: %+v (err %v)", a, err)
	}

	if err := lib.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
	}, waxbin.AcquisitionEditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}
	a, err := lib.Acquisition(ctx, pid)
	if err != nil {
		t.Fatalf("Acquisition: %v", err)
	}
	if a.SourceType != model.SourceManual || a.SourceURL != "https://right.test/y" || a.SourceID != "" {
		t.Errorf("corrected row = %+v, want the authoritative replace", a)
	}
	// The lock is what an import or a later scan has to respect.
	rows, err := lib.Provenance(ctx, pid)
	if err != nil {
		t.Fatalf("Provenance: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.Field == "acquisition" {
			found = true
			if !r.Locked || r.Source != model.SourceUser {
				t.Errorf("acquisition provenance = %+v, want a locked user row", r)
			}
		}
	}
	if !found {
		t.Error("no acquisition provenance row after a locking set")
	}

	// A forced rescan re-derives the file's tags into every unlocked field, and leaves
	// the locked correction alone.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if a, _ := lib.Acquisition(ctx, pid); a.SourceURL != "https://right.test/y" {
		t.Errorf("a forced rescan re-derived over the locked correction: %+v", a)
	}
}

// TestClearAcquisitionHoldsAcrossARescan is the durability case. Before the default
// lock, the file's own tags put the wrong origin straight back on the next full scan.
func TestClearAcquisitionHoldsAcrossARescan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1))
	tagAcquisition(t, src, "https://wrong.test/x", "wrong-1")

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Ripped")

	if err := lib.ClearAcquisition(ctx, pid, waxbin.AcquisitionEditOptions{}); err != nil {
		t.Fatalf("ClearAcquisition: %v", err)
	}
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := lib.Acquisition(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		a, _ := lib.Acquisition(ctx, pid)
		t.Fatalf("the cleared origin came back across a rescan: %+v (err %v)", a, err)
	}
	if v, _ := lib.Get(ctx, pid); v.Source != model.SourceLocal {
		t.Errorf("source after clear = %q, want local", v.Source)
	}
}

// TestClearAcquisitionWriteBackStripsTheTags is the durable half. The lock is the
// catalog agreeing to ignore evidence still sitting in the file; this removes the
// evidence, so the origin stays gone with no lock holding it.
func TestClearAcquisitionWriteBackStripsTheTags(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1))
	tagAcquisition(t, src, "https://wrong.test/x", "wrong-1")

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Ripped")
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	path := v.DisplayPath

	if err := lib.ClearAcquisition(ctx, pid, waxbin.AcquisitionEditOptions{
		Lock: model.LockOff, WriteBack: true,
	}); err != nil {
		t.Fatalf("ClearAcquisition with write-back: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fm.Tags.Acquisition.Present() {
		t.Errorf("acquisition tags survived the write-back: %+v", fm.Tags.Acquisition)
	}
	// With the lock released, only the stripped tags keep the origin gone.
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{Force: true}); err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if _, err := lib.Acquisition(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Errorf("the origin came back after the tags were stripped: %v", err)
	}
}

// TestSetAcquisitionWriteBackWritesTheTags: the correction goes onto disk too, so an
// export, a copy or a rebuild carries the right origin rather than the wrong one.
func TestSetAcquisitionWriteBackWritesTheTags(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Ripped", "Artist", "Album", 1))
	tagAcquisition(t, src, "https://wrong.test/x", "wrong-1")

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Ripped")
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	path := v.DisplayPath

	if err := lib.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
	}, waxbin.AcquisitionEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
		t.Fatalf("SetAcquisition with write-back: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fm.Tags.Acquisition.SourceURL != "https://right.test/y" {
		t.Errorf("source url on disk = %q, want the corrected one", fm.Tags.Acquisition.SourceURL)
	}
	// The set emptied the id, so the file must lose it too: the replace is authoritative
	// on disk exactly as it is in the catalog.
	if fm.Tags.Acquisition.SourceID != "" {
		t.Errorf("source id on disk = %q, want it emptied with the row", fm.Tags.Acquisition.SourceID)
	}
	// The row's own stamp reached the file, at the day precision the tag can hold.
	if fm.Tags.Acquisition.AcquiredAt == 0 {
		t.Error("acquisition date was not written back")
	}
}

// TestImportAcquiredRespectsACuratedOrigin: the automatic writers skip a locked item in
// silence, so re-importing an episode does not undo a curated origin. An episode is the
// case that matters most, since its acquisition row is the only thing overriding the
// show's source type.
func TestImportAcquiredRespectsACuratedOrigin(t *testing.T) {
	ctx := context.Background()
	acq := t.TempDir()
	podDir := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openMediaTyped(t, ctx, db, t.TempDir(), t.TempDir(), podDir)

	// The show pid is carried on the second import so it lands on the same show, and the
	// enclosure url is the episode's identity within it, so the re-import re-records
	// against the standing episode rather than minting a new one.
	imp := func(name string, showPID model.PID) *waxbin.AcquiredResult {
		t.Helper()
		f := filepath.Join(acq, name)
		writeFile(t, f, testaudio.BuildMP3WithAudio("Bonus Ep", "Host", "Show", 1, testaudio.AudioWithSeed(7)))
		res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: f}, model.KindEpisode, waxbin.AcquiredMeta{
			ShowPID: showPID, ShowTitle: "My Acquired Show", Title: "Bonus Ep",
			SourceType: model.SourceYouTube, SourceURL: "https://y/watch?v=9", Provider: "waxtap",
		})
		if err != nil {
			t.Fatalf("ImportAcquired: %v", err)
		}
		return res
	}
	first := imp("ep.mp3", "")
	pid := first.EpisodePID
	ep, err := lib.Podcasts().Episode(ctx, pid)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}

	if err := lib.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
	}, waxbin.AcquisitionEditOptions{Lock: model.LockOn}); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}

	if got := imp("ep2.mp3", ep.Episode.PodcastPID).EpisodePID; got != pid {
		t.Fatalf("re-import landed on episode %s, want the standing %s", got, pid)
	}
	a, err := lib.Acquisition(ctx, pid)
	if err != nil {
		t.Fatalf("Acquisition: %v", err)
	}
	if a.SourceType != model.SourceManual || a.SourceURL != "https://right.test/y" || a.Provider != "" {
		t.Errorf("a re-import wrote over the curated row: %+v", a)
	}
	if v, _ := lib.Get(ctx, pid); v.Source != model.SourceManual {
		t.Errorf("episode source = %q, want the curated override of the show's type", v.Source)
	}
}

// TestSetAcquisitionWriteBackWithholdsAMintedStamp: a brand-new row gets scan time,
// which the catalog holds as the approximation it is. A file cannot say that, so the
// write-back leaves ACQUISITION_DATE off rather than stating a wrong date with
// confidence that then outlives the catalog. The same rule insertAcquisitionIfAbsentTx
// refuses file mtime for.
func TestSetAcquisitionWriteBackWithholdsAMintedStamp(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	src := filepath.Join(root, "song.mp3")
	writeFile(t, src, testaudio.BuildMP3("Plain", "Artist", "Album", 1))

	lib := openManaged(t, ctx, db, root)
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	pid := itemPIDByTitle(t, ctx, lib, "Plain")
	// No acquisition tags, so no row: the set below is what creates one.
	if _, err := lib.Acquisition(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("fixture already has a row: %v", err)
	}

	if err := lib.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
	}, waxbin.AcquisitionEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
		t.Fatalf("SetAcquisition: %v", err)
	}
	v, err := lib.Get(ctx, pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	fm, err := meta.NewReader().Read(ctx, v.DisplayPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fm.Tags.Acquisition.SourceURL != "https://right.test/y" {
		t.Errorf("source url on disk = %q, want the correction", fm.Tags.Acquisition.SourceURL)
	}
	if fm.Tags.Acquisition.AcquiredAt != 0 {
		t.Error("the minted scan-time stamp reached the file, where it reads as evidence")
	}
	// The catalog keeps it, which is the half that is allowed to be an approximation.
	if a, _ := lib.Acquisition(ctx, pid); a.AcquiredAt == 0 {
		t.Error("the row lost its stamp")
	}

	// A stamp someone actually stated does travel.
	if err := lib.SetAcquisition(ctx, pid, model.AcquisitionInput{
		SourceType: model.SourceManual, AcquiredAt: 1_588_561_321_000_000_000,
	}, waxbin.AcquisitionEditOptions{Lock: model.LockOff, WriteBack: true}); err != nil {
		t.Fatalf("SetAcquisition with a stamp: %v", err)
	}
	fm, err = meta.NewReader().Read(ctx, v.DisplayPath)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if fm.Tags.Acquisition.AcquiredAt == 0 {
		t.Error("a stated stamp did not reach the file")
	}
}

// TestAcquisitionWriteBackRefusesAnEpisode pins the one kind this diff newly admits to
// acquisition curation but that can never take a write-back: retention re-fetches the
// file, so a tag write would be undone by the next download. The refusal is a
// WriteBackError, so the CLI surfaces it as a warning while the catalog change stands.
func TestAcquisitionWriteBackRefusesAnEpisode(t *testing.T) {
	ctx := context.Background()
	acq := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openMediaTyped(t, ctx, db, t.TempDir(), t.TempDir(), t.TempDir())

	f := filepath.Join(acq, "ep.mp3")
	writeFile(t, f, testaudio.BuildMP3WithAudio("Ep", "Host", "Show", 1, testaudio.AudioWithSeed(11)))
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: f}, model.KindEpisode, waxbin.AcquiredMeta{
		ShowTitle: "Show", Title: "Ep", SourceType: model.SourceRSS,
	})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}

	err = lib.SetAcquisition(ctx, res.EpisodePID, model.AcquisitionInput{SourceType: model.SourceManual},
		waxbin.AcquisitionEditOptions{Force: true, WriteBack: true})
	var wbErr *waxbin.WriteBackError
	if !errors.As(err, &wbErr) {
		t.Fatalf("episode write-back = %v, want a *WriteBackError", err)
	}
	if len(wbErr.Failures) == 0 || !strings.Contains(wbErr.Failures[0].Reason, "episode") {
		t.Errorf("failures = %+v, want the refusal to name the kind", wbErr.Failures)
	}
	// The catalog change stands, which is what makes this a warning rather than a failure.
	if a, aerr := lib.Acquisition(ctx, res.EpisodePID); aerr != nil || a.SourceType != model.SourceManual {
		t.Errorf("acquisition = %+v (err %v), want the committed correction", a, aerr)
	}
}

// TestForcedBookTakesNoDisplayFallback: a file imported as a book takes none of a track's
// display fallbacks, though its tags do not say it is one. A book's title and author are
// the key its parts group by, so neither its destination nor its catalog row takes a sort
// tag in their place.
func TestForcedBookTakesNoDisplayFallback(t *testing.T) {
	ctx := context.Background()
	musicRoot, bookRoot, acq := t.TempDir(), t.TempDir(), t.TempDir()
	lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), musicRoot, bookRoot, t.TempDir())
	src := filepath.Join(acq, "08 Part.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Genre: "Fiction", Audio: testaudio.AudioWithSeed(3)}))
	if _, err := meta.NewWriter().Apply(ctx, src, []meta.TagEdit{
		{Key: "ARTISTSORT", Values: []string{"Writer"}},
		{Key: "ALBUMSORT", Values: []string{"Tome"}},
	}); err != nil {
		t.Fatalf("stage sort tags: %v", err)
	}
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindBook, waxbin.AcquiredMeta{SourceType: model.SourceManual})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	for _, a := range res.Plan.Actions {
		if strings.Contains(a.RelDst, "Writer") || strings.Contains(a.RelDst, "Tome") {
			t.Errorf("destination %q took a sort tag", a.RelDst)
		}
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("apply: rep=%+v err=%v", rep, err)
	}
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build(), "")
	if err != nil || len(books) != 1 {
		t.Fatalf("books = %d (err %v), want 1", len(books), err)
	}
	if b := books[0]; b.Title == "Tome" || b.Artist == "Writer" {
		t.Errorf("book = title %q author %q, want neither taken from a sort tag", b.Title, b.Artist)
	}
	diags, err := lib.FileDiagnostics(ctx, model.DiagnosticFilter{ItemPID: books[0].PID, Code: model.DiagSortNameFallback})
	if err != nil || len(diags) != 0 {
		t.Errorf("fallback diagnostics = %+v (err %v), want none on a book", diags, err)
	}
}

// TestImportedTrackLandsUnderItsFallbackNames: an imported track's destination uses the
// display fallbacks the scan at that destination catalogs it with, so the file lands
// where organize would put it.
func TestImportedTrackLandsUnderItsFallbackNames(t *testing.T) {
	ctx := context.Background()
	musicRoot, acq := t.TempDir(), t.TempDir()
	lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), musicRoot, t.TempDir(), t.TempDir())
	src := filepath.Join(acq, "08 Parking Lot.mp3")
	writeFile(t, src, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{Genre: "Hip-Hop", Audio: testaudio.AudioWithSeed(5)}))
	if _, err := meta.NewWriter().Apply(ctx, src, []meta.TagEdit{
		{Key: "ARTISTSORT", Values: []string{"Anderson .Paak"}},
		{Key: "ALBUMSORT", Values: []string{"Malibu"}},
	}); err != nil {
		t.Fatalf("stage sort tags: %v", err)
	}
	res, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindTrack, waxbin.AcquiredMeta{SourceType: model.SourceManual})
	if err != nil {
		t.Fatalf("ImportAcquired: %v", err)
	}
	if rep, err := lib.ApplyImport(ctx, res.Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("apply: rep=%+v err=%v", rep, err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("items = %d (err %v), want 1", len(items), err)
	}
	it := items[0]
	if it.Artist != "Anderson .Paak" || it.Album != "Malibu" || it.Title != "Parking Lot" || it.TrackNo != 8 {
		t.Fatalf("item = %q by %q on %q, track %d", it.Title, it.Artist, it.Album, it.TrackNo)
	}
	rel, err := filepath.Rel(musicRoot, it.DisplayPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := res.Plan.Actions[0].RelDst; rel != want || !strings.Contains(rel, "Malibu") {
		t.Errorf("landed at %q, planned %q, want a destination under the fallback album", rel, want)
	}
	plan, err := lib.PlanOrganize(ctx, query.New(query.EntityItems).Build(), waxbin.OrganizeOptions{})
	if err != nil {
		t.Fatalf("plan organize: %v", err)
	}
	for _, a := range plan.Actions {
		if !a.Skip {
			t.Errorf("organize would move %s to %s", a.Src, a.Dst)
		}
	}
}

// TestImportedTrackIsNotNumberedByItsStagingFolder: the folder an import reads from is the
// edge of what the folders above a file can name, so tracks handed over in a folder called
// "CD1" get no disc from it, while a disc folder inside the staged tree still numbers its own.
func TestImportedTrackIsNotNumberedByItsStagingFolder(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		staged  func(acq string) (source, file string)
		wantRel string
	}{
		{"a staging folder named like a disc", func(acq string) (string, string) {
			dir := filepath.Join(acq, "CD1")
			return dir, filepath.Join(dir, "08 Parking Lot.mp3")
		}, "08 - Parking Lot.mp3"},
		{"a disc folder inside the staging folder", func(acq string) (string, string) {
			return acq, filepath.Join(acq, "CD1", "08 Parking Lot.mp3")
		}, "1-08 - Parking Lot.mp3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			musicRoot, acq := t.TempDir(), t.TempDir()
			lib := openMediaTyped(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), musicRoot, t.TempDir(), t.TempDir())
			source, file := tc.staged(acq)
			writeFile(t, file, testaudio.BuildMP3FromSpec(testaudio.MP3Spec{
				Title: "Parking Lot", Artist: "Band", Album: "Malibu", Audio: testaudio.AudioWithSeed(5)}))
			plan, err := lib.PlanImport(ctx, waxbin.ImportRequest{Source: source})
			if err != nil || len(plan.Actions) != 1 {
				t.Fatalf("plan = %+v (err %v), want one action", plan, err)
			}
			if got := filepath.Base(plan.Actions[0].RelDst); got != tc.wantRel {
				t.Errorf("destination file = %q, want %q", got, tc.wantRel)
			}
		})
	}
}

// TestDupSkipImportsRaceToOne: two uploads of the same audio planned before either is
// applied both look new, and the second apply finds the first's file and skips its own
// as a duplicate.
func TestDupSkipImportsRaceToOne(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	lib := openManaged(t, ctx, filepath.Join(t.TempDir(), "catalog.db"), root)
	var plans []*waxbin.AcquiredResult
	for i, title := range []string{"Upload One", "Upload Two"} {
		src := filepath.Join(t.TempDir(), "up.mp3")
		writeFile(t, src, testaudio.BuildMP3WithAudio(title, "Band", "Album", i+1, testaudio.AudioWithSeed(77)))
		r, err := lib.ImportAcquired(ctx, waxbin.AcquiredFile{Path: src}, model.KindTrack, waxbin.AcquiredMeta{DupPolicy: model.DupSkip})
		if err != nil || r.Plan.Importable() != 1 {
			t.Fatalf("plan %d = %+v (err %v), want it importable", i, r, err)
		}
		plans = append(plans, r)
	}
	if rep, err := lib.ApplyImport(ctx, plans[0].Plan); err != nil || rep.Imported != 1 {
		t.Fatalf("first apply = %+v (err %v), want it imported", rep, err)
	}
	rep, err := lib.ApplyImport(ctx, plans[1].Plan)
	if err != nil || rep.Imported != 0 || rep.Duplicates != 1 {
		t.Fatalf("second apply = %+v (err %v), want it skipped as a duplicate", rep, err)
	}
	items, err := lib.Query(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil || len(items) != 1 {
		t.Errorf("items = %d (err %v), want one", len(items), err)
	}
}
