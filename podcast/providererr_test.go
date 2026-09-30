package podcast_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

// providerFailure asserts err is the provider's failure for op, keeping class.
func providerFailure(t *testing.T, step string, err error, op string, class waxerr.Code) {
	t.Helper()
	var pe *source.ProviderError
	if !errors.As(err, &pe) || !source.IsProviderError(err) {
		t.Fatalf("%s: %v is not a provider error", step, err)
	}
	if pe.Op != op || pe.SourceType != model.SourceYouTube || !waxerr.Is(err, class) {
		t.Fatalf("%s: provider error %s/%q class %s, want youtube/%q class %s",
			step, pe.SourceType, pe.Op, waxerr.CodeOf(err), op, class)
	}
}

// TestProviderFailuresAreToldFromTheCatalogs: every call into a provider that fails
// comes back as a ProviderError keeping its class, while a failed catalog write and a
// canceled call do not.
func TestProviderFailuresAreToldFromTheCatalogs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	failing := &failFeedStore{Store: st}
	var enumErr, fetchErr error
	var answer *source.Enumeration
	yt := &source.Mock{Type: model.SourceYouTube,
		EnumerateFunc: func(context.Context, source.Request) (*source.Enumeration, error) {
			return answer, enumErr
		},
		FetchFunc: func(_ context.Context, _ source.FetchRequest, w io.Writer) (*source.FetchResult, error) {
			if fetchErr != nil {
				return nil, fetchErr
			}
			n, _ := w.Write([]byte("audio"))
			return &source.FetchResult{Bytes: int64(n), ContentHash: "sha256:x"}, nil
		}}
	svc := podcast.New(failing, meta.NewReader(),
		podcast.Config{Dir: t.TempDir(), Providers: []source.Provider{yt}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	enumErr = waxerr.New(waxerr.CodeNotFound, "youtube", "channel removed")
	_, err := svc.AddSource(ctx, "yt://gone", model.SourceYouTube, podcast.AddOptions{})
	providerFailure(t, "add", err, "enumerate", waxerr.CodeNotFound)

	enumErr = nil
	answer = enumeration("youtube:channel:c1", "c1", 1)
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	enumErr = waxerr.New(waxerr.CodeNotFound, "youtube", "channel removed")
	_, err = svc.Sync(ctx, pod.PID)
	providerFailure(t, "sync", err, "enumerate", waxerr.CodeNotFound)

	enumErr = nil
	answer = &source.Enumeration{ETag: "c2"}
	_, err = svc.Sync(ctx, pod.PID)
	providerFailure(t, "an answer the contract rules out", err, "enumerate", waxerr.CodeIO)

	answer = enumeration("youtube:channel:c1", "c2", 1)
	failing.fail = true
	if _, err := svc.Sync(ctx, pod.PID); err == nil || source.IsProviderError(err) {
		t.Fatalf("a failed catalog write = %v, want an error that is not the provider's", err)
	}
	failing.fail = false

	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}
	fetchErr = waxerr.New(waxerr.CodeIO, "youtube", "stream reset")
	_, err = svc.Download(ctx, eps[0].PID)
	providerFailure(t, "download", err, "fetch", waxerr.CodeIO)

	cctx, cancel := context.WithCancel(ctx)
	enumErr = errors.New("connection closed")
	yt.EnumerateFunc = func(context.Context, source.Request) (*source.Enumeration, error) {
		cancel()
		return nil, enumErr
	}
	if _, err := svc.Sync(cctx, pod.PID); !waxerr.Is(err, waxerr.CodeCanceled) || source.IsProviderError(err) {
		t.Fatalf("a sync canceled mid-enumeration = %v (class %s), want a cancellation that is not the provider's",
			err, waxerr.CodeOf(err))
	}
}

// TestDownloadBlamesAFailedCatalogWriteOnTheCatalog: bytes that land but whose
// catalog write fails are not the provider's failure.
func TestDownloadBlamesAFailedCatalogWriteOnTheCatalog(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	yt := &source.Mock{Type: model.SourceYouTube, IdentityKey: "youtube:channel:c1",
		Feed:    &model.Feed{Title: "Chan", Episodes: []model.FeedEpisode{{Title: "One", GUID: "youtube:video:v1", EnclosureURL: "yt://v1"}}},
		Payload: []byte("audio")}
	svc := podcast.New(failAttachStore{st}, meta.NewReader(),
		podcast.Config{Dir: t.TempDir(), Providers: []source.Provider{yt}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	pod, err := svc.AddSource(ctx, "yt://c1", model.SourceYouTube, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 1 {
		t.Fatalf("episodes = %d (err %v), want 1", len(eps), err)
	}
	if _, err := svc.Download(ctx, eps[0].PID); err == nil || source.IsProviderError(err) {
		t.Fatalf("Download with a failed catalog write = %v, want an error that is not the provider's", err)
	}
}

// TestSyncAllReportsEachFailure: a batch keeps going past a dead feed and hands back
// its error, the provider's mark included, beside the shows that synced.
func TestSyncAllReportsEachFailure(t *testing.T) {
	ctx := context.Background()
	answers := map[string]*source.Enumeration{}
	var dead string
	yt := &source.Mock{Type: model.SourceYouTube,
		EnumerateFunc: func(_ context.Context, req source.Request) (*source.Enumeration, error) {
			if req.URL == dead {
				return nil, waxerr.New(waxerr.CodeIO, "youtube", "unreachable")
			}
			return answers[req.URL], nil
		}}
	svc, _, _ := newTestService(t, yt)
	var pids []model.PID
	for _, c := range []string{"a", "b"} {
		answers["yt://"+c] = enumeration("youtube:channel:"+c, c, 1)
		pod, err := svc.AddSource(ctx, "yt://"+c, model.SourceYouTube, podcast.AddOptions{})
		if err != nil {
			t.Fatalf("AddSource %s: %v", c, err)
		}
		pids = append(pids, pod.PID)
	}
	answers["yt://a"] = enumeration("youtube:channel:a", "a2", 2)
	dead = "yt://b"

	res, err := svc.SyncAll(ctx)
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if got := res.Results[pids[0]]; got == nil || got.EpisodesAdded != 1 || len(res.Results) != 1 {
		t.Fatalf("results = %v, want the live show alone with one new episode", res.Results)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %v, want the dead show alone", res.Failures)
	}
	providerFailure(t, "the dead show", res.Failures[pids[1]], "enumerate", waxerr.CodeIO)
}
