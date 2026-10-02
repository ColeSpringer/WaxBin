package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
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

// TestReAddTwoSubscriptionsThatGainOneGUID: one show subscribed under two feed URLs before
// it carried a guid has two rows. When both are re-added under the guid, the first adopts
// the guid's key, and the second updates its own row under its own key rather than taking
// over the first row and colliding on its feed URL.
func TestReAddTwoSubscriptionsThatGainOneGUID(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	urls := []string{"http://feed.example/f", "http://mirror.example/f"}
	var pids []model.PID
	for _, u := range urls {
		res, err := st.UpsertFeed(ctx, feedInput(u, "Alpha"))
		if err != nil {
			t.Fatalf("subscribe %s: %v", u, err)
		}
		pids = append(pids, res.PodcastPID)
	}
	for i, u := range urls {
		in := feedInput(u, "Alpha")
		in.Feed.GUID = "show-guid-xyz"
		in.IdentityKey = identity.PodcastKey("show-guid-xyz", u)
		res, err := st.UpsertFeed(ctx, in)
		if err != nil {
			t.Fatalf("re-add %s: %v", u, err)
		}
		if res.PodcastPID != pids[i] || res.Created {
			t.Errorf("re-add %s updated %s (created %v), want its own row %s", u, res.PodcastPID, res.Created, pids[i])
		}
	}
	for i, pid := range pids {
		p, err := st.PodcastByPID(ctx, pid)
		if err != nil || p.FeedURL != urls[i] {
			t.Errorf("podcast %d feed url = %q (err %v), want %q", i, p.FeedURL, err, urls[i])
		}
		if eps, _ := st.EpisodesByPodcast(ctx, pid, 0); len(eps) != 1 {
			t.Errorf("podcast %d holds %d episodes, want its one", i, len(eps))
		}
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

// EnsurePodcastLibrary refuses to adopt a library that is not a podcast one. The root
// lookup folds case where the platform's path rule does, which widened the ways a
// podcast dir and a music root can collide; filing episodes into a scanned library
// would leave the scanner treating every download as a track.
func TestEnsurePodcastLibraryRefusesAMusicRoot(t *testing.T) {
	ctx := context.Background()
	st, _, lib := openStoreAt(t)

	if _, err := st.EnsurePodcastLibrary(ctx, string(lib.Root)); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("EnsurePodcastLibrary on a music root = %v, want CodeConflict", err)
	}
	id, err := st.EnsurePodcastLibrary(ctx, "/pods")
	if err != nil {
		t.Fatalf("EnsurePodcastLibrary: %v", err)
	}
	again, err := st.EnsurePodcastLibrary(ctx, "/pods")
	if err != nil || again != id {
		t.Errorf("second EnsurePodcastLibrary = %d (err %v), want the first id %d", again, err, id)
	}
}

// TestUpsertFeedFailureKeepsPriorValidators: a sync whose transaction fails after the
// show row was rewritten commits neither the new validators nor the new episode,
// which is the receipt source.Provider.Enumerate promises. The failure is a feed image
// with no attribution, which the art writer refuses between the show row and the
// episode loop.
func TestUpsertFeedFailureKeepsPriorValidators(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	first := feedInput("http://feed.example/f", "Alpha")
	first.ETag, first.LastModified, first.FetchedAtNS = `"v1"`, "Mon, 01 Jan 2024 00:00:00 GMT", 100
	res, err := st.UpsertFeed(ctx, first)
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	second := feedInput("http://feed.example/f", "Alpha", "Beta")
	second.ETag, second.LastModified, second.FetchedAtNS = `"v2"`, "Tue, 02 Jan 2024 00:00:00 GMT", 200
	second.Image = &model.ArtImage{Data: []byte{1}, Hash: "sha256:x", Format: "png", Width: 1, Height: 1}
	if _, err := st.UpsertFeed(ctx, second); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("UpsertFeed with an unattributed image = %v, want CodeInvalid", err)
	}
	pod, err := st.PodcastByPID(ctx, res.PodcastPID)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if pod.ETag != `"v1"` || pod.LastModified != first.LastModified || pod.LastFetchedAt != 100 || pod.EpisodeCount != 1 {
		t.Fatalf("after failed sync: etag %q last-modified %q fetched %d episodes %d, want the first sync's",
			pod.ETag, pod.LastModified, pod.LastFetchedAt, pod.EpisodeCount)
	}

	second.Image = nil
	if _, err := st.UpsertFeed(ctx, second); err != nil {
		t.Fatalf("UpsertFeed retry: %v", err)
	}
	if pod, err = st.PodcastByPID(ctx, res.PodcastPID); err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if pod.ETag != `"v2"` || pod.LastFetchedAt != 200 || pod.EpisodeCount != 2 {
		t.Fatalf("after retry: etag %q fetched %d episodes %d, want v2 / 200 / 2",
			pod.ETag, pod.LastFetchedAt, pod.EpisodeCount)
	}
}

