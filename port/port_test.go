package port_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/waxerr"
)

func TestSnapshotRoundTrip(t *testing.T) {
	libs := []*model.Library{{PID: "L1", DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native"}}
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent, Title: "Song", Artist: "A",
		MBID: "rec-1", ISRC: "USRC17607839"}}
	rating := 80
	plays := []model.PlayState{{UserPID: "U1", ItemPID: "I1", PlayCount: 2, Starred: true, HasRating: true, Rating: rating}}

	snap := port.BuildSnapshot(12, 1700000000, libs, items, plays, nil)
	if snap.Manifest.Format != port.ExportFormat || snap.Manifest.Items != 1 || snap.Manifest.PlayStates != 1 {
		t.Fatalf("manifest wrong: %+v", snap.Manifest)
	}

	var buf bytes.Buffer
	if err := port.WriteSnapshot(&buf, snap); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := port.ReadSnapshot(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].Title != "Song" {
		t.Fatalf("items round-trip wrong: %+v", got.Items)
	}
	if got.Items[0].MBID != "rec-1" || got.Items[0].ISRC != "USRC17607839" {
		t.Errorf("item ids round-trip wrong: %+v", got.Items[0])
	}
	if got.Manifest.Version != port.ExportVersion {
		t.Errorf("manifest version = %d, want %d", got.Manifest.Version, port.ExportVersion)
	}
	if got.PlayState[0].Rating == nil || *got.PlayState[0].Rating != 80 {
		t.Fatalf("rating round-trip wrong: %+v", got.PlayState[0])
	}
}

// TestItemExportCarriesNoLibraryHandle pins the intent in ItemExport's doc comment.
// ItemView now projects a library pid, so copying it across is a change someone could
// reasonably make; this is what says it would be wrong.
func TestItemExportCarriesNoLibraryHandle(t *testing.T) {
	libs := []*model.Library{{PID: "L1", DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native"}}
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent,
		Title: "Song", LibraryPID: "L1"}}

	snap := port.BuildSnapshot(12, 1700000000, libs, items, nil, nil)
	// Marshal the items alone, so the library pid in the top-level Libraries array
	// cannot mask a leak here.
	encoded, err := json.Marshal(snap.Items)
	if err != nil {
		t.Fatalf("marshal items: %v", err)
	}
	if strings.Contains(string(encoded), "L1") {
		t.Errorf("marshalled items carry the library pid: %s", encoded)
	}
	if len(snap.Libraries) != 1 || snap.Libraries[0].PID != "L1" {
		t.Errorf("libraries = %+v, want the one root as a top-level entry", snap.Libraries)
	}
}

// TestItemExportCarriesNoSplitArtistCredit is the sibling of the library-handle test
// above, for the same reason: a track's credit now fans out to one artist row each,
// and copying that list into the flat per-item record is a change someone could
// reasonably make. Only the combined display crosses.
func TestItemExportCarriesNoSplitArtistCredit(t *testing.T) {
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent,
		Title: "Empire State of Mind", Artist: "Jay-Z feat. Alicia Keys"}}

	snap := port.BuildSnapshot(12, 1700000000, nil, items, nil, nil)
	encoded, err := json.Marshal(snap.Items)
	if err != nil {
		t.Fatalf("marshal items: %v", err)
	}
	for _, key := range []string{"artists", "credits", "contributors"} {
		if strings.Contains(string(encoded), `"`+key+`"`) {
			t.Errorf("marshalled items carry a %q key: %s", key, encoded)
		}
	}
	if snap.Items[0].Artist != "Jay-Z feat. Alicia Keys" {
		t.Errorf("exported artist = %q, want the combined display", snap.Items[0].Artist)
	}
}

func TestReadSnapshotRejectsForeignJSON(t *testing.T) {
	_, err := port.ReadSnapshot(strings.NewReader(`{"manifest":{"format":"something-else"}}`))
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for a non-WaxBin export, got %v", err)
	}
}

func TestValidateBackupRejectsNonCatalog(t *testing.T) {
	f := filepath.Join(t.TempDir(), "notdb.txt")
	if err := os.WriteFile(f, []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := port.ValidateBackup(context.Background(), f); err == nil {
		t.Fatal("validating a non-catalog file should fail")
	}
}
