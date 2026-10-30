package podcast_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

func longURL(n int) string {
	return "https://feeds.example/" + strings.Repeat("u", n-len("https://feeds.example/"))
}

// TestAddManualCapsTitles: a show or episode added by hand has its text held to the
// feed bounds, and a URL or GUID past its bound is refused rather than stored cut or left
// out.
func TestAddManualCapsTitles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	pod, err := svc.AddManual(ctx, strings.Repeat("é", podcast.MaxTitleChars+1), podcast.ManualOptions{
		Author: strings.Repeat("a", podcast.MaxNameChars+1), Description: strings.Repeat("d", podcast.MaxDescriptionBytes+1),
	})
	if err != nil {
		t.Fatalf("add manual: %v", err)
	}
	if n := utf8.RuneCountInString(pod.Title); n != podcast.MaxTitleChars ||
		len(pod.Author) != podcast.MaxNameChars || len(pod.Description) != podcast.MaxDescriptionBytes {
		t.Fatalf("show title %d characters, author %d, description %d bytes, want %d, %d and %d",
			n, len(pod.Author), len(pod.Description), podcast.MaxTitleChars, podcast.MaxNameChars, podcast.MaxDescriptionBytes)
	}
	if _, err := svc.AddManual(ctx, "Linked", podcast.ManualOptions{Link: longURL(podcast.MaxURLBytes + 1)}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("add manual with an overlong link = %v, want CodeInvalid", err)
	}

	res, err := svc.AddEpisode(ctx, pod.PID, model.FeedEpisode{Title: strings.Repeat("é", podcast.MaxTitleChars+1)}, true)
	if err != nil {
		t.Fatalf("add episode: %v", err)
	}
	d, err := svc.Episode(ctx, res.EpisodePID)
	if err != nil {
		t.Fatalf("episode: %v", err)
	}
	if n := utf8.RuneCountInString(d.Episode.Title); n != podcast.MaxTitleChars {
		t.Fatalf("episode title holds %d characters, want %d", n, podcast.MaxTitleChars)
	}
	if _, err := svc.AddEpisode(ctx, pod.PID, model.FeedEpisode{Title: "Long", EnclosureURL: longURL(podcast.MaxURLBytes + 1)}, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("add episode with an overlong enclosure = %v, want CodeInvalid", err)
	}
	// A GUID keys the episode, so one cut short could merge two; it is refused instead.
	if _, err := svc.AddEpisode(ctx, pod.PID, model.FeedEpisode{GUID: strings.Repeat("g", podcast.MaxGUIDBytes+1)}, true); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("add episode with an overlong guid = %v, want CodeInvalid", err)
	}
}

// TestAProvidersFeedIsCapped: an injected provider's feed is held to the same bounds as
// a parsed one.
func TestAProvidersFeedIsCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", podcast.MaxTitleChars+1)
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed: &model.Feed{Title: long, Episodes: []model.FeedEpisode{{Title: long, GUID: "youtube:video:v1"}}}}
	svc, _, _ := newTestService(t, yt)
	ctx := context.Background()
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}
	if a, b := utf8.RuneCountInString(pod.Title), utf8.RuneCountInString(eps[0].Title); a != podcast.MaxTitleChars || b != podcast.MaxTitleChars {
		t.Fatalf("show title %d and episode title %d characters, want %d", a, b, podcast.MaxTitleChars)
	}
}

// TestDroppedEpisodesAreWarnedOnce: a feed that listed more items than a show keeps is
// named by its host, with the count, once per enumeration.
func TestDroppedEpisodesAreWarnedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rss := &source.Mock{Type: model.SourceRSS, IdentityKey: "pguid:long",
		Feed: &model.Feed{Title: "Long", Episodes: []model.FeedEpisode{{Title: "One", GUID: "one"}}, EpisodesDropped: 3}}
	logs := &syncBuffer{}
	svc := podcast.New(newTestStore(t), meta.NewReader(), podcast.Config{Dir: t.TempDir(), Providers: []source.Provider{rss}},
		slog.New(slog.NewTextHandler(logs, nil)))
	pod, err := svc.Add(ctx, "https://feeds.example/private/feed.xml?token=hush", podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("sync: %v", err)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 2 || strings.Count(out, "dropped=3") != 2 ||
		strings.Count(out, "host=https://feeds.example") != 2 || strings.Contains(out, "hush") {
		t.Fatalf("logs = %q, want one warning naming the host and the 3 dropped for the add and one for the sync", out)
	}
}

