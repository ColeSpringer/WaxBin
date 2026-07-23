package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
)

func feedInput(feedURL string, titles ...string) model.UpsertFeedInput {
	key := identity.PodcastKey("", feedURL)
	eps := make([]model.FeedEpisode, len(titles))
	for i, tt := range titles {
		eps[i] = model.FeedEpisode{
			GUID: "guid-" + tt, Title: tt, EnclosureURL: feedURL + "/" + tt + ".mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: int64(i+1) * 1_000_000_000,
		}
	}
	return model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: key,
		Feed:        model.Feed{Title: "My Show", Author: "Host", Episodes: eps},
		FetchedAtNS: 1,
	}
}

func TestUpsertFeedAndItemView(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	if !res.Created || res.EpisodesAdded != 2 {
		t.Fatalf("created=%v added=%d", res.Created, res.EpisodesAdded)
	}

	// Episodes read back through the shared item view as kind=episode, with the
	// podcast title standing in for artist/album.
	items, err := st.QueryItems(ctx, query.New(query.EntityItems).
		Where("kind", query.OpIs, "episode").Build(), "")
	if err != nil {
		t.Fatalf("QueryItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("episode items = %d", len(items))
	}
	for _, it := range items {
		if it.Album != "My Show" || it.Artist != "My Show" {
			t.Fatalf("episode view artist/album = %q/%q, want podcast title", it.Artist, it.Album)
		}
		if it.State != model.StateRemote {
			t.Fatalf("fresh episode should be remote, got %s", it.State)
		}
	}

	eps, err := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if err != nil {
		t.Fatalf("EpisodesByPodcast: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("episodes = %d", len(eps))
	}
}

func TestReSyncDoesNotDowngradeDownloaded(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	target := eps[0]

	libID, err := st.EnsurePodcastLibrary(ctx, "/podcasts")
	if err != nil {
		t.Fatalf("EnsurePodcastLibrary: %v", err)
	}
	if _, err := st.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: target.PID,
		LibraryID:  libID,
		File: model.File{
			Path: []byte("/podcasts/a.mp3"), DisplayPath: "/podcasts/a.mp3",
			RelPath: []byte("a.mp3"), Kind: model.FileAudio, ContentHash: "h1", ScanState: model.ScanIndexed,
		},
	}); err != nil {
		t.Fatalf("AttachEpisodeFile: %v", err)
	}

	// A re-sync (same feed) must not knock the downloaded episode back to remote.
	if _, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta")); err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	d, err := st.EpisodeByPID(ctx, target.PID)
	if err != nil {
		t.Fatalf("EpisodeByPID: %v", err)
	}
	if d.Episode.State != model.StatePresent || !d.Episode.Downloaded {
		t.Fatalf("re-sync downgraded a downloaded episode: %+v", d.Episode)
	}

	// DropEpisodeFile returns it to remote (retention), removing the file row.
	if err := st.DropEpisodeFile(ctx, target.PID); err != nil {
		t.Fatalf("DropEpisodeFile: %v", err)
	}
	d2, _ := st.EpisodeByPID(ctx, target.PID)
	if d2.Episode.State != model.StateRemote || d2.Episode.Downloaded {
		t.Fatalf("drop should return episode to remote: %+v", d2.Episode)
	}
}

func TestReSyncUnchangedSkipsEpisodeWrites(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	in := feedInput("http://feed.example/f", "Alpha", "Beta")
	if _, err := st.UpsertFeed(ctx, in); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	seqBefore, _ := st.LatestChangeSeq(ctx)
	res, err := st.UpsertFeed(ctx, in) // identical re-sync
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.EpisodesAdded != 0 || res.EpisodesUpdated != 0 {
		t.Fatalf("identical re-sync touched episodes: added=%d updated=%d", res.EpisodesAdded, res.EpisodesUpdated)
	}
	// A no-op re-sync emits nothing at all: the podcast row's fetch-time/validator
	// refresh is bookkeeping, not a change a consumer needs to see.
	seqAfter, _ := st.LatestChangeSeq(ctx)
	if got := seqAfter - seqBefore; got != 0 {
		t.Fatalf("unchanged re-sync emitted %d deltas, want 0", got)
	}
}

func TestReSyncChangedEpisodeEmitsUpdate(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta")); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	// Re-sync with one episode's description changed.
	in := feedInput("http://feed.example/f", "Alpha", "Beta")
	in.Feed.Episodes[1].Description = "now with show notes"
	res, err := st.UpsertFeed(ctx, in)
	if err != nil {
		t.Fatalf("re-sync changed: %v", err)
	}
	if res.EpisodesAdded != 0 || res.EpisodesUpdated != 1 {
		t.Fatalf("one changed episode: added=%d updated=%d", res.EpisodesAdded, res.EpisodesUpdated)
	}
}

func TestReAddFeedThatGainsGUID(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	url := "http://feed.example/f"

	// Subscribed without a <podcast:guid> -> identity_key is feed:URL.
	first, err := st.UpsertFeed(ctx, feedInput(url, "Alpha"))
	if err != nil {
		t.Fatalf("first UpsertFeed: %v", err)
	}

	// The publisher later adds a guid, flipping the computed identity_key to pguid:...
	// A re-add/OPML-reimport must update the same row (matched by feed_url), not INSERT
	// and violate UNIQUE(feed_url).
	in := feedInput(url, "Alpha")
	in.Feed.GUID = "show-guid-xyz"
	in.IdentityKey = identity.PodcastKey("show-guid-xyz", url)
	second, err := st.UpsertFeed(ctx, in)
	if err != nil {
		t.Fatalf("re-add after gaining a guid failed (UNIQUE(feed_url)?): %v", err)
	}
	if second.Created {
		t.Fatal("re-add should update the existing podcast, not create a new one")
	}
	if second.PodcastPID != first.PodcastPID {
		t.Fatalf("re-add changed the podcast pid: %s -> %s", first.PodcastPID, second.PodcastPID)
	}
	// The row now resolves under the new guid-based identity key.
	if _, err := st.PodcastByIdentity(ctx, identity.PodcastKey("show-guid-xyz", url)); err != nil {
		t.Fatalf("podcast should be findable by its new guid identity: %v", err)
	}
}

func TestTruncatedFeedDoesNotDeleteEpisodes(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()

	res, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Alpha", "Beta", "Gamma"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	// A later sync lists only the newest episode; the older two must remain.
	if _, err := st.UpsertFeed(ctx, feedInput("http://feed.example/f", "Gamma")); err != nil {
		t.Fatalf("truncated sync: %v", err)
	}
	eps, _ := st.EpisodesByPodcast(ctx, res.PodcastPID, 0)
	if len(eps) != 3 {
		t.Fatalf("truncated feed deleted episodes: have %d, want 3", len(eps))
	}
}
