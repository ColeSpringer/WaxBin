package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/read"
)

// TestSearchViewCarriesEveryGroup: the JSON view has a key for each group of the result,
// each holding that group's hits.
func TestSearchViewCarriesEveryGroup(t *testing.T) {
	t.Parallel()
	res := &read.SearchResult{
		Artists: []read.SearchHit{{Kind: "artist"}}, Albums: []read.SearchHit{{Kind: "album"}},
		Tracks: []read.SearchHit{{Kind: "track"}}, Authors: []read.SearchHit{{Kind: "author"}},
		Narrators: []read.SearchHit{{Kind: "narrator"}}, Series: []read.SearchHit{{Kind: "series"}},
		Books: []read.SearchHit{{Kind: "book"}}, Podcasts: []read.SearchHit{{Kind: "podcast"}},
		Episodes: []read.SearchHit{{Kind: "episode"}},
	}
	raw, err := json.Marshal(toSearchView(res))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"artists": "artist", "albums": "album", "tracks": "track", "authors": "author",
		"narrators": "narrator", "series": "series", "books": "book", "podcasts": "podcast", "episodes": "episode"}
	if len(keys) != len(res.Groups()) {
		t.Fatalf("the test names %d groups, the result has %d", len(keys), len(res.Groups()))
	}
	for key, kind := range keys {
		var hits []struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(got[key], &hits); err != nil || len(hits) != 1 || hits[0].Kind != kind {
			t.Errorf("%s = %s (err %v), want its one %s hit", key, got[key], err, kind)
		}
	}
}

// TestSearchJSONMarksExactNames: the JSON view carries a hit's exact mark, so a client
// can tell the artist the query names from the ones its items brought in.
func TestSearchJSONMarksExactNames(t *testing.T) {
	t.Parallel()
	db, root, _ := creditCLIFixture(t)
	out, err := runCLIJSON(t, db, root, "search", "artist")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var env struct {
		Data struct {
			Artists []struct {
				Title string `json:"title"`
				Exact bool   `json:"exact"`
			} `json:"artists"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("search printed %q: %v", out, err)
	}
	if a := env.Data.Artists; len(a) != 1 || a[0].Title != "Artist" || !a[0].Exact {
		t.Errorf("artists = %+v, want Artist marked exact", a)
	}
}

// TestSearchPodcastFlagScopesEpisodes: --podcast keeps the episodes of the named shows
// and leaves the rest out, their shows with them.
func TestSearchPodcastFlagScopesEpisodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := openCLILib(t, ctx, db, root, false)
	shows := map[string]model.PID{}
	for _, name := range []string{"Followed", "Other"} {
		show, err := lib.Podcasts().AddManual(ctx, name, podcast.ManualOptions{})
		if err != nil {
			t.Fatalf("manual show: %v", err)
		}
		shows[name] = show.PID
		if _, err := lib.Podcasts().AddEpisode(ctx, show.PID, model.FeedEpisode{Title: "Harbor " + name,
			EnclosureURL: "https://example.invalid/" + name + ".mp3"}, true); err != nil {
			t.Fatalf("episode: %v", err)
		}
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := runCLIJSON(t, db, root, "search", "harbor", "--podcast", string(shows["Followed"]))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	type hit struct {
		PID   string `json:"pid"`
		Kind  string `json:"kind"`
		Title string `json:"title"`
	}
	var env struct {
		Data struct {
			Podcasts []hit `json:"podcasts"`
			Episodes []hit `json:"episodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("search printed %q: %v", out, err)
	}
	if e := env.Data.Episodes; len(e) != 1 || e[0].Title != "Harbor Followed" {
		t.Errorf("episodes = %+v, want only the followed show's", e)
	}
	want := hit{PID: string(shows["Followed"]), Kind: "podcast", Title: "Followed"}
	if p := env.Data.Podcasts; len(p) != 1 || p[0] != want {
		t.Errorf("podcasts = %+v, want only the followed show", p)
	}
	text, err := runCLIText(t, db, root, "search", "harbor")
	if err != nil || !strings.Contains(text, "PODCASTS\n") || !strings.Contains(text, "EPISODES\n") {
		t.Errorf("search printed %q (err %v), want its podcasts and episodes groups", text, err)
	}
}