// TestImportOPMLRefusesADocumentPastItsCaps: an OPML document longer than a feed may be,
// or listing more feeds than an import takes, is refused before any feed is fetched.
func TestImportOPMLRefusesADocumentPastItsCaps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	small := podcast.New(newTestStore(t), meta.NewReader(), podcast.Config{Dir: t.TempDir(), MaxFeedBytes: 64},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	doc := `<opml version="2.0"><body><outline type="rss" xmlUrl="http://feeds.example/a.xml"/></body></opml>`
	if _, err := small.ImportOPML(ctx, []byte(doc)); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("import of a %d-byte document under a 64-byte cap = %v, want CodeInvalid", len(doc), err)
	}

	var b strings.Builder
	b.WriteString(`<opml version="2.0"><body>`)
	for i := range podcast.MaxOPMLEntries + 1 {
		fmt.Fprintf(&b, `<outline type="rss" xmlUrl="http://feeds.example/%d.xml"/>`, i)
	}
	b.WriteString(`</body></opml>`)
	if _, err := podcast.ParseOPML([]byte(b.String())); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("an OPML listing %d feeds = %v, want CodeInvalid", podcast.MaxOPMLEntries+1, err)
	}

	entries, err := podcast.ParseOPML([]byte(`<opml><body><outline xmlUrl="http://feeds.example/a.xml" title="` +
		strings.Repeat("é", podcast.MaxTitleChars+1) + `"/></body></opml>`))
	if err != nil || len(entries) != 1 || utf8.RuneCountInString(entries[0].Title) != podcast.MaxTitleChars {
		t.Fatalf("entries = %+v (err %v), want one with a title of %d characters", entries, err, podcast.MaxTitleChars)
	}
}

// TestAddRefusesAnOverlongFeedURL: a feed address past MaxURLBytes is refused before it
// is fetched.
func TestAddRefusesAnOverlongFeedURL(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	svc, _, _ := newTestService(t)
	url := srv.URL + "/" + strings.Repeat("u", podcast.MaxURLBytes)
	if _, err := svc.Add(context.Background(), url, podcast.AddOptions{}); !waxerr.Is(err, waxerr.CodeInvalid) || hits.Load() != 0 {
		t.Fatalf("add of an overlong address = %v after %d requests, want CodeInvalid and none", err, hits.Load())
	}
}

// TestCappingLeavesTheProvidersFeedAlone: holding a provider's feed or a caller's episode
// to the bounds writes the bounded copy and leaves the one handed over as it was.
func TestCappingLeavesTheProvidersFeedAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	long := strings.Repeat("é", podcast.MaxTitleChars+1)
	persons := make([]model.FeedPerson, podcast.MaxPersons+1)
	for i := range persons {
		persons[i] = model.FeedPerson{Name: long}
	}
	feed := &model.Feed{Title: long, Persons: persons,
		Episodes: []model.FeedEpisode{{Title: long, GUID: "youtube:video:v1", Persons: persons}}}
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1", Feed: feed}
	svc, _, _ := newTestService(t, yt)
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	ep := model.FeedEpisode{Title: long, GUID: "hand", Persons: persons}
	if _, err := svc.AddEpisode(ctx, pod.PID, ep, true); err != nil {
		t.Fatalf("add episode: %v", err)
	}
	if feed.Title != long || feed.Episodes[0].Title != long || len(feed.Persons) != podcast.MaxPersons+1 ||
		len(feed.Episodes[0].Persons) != podcast.MaxPersons+1 || ep.Title != long {
		t.Fatal("capping changed the feed the provider handed over or the episode the caller passed")
	}
	for i, p := range persons {
		if p.Name != long {
			t.Fatalf("person %d was cut in the caller's own slice", i)
		}
	}
}
