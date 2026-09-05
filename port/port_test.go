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
		MBID: "rec-1", ISRC: "USRC17607839", BPM: 128}}
	rating := 80
	plays := []model.PlayState{{UserPID: "U1", ItemPID: "I1", PlayCount: 2, Starred: true, HasRating: true, Rating: rating}}
	sessions := []model.PlaySession{
		{PID: "S1", UserPID: "U1", ItemPID: "I1", StartedAt: 1 << 60, EndedAt: 1<<60 + 5, MsPlayed: 240000, Client: "lastfm"},
		{PID: "S2", UserPID: "U1", ItemPID: "I1", StartedAt: 1<<60 + 9}, // still open
	}

	snap := port.BuildSnapshot(12, 1700000000, libs, items, plays, sessions, nil)
	if snap.Manifest.Format != port.ExportFormat || snap.Manifest.Items != 1 || snap.Manifest.PlayStates != 1 || snap.Manifest.PlaySessions != 2 {
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
	if got.Items[0].BPM != 128 {
		t.Errorf("bpm round-trip = %d, want 128", got.Items[0].BPM)
	}
	if got.Manifest.Version != port.ExportVersion {
		t.Errorf("manifest version = %d, want %d", got.Manifest.Version, port.ExportVersion)
	}
	if port.ExportVersion != 7 {
		t.Errorf("ExportVersion = %d, want 7 now that the listening log is carried", port.ExportVersion)
	}
	if got.PlayState[0].Rating == nil || *got.PlayState[0].Rating != 80 {
		t.Fatalf("rating round-trip wrong: %+v", got.PlayState[0])
	}
	if len(got.PlaySessions) != 2 {
		t.Fatalf("sessions round-trip wrong: %+v", got.PlaySessions)
	}
	if s := got.PlaySessions[0]; s.PID != "S1" || s.UserPID != "U1" || s.ItemPID != "I1" ||
		s.StartedAtNS != 1<<60 || s.EndedAtNS != 1<<60+5 || s.MsPlayed != 240000 || s.Client != "lastfm" {
		t.Errorf("session round-trip = %+v, want every recorded value back", s)
	}
	if s := got.PlaySessions[1]; s.EndedAtNS != 0 || s.MsPlayed != 0 {
		t.Errorf("open session round-trip = %+v, want no end and no play time", s)
	}
}

