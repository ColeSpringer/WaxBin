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
