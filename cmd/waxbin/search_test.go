package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
)

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
// and leaves the rest out.
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
	var env struct {
		Data struct {
			Episodes []struct {
				Title string `json:"title"`
			} `json:"episodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("search printed %q: %v", out, err)
	}
	if e := env.Data.Episodes; len(e) != 1 || e[0].Title != "Harbor Followed" {
		t.Errorf("episodes = %+v, want only the followed show's", e)
	}
}
