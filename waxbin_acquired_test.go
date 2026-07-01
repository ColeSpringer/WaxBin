package waxbin_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
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
	tracks, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build())
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
	books, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "book").Build())
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
	yt, err := lib.Query(ctx, query.New(query.EntityItems).Where("source", query.OpIs, "youtube").Build())
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
