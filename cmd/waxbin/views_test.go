package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

// TestPlayStateViewJSON pins the `state --json` payload shape, in particular
// that the unix-ns change stamps encode as decimal STRINGS: the values exceed
// IEEE-754 double precision, so a bare number would be silently corrupted by
// any consumer that parses JSON numbers into doubles (JS, jq 1.6, loose Go
// decoding). Zero stamps (never changed) are omitted.
func TestPlayStateViewJSON(t *testing.T) {
	r := 80
	full := &model.PlayState{
		ItemPID: "i1", PositionMS: 42000, Played: true, PlayCount: 3,
		Rating: r, HasRating: true, Starred: true,
		RatingChangedAt: 1784777333683766021, StarredChangedAt: 1784777321347098926,
	}
	b, err := json.Marshal(toPlayStateView(full))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"itemPid":"i1","positionMs":42000,"played":true,"finished":false,` +
		`"playCount":3,"rating":80,"starred":true,` +
		`"ratingChangedAt":"1784777333683766021","starredChangedAt":"1784777321347098926"}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}

	zero := &model.PlayState{ItemPID: "i1"}
	b, err = json.Marshal(toPlayStateView(zero))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"itemPid":"i1","positionMs":0,"played":false,"finished":false,"playCount":0,"starred":false}`
	if string(b) != want {
		t.Errorf("zero json = %s\nwant %s (never-changed stamps omitted)", b, want)
	}
}

// TestPrintItemTableEpisodeColumn pins the kind-aware header of the shared item
// table, which query, query --page, and browse all render through.
//
// The PUBLISHED column is what makes a publication-ordered listing legible: an
// episode's year is derived from its pub date, so a whole season reads as one
// repeated year and `browse recent-episodes` looks unordered without the date. It is
// chosen from the rows rather than always emitted, so a music-only catalog does not
// grow a permanently blank column.
func TestPrintItemTableEpisodeColumn(t *testing.T) {
	track := &model.ItemView{
		PID: "t1", Kind: model.KindTrack, Title: "Airbag",
		Artist: "Radiohead", Album: "OK Computer", TrackNo: 1, Year: 1997,
	}
	// 2025-01-09T10:00:00Z, and a book with neither a track number nor a year.
	episode := &model.ItemView{
		PID: "e1", Kind: model.KindEpisode, Title: "Ep One",
		Artist: "My Show", Album: "My Show", Year: 2025, PubDateNS: 1736416800_000000000,
	}
	book := &model.ItemView{PID: "b1", Kind: model.KindBook, Title: "Tome", Artist: "Author"}

	var musicOnly strings.Builder
	if err := printItemTable(&musicOnly, []*model.ItemView{track, book}); err != nil {
		t.Fatalf("music-only table: %v", err)
	}
	if strings.Contains(musicOnly.String(), "PUBLISHED") {
		t.Errorf("a catalog with no episodes grew a PUBLISHED column:\n%s", musicOnly.String())
	}
	// A zero track number and year print blank, not "0": neither is a real value for
	// a book, and printing 0 reads as data.
	bookLine := lineWith(t, musicOnly.String(), "Tome")
	if strings.Contains(bookLine, "0") {
		t.Errorf("book row renders a zero track/year as 0: %q", bookLine)
	}

	var mixed strings.Builder
	if err := printItemTable(&mixed, []*model.ItemView{track, episode}); err != nil {
		t.Fatalf("mixed table: %v", err)
	}
	out := mixed.String()
	if !strings.Contains(out, "PUBLISHED") {
		t.Errorf("a listing containing an episode has no PUBLISHED column:\n%s", out)
	}
	if got := lineWith(t, out, "Ep One"); !strings.Contains(got, "2025-01-09") {
		t.Errorf("episode row = %q, want the UTC publication date", got)
	}
	// No row in either table ends in whitespace. This is the assertion that matters
	// for the blank-cell rendering: tabwriter pads an empty tab-terminated cell, so a
	// book (no track number, no year) would otherwise trail a run of spaces from the
	// columns it leaves blank. Checking only the track line missed it, since a track
	// populates both.
	for name, table := range map[string]string{"music-only": musicOnly.String(), "mixed": out} {
		for _, line := range strings.Split(strings.TrimRight(table, "\n"), "\n") {
			if line != strings.TrimRight(line, " \t") {
				t.Errorf("%s table has a row ending in whitespace: %q", name, line)
			}
		}
	}
	// Alignment survives the trimming: the date lines up under its header.
	if hdr, ep := lineWith(t, out, "PUBLISHED"), lineWith(t, out, "Ep One"); //
	strings.Index(hdr, "PUBLISHED") != strings.Index(ep, "2025-01-09") {
		t.Errorf("PUBLISHED column is misaligned:\n%s\n%s", hdr, ep)
	}
}

// TestEnrichViewAuxCounts pins the aux-backfill counters in the `enrich --json`
// payload: present when the phase ran, absent when it did not, so an install with no
// aux-capable provider emits exactly the shape it always did. They matter because
// Result.total() counts them, which means `enrich --limit N` can spend its budget on
// that phase, and a payload that never mentions it cannot explain where N went.
func TestEnrichViewAuxCounts(t *testing.T) {
	b, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{ArtistsEnriched: 1, ArtistsMatched: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "auxArt") {
		t.Errorf("a run without the phase emitted aux counts: %s", b)
	}

	b, err = json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{AuxArtEnriched: 3, AuxArtMatched: 2, AuxArtFetched: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"auxArtEnriched":3`, `"auxArtMatched":2`, `"auxArtFetched":4`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json = %s\nwant it to carry %s", b, want)
		}
	}
}

// TestRenderEnrichResultAuxLine: the backfill phase gets a summary line of its own
// when it ran, and the image tally beside it stays distinguishable from it.
func TestRenderEnrichResultAuxLine(t *testing.T) {
	render := func(r enrich.Result) string {
		t.Helper()
		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := renderEnrichResult(cmd, &globals{}, &waxbin.EnrichResult{Result: r}); err != nil {
			t.Fatalf("render: %v", err)
		}
		return buf.String()
	}

	ran := render(enrich.Result{AuxArtEnriched: 3, AuxArtMatched: 2, AuxArtFetched: 4})
	if got := lineWith(t, ran, "aux art:"); !strings.Contains(got, "3 backfilled (2 matched)") {
		t.Errorf("aux art line = %q, want the release groups walked and matched", got)
	}
	if got := lineWith(t, ran, "aux art images:"); !strings.Contains(got, "4 fetched") {
		t.Errorf("aux art images line = %q, want the image tally", got)
	}

	// A stock run (no aux-capable provider) keeps the summary it always had.
	if got := render(enrich.Result{ArtistsEnriched: 1}); strings.Contains(got, "aux art") {
		t.Errorf("a run without the phase printed an aux line:\n%s", got)
	}
}

// lineWith returns the single output line containing want.
func lineWith(t *testing.T, out, want string) string {
	t.Helper()
	var found string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, want) {
			if found != "" {
				t.Fatalf("more than one line contains %q", want)
			}
			found = l
		}
	}
	if found == "" {
		t.Fatalf("no line contains %q in:\n%s", want, out)
	}
	return found
}
