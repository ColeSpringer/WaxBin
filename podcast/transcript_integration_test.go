package podcast_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// transcriptFixture opens a store plus service and creates one manual show with
// one episode whose transcript URL is transcriptURL ("" for none).
func transcriptFixture(t *testing.T, transcriptURL string) (*podcast.Service, *sqlite.Store, model.PID) {
	t.Helper()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir()},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	show, err := svc.AddManual(ctx, "Transcripted", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	res, err := svc.AddEpisode(ctx, show.PID, model.FeedEpisode{
		Title: "Ep", GUID: "g1", TranscriptURL: transcriptURL, TranscriptType: "application/srt",
	}, true)
	if err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}
	return svc, st, res.EpisodePID
}

func TestPutTranscriptValidatesAndReduces(t *testing.T) {
	svc, _, ep := transcriptFixture(t, "")
	ctx := context.Background()

	// Unknown format and an unknown episode are refused.
	if err := svc.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: ep, Format: "docx", Body: "x",
	}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("bad format = %v, want CodeInvalid", err)
	}
	if err := svc.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: "nope", Format: "text", Body: "x",
	}); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unknown episode = %v, want CodeNotFound", err)
	}
	// A body that reduces to nothing is refused rather than stored empty.
	if err := svc.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: ep, Format: "srt", Body: "1\n00:00:01,000 --> 00:00:02,000\n\n",
	}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("empty-after-reduction = %v, want CodeInvalid", err)
	}

	// A stored SRT lands reduced, and the read round-trips it.
	srt := "1\n00:00:01,000 --> 00:00:04,000\nthe searchable words\n"
	if err := svc.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: ep, Format: "srt", Body: srt, SourceURL: "https://h/t.srt",
	}); err != nil {
		t.Fatalf("PutTranscript: %v", err)
	}
	tr, err := svc.Transcript(ctx, ep)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if tr.Format != "srt" || tr.SourceURL != "https://h/t.srt" || tr.CreatedAt == 0 {
		t.Fatalf("transcript meta = %+v", tr)
	}
	if strings.Contains(tr.Body, "-->") || !strings.Contains(tr.Body, "the searchable words") {
		t.Fatalf("stored body not reduced: %q", tr.Body)
	}
}

func TestPutTranscriptRejectsOversizedBody(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(ctx, sqlite.OpenOptions{Path: db, Owner: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A tiny MaxFeedBytes makes the cap testable without a 16 MiB body.
	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir(), MaxFeedBytes: 8},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	show, err := svc.AddManual(ctx, "Cap", podcast.ManualOptions{})
	if err != nil {
		t.Fatalf("AddManual: %v", err)
	}
	res, err := svc.AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Ep", GUID: "g1"}, true)
	if err != nil {
		t.Fatalf("AddEpisode: %v", err)
	}
	if err := svc.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: res.EpisodePID, Format: "text", Body: "well past eight bytes",
	}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("oversized body = %v, want CodeInvalid", err)
	}
}

func TestFetchTranscriptPropagatesAndStores(t *testing.T) {
	ctx := context.Background()
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/srt")
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:04,000\nfetched transcript body\n")
	}))
	defer srv.Close()

	svc, _, ep := transcriptFixture(t, srv.URL+"/t.srt")

	// Success: fetched, reduced, stored with provenance; the episode was never
	// downloaded (the streamed-episode case FetchTranscript exists for).
	if err := svc.FetchTranscript(ctx, ep); err != nil {
		t.Fatalf("FetchTranscript: %v", err)
	}
	tr, err := svc.Transcript(ctx, ep)
	if err != nil {
		t.Fatalf("Transcript: %v", err)
	}
	if !strings.Contains(tr.Body, "fetched transcript body") || tr.SourceURL != srv.URL+"/t.srt" {
		t.Fatalf("fetched transcript = %+v", tr)
	}

	// A failing fetch PROPAGATES (unlike Download's best-effort side fetch).
	status = http.StatusNotFound
	if err := svc.FetchTranscript(ctx, ep); err == nil {
		t.Fatal("404 fetch should propagate an error")
	}
}

func TestFetchTranscriptWithoutURLIsInvalid(t *testing.T) {
	svc, _, ep := transcriptFixture(t, "")
	if err := svc.FetchTranscript(context.Background(), ep); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("no-url fetch = %v, want CodeInvalid", err)
	}
}

func TestTranscriptReadAbsences(t *testing.T) {
	svc, st, ep := transcriptFixture(t, "")
	ctx := context.Background()
	// No transcript stored yet: CodeNotFound, distinct from an unknown episode.
	if _, err := svc.Transcript(ctx, ep); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("absent transcript = %v, want CodeNotFound", err)
	}
	if _, err := st.TranscriptByEpisode(ctx, "missing-pid"); !waxerr.Is(err, waxerr.CodeNotFound) {
		t.Fatalf("unknown episode = %v, want CodeNotFound", err)
	}
}
