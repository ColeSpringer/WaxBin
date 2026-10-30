package podcast_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/source"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	svc, _, ep := transcriptFixture(t, "")
	if err := svc.FetchTranscript(context.Background(), ep); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("no-url fetch = %v, want CodeInvalid", err)
	}
}

func TestTranscriptReadAbsences(t *testing.T) {
	t.Parallel()
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

// privateShow serves a show that answers 401 to anything asked without the login
// alice:pw: its feed, enclosure, transcript, chapters and both images. Its second
// episode's transcript lives on another host, which counts the logins it is sent.
type privateShow struct {
	base       string
	elsewhere  *atomic.Int64
	transcript *atomic.Int64 // the status the first episode's transcript answers with
	chapters   *atomic.Value // the first episode's chapters document, or nil for a 404
}

func newPrivateShow(t *testing.T) *privateShow {
	t.Helper()
	ps := &privateShow{elsewhere: new(atomic.Int64), transcript: new(atomic.Int64), chapters: new(atomic.Value)}
	ps.transcript.Store(http.StatusOK)
	ps.chapters.Store(`{"version":"1.2.0","chapters":[{"startTime":0,"title":"Intro"},{"startTime":60,"title":"Main"}]}`)
	png := tinyPNG(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			ps.elsewhere.Add(1)
		}
		w.Header().Set("Content-Type", "application/srt")
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nhosted elsewhere\n")
	}))
	t.Cleanup(other.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = fmt.Fprintf(w, `<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Private</title><itunes:image href="%[1]s/art.png"/>
<item><title>One</title><guid>one</guid><enclosure url="%[1]s/1.mp3" type="audio/mpeg"/>
<itunes:image href="%[1]s/ep.png"/>
<podcast:transcript url="%[1]s/1.srt" type="application/srt"/>
<podcast:chapters url="%[1]s/1.json" type="application/json+chapters"/></item>
<item><title>Two</title><guid>two</guid><podcast:transcript url="%[2]s/2.srt" type="application/srt"/></item>
</channel></rss>`, ps.base, other.URL)
		case "/1.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
		case "/1.srt":
			if status := int(ps.transcript.Load()); status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/srt")
			_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:04,000\nbehind the login\n")
		case "/1.json":
			doc, _ := ps.chapters.Load().(string)
			if doc == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json+chapters")
			_, _ = io.WriteString(w, doc)
		case "/art.png", "/ep.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	ps.base = srv.URL
	return ps
}

// add subscribes svc to the show with its login and returns the show and its episodes,
// One first.
func (ps *privateShow) add(t *testing.T, svc *podcast.Service) (*model.Podcast, []*model.Episode) {
	t.Helper()
	ctx := context.Background()
	pod, err := svc.Add(ctx, ps.base+"/feed.xml", podcast.AddOptions{User: "alice", Pass: "pw"})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	if eps[0].Title != "One" {
		eps[0], eps[1] = eps[1], eps[0]
	}
	return pod.Podcast, eps
}

// TestTranscriptFetchCarriesTheShowsLoginAtItsOrigin: a private show's transcript is
// fetched with the show's login at the feed's origin, and without it anywhere else.
func TestTranscriptFetchCarriesTheShowsLoginAtItsOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ps := newPrivateShow(t)
	svc, _, _ := newTestService(t)
	_, eps := ps.add(t, svc)
	if err := svc.FetchTranscript(ctx, eps[0].PID); err != nil {
		t.Fatalf("fetch transcript: %v", err)
	}
	tr, err := svc.Transcript(ctx, eps[0].PID)
	if err != nil || !strings.Contains(tr.Body, "behind the login") {
		t.Fatalf("transcript = %+v (err %v), want the private body", tr, err)
	}
	if err := svc.FetchTranscript(ctx, eps[1].PID); err != nil {
		t.Fatalf("fetch the transcript hosted elsewhere: %v", err)
	}
	if n := ps.elsewhere.Load(); n != 0 {
		t.Fatalf("the show's login went to another host %d times", n)
	}
}

// TestChaptersFetchCarriesTheShowsLoginAtItsOrigin: a download's chapters document and
// transcript come from the private host with the show's login.
func TestChaptersFetchCarriesTheShowsLoginAtItsOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ps := newPrivateShow(t)
	svc, _, _ := newTestService(t)
	_, eps := ps.add(t, svc)
	res, err := svc.Download(ctx, eps[0].PID)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !res.Chapters || !res.Transcript {
		t.Fatalf("download stored chapters %v, transcript %v, want both", res.Chapters, res.Transcript)
	}
	d, err := svc.Episode(ctx, eps[0].PID)
	if err != nil || len(d.Chapters) != 2 || d.Chapters[1].Title != "Main" {
		t.Fatalf("chapters = %+v (err %v), want Intro and Main", d, err)
	}
}