// TestUpsertShowKeepsWhatItDoesNotCarry: UpsertShow on a synced show updates the
// fields it carries and leaves the rest alone, since it is not a sync: the feed-only
// columns, the validators and fetch time, and the source type when the input leaves it
// empty. Losing them used to be masked by the validators going with them, which made
// the next sync a full re-enumeration that filled them back in.
func TestUpsertShowKeepsWhatItDoesNotCarry(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	res, err := st.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL: "yt://c1", IdentityKey: "youtube:channel:c1", SourceType: model.SourceYouTube,
		Feed: model.Feed{Title: "Chan", Language: "en", Category: "Tech", Explicit: true, GUID: "g1",
			Episodes: []model.FeedEpisode{{Title: "One", GUID: "youtube:video:1"}}},
		ETag: "cursor-1", LastModified: "lm-1", FetchedAtNS: 100,
	})
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	pid, created, err := st.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: "youtube:channel:c1", FeedURL: "yt://c1", Title: "Chan, renamed",
	})
	if err != nil || created || pid != res.PodcastPID {
		t.Fatalf("UpsertShow = %s created=%v err=%v, want the existing show updated", pid, created, err)
	}
	pod, err := st.PodcastByPID(ctx, pid)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if pod.Title != "Chan, renamed" || pod.SourceType != model.SourceYouTube {
		t.Fatalf("after UpsertShow: title %q source %q, want the title changed and youtube kept", pod.Title, pod.SourceType)
	}
	if pod.ETag != "cursor-1" || pod.LastModified != "lm-1" || pod.LastFetchedAt != 100 {
		t.Fatalf("after UpsertShow: etag %q last-modified %q fetched %d, want the sync's kept", pod.ETag, pod.LastModified, pod.LastFetchedAt)
	}
	if pod.Language != "en" || pod.Category != "Tech" || !pod.Explicit || pod.GUID != "g1" {
		t.Fatalf("after UpsertShow: language %q category %q explicit %v guid %q, want the feed's kept",
			pod.Language, pod.Category, pod.Explicit, pod.GUID)
	}
}

// TestMarkPodcastsFetched: the mark moves only the fetch time, never backwards, emits
// no delta, and skips a show that is gone.
func TestMarkPodcastsFetched(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	in := feedInput("http://feed.example/f", "Alpha")
	in.ETag, in.FetchedAtNS = `"v1"`, 100
	res, err := st.UpsertFeed(ctx, in)
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	seq, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	fetched := func(step string, want int64) {
		t.Helper()
		pod, err := st.PodcastByPID(ctx, res.PodcastPID)
		if err != nil {
			t.Fatalf("PodcastByPID: %v", err)
		}
		if pod.LastFetchedAt != want || pod.ETag != `"v1"` {
			t.Fatalf("%s: fetched %d etag %q, want %d / v1", step, pod.LastFetchedAt, pod.ETag, want)
		}
	}
	if err := st.MarkPodcastsFetched(ctx, []model.PID{res.PodcastPID, "gone"}, 500); err != nil {
		t.Fatalf("MarkPodcastsFetched: %v", err)
	}
	fetched("after mark", 500)
	if err := st.MarkPodcastsFetched(ctx, []model.PID{res.PodcastPID}, 50); err != nil {
		t.Fatalf("MarkPodcastsFetched older: %v", err)
	}
	fetched("after an older mark", 500)
	after, err := st.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatalf("LatestChangeSeq: %v", err)
	}
	if after != seq {
		t.Fatalf("MarkPodcastsFetched advanced the change feed %d -> %d", seq, after)
	}
}
