package waxbin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// ownershipFixture opens a library with one managed music root and a podcast
// download dir, then ingests a track into the music root and a downloaded episode
// into the podcast library. It is an internal test so the RestoreTrash case can seed
// a podcast-library trash entry directly, which the public API no longer allows.
func ownershipFixture(t *testing.T) (lib *Library, podDir string, trackPID, episodePID model.PID) {
	t.Helper()
	ctx := context.Background()
	musicRoot := t.TempDir()
	podDir = t.TempDir()
	acq := t.TempDir()

	var err error
	lib, err = Open(ctx, Options{
		DBPath: filepath.Join(t.TempDir(), "catalog.db"),
		Roots: []config.Root{
			{Path: musicRoot, Mode: model.ModeManaged, Media: model.MediaMusic, Profile: "waxbin-native"},
		},
		Podcasts: config.PodcastConfig{Dir: podDir},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	trackFile := filepath.Join(acq, "song.mp3")
	writeTestFile(t, trackFile, testaudio.BuildMP3WithAudio("Song", "Artist", "Album", 1, testaudio.AudioWithSeed(1)))
	tr, err := lib.ImportAcquired(ctx, AcquiredFile{Path: trackFile}, model.KindTrack, AcquiredMeta{})
	if err != nil {
		t.Fatalf("import track: %v", err)
	}
	if _, err := lib.ApplyImport(ctx, tr.Plan); err != nil {
		t.Fatalf("apply import: %v", err)
	}

	epFile := filepath.Join(acq, "ep.mp3")
	writeTestFile(t, epFile, testaudio.BuildMP3WithAudio("Ep One", "Host", "Show", 1, testaudio.AudioWithSeed(2)))
	res, err := lib.ImportAcquired(ctx, AcquiredFile{Path: epFile}, model.KindEpisode, AcquiredMeta{
		ShowTitle: "My Show", SourceType: model.SourceManual, Title: "Ep One",
	})
	if err != nil {
		t.Fatalf("import episode: %v", err)
	}
	episodePID = res.EpisodePID

	items, err := lib.Query(ctx, query.New(query.EntityItems).Where("kind", query.OpIs, "track").Build(), "")
	if err != nil || len(items) != 1 {
		t.Fatalf("track items = %d (err %v), want 1", len(items), err)
	}
	return lib, podDir, items[0].PID, episodePID
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestRmRefusesAnEpisodeAndNamesTheVerb pins the explicit path: the caller named the
// item, so it gets an error naming the verb that does own those bytes rather than a
// quiet skip.
func TestRmRefusesAnEpisodeAndNamesTheVerb(t *testing.T) {
	lib, _, trackPID, episodePID := ownershipFixture(t)
	ctx := context.Background()

	_, err := lib.PlanDeletePIDs(ctx, []model.PID{episodePID}, model.DeleteTrash)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("rm of an episode = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "podcast unfetch") {
		t.Errorf("error %q does not name `podcast unfetch`", err)
	}
	// A mixed explicit list is refused whole rather than partly applied.
	if _, err := lib.PlanDeletePIDs(ctx, []model.PID{trackPID, episodePID}, model.DeleteTrash); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("rm of a track plus an episode = %v, want CodeInvalid", err)
	}
	// A track alone still plans.
	plan, err := lib.PlanDeletePIDs(ctx, []model.PID{trackPID}, model.DeleteTrash)
	if err != nil {
		t.Fatalf("rm of a track: %v", err)
	}
	if plan.Pending() != 1 || plan.SkippedPodcast != 0 {
		t.Errorf("track plan = %d pending, %d podcast-skipped; want 1 and 0", plan.Pending(), plan.SkippedPodcast)
	}
}

// TestRmRefusesANeverDownloadedEpisode is the hole a path-only guard leaves: a remote
// episode has no file, so there is no path to resolve to a library, and rm fell
// through to an empty plan reporting "0 action(s)" rather than the refusal.
func TestRmRefusesANeverDownloadedEpisode(t *testing.T) {
	lib, _, _, episodePID := ownershipFixture(t)
	ctx := context.Background()
	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	added, err := lib.Podcasts().AddEpisode(ctx, ep.Episode.PodcastPID,
		model.FeedEpisode{Title: "Never Fetched", EnclosureURL: "http://example.invalid/y.mp3"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	remote, err := lib.Podcasts().Episode(ctx, added.EpisodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if remote.Episode.Downloaded || remote.Episode.DisplayPath != "" {
		t.Fatalf("fixture episode is not remote: %+v", remote.Episode)
	}

	if _, err := lib.PlanDeletePIDs(ctx, []model.PID{added.EpisodePID}, model.DeleteTrash); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("rm of a never-downloaded episode = %v, want CodeInvalid", err)
	}
	// The query-driven path skips it for the same reason, and counts it.
	plan, err := lib.PlanDelete(ctx, query.New(query.EntityItems).Build(), model.DeleteTrash)
	if err != nil {
		t.Fatalf("PlanDelete: %v", err)
	}
	if plan.SkippedPodcast != 2 {
		t.Errorf("SkippedPodcast = %d, want 2 (the downloaded and the remote episode)", plan.SkippedPodcast)
	}
}

// TestPlanDeleteSkipsEpisodesAndReportsIt pins the query-driven path: a sweep over a
// mixed match set plans the tracks, leaves the episodes alone, and says how many it
// left alone. Failing the whole sweep would break retention and dedup.
func TestPlanDeleteSkipsEpisodesAndReportsIt(t *testing.T) {
	lib, podDir, _, episodePID := ownershipFixture(t)
	ctx := context.Background()

	plan, err := lib.PlanDelete(ctx, query.New(query.EntityItems).Build(), model.DeleteTrash)
	if err != nil {
		t.Fatalf("PlanDelete over everything: %v", err)
	}
	if plan.SkippedPodcast != 1 {
		t.Errorf("SkippedPodcast = %d, want 1", plan.SkippedPodcast)
	}
	for _, a := range plan.Actions {
		if strings.HasPrefix(a.Src, podDir) {
			t.Errorf("planned an action against the podcast library: %+v", a)
		}
	}
	if plan.Pending() != 1 {
		t.Errorf("pending = %d, want just the track", plan.Pending())
	}

	rep, err := lib.ApplyDelete(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyDelete: %v", err)
	}
	if rep.SkippedPodcast != 1 {
		t.Errorf("report SkippedPodcast = %d, want the plan's 1", rep.SkippedPodcast)
	}
	// The episode's bytes survive the sweep.
	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if !ep.Episode.Downloaded {
		t.Error("the sweep unfetched the episode")
	}
}

// TestRestoreTrashRefusesThePodcastLibrary covers the entry a pre-1.0 catalog may
// already hold. Restoring re-scans through the scanner directly, bypassing
// resolveLibraries, so without this the restore would generic-scan the one library
// resolveLibraries refuses.
func TestRestoreTrashRefusesThePodcastLibrary(t *testing.T) {
	lib, podDir, _, episodePID := ownershipFixture(t)
	ctx := context.Background()

	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if ep.Episode.FilePID == "" {
		t.Fatal("the fixture's episode has no file")
	}
	// Seed the entry the public API can no longer create.
	dst := filepath.Join(podDir, model.TrashDirName, "seeded", filepath.Base(ep.Episode.DisplayPath))
	writeTestFile(t, dst, []byte("moved"))
	trashPID, err := lib.store.TrashFile(ctx, model.TrashFileInput{
		FilePID: ep.Episode.FilePID, Reason: "seeded",
		TrashPath: []byte(dst), TrashDisplay: dst,
	})
	if err != nil {
		t.Fatalf("seed trash entry: %v", err)
	}

	err = lib.RestoreTrash(ctx, trashPID)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("restore into the podcast library = %v, want CodeInvalid", err)
	}
	if !strings.Contains(err.Error(), "podcast library") {
		t.Errorf("error %q does not say why", err)
	}
}

// TestUnfetchStillLeavesTheEpisodeRemote is the regression guard on the verb the rest
// of this now routes to: it reclaims the bytes and keeps the episode re-fetchable,
// with its play state intact, which is what `rm` cannot do.
func TestUnfetchStillLeavesTheEpisodeRemote(t *testing.T) {
	lib, _, _, episodePID := ownershipFixture(t)
	ctx := context.Background()

	if err := lib.Playback().MarkPlayed(ctx, "", episodePID, true); err != nil {
		t.Fatalf("mark played: %v", err)
	}
	before, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	path := before.Episode.DisplayPath

	res, err := lib.Podcasts().Unfetch(ctx, episodePID)
	if err != nil {
		t.Fatalf("Unfetch: %v", err)
	}
	if !res.Unfetched || res.ReclaimedBytes <= 0 {
		t.Errorf("unfetch = %+v, want the bytes reclaimed", res)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the episode file survives at %s", path)
	}
	after, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode after: %v", err)
	}
	if after.Episode.Downloaded {
		t.Error("the episode is still marked downloaded")
	}
	// remote, not archived: `rm` would have archived it ("files gone, history kept"),
	// which is the wrong end state for something re-fetchable.
	if after.Episode.State != model.StateRemote {
		t.Errorf("episode state = %q, want remote and re-fetchable", after.Episode.State)
	}
	st, err := lib.Playback().State(ctx, "", episodePID)
	if err != nil {
		t.Fatalf("play state: %v", err)
	}
	if st == nil || !st.Played {
		t.Errorf("play state = %+v, want the played flag the unfetch had to preserve", st)
	}
}

// holdScope takes a lease on scope under a foreign owner and releases it at test end,
// standing in for another mutator holding it. Leases are unique per scope regardless
// of owner, so this is exactly what a concurrent job looks like.
func holdScope(t *testing.T, lib *Library, scope string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixNano()
	ok, err := lib.store.AcquireLease(ctx, &model.Lease{
		Scope: scope, Owner: "other-test-owner", AcquiredAt: now, HeartbeatAt: now,
	})
	if err != nil || !ok {
		t.Fatalf("hold %s = %v (err %v)", scope, ok, err)
	}
	t.Cleanup(func() { _ = lib.store.ReleaseLease(context.Background(), scope, "other-test-owner") })
}

// TestApplyRetentionAllUnderARealLeaser is the self-conflict regression. Both
// ApplyRetention entry points lease, so an ApplyRetentionAll that called the exported
// per-podcast form would fail CodeConflict against itself on the first podcast of
// every watch tick.
func TestApplyRetentionAllUnderARealLeaser(t *testing.T) {
	lib, _, _, episodePID := ownershipFixture(t)
	ctx := context.Background()

	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	if err := lib.Podcasts().SetRetention(ctx, ep.Episode.PodcastPID, 1); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	// A second show, so the loop runs more than one iteration.
	if _, err := lib.Podcasts().AddManual(ctx, "Second Show", podcast.ManualOptions{}); err != nil {
		t.Fatalf("add second show: %v", err)
	}
	if _, err := lib.Podcasts().ApplyRetentionAll(ctx); err != nil {
		t.Fatalf("ApplyRetentionAll under a real leaser: %v", err)
	}
	// The exported per-podcast form is the CLI's entry point and leases once itself.
	if _, err := lib.Podcasts().ApplyRetention(ctx, ep.Episode.PodcastPID); err != nil {
		t.Fatalf("ApplyRetention under a real leaser: %v", err)
	}
}

// TestPodcastVerbsConflictOnTheirOwnScope pins that the verbs do serialize against
// each other.
func TestPodcastVerbsConflictOnTheirOwnScope(t *testing.T) {
	lib, _, _, episodePID := ownershipFixture(t)
	ctx := context.Background()
	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	holdScope(t, lib, podcastFSScope)

	if _, err := lib.Podcasts().Unfetch(ctx, episodePID); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("Unfetch against a held podcast lease = %v, want CodeConflict", err)
	}
	if _, err := lib.Podcasts().ApplyRetentionAll(ctx); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("ApplyRetentionAll against a held podcast lease = %v, want CodeConflict", err)
	}
	if err := lib.Podcasts().Remove(ctx, ep.Episode.PodcastPID); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("Remove against a held podcast lease = %v, want CodeConflict", err)
	}
}

