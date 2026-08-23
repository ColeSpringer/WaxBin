package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestParseBytes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"1024", 1024},
		{"512KB", 512_000},
		{"512KiB", 524_288},
		{"200MB", 200_000_000},
		{"200MiB", 209_715_200},
		{"2GB", 2_000_000_000},
		{"2GiB", 2_147_483_648},
		{"200mb", 200_000_000},
		{" 200 MB ", 200_000_000},
		{"4096B", 4096},
	} {
		got, err := parseBytes("db thumbs", tc.in)
		if err != nil {
			t.Errorf("parseBytes(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseBytes(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseBytesRejects(t *testing.T) {
	// A negative budget has no meaning, and an unrecognized unit must not fall back
	// to reading the digits as bytes: 200TB silently becoming 200 would prune a cache
	// the operator meant to leave alone.
	for _, in := range []string{"", "abc", "-1", "-200MB", "1.5GB", "200PB", "MB", "200 200"} {
		if got, err := parseBytes("db thumbs", in); err == nil {
			t.Errorf("parseBytes(%q) = %d, want a refusal", in, got)
		} else if waxerr.CodeOf(err) != waxerr.CodeInvalid {
			t.Errorf("parseBytes(%q) err = %v, want CodeInvalid", in, err)
		}
	}
}

func TestByteLabel(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1.0 KB"},
		{312_400_000, "312.4 MB"},
		{1_100_000_000, "1.1 GB"},
	} {
		if got := byteLabel(tc.in); got != tc.want {
			t.Errorf("byteLabel(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// westOfUTC moves the process zone west of UTC for one test, so an instant just past
// midnight UTC renders a day earlier in local time. Without it a report that formats
// in local time passes everywhere the developer happens to sit in UTC.
func westOfUTC(t *testing.T) {
	t.Helper()
	saved := time.Local
	time.Local = time.FixedZone("test-0800", -8*60*60)
	t.Cleanup(func() { time.Local = saved })
}

// TestPrintThumbReportShowsEveryRung is the surface the census exists for: a cover
// browsed at three rungs has to read as three derivatives with their own costs, not
// as one line of totals. The dates are UTC, the rule views.go states for a stored
// instant, so the printed day does not shift under the reader.
func TestPrintThumbReportShowsEveryRung(t *testing.T) {
	westOfUTC(t)
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	rep := &model.ThumbCacheReport{
		Rows: 1284, Bytes: 312_400_000, Sources: 642,
		ArtSources: 700, ArtSourceBytes: 1_100_000_000,
		OldestAt: time.Date(2026, 8, 20, 2, 0, 0, 0, time.UTC).UnixNano(),
		NewestAt: time.Date(2026, 8, 23, 2, 0, 0, 0, time.UTC).UnixNano(),
		Rungs: []model.ThumbRung{
			{Size: 1200, Rows: 210, Bytes: 198_200_000},
			{Size: 600, Rows: 412, Bytes: 81_000_000},
			{Size: 300, Rows: 662, Bytes: 33_200_000},
		},
	}
	var buf bytes.Buffer
	printThumbReport(&buf, rep, now)
	got := buf.String()

	for _, want := range []string{
		"312.4 MB", "642 source image", "1.1 GB", "1200", "198.2 MB", "300",
		// Both ends of the range: an operator choosing a retention age needs the near
		// end as much as the far one.
		"2026-08-20 to 2026-08-23", "oldest 3 days ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
}

// TestAgoLabelUnderADay pins that a fresh entry does not claim a calendar day. The
// date beside it is UTC while "today" would be a local-calendar claim, and the two
// disagree for anything generated after the reader's local midnight.
func TestAgoLabelUnderADay(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	if got := agoLabel(now, now.Add(-3*time.Hour)); got != "under a day ago" {
		t.Errorf("agoLabel 3h back = %q, want %q", got, "under a day ago")
	}
	if got := agoLabel(now, now.Add(-25*time.Hour)); got != "1 day ago" {
		t.Errorf("agoLabel 25h back = %q, want %q", got, "1 day ago")
	}
}

// TestPrintThumbReportOnAnEmptyCacheSaysSo pins that an empty cache reads as empty
// rather than as a table of zeroes with an epoch timestamp under it.
func TestPrintThumbReportOnAnEmptyCacheSaysSo(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	printThumbReport(&buf, &model.ThumbCacheReport{ArtSources: 700, ArtSourceBytes: 1_100_000_000}, now)
	got := buf.String()

	if strings.Contains(got, "1970") || strings.Contains(got, "ago") {
		t.Errorf("an empty cache printed an age:\n%s", got)
	}
	if !strings.Contains(got, "1.1 GB") {
		t.Errorf("an empty cache dropped the originals it would derive from:\n%s", got)
	}
}

func TestThumbCacheViewJSON(t *testing.T) {
	rep := &model.ThumbCacheReport{
		Rows: 2, Bytes: 900, Sources: 1, ArtSources: 1, ArtSourceBytes: 4096,
		OldestAt: 1784777333683766021, NewestAt: 1784777333683766022,
		Rungs: []model.ThumbRung{{Size: 300, Rows: 2, Bytes: 900}},
	}
	b, err := json.Marshal(toThumbCacheView(rep, true, 4, 512))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"rows":2,"bytes":900,"sources":1,"artSources":1,"artSourceBytes":4096,` +
		`"oldestAt":"1784777333683766021","newestAt":"1784777333683766022",` +
		`"rungs":[{"size":300,"rows":2,"bytes":900}],"pruned":{"rows":4,"bytes":512}}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	// An empty cache has no age, and a report with no prune has no prune block: an
	// epoch stamp and a zeroed "pruned" would both read as things that happened.
	b, err = json.Marshal(toThumbCacheView(&model.ThumbCacheReport{}, false, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"rows":0,"bytes":0,"sources":0,"artSources":0,"artSourceBytes":0,"rungs":[]}`
	if string(b) != want {
		t.Errorf("empty json = %s\nwant %s", b, want)
	}
}