// TestSnapshotSessionTimesEncodeAsStrings pins the listening log's timestamps to the
// same quoted decimal encoding as every other unix-ns field, and keeps an open
// session's absent end out of the document rather than writing it as "0".
func TestSnapshotSessionTimesEncodeAsStrings(t *testing.T) {
	sessions := []model.PlaySession{
		{PID: "S1", UserPID: "U1", ItemPID: "I1", StartedAt: 1 << 60, EndedAt: 1<<60 + 5, MsPlayed: 10},
		{PID: "S2", UserPID: "U1", ItemPID: "I1", StartedAt: 1<<60 + 9},
	}
	snap := port.BuildSnapshot(12, 1700000000, nil, nil, nil, sessions, nil)
	var buf bytes.Buffer
	if err := port.WriteSnapshot(&buf, snap); err != nil {
		t.Fatalf("write: %v", err)
	}
	var raw struct {
		PlaySessions []map[string]any `json:"playSessions"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.PlaySessions[0]["startedAtNs"] != "1152921504606846976" || raw.PlaySessions[0]["endedAtNs"] != "1152921504606846981" {
		t.Errorf("session times encoded as %v, want quoted decimal strings", raw.PlaySessions[0])
	}
	if _, ok := raw.PlaySessions[1]["endedAtNs"]; ok {
		t.Errorf("an open session encoded an end: %v", raw.PlaySessions[1])
	}
}

// TestItemExportCarriesNoLibraryHandle pins the intent in ItemExport's doc comment.
// ItemView now projects a library pid, so copying it across is a change someone could
// reasonably make; this is what says it would be wrong.
func TestItemExportCarriesNoLibraryHandle(t *testing.T) {
	libs := []*model.Library{{PID: "L1", DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native"}}
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent,
		Title: "Song", LibraryPID: "L1"}}

	snap := port.BuildSnapshot(12, 1700000000, libs, items, nil, nil, nil)
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

	snap := port.BuildSnapshot(12, 1700000000, nil, items, nil, nil, nil)
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

// TestSnapshotCarriesAcquisitionSource pins both halves of the export's source field:
// an acquired item carries its type, and a locally scanned one omits the key entirely.
// The omission is the assertion that needs the explicit SourceLocal skip, since the read
// side is a COALESCE ending in 'local' and so never hands back an empty string for
// omitempty to drop.
func TestSnapshotCarriesAcquisitionSource(t *testing.T) {
	items := []*model.ItemView{
		{PID: "I1", Kind: model.KindTrack, State: model.StatePresent, Title: "Acquired", Source: model.SourceYouTube},
		{PID: "I2", Kind: model.KindTrack, State: model.StatePresent, Title: "Scanned", Source: model.SourceLocal},
	}
	snap := port.BuildSnapshot(12, 1700000000, nil, items, nil, nil, nil)
	if snap.Items[0].Source != string(model.SourceYouTube) {
		t.Errorf("acquired item source = %q, want youtube", snap.Items[0].Source)
	}
	if snap.Items[1].Source != "" {
		t.Errorf("scanned item source = %q, want the field left empty", snap.Items[1].Source)
	}

	var buf bytes.Buffer
	if err := port.WriteSnapshot(&buf, snap); err != nil {
		t.Fatalf("write: %v", err)
	}
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw.Items[0]["source"] != string(model.SourceYouTube) {
		t.Errorf("encoded acquired source = %v, want youtube", raw.Items[0]["source"])
	}
	if _, ok := raw.Items[1]["source"]; ok {
		t.Errorf("a locally scanned item encoded a source key: %v", raw.Items[1])
	}
}

// TestSnapshotWriterStreamsSessions pins the incremental writer: a document written
// header first and one session at a time is byte for byte the one WriteSnapshot
// produces from the whole snapshot, an empty log is an empty array, and a row count
// the manifest did not announce is refused at Close rather than written as a lie.
func TestSnapshotWriterStreamsSessions(t *testing.T) {
	libs := []*model.Library{{PID: "L1", DisplayRoot: "/music", Mode: model.ModeManaged, Profile: "waxbin-native"}}
	items := []*model.ItemView{{PID: "I1", Kind: model.KindTrack, State: model.StatePresent, Title: "Song"}}
	plays := []model.PlayState{{UserPID: "U1", ItemPID: "I1", PlayCount: 2}}
	sessions := []model.PlaySession{
		{PID: "S1", UserPID: "U1", ItemPID: "I1", StartedAt: 1 << 60, EndedAt: 1<<60 + 5, MsPlayed: 240000, Client: "lastfm"},
		{PID: "S2", UserPID: "U1", ItemPID: "I1", StartedAt: 1<<60 + 9},
	}
	var whole bytes.Buffer
	if err := port.WriteSnapshot(&whole, port.BuildSnapshot(12, 1700000000, libs, items, plays, sessions, nil)); err != nil {
		t.Fatalf("write whole: %v", err)
	}

	head := port.BuildSnapshot(12, 1700000000, libs, items, plays, nil, nil)
	head.Manifest.PlaySessions = len(sessions)
	var streamed bytes.Buffer
	sw := port.NewSnapshotWriter(&streamed)
	if err := sw.Begin(head); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, s := range sessions {
		if err := sw.Session(port.SessionExport(s)); err != nil {
			t.Fatalf("session: %v", err)
		}
	}
	if err := sw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if streamed.String() != whole.String() {
		t.Errorf("streamed document differs from the whole one:\n%s\n---\n%s", streamed.String(), whole.String())
	}
	got, err := port.ReadSnapshot(&streamed)
	if err != nil {
		t.Fatalf("read streamed: %v", err)
	}
	if len(got.PlaySessions) != 2 || got.PlaySessions[0].PID != "S1" || got.PlaySessions[1].PID != "S2" || got.Manifest.PlaySessions != 2 {
		t.Errorf("streamed sessions read back as %+v under manifest %+v", got.PlaySessions, got.Manifest)
	}

	var empty bytes.Buffer
	if err := port.WriteSnapshot(&empty, port.BuildSnapshot(12, 1700000000, libs, items, plays, nil, nil)); err != nil {
		t.Fatalf("write empty: %v", err)
	}
	if !strings.Contains(empty.String(), `"playSessions": []`) {
		t.Errorf("an empty log should encode as an empty array, got:\n%s", empty.String())
	}

	var short bytes.Buffer
	sw = port.NewSnapshotWriter(&short)
	if err := sw.Begin(head); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := sw.Session(port.SessionExport(sessions[0])); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := sw.Close(); !waxerr.Is(err, waxerr.CodeInternal) {
		t.Errorf("closing with one row under a manifest announcing two: got %v, want CodeInternal", err)
	}
}