// TestPodcastVerbsIgnoreTheFsMutateScope is the point of the second scope: a scan or
// organize holding the user-tree lease provably cannot touch episode files, so the
// podcast verbs must not be blocked by it. ImportEpisodeFile is the exception, since
// it moves a file out of an arbitrary source path.
func TestPodcastVerbsIgnoreTheFsMutateScope(t *testing.T) {
	lib, _, _, episodePID := ownershipFixture(t)
	ctx := context.Background()
	ep, err := lib.Podcasts().Episode(ctx, episodePID)
	if err != nil {
		t.Fatalf("Episode: %v", err)
	}
	holdScope(t, lib, fsMutateScope)

	if _, err := lib.Podcasts().ApplyRetentionAll(ctx); err != nil {
		t.Errorf("ApplyRetentionAll during a scan = %v, want success", err)
	}
	if _, err := lib.Podcasts().Unfetch(ctx, episodePID); err != nil {
		t.Errorf("Unfetch during a scan = %v, want success", err)
	}

	// The one verb that spans both trees is blocked, and by the user-tree lease.
	acq := t.TempDir()
	src := filepath.Join(acq, "new.mp3")
	writeTestFile(t, src, testaudio.BuildMP3WithAudio("New Ep", "Host", "Show", 2, testaudio.AudioWithSeed(9)))
	added, err := lib.Podcasts().AddEpisode(ctx, ep.Episode.PodcastPID, model.FeedEpisode{Title: "New Ep"}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	if _, err := lib.Podcasts().ImportEpisodeFile(ctx, added.EpisodePID, src, true); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Errorf("ImportEpisodeFile during a scan = %v, want CodeConflict", err)
	}

	if err := lib.Podcasts().Remove(ctx, ep.Episode.PodcastPID); err != nil {
		t.Errorf("Remove during a scan = %v, want success", err)
	}
}
