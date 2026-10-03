package podcast_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

// failFeedStore delegates to a real store but can refuse UpsertFeed, standing in for
// a sync whose catalog write did not commit.
type failFeedStore struct {
	podcast.Store
	fail bool
}

func (f *failFeedStore) UpsertFeed(ctx context.Context, in model.UpsertFeedInput) (*model.UpsertFeedResult, error) {
	if f.fail {
		return nil, errors.New("simulated catalog failure")
	}
	return f.Store.UpsertFeed(ctx, in)
}

// scriptedProvider is a youtube mock whose next Enumerate answer the test sets,
// recording the ETag each call received.
type scriptedProvider struct {
	answer *source.Enumeration
	seen   []string
}

func (p *scriptedProvider) mock() *source.Mock {
	return &source.Mock{Type: model.SourceYouTube,
		EnumerateFunc: func(_ context.Context, req source.Request) (*source.Enumeration, error) {
			p.seen = append(p.seen, req.ETag)
			return p.answer, nil
		}}
}

// enumeration is a channel answer with n videos under the validators tag and lm-tag
// (none when tag is empty).
func enumeration(key, tag string, n int) *source.Enumeration {
	eps := make([]model.FeedEpisode, n)
	for i := range eps {
		id := fmt.Sprintf("v%d", i+1)
		eps[i] = model.FeedEpisode{Title: "Video " + id, GUID: "youtube:video:" + id, EnclosureURL: "yt://" + id}
	}
	out := &source.Enumeration{IdentityKey: key, ETag: tag, Feed: &model.Feed{Title: "Chan", Episodes: eps}}
	if tag != "" {
		out.LastModified = "lm-" + tag
	}
	return out
}

// TestSyncHandsBackCommittedValidators pins what source.Provider.Enumerate promises:
// the validators a sync commits come back on the next request, a NotModified answer's
// do not, and a failed write leaves the previous pair in place.
func TestSyncHandsBackCommittedValidators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)
	failing := &failFeedStore{Store: st}
	prov := &scriptedProvider{}
	svc := podcast.New(failing, meta.NewReader(),
		podcast.Config{Dir: t.TempDir(), Providers: []source.Provider{prov.mock()}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	// A clock the test moves, so a fetch time is asserted exactly rather than against a
	// wall clock that may not have ticked.
	clock := time.Now()
	podcast.SetClock(svc, func() time.Time { return clock })
	tick := func() int64 {
		clock = clock.Add(time.Minute)
		return clock.UnixNano()
	}

	prov.answer = enumeration("youtube:channel:c1", "c1", 1)
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if pod.ETag != "c1" || pod.LastModified != "lm-c1" {
		t.Fatalf("stored validators after add = %q/%q, want c1/lm-c1", pod.ETag, pod.LastModified)
	}
	expect := func(step, etag string, episodes int, fetched int64) {
		t.Helper()
		got, err := st.PodcastByPID(ctx, pod.PID)
		if err != nil {
			t.Fatalf("PodcastByPID: %v", err)
		}
		if got.ETag != etag || got.LastModified != "lm-"+etag || got.EpisodeCount != episodes || got.LastFetchedAt != fetched {
			t.Fatalf("%s: validators %q/%q episodes %d fetched %d, want %s/lm-%s %d %d",
				step, got.ETag, got.LastModified, got.EpisodeCount, got.LastFetchedAt, etag, etag, episodes, fetched)
		}
	}

	at := tick()
	prov.answer = enumeration("youtube:channel:c1", "c2", 2)
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	expect("second enumeration", "c2", 2, at)

	// NotModified keeps the stored pair whatever it carries, and still counts as a sync.
	at = tick()
	prov.answer = &source.Enumeration{NotModified: true, ETag: "c3", LastModified: "lm-c3"}
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("Sync not-modified: %v", err)
	}
	expect("not-modified", "c2", 2, at)

	// A write that does not commit hands nothing back and records no fetch: the
	// provider sees c2 again.
	failing.fail = true
	tick()
	prov.answer = enumeration("youtube:channel:c1", "c4", 3)
	if _, err := svc.Sync(ctx, pod.PID); err == nil {
		t.Fatal("Sync should surface the failed catalog write")
	}
	expect("failed write", "c2", 2, at)
	failing.fail = false
	at = tick()
	if _, err := svc.Sync(ctx, pod.PID); err != nil {
		t.Fatalf("Sync after failure: %v", err)
	}
	expect("retry", "c4", 3, at)

	// A provider that answers neither a feed nor NotModified is a bug, not a quiet sync.
	tick()
	prov.answer = &source.Enumeration{ETag: "c5", LastModified: "lm-c5"}
	if _, err := svc.Sync(ctx, pod.PID); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("Sync with no feed and no NotModified = %v, want CodeIO", err)
	}
	expect("malformed answer", "c4", 3, at)

	want := []string{"", "c1", "c2", "c2", "c2", "c4"}
	if strings.Join(prov.seen, ",") != strings.Join(want, ",") {
		t.Fatalf("validators handed to Enumerate = %q, want %q", prov.seen, want)
	}
}

// TestSyncRefusesUnconditionalNotModified: a show whose last enumeration carried no
// validators sends none, so a NotModified answer is a misbehaving source, refused the
// way AddSource refuses it, and not recorded as a sync.
func TestSyncRefusesUnconditionalNotModified(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prov := &scriptedProvider{}
	svc, st, _ := newTestService(t, prov.mock())
	clock := time.Now()
	podcast.SetClock(svc, func() time.Time { return clock })

	prov.answer = enumeration("youtube:channel:c1", "", 1)
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	clock = clock.Add(time.Minute)
	prov.answer = &source.Enumeration{NotModified: true}
	if _, err := svc.Sync(ctx, pod.PID); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("unconditional not-modified = %v, want CodeIO", err)
	}
	got, err := st.PodcastByPID(ctx, pod.PID)
	if err != nil {
		t.Fatalf("PodcastByPID: %v", err)
	}
	if got.LastFetchedAt != pod.LastFetchedAt {
		t.Fatalf("fetch time moved to %d on a refused answer, want %d", got.LastFetchedAt, pod.LastFetchedAt)
	}
}

// TestSyncAllMarksUnchangedShows: every show that answered NotModified in a pass gets
// the pass's fetch time.
func TestSyncAllMarksUnchangedShows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prov := &scriptedProvider{}
	svc, st, _ := newTestService(t, prov.mock())
	clock := time.Now()
	podcast.SetClock(svc, func() time.Time { return clock })
	var pids []model.PID
	for _, c := range []string{"a", "b"} {
		prov.answer = enumeration("youtube:channel:"+c, c, 1)
		pod, err := svc.AddSource(ctx, "yt://"+c, model.SourceYouTube, podcast.AddOptions{})
		if err != nil {
			t.Fatalf("AddSource %s: %v", c, err)
		}
		pids = append(pids, pod.PID)
	}
	clock = clock.Add(time.Minute)
	prov.answer = &source.Enumeration{NotModified: true}
	if _, err := svc.SyncAll(ctx); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	for _, pid := range pids {
		got, err := st.PodcastByPID(ctx, pid)
		if err != nil {
			t.Fatalf("PodcastByPID: %v", err)
		}
		if got.LastFetchedAt != clock.UnixNano() {
			t.Fatalf("%s fetched %d after SyncAll, want %d", pid, got.LastFetchedAt, clock.UnixNano())
		}
	}
}
