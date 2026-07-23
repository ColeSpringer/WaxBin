package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
)

// TestEntityInfoViewJSON pins the `entity info --json` payload shape: field
// names are part of the CLI contract, and the omitempty set keeps kind-foreign
// fields (a genre's mbid, a track entity's year) out of the document.
func TestEntityInfoViewJSON(t *testing.T) {
	full := &read.EntityInfo{
		Kind: read.EntityReleaseGroup, PID: "rg1", Name: "OK Computer", SortKey: "ok computer",
		MBID: "11111111-2222-3333-4444-555555555555", Type: "album",
		ArtistPID: "a1", ItemCount: 12, TotalDurationMS: 3600000,
		LibraryPIDs: []model.PID{"l1", "l2"},
	}
	b, err := json.Marshal(toEntityInfoView(full))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"release_group","pid":"rg1","name":"OK Computer","sortKey":"ok computer",` +
		`"mbid":"11111111-2222-3333-4444-555555555555","type":"album","artistPid":"a1",` +
		`"itemCount":12,"totalDurationMs":3600000,"libraryPids":["l1","l2"]}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	minimal := &read.EntityInfo{Kind: read.EntityGenre, PID: "g1", Name: "Rock", SortKey: "rock", ItemCount: 3}
	b, err = json.Marshal(toEntityInfoView(minimal))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"kind":"genre","pid":"g1","name":"Rock","sortKey":"rock","itemCount":3,` +
		`"totalDurationMs":0,"libraryPids":[]}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}
