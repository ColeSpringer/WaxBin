package podcast_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/podcast"
)

// syncBuffer is a log destination the server's goroutines and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestPodcastLogsCarryNoURL: the service's log lines about a failed image, chapters,
// transcript or OPML feed name the host they asked, never the path or query, which
// carry a private feed's token.
func TestPodcastLogsCarryNoURL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const token = "token=hush-hush"
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private/feed.xml":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = io.WriteString(w, strings.NewReplacer("BASE", base, "TOKEN", token).Replace(`<?xml version="1.0"?>
<rss version="2.0" xmlns:itunes="http://www.itunes.com/dtds/podcast-1.0.dtd" xmlns:podcast="https://podcastindex.org/namespace/1.0">
<channel><title>Logged</title><itunes:image href="BASE/private/art.png?TOKEN"/>
<item><title>One</title><guid>one</guid><enclosure url="BASE/private/1.mp3?TOKEN" type="audio/mpeg"/>
<itunes:image href="BASE/private/garbled.png?TOKEN"/>
<podcast:transcript url="BASE/private/1.srt?TOKEN" type="application/srt"/>
<podcast:chapters url="BASE/private/garbled.json?TOKEN" type="application/json+chapters"/></item>
<item><title>Two</title><guid>two</guid><enclosure url="BASE/private/2.mp3?TOKEN" type="audio/mpeg"/>
<podcast:chapters url="BASE/private/2.json?TOKEN" type="application/json+chapters"/></item>
</channel></rss>`))
		case "/private/1.mp3", "/private/2.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(fakeAudio)
		case "/private/garbled.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "not a picture")
		case "/private/garbled.json":
			w.Header().Set("Content-Type", "application/json+chapters")
			_, _ = io.WriteString(w, "{not json")
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL

	logs := &syncBuffer{}
	st := newTestStore(t)
	svc := podcast.New(st, meta.NewReader(), podcast.Config{Dir: t.TempDir()},
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	pod, err := svc.Add(ctx, base+"/private/feed.xml?"+token, podcast.AddOptions{})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	eps, err := svc.Episodes(ctx, pod.PID, 0)
	if err != nil || len(eps) != 2 {
		t.Fatalf("episodes = %d (err %v), want 2", len(eps), err)
	}
	for _, ep := range eps {
		if _, err := svc.Download(ctx, ep.PID); err != nil {
			t.Fatalf("download %s: %v", ep.Title, err)
		}
	}
	opml := `<opml version="2.0"><body><outline type="rss" xmlUrl="` + base + `/private/gone.xml?` + token + `"/></body></opml>`
	if _, err := svc.ImportOPML(ctx, []byte(opml)); err != nil {
		t.Fatalf("import opml: %v", err)
	}

	out := logs.String()
	for msg, n := range map[string]int{"podcast image fetch failed": 1, "podcast image undecodable": 1,
		"transcript fetch failed": 1, "chapters fetch failed": 2, "OPML import: feed failed": 1} {
		if got := strings.Count(out, msg); got != n {
			t.Errorf("%d %q lines were logged, want %d", got, msg, n)
		}
	}
	for _, leak := range []string{"hush-hush", "/private"} {
		if strings.Contains(out, leak) {
			t.Errorf("the logs carry %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, base) {
		t.Errorf("the logs never name the host %s:\n%s", base, out)
	}
}