// TestShowImageCarriesTheShowsLoginAtItsOrigin: the show's cover, a downloaded
// episode's image and an imported episode's image are fetched with the show's login.
func TestShowImageCarriesTheShowsLoginAtItsOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ps := newPrivateShow(t)
	svc, st, _ := newTestService(t)
	pod, eps := ps.add(t, svc)
	if got, err := st.PodcastByPID(ctx, pod.PID); err != nil || got.CoverSourceURL != ps.base+"/art.png" {
		t.Fatalf("show cover from %q (err %v), want the private art.png", got.CoverSourceURL, err)
	}
	episodeImage := func(step string, st *sqlite.Store, pid model.PID) {
		t.Helper()
		prov, err := st.ArtProvenance(ctx, model.EntityRef{Type: model.ArtEpisode, PID: pid}, "")
		if err != nil || prov.Level != model.ArtEpisode || prov.SourceURL != ps.base+"/ep.png" {
			t.Fatalf("%s: episode art %+v (err %v), want its own private ep.png", step, prov, err)
		}
	}
	if _, err := svc.Download(ctx, eps[0].PID); err != nil {
		t.Fatalf("download: %v", err)
	}
	episodeImage("download", st, eps[0].PID)

	other, ost, _ := newTestService(t)
	_, oeps := ps.add(t, other)
	src := filepath.Join(t.TempDir(), "one.mp3")
	if err := os.WriteFile(src, fakeAudio, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ImportEpisodeFile(ctx, oeps[0].PID, src, true); err != nil {
		t.Fatalf("import: %v", err)
	}
	episodeImage("import", ost, oeps[0].PID)
}

// TestTranscriptFailureIsTheProviders: a transcript host's refusal comes back as the
// provider's failure with its status, so a caller tells it from the catalog's.
func TestTranscriptFailureIsTheProviders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ps := newPrivateShow(t)
	svc, _, _ := newTestService(t)
	_, eps := ps.add(t, svc)
	ps.transcript.Store(http.StatusGone)
	err := svc.FetchTranscript(ctx, eps[0].PID)
	var pe *source.ProviderError
	if !errors.As(err, &pe) || pe.Reason != source.ReasonHTTPStatus || pe.Status != http.StatusGone {
		t.Fatalf("fetch transcript = %v (%+v), want the provider's http_status 410", err, pe)
	}
}

// TestFetchChaptersStoresWithoutADownload: an episode's declared chapters are fetched on
// demand, with the show's login, and stored with no download; a refusal or a document
// with nothing in it is the provider's failure, and the chapters stored stay.
func TestFetchChaptersStoresWithoutADownload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ps := newPrivateShow(t)
	svc, _, _ := newTestService(t)
	_, eps := ps.add(t, svc)
	if err := svc.FetchChapters(ctx, eps[0].PID); err != nil {
		t.Fatalf("fetch chapters: %v", err)
	}
	chapters := func() []model.Chapter {
		t.Helper()
		d, err := svc.Episode(ctx, eps[0].PID)
		if err != nil {
			t.Fatalf("episode: %v", err)
		}
		if d.Episode.Downloaded {
			t.Fatal("fetching chapters downloaded the episode")
		}
		return d.Chapters
	}
	if got := chapters(); len(got) != 2 || got[1].Title != "Main" || got[1].StartMS != 60000 || got[0].FilePID != "" {
		t.Fatalf("chapters = %+v, want Intro and Main at 60 s with no file", got)
	}

	if err := svc.FetchChapters(ctx, eps[1].PID); !waxerr.Is(err, waxerr.CodeInvalid) || source.IsProviderError(err) {
		t.Fatalf("fetch for an episode declaring none = %v, want the catalog's CodeInvalid", err)
	}
	for doc, want := range map[string]waxerr.Code{"": waxerr.CodeNotFound, `{"chapters":[]}`: waxerr.CodeInvalid, "{not json": waxerr.CodeInvalid} {
		ps.chapters.Store(doc)
		err := svc.FetchChapters(ctx, eps[0].PID)
		if !source.IsProviderError(err) || !waxerr.Is(err, want) {
			t.Errorf("fetch of %q = %v (class %s), want the provider's %s", doc, err, waxerr.CodeOf(err), want)
		}
	}
	if got := chapters(); len(got) != 2 {
		t.Fatalf("chapters after failed fetches = %+v, want the two stored before", got)
	}
}
