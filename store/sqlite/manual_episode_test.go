package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// TestFeedSyncPreservesPinnedEpisode verifies the explicit pin path can pin an existing
// feed episode and that a later feed re-sync, which passes pinned=false, never un-pins
// it. Otherwise retention could delete a file the user explicitly kept.
func TestFeedSyncPreservesPinnedEpisode(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)

	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/x", "Alpha"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	ep := eps[0]

	// Pin the existing feed episode via the managed pin path.
	if _, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{
		PodcastPID: res.PodcastPID, Pinned: true,
		Episode: model.FeedEpisode{Title: "Alpha", GUID: "guid-Alpha"},
	}); err != nil {
		t.Fatalf("UpsertEpisode pin: %v", err)
	}
	if d, _ := st.EpisodeByPID(ctx, ep.PID); !d.Episode.Pinned {
		t.Fatal("explicit pin was not applied to an existing feed episode")
	}

	// A feed re-sync passes pinned=false but must not un-pin the user-pinned episode.
	if _, err := st.UpsertFeed(ctx, feedInput("http://feed.example/x", "Alpha")); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if d, _ := st.EpisodeByPID(ctx, ep.PID); !d.Episode.Pinned {
		t.Fatal("feed re-sync un-pinned a pinned episode")
	}
}

// TestFeedGainingGUIDDoesNotDuplicate verifies that when a guid-less feed later
// publishes a <podcast:guid> and is re-added (matched by feed URL), its episodes are
// re-keyed rather than re-inserted, so the catalog is not doubled.
func TestFeedGainingGUIDDoesNotDuplicate(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)
	url := "http://feed.example/g"

	if _, err := st.UpsertFeed(ctx, feedInput(url, "Alpha", "Beta")); err != nil {
		t.Fatalf("initial UpsertFeed: %v", err)
	}
	// The feed now carries a <podcast:guid>; a re-add keys by pguid but matches the row
	// by feed URL and flips its identity.
	in := feedInput(url, "Alpha", "Beta")
	in.Feed.GUID = "show-guid"
	in.IdentityKey = identity.PodcastKey("show-guid", url)
	res, err := st.UpsertFeed(ctx, in)
	if err != nil {
		t.Fatalf("re-add UpsertFeed: %v", err)
	}
	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil {
		t.Fatalf("EpisodesByPodcast: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("catalog doubled on identity flip: %d episodes, want 2", len(eps))
	}
}

// TestEpisodeSourceReflectsShowType verifies an episode with no acquisition row reads
// its source from the show's source_type (rss), not the local default.
func TestEpisodeSourceReflectsShowType(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)
	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/s", "Alpha"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	v, err := st.ItemByPID(ctx, eps[0].PID)
	if err != nil {
		t.Fatalf("ItemByPID: %v", err)
	}
	if v.Source != model.SourceRSS {
		t.Fatalf("episode source = %q, want rss", v.Source)
	}
}

func TestUpsertShowAndEpisodePinnedRetention(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t)

	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("EnsurePodcastLibrary: %v", err)
	}

	// A manual show has no feed to sync.
	showPID, created, err := st.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: "manual:show1", FeedURL: "manual:show1", SourceType: model.SourceManual, Title: "My Curated Show",
	})
	if err != nil || !created {
		t.Fatalf("UpsertShow: pid=%s created=%v err=%v", showPID, created, err)
	}
	pod, err := st.PodcastByPID(ctx, showPID)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if pod.SourceType != model.SourceManual {
		t.Fatalf("show source = %q, want manual", pod.SourceType)
	}

	// Add one pinned episode and one ordinary episode.
	pinned, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{
		PodcastPID: showPID, Pinned: true,
		Episode: model.FeedEpisode{Title: "Pinned Ep", GUID: "p1", EnclosureURL: "http://x/p1.mp3", EnclosureType: "audio/mpeg", PubDateNS: 2_000_000_000},
	})
	if err != nil || !pinned.Created {
		t.Fatalf("UpsertEpisode pinned: %+v err=%v", pinned, err)
	}
	ordinary, err := st.UpsertEpisode(ctx, model.UpsertEpisodeInput{
		PodcastPID: showPID, Pinned: false,
		Episode: model.FeedEpisode{Title: "Ordinary Ep", GUID: "o1", EnclosureURL: "http://x/o1.mp3", EnclosureType: "audio/mpeg", PubDateNS: 1_000_000_000},
	})
	if err != nil {
		t.Fatalf("UpsertEpisode ordinary: %v", err)
	}

	ep, err := st.EpisodeByPID(ctx, pinned.EpisodePID)
	if err != nil {
		t.Fatalf("EpisodeByPID: %v", err)
	}
	if !ep.Episode.Pinned {
		t.Fatal("pinned episode did not read back pinned")
	}

	// Download both (attach files -> state present).
	attach := func(epPID model.PID, name string) {
		if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
			EpisodePID: epPID, LibraryID: libID,
			File: model.File{
				Path: []byte("/podcasts/" + name), DisplayPath: "/podcasts/" + name,
				RelPath: []byte(name), Kind: model.FileAudio, ContentHash: name, ScanState: model.ScanIndexed,
			},
		}); err != nil {
			t.Fatalf("AttachEpisodeFile %s: %v", name, err)
		}
	}
	attach(pinned.EpisodePID, "p1.mp3")
	attach(ordinary.EpisodePID, "o1.mp3")

	// Retention operates only on non-pinned downloads, so the pinned episode is
	// excluded (never counted, never reclaimed).
	downloaded, err := st.DownloadedEpisodes(ctx, showPID)
	if err != nil {
		t.Fatalf("DownloadedEpisodes: %v", err)
	}
	if len(downloaded) != 1 || downloaded[0].PID != ordinary.EpisodePID {
		t.Fatalf("DownloadedEpisodes = %d entries, want only the non-pinned one", len(downloaded))
	}
}
