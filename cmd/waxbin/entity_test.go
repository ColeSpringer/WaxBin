package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
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

// TestEntityStarRateArgValidation checks the star/rate argument errors fire in the
// command itself, before the catalog is opened and its write lock taken. Reaching the
// errors with an empty globals (no database configured) proves they run on the early
// path, ahead of openMutator.
func TestEntityStarRateArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		// A non-star-able type is rejected before anything opens. An unknown type and a
		// known one that carries no per-user state (series merges, but nothing stars it)
		// refuse on their own grounds.
		{"bad star type", []string{"star", "bogus", "01J0X"}, "unknown entity type"},
		{"bad rate type", []string{"rate", "series", "01J0X", "50"}, "carries no per-user state"},
		// A rating outside 0-100 (or a non-integer, non-"clear") is rejected.
		{"rating over 100", []string{"rate", "album", "01J0X", "150"}, "0-100"},
		{"rating not a number", []string{"rate", "album", "01J0X", "high"}, "0-100"},
		// A malformed --as-of is caught by the shared parser before opening.
		{"bad as-of", []string{"star", "album", "01J0X", "--as-of", "notatime"}, "invalid --as-of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newEntityCmd(&globals{})
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("args %v: expected a validation error, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("args %v: error = %q, want it to mention %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestEntityClearDurabilityWarning: the caveat rides on an album or release-group mbid
// clear alone, and tracks whether the id actually came off the member files rather than
// whether --write-back was asked for. A strip that reported failures is surfaced as a
// warning and returns a nil error, which is the case where the identity forks back.
func TestEntityClearDurabilityWarning(t *testing.T) {
	stripFailed := &waxbin.WriteBackError{
		ItemPID: "al1",
		Failures: []waxbin.WriteBackFailure{{
			Path:   "/lib/one/rip.flac",
			Reason: "on-disk tag write-back is unavailable for a file shared by multiple items",
		}},
	}
	clear := map[string]string{"mbid": ""}
	for _, tc := range []struct {
		name      string
		et        model.MergeEntity
		edits     map[string]string
		writeBack bool
		err       error
		want      string
	}{
		{"no write-back", model.MergeAlbum, clear, false, nil, "pass --write-back"},
		{"clean strip", model.MergeAlbum, clear, true, nil, ""},
		{"failed strip", model.MergeAlbum, clear, true, error(stripFailed), "shared by several items"},
		{"failed strip wrapped", model.MergeReleaseGroup, clear, true,
			errors.Join(errors.New("ctx"), stripFailed), "shared by several items"},
		{"release group without write-back", model.MergeReleaseGroup, clear, false, nil, "pass --write-back"},
		// An artist mbid clear re-keys nothing, and a set is not a clear, so neither is
		// the gesture the caveat is about.
		{"artist clear", model.MergeArtist, clear, false, nil, ""},
		{"mbid set", model.MergeAlbum, map[string]string{"mbid": "  " + strings.Repeat("a", 8) + "  "}, false, nil, ""},
		{"other field", model.MergeAlbum, map[string]string{"label": ""}, false, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := entityClearDurabilityWarning(tc.et, tc.edits, tc.writeBack, tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("warning = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestParseEntityRating covers the `entity rate` rating argument: a 0-100 integer, a
// case-insensitive "clear" keyword to a nil pointer (the clear), and out-of-range or
// non-numeric input rejected.
func TestParseEntityRating(t *testing.T) {
	for _, clear := range []string{"clear", "CLEAR", "Clear", "cLeAr"} {
		got, err := parseEntityRating(clear)
		if err != nil || got != nil {
			t.Errorf("parseEntityRating(%q) = (%v, %v), want (nil, nil)", clear, got, err)
		}
	}
	if got, err := parseEntityRating("80"); err != nil || got == nil || *got != 80 {
		t.Errorf("parseEntityRating(\"80\") = (%v, %v), want (80, nil)", got, err)
	}
	if got, err := parseEntityRating("0"); err != nil || got == nil || *got != 0 {
		t.Errorf("parseEntityRating(\"0\") = (%v, %v), want (0, nil)", got, err)
	}
	for _, bad := range []string{"150", "-1", "high", "", "8.5"} {
		if _, err := parseEntityRating(bad); err == nil {
			t.Errorf("parseEntityRating(%q) = nil error, want a validation error", bad)
		}
	}
}

// TestEntityPlayStateViewJSON pins the `entity state --json` payload shape: the change
// stamps are decimal strings (the ns-precision contract shared with playStateView), a
// zero stamp and an unset rating are omitted, and starred is always present.
func TestEntityPlayStateViewJSON(t *testing.T) {
	full := &model.EntityPlayState{
		Kind: model.MergeAlbum, EntityPID: "al1", Rating: 80, HasRating: true,
		Starred: true, StarredAt: 100, RatingChangedAt: 200, StarredChangedAt: 100, UpdatedAt: 300,
	}
	b, err := json.Marshal(toEntityPlayStateView(full))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"album","entityPid":"al1","rating":80,"starred":true,` +
		`"starredAt":"100","ratingChangedAt":"200","starredChangedAt":"100"}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	zero := &model.EntityPlayState{Kind: model.MergeArtist, EntityPID: "ar1"}
	b, err = json.Marshal(toEntityPlayStateView(zero))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"kind":"artist","entityPid":"ar1","starred":false}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}
