package main

import (
	"encoding/json"
	"testing"
)

// TestSearchJSONMarksExactNames: the JSON view carries a hit's exact mark, so a client
// can tell the artist the query names from the ones its items brought in.
func TestSearchJSONMarksExactNames(t *testing.T) {
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
