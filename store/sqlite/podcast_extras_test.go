package sqlite

import (
	"context"
	"testing"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// extrasFeedInput builds a two-episode feed whose channel and first episode carry
// Podcasting 2.0 extras (funding, medium, persons, soundbites).
func extrasFeedInput(feedURL string) model.UpsertFeedInput {
	eps := make([]model.FeedEpisode, 0, 2)
	for i, title := range []string{"Alpha", "Beta"} {
		eps = append(eps, model.FeedEpisode{
			GUID: "guid-" + title, Title: title, EnclosureURL: feedURL + "/" + title + ".mp3",
			EnclosureType: "audio/mpeg", DurationMS: 1000, PubDateNS: int64(i+1) * 1_000_000_000,
		})
	}
	eps[0].Persons = []model.FeedPerson{{Name: "Alex Guest", Role: "guest"}}
	eps[0].Soundbites = []model.FeedSoundbite{
		{StartMS: 73000, DurationMS: 60500, Title: "The best bit"},
		{StartMS: 200000, DurationMS: 30000},
	}
	return model.UpsertFeedInput{
		FeedURL:     feedURL,
		IdentityKey: identity.PodcastKey("", feedURL),
		FetchedAtNS: 1,
		Feed: model.Feed{
			Title: "My Show", Author: "Host",
			FundingURL: "https://h/support", FundingMessage: "Support us", Medium: "podcast",
			Persons: []model.FeedPerson{
				{Name: "Jane Host", Role: "host", Group: "cast", Img: "https://h/j.jpg", Href: "https://h/j"},
				{Name: "Sam Producer"},
			},
			Episodes: eps,
		},
	}
}

func episodeByTitle(t *testing.T, st *Store, podPID model.PID, title string) *model.Episode {
	t.Helper()
	eps, err := st.EpisodesByPodcast(context.Background(), podPID, 0)
	if err != nil {
		t.Fatalf("EpisodesByPodcast: %v", err)
	}
	for _, e := range eps {
		if e.Title == title {
			return e
		}
	}
	t.Fatalf("no episode titled %q", title)
	return nil
}

func TestPodcasting20ExtrasPersistAndRead(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()

	res, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/f"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	pod, err := st.PodcastByPID(ctx, res.PodcastPID)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if pod.FundingURL != "https://h/support" || pod.FundingMessage != "Support us" || pod.Medium != "podcast" {
		t.Fatalf("podcast extras = %q/%q/%q", pod.FundingURL, pod.FundingMessage, pod.Medium)
	}
	if len(pod.Persons) != 2 || pod.Persons[0].Name != "Jane Host" || pod.Persons[0].Role != "host" ||
		pod.Persons[0].Group != "cast" || pod.Persons[1].Name != "Sam Producer" {
		t.Fatalf("channel persons = %+v", pod.Persons)
	}
	// The list read leaves persons empty (detail-only load).
	pods, err := st.Podcasts(ctx)
	if err != nil || len(pods) != 1 {
		t.Fatalf("Podcasts: %v (%d)", err, len(pods))
	}
	if len(pods[0].Persons) != 0 {
		t.Fatalf("list read loaded persons: %+v", pods[0].Persons)
	}

	alpha := episodeByTitle(t, st, res.PodcastPID, "Alpha")
	d, err := st.EpisodeByPID(ctx, alpha.PID)
	if err != nil {
		t.Fatalf("EpisodeByPID: %v", err)
	}
	if len(d.Persons) != 1 || d.Persons[0].Name != "Alex Guest" || d.Persons[0].Role != "guest" {
		t.Fatalf("episode persons = %+v", d.Persons)
	}
	if len(d.Soundbites) != 2 || d.Soundbites[0].StartMS != 73000 || d.Soundbites[0].DurationMS != 60500 ||
		d.Soundbites[0].Title != "The best bit" || d.Soundbites[1].Title != "" {
		t.Fatalf("episode soundbites = %+v", d.Soundbites)
	}
	// The extras-free episode reads back empty.
	beta := episodeByTitle(t, st, res.PodcastPID, "Beta")
	db, err := st.EpisodeByPID(ctx, beta.PID)
	if err != nil {
		t.Fatalf("EpisodeByPID(beta): %v", err)
	}
	if len(db.Persons) != 0 || len(db.Soundbites) != 0 {
		t.Fatalf("beta extras should be empty: %+v %+v", db.Persons, db.Soundbites)
	}
}

