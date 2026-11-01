package main

import (
	"encoding/json"
	"testing"

	"github.com/colespringer/waxbin/audit"
	"github.com/colespringer/waxbin/model"
)

// TestAuditViewCarriesTheFileAndLengths: a finding's file pid and a duration mismatch's
// two lengths reach the JSON, and a finding without them leaves the keys out.
func TestAuditViewCarriesTheFileAndLengths(t *testing.T) {
	t.Parallel()
	rep := &audit.Report{Findings: []model.AuditFinding{
		{Check: model.CheckDurationMismatch, Severity: model.SeverityWarn, Message: "m", Path: "/lib/a.mp3",
			FilePID: "f1", HeaderMS: 60_000, DecodedMS: 3_000},
		{Check: model.CheckDuplicateArtist, Severity: model.SeverityWarn, Message: "d"},
		{Check: model.CheckDurationMismatch, Severity: model.SeverityWarn, Message: "z", Path: "/lib/z.mp3",
			FilePID: "f2", HeaderMS: 60_000},
	}}
	b, err := json.Marshal(toAuditView(rep))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Findings) != 3 {
		t.Fatalf("findings = %s, want three", b)
	}
	f := v.Findings[0]
	if f["filePid"] != "f1" || f["headerMs"] != float64(60_000) || f["decodedMs"] != float64(3_000) {
		t.Errorf("duration finding = %v, want filePid f1, headerMs 60000, decodedMs 3000", f)
	}
	for _, k := range []string{"filePid", "headerMs", "decodedMs"} {
		if _, ok := v.Findings[1][k]; ok {
			t.Errorf("duplicate finding carries %s: %v", k, v.Findings[1])
		}
	}
	if d, ok := v.Findings[2]["decodedMs"]; !ok || d != float64(0) {
		t.Errorf("audio that decodes to nothing = %v, want decodedMs 0 rather than no key", v.Findings[2])
	}
}

// TestAuditViewCarriesReasonsMembersAndRollUps: a finding's reason, a duplicate's members
// with names and counts, and a roll-up's count of findings beyond those shown reach the
// JSON, and a finding without them leaves the keys out.
func TestAuditViewCarriesReasonsMembersAndRollUps(t *testing.T) {
	t.Parallel()
	rep := &audit.Report{Findings: []model.AuditFinding{
		{Check: model.CheckDuplicateArtist, Severity: model.SeverityWarn, Message: "d", Reason: string(model.ReasonSharedMBID),
			Members: []model.DuplicateMember{{PID: "a1", Name: "The Beatles", TrackCount: 5}, {PID: "a2", Name: "Beatles", TrackCount: 2}}},
		{Check: model.CheckIntegrity, Severity: model.SeverityError, Message: "7 integrity failures (2 shown)", More: 5},
		{Check: model.CheckMissingArt, Severity: model.SeverityInfo, Message: "no cover art: X"},
	}}
	b, err := json.Marshal(toAuditView(rep))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Findings []struct {
			Reason  string `json:"reason"`
			Members []struct {
				PID        string `json:"pid"`
				Name       string `json:"name"`
				TrackCount int    `json:"trackCount"`
			} `json:"members"`
			More int `json:"more"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(b, &v); err != nil || len(v.Findings) != 3 {
		t.Fatalf("view = %s (err %v)", b, err)
	}
	if f := v.Findings[0]; f.Reason != "shared MBID" || len(f.Members) != 2 || f.Members[1].Name != "Beatles" || f.Members[1].TrackCount != 2 {
		t.Errorf("duplicate finding = %+v, want its reason and both members", f)
	}
	if v.Findings[1].More != 5 {
		t.Errorf("roll-up = %+v, want more 5", v.Findings[1])
	}
	var raw struct {
		Findings []map[string]any `json:"findings"`
	}
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"reason", "members", "more"} {
		if _, ok := raw.Findings[2][k]; ok {
			t.Errorf("a plain finding carries %s: %v", k, raw.Findings[2])
		}
	}
}
