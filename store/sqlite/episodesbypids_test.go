package sqlite

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// episodePIDsByTitle reads the pids of a feed's episodes keyed by title.
func episodePIDsByTitle(t *testing.T, st *Store) map[string]model.PID {
	t.Helper()
	rows, err := st.read.QueryContext(context.Background(),
		"SELECT pi.title, pi.pid FROM playable_item pi WHERE pi.kind = 'episode'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]model.PID{}
	for rows.Next() {
		var title, pid string
		if err := rows.Scan(&title, &pid); err != nil {
			t.Fatal(err)
		}
		out[title] = model.PID(pid)
	}
	return out
}

func episodeTitles(eps []*model.Episode) []string {
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.Title
	}
	return out
}

// TestEpisodesByPIDsKeepsTheRequestOrder: the batch read answers in request order, each
// episode once, leaving out a pid that is no episode (a track, an unknown one), and
// says which episodes hold a transcript.
func TestEpisodesByPIDsKeepsTheRequestOrder(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/t.flac", essence: "et", content: "ct",
		title: "A Track", artist: "X", album: "Al"}).ItemPID
	putFeed(t, st, "http://cast.example/f", "One", "Two", "Three")
	eps := episodePIDsByTitle(t, st)
	if err := st.PutTranscript(ctx, model.PutTranscriptInput{EpisodePID: eps["Two"], Format: "text", Body: "words"}); err != nil {
		t.Fatalf("transcript: %v", err)
	}

	got, err := st.EpisodesByPIDs(ctx, []model.PID{eps["Three"], track, model.NewPID(), eps["One"], eps["Three"], eps["Two"]})
	if err != nil {
		t.Fatalf("EpisodesByPIDs: %v", err)
	}
	if titles := episodeTitles(got); !equalStrings(titles, []string{"Three", "One", "Two"}) {
		t.Fatalf("episodes = %v, want [Three One Two]", titles)
	}
	for _, e := range got {
		if want := e.Title == "Two"; e.HasTranscript != want {
			t.Errorf("%s HasTranscript = %t, want %t", e.Title, e.HasTranscript, want)
		}
		if e.PodcastTitle != "Cast" || e.EnclosureURL == "" {
			t.Errorf("%s = %+v, want the whole episode row", e.Title, e)
		}
	}
	if empty, err := st.EpisodesByPIDs(ctx, nil); err != nil || empty != nil {
		t.Fatalf("empty input = (%v, %v), want (nil, nil)", empty, err)
	}
}

// TestEpisodesByPIDsCrossesTheChunkBoundary: a request longer than one IN batch (500)
// spans several statements and still answers each episode once, in request order.
func TestEpisodesByPIDsCrossesTheChunkBoundary(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	ctx := context.Background()
	putFeed(t, st, "http://cast.example/f", "E0", "E1", "E2", "E3")
	eps := episodePIDsByTitle(t, st)
	realAt := map[int]string{3: "E0", 499: "E1", 500: "E2", 1250: "E3"}
	req := make([]model.PID, 0, 1300)
	for i := 0; i < 1300; i++ {
		if title, ok := realAt[i]; ok {
			req = append(req, eps[title])
			continue
		}
		req = append(req, model.NewPID())
	}
	got, err := st.EpisodesByPIDs(ctx, req)
	if err != nil {
		t.Fatalf("EpisodesByPIDs: %v", err)
	}
	if titles := episodeTitles(got); !equalStrings(titles, []string{"E0", "E1", "E2", "E3"}) {
		t.Fatalf("episodes = %v, want [E0 E1 E2 E3]", titles)
	}
}

// TestEpisodeMetaIsTheDetailsEpisode: the light single read answers the same episode row
// the detail read carries, transcript flag included, and refuses a pid that is no
// episode as the detail read does.
func TestEpisodeMetaIsTheDetailsEpisode(t *testing.T) {
	t.Parallel()
	st, lib := entityFixture(t)
	ctx := context.Background()
	track := putTrack(t, st, lib.ID, trackSpec{path: "/lib/t.flac", essence: "et", content: "ct",
		title: "A Track", artist: "X", album: "Al"}).ItemPID
	putFeed(t, st, "http://cast.example/f", "One", "Two")
	eps := episodePIDsByTitle(t, st)
	if err := st.PutTranscript(ctx, model.PutTranscriptInput{EpisodePID: eps["Two"], Format: "text", Body: "words"}); err != nil {
		t.Fatalf("transcript: %v", err)
	}
	for _, title := range []string{"One", "Two"} {
		meta, err := st.EpisodeMeta(ctx, eps[title])
		if err != nil {
			t.Fatalf("EpisodeMeta %s: %v", title, err)
		}
		d, err := st.EpisodeByPID(ctx, eps[title])
		if err != nil {
			t.Fatalf("EpisodeByPID %s: %v", title, err)
		}
		if !reflect.DeepEqual(meta, d.Episode) {
			t.Errorf("EpisodeMeta %s = %+v, want the detail's %+v", title, meta, d.Episode)
		}
		if meta.HasTranscript != (title == "Two") {
			t.Errorf("%s HasTranscript = %t", title, meta.HasTranscript)
		}
	}
	for _, pid := range []model.PID{track, model.NewPID()} {
		if _, err := st.EpisodeMeta(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) {
			t.Errorf("EpisodeMeta(%s) = %v, want CodeNotFound", pid, err)
		}
		// Each read reports its own refusal, since that is the name a caller sees.
		if _, err := st.EpisodeByPID(ctx, pid); !waxerr.Is(err, waxerr.CodeNotFound) ||
			!strings.Contains(err.Error(), "store.EpisodeByPID") {
			t.Errorf("EpisodeByPID(%s) = %v, want its own CodeNotFound", pid, err)
		}
	}
}
