package sqlite_test

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// oneEpisodeFeed builds a feed carrying a single episode with a stable GUID, so a
// re-sync updates the same episode rather than inserting a new one.
func oneEpisodeFeed(url, guid, title, desc, link string) model.UpsertFeedInput {
	return model.UpsertFeedInput{
		FeedURL:     url,
		IdentityKey: identity.PodcastKey("", url),
		Feed: model.Feed{Title: "Show", Author: "Host", Episodes: []model.FeedEpisode{{
			GUID: guid, Title: title, Description: desc, Link: link,
			EnclosureURL: url + "/e.mp3", EnclosureType: "audio/mpeg",
			DurationMS: 1000, PubDateNS: 1_000_000_000,
		}}},
		FetchedAtNS: 1,
	}
}

func episodePID(t *testing.T, st *sqlite.Store) model.PID {
	t.Helper()
	items, err := st.QueryItems(context.Background(),
		query.New(query.EntityItems).Where("kind", query.OpIs, "episode").Build(), "")
	if err != nil || len(items) == 0 {
		t.Fatalf("episode items: %v (n=%d)", err, len(items))
	}
	return items[0].PID
}

func TestEditEpisodeFields(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := st.UpsertFeed(ctx, oneEpisodeFeed("http://feed/x", "g1", "Original", "Original Desc", "http://link/1")); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	pid := episodePID(t, st)

	edits := map[string]string{
		"title": "Curated", "description": "Curated Desc", "explicit": "true",
		"season": "2", "episode_no": "5", "episode_type": "bonus", "pinned": "true",
	}
	if err := st.EditItemFields(ctx, pid, edits, model.SourceUser, true, false); err != nil {
		t.Fatalf("edit episode: %v", err)
	}

	d, err := st.EpisodeByPID(ctx, pid)
	if err != nil {
		t.Fatalf("read episode: %v", err)
	}
	e := d.Episode
	if e.Title != "Curated" || e.Description != "Curated Desc" || !e.Explicit ||
		e.Season != 2 || e.EpisodeNo != 5 || e.EpisodeType != model.EpisodeBonus || !e.Pinned {
		t.Fatalf("episode = %+v", e)
	}

	// A bad episode_type is rejected.
	if err := st.EditItemField(ctx, pid, "episode_type", "weird", model.SourceUser, false, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad episode_type = %v, want CodeInvalid", err)
	}
	// A track/book field is rejected on an episode.
	if err := st.EditItemField(ctx, pid, "artist", "X", model.SourceUser, false, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("artist on episode = %v, want CodeInvalid", err)
	}
}

func TestEditEpisodeSurvivesFeedResync(t *testing.T) {
	st, _ := openTestStore(t)
	ctx := context.Background()
	if _, err := st.UpsertFeed(ctx, oneEpisodeFeed("http://feed/x", "g1", "Original", "Original Desc", "http://link/1")); err != nil {
		t.Fatalf("upsert feed: %v", err)
	}
	pid := episodePID(t, st)

	// Lock title + description via an edit; leave link unlocked.
	if err := st.EditItemFields(ctx, pid, map[string]string{"title": "Curated", "description": "Curated Desc"},
		model.SourceUser, true, false); err != nil {
		t.Fatalf("edit: %v", err)
	}

	// The feed re-publishes the episode with a new title, description, AND link.
	if _, err := st.UpsertFeed(ctx, oneEpisodeFeed("http://feed/x", "g1", "Feed Title", "Feed Desc", "http://link/2")); err != nil {
		t.Fatalf("resync: %v", err)
	}

	d, err := st.EpisodeByPID(ctx, pid)
	if err != nil {
		t.Fatalf("read episode: %v", err)
	}
	e := d.Episode
	if e.Title != "Curated" || e.Description != "Curated Desc" {
		t.Fatalf("locked fields clobbered by resync: title %q desc %q", e.Title, e.Description)
	}
	// The unlocked link DID update from the feed.
	if e.Link != "http://link/2" {
		t.Fatalf("unlocked link = %q, want the feed's new value", e.Link)
	}
}
