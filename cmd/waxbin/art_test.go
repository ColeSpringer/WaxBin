package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin/model"
)

// TestArtRoleViewsJSON pins the `art roles --json` payload shape. The field names are
// part of the CLI contract, and the omitempty set is what makes a lock with no
// artifact readable: it arrives as a locked role with no image rather than one
// wearing a zero size and an empty format, which is the state the command exists to
// report and the only one no other JSON read can reach for a playlist or a podcast.
func TestArtRoleViewsJSON(t *testing.T) {
	b, err := json.Marshal(artRoleViews([]model.ArtRoleInfo{
		{
			Role: model.ArtRoleBack, Format: "png", Width: 500, Height: 500,
			SourceHash: "h1", Attribution: model.Attribution{Source: model.SourceUser}, UpdatedAt: 7,
		},
		{Role: model.ArtRoleFront, Attribution: model.Attribution{Source: model.SourceUser},
			UpdatedAt: 9, Locked: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"back","format":"png","width":500,"height":500,"sourceHash":"h1",` +
		`"source":"user","updatedAt":"7","locked":false},` +
		`{"role":"front","source":"user","updatedAt":"9","locked":true}]`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}