func TestPodcasting20IdenticalReSyncIsSilent(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	if _, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/f")); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}

	// Capture the child rowids so the re-sync can be shown to leave them alone.
	rowids := func() []int64 {
		var ids []int64
		for _, q := range []string{
			"SELECT id FROM podcast_person ORDER BY id",
			"SELECT rowid FROM episode_soundbite ORDER BY rowid",
		} {
			rows, err := st.read.QueryContext(ctx, q)
			if err != nil {
				t.Fatalf("rowids: %v", err)
			}
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan: %v", err)
				}
				ids = append(ids, id)
			}
			rows.Close()
		}
		return ids
	}
	before := rowids()
	if len(before) != 5 { // 2 channel persons + 1 episode person + 2 soundbites
		t.Fatalf("child rows = %d, want 5", len(before))
	}

	seqBefore, _ := st.LatestChangeSeq(ctx)
	res, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/f"))
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.EpisodesAdded != 0 || res.EpisodesUpdated != 0 {
		t.Fatalf("identical re-sync touched episodes: %+v", res)
	}
	seqAfter, _ := st.LatestChangeSeq(ctx)
	if got := seqAfter - seqBefore; got != 0 {
		t.Fatalf("identical extras re-sync emitted %d deltas, want 0", got)
	}
	after := rowids()
	if len(after) != len(before) {
		t.Fatalf("child rows changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("child rowids rewritten: %v -> %v", before, after)
		}
	}
}

func TestPodcasting20OneEpisodePersonChange(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	if _, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/f")); err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	seqBefore, _ := st.LatestChangeSeq(ctx)

	in := extrasFeedInput("http://feed.example/f")
	in.Feed.Episodes[0].Persons = []model.FeedPerson{{Name: "New Guest", Role: "guest"}}
	res, err := st.UpsertFeed(ctx, in)
	if err != nil {
		t.Fatalf("re-sync: %v", err)
	}
	if res.EpisodesAdded != 0 || res.EpisodesUpdated != 1 {
		t.Fatalf("one-person change: %+v", res)
	}
	// Exactly one item delta: the changed episode, nothing else.
	changes, err := st.ChangesSince(ctx, seqBefore)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	items := 0
	for _, c := range changes {
		if c.EntityType == "item" {
			items++
		}
	}
	if items != 1 {
		t.Fatalf("item deltas = %d, want 1: %+v", items, changes)
	}
	// The replace was scoped: channel persons and the untouched soundbites
	// survive, and the changed episode reads the new credit.
	alpha := episodeByTitle(t, st, res.PodcastPID, "Alpha")
	d, err := st.EpisodeByPID(ctx, alpha.PID)
	if err != nil {
		t.Fatalf("EpisodeByPID: %v", err)
	}
	if len(d.Persons) != 1 || d.Persons[0].Name != "New Guest" {
		t.Fatalf("alpha persons = %+v", d.Persons)
	}
	if len(d.Soundbites) != 2 {
		t.Fatalf("alpha soundbites disturbed: %+v", d.Soundbites)
	}
	pod, err := st.PodcastByPID(ctx, res.PodcastPID)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if len(pod.Persons) != 2 {
		t.Fatalf("channel persons disturbed: %+v", pod.Persons)
	}
}

func TestPodcasting20RemoveLeavesNoOrphans(t *testing.T) {
	st, _ := entityFixture(t)
	ctx := context.Background()
	res, err := st.UpsertFeed(ctx, extrasFeedInput("http://feed.example/f"))
	if err != nil {
		t.Fatalf("UpsertFeed: %v", err)
	}
	if _, err := st.RemovePodcast(ctx, res.PodcastPID); err != nil {
		t.Fatalf("RemovePodcast: %v", err)
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM podcast_person",
		"SELECT COUNT(*) FROM episode_soundbite",
	} {
		var n int
		if err := st.read.QueryRowContext(ctx, q).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("%s = %d after remove, want 0", q, n)
		}
	}
}
