package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
)

// TestPrintEnrichCacheReportShowsEveryKind: the breakdown is what a prune decision
// reads, and the exempt share has to be visible in both the total line and the table.
func TestPrintEnrichCacheReportShowsEveryKind(t *testing.T) {
	westOfUTC(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	rep := &model.EnrichmentCacheReport{
		Rows: 4213, Bytes: 61_200_000,
		OldestAt:   time.Date(2026, 7, 2, 2, 0, 0, 0, time.UTC).UnixNano(),
		NewestAt:   time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC).UnixNano(),
		ExemptRows: 312, ExemptBytes: 41_000,
		Kinds: []model.EnrichmentCacheKind{
			{Kind: "mb:rg-search", Rows: 1440, Bytes: 31_000_000},
			{Kind: "mb:artist-search", Rows: 900, Bytes: 12_400_000},
			{Kind: "caa:rg-front", Rows: 312, Bytes: 41_000, Exempt: true},
		},
	}
	var buf bytes.Buffer
	printEnrichCacheReport(&buf, rep, now)
	got := buf.String()

	for _, want := range []string{
		"4213 rows", "61.2 MB", "312 rows", "41.0 KB", "exempt from prune",
		"mb:rg-search", "1440", "31.0 MB", "mb:artist-search", "caa:rg-front", "exempt",
		"2026-07-02 to 2026-09-06", "oldest 66 days ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

// TestPrintEnrichCacheReportOnAnEmptyCacheSaysSo: an empty cache reads as empty rather
// than as an epoch timestamp with a table of nothing under it.
func TestPrintEnrichCacheReportOnAnEmptyCacheSaysSo(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	printEnrichCacheReport(&buf, &model.EnrichmentCacheReport{}, now)
	got := buf.String()

	if strings.Contains(got, "1970") || strings.Contains(got, "ago") {
		t.Errorf("an empty cache printed an age:\n%s", got)
	}
	if strings.Contains(got, "exempt") {
		t.Errorf("an empty cache claimed an exempt share:\n%s", got)
	}
	if !strings.Contains(got, "0 rows") {
		t.Errorf("an empty cache did not say so:\n%s", got)
	}
}

func TestEnrichCacheViewJSON(t *testing.T) {
	rep := &model.EnrichmentCacheReport{
		Rows: 3, Bytes: 900, OldestAt: 1784777333683766021, NewestAt: 1784777333683766022,
		ExemptRows: 1, ExemptBytes: 20,
		Kinds: []model.EnrichmentCacheKind{
			{Kind: "mb:artist", Rows: 2, Bytes: 880},
			{Kind: "caa:rg-front", Rows: 1, Bytes: 20, Exempt: true},
		},
	}
	b, err := json.Marshal(toEnrichCacheView(rep, false, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"rows":3,"bytes":900,"oldestAt":"1784777333683766021","newestAt":"1784777333683766022",` +
		`"exemptRows":1,"exemptBytes":20,` +
		`"kinds":[{"kind":"mb:artist","rows":2,"bytes":880},{"kind":"caa:rg-front","rows":1,"bytes":20,"exempt":true}]}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	pruned, err := json.Marshal(toEnrichCacheView(rep, true, 4, 512))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pruned), `"pruned":{"rows":4,"bytes":512}`) {
		t.Errorf("pruned json = %s, want a pruned block", pruned)
	}
}
