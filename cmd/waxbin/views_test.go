package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestPlayStateViewJSON pins the `state --json` payload shape, in particular
// that the unix-ns change stamps encode as decimal STRINGS: the values exceed
// IEEE-754 double precision, so a bare number would be silently corrupted by
// any consumer that parses JSON numbers into doubles (JS, jq 1.6, loose Go
// decoding). Zero stamps (never changed) are omitted.
func TestPlayStateViewJSON(t *testing.T) {
	r := 80
	full := &model.PlayState{
		ItemPID: "i1", PositionMS: 42000, Played: true, PlayCount: 3,
		Rating: r, HasRating: true, Starred: true,
		RatingChangedAt: 1784777333683766021, StarredChangedAt: 1784777321347098926,
	}
	b, err := json.Marshal(toPlayStateView(full))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"itemPid":"i1","positionMs":42000,"played":true,"finished":false,` +
		`"playCount":3,"rating":80,"starred":true,` +
		`"ratingChangedAt":"1784777333683766021","starredChangedAt":"1784777321347098926"}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	zero := &model.PlayState{ItemPID: "i1"}
	b, err = json.Marshal(toPlayStateView(zero))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"itemPid":"i1","positionMs":0,"played":false,"finished":false,"playCount":0,"starred":false}`
	if string(b) != want {
		t.Errorf("zero json = %s\nwant %s (never-changed stamps omitted)", b, want)
	}
}
