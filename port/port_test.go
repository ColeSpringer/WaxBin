package port_test

import (
	"bytes"
	"context"
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
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent, Title: "Song", Artist: "A"}}
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
	if got.PlayState[0].Rating == nil || *got.PlayState[0].Rating != 80 {
		t.Fatalf("rating round-trip wrong: %+v", got.PlayState[0])
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
