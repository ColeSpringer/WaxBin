package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
)

// TestDiagnosticsListNamesTheSetter: `diagnostics list` filters owed rows by who set the
// value, prints the setter and the item in JSON and the setter in text, and refuses a
// source outside the vocabulary.
func TestDiagnosticsListNamesTheSetter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, root, pid := creditCLIFixture(t)
	lib := openCLILib(t, ctx, db, root, false)
	if err := lib.EditField(ctx, pid, "genre", "Jazz", waxbin.EditOptions{}); err != nil {
		t.Fatalf("user edit: %v", err)
	}
	if err := lib.EditField(ctx, pid, "year", "1999",
		waxbin.EditOptions{Source: model.SourceEnrichment, Provider: "musicbrainz"}); err != nil {
		t.Fatalf("enrichment edit: %v", err)
	}
	if err := lib.EditField(ctx, pid, "title", "Song (Live)", waxbin.EditOptions{Source: model.SourceNormalize}); err != nil {
		t.Fatalf("normalize edit: %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out, err := runCLIJSON(t, db, root, "diagnostics", "list", "--code", "tag_write_owed", "--exclude-source", "normalize")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var env struct {
		Data []struct {
			ItemPID  string `json:"itemPid"`
			TagKey   string `json:"tagKey"`
			Source   string `json:"source"`
			Provider string `json:"provider"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	got := map[string]string{}
	for _, r := range env.Data {
		if r.ItemPID != string(pid) {
			t.Errorf("%s item = %q, want %s", r.TagKey, r.ItemPID, pid)
		}
		got[r.TagKey] = r.Source + "/" + r.Provider
	}
	if len(got) != 2 || got["genre"] != "user/" || got["year"] != "enrichment/musicbrainz" {
		t.Errorf("rows excluding normalize = %v, want genre by the user and year by musicbrainz", got)
	}

	text, err := runCLIText(t, db, root, "diagnostics", "list", "--source", "normalize")
	if err != nil {
		t.Fatalf("list text: %v", err)
	}
	if !strings.Contains(text, "[title]") || !strings.Contains(text, "set by normalize") || strings.Contains(text, "[genre]") {
		t.Errorf("text for normalize = %q, want the title row naming its setter alone", text)
	}

	if _, err := runCLIText(t, db, root, "diagnostics", "summary", "--source", "robot"); err == nil ||
		!strings.Contains(err.Error(), "unknown provenance source") {
		t.Errorf("summary --source robot = %v, want the vocabulary refusal", err)
	}
}
