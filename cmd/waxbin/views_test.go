package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/analyze"
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
	t.Parallel()
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
	t.Parallel()
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

// TestEnrichViewGroupArtCounts pins the group-art backfill counters in the `enrich
// --json` payload: present when the phase ran, absent when it did not. They matter
// because Result.total() counts them, which means `enrich --limit N` can spend its
// budget on that phase, and a payload that never mentions it cannot explain where N
// went.
func TestEnrichViewGroupArtCounts(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{ArtistsEnriched: 1, ArtistsMatched: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "groupArt") || strings.Contains(string(b), "auxArt") {
		t.Errorf("a run without the phase emitted its counts: %s", b)
	}

	b, err = json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{GroupArtEnriched: 3, GroupArtMatched: 2, AuxArtFetched: 4},
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"groupArtEnriched":3`, `"groupArtMatched":2`, `"auxArtFetched":4`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json = %s\nwant it to carry %s", b, want)
		}
	}
}

// TestRenderEnrichResultGroupArtLine: the backfill phase gets a summary line of its
// own when it ran, and the aux image tally beside it stays distinguishable from it.
func TestRenderEnrichResultGroupArtLine(t *testing.T) {
	t.Parallel()
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

	ran := render(enrich.Result{GroupArtEnriched: 3, GroupArtMatched: 2, AuxArtFetched: 4})
	if got := lineWith(t, ran, "group art:"); !strings.Contains(got, "3 backfilled (2 matched)") {
		t.Errorf("group art line = %q, want the release groups walked and matched", got)
	}
	if got := lineWith(t, ran, "aux art images:"); !strings.Contains(got, "4 fetched") {
		t.Errorf("aux art images line = %q, want the image tally", got)
	}

	// A run that did not walk the phase keeps the summary it always had.
	if got := render(enrich.Result{ArtistsEnriched: 1}); strings.Contains(got, "group art") {
		t.Errorf("a run without the phase printed a group art line:\n%s", got)
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

// TestEnrichViewRetriedCount: the retry tally rides the same rule as the gated phases.
// It is absent from a run that re-asked nothing, so the ordinary payload keeps its
// shape, and present when it did, because those targets spent the --limit budget too.
func TestEnrichViewRetriedCount(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{ArtistsEnriched: 1, ArtistsMatched: 1},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "retried") {
		t.Errorf("a run that re-asked nothing emitted a retry count: %s", b)
	}

	b, err = json.Marshal(toEnrichView(&waxbin.EnrichResult{
		Result: enrich.Result{ArtistsEnriched: 4, Retried: 3},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"retried":3`) {
		t.Errorf("json = %s\nwant it to carry the retry count", b)
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderEnrichResult(cmd, &globals{}, &waxbin.EnrichResult{
		Result: enrich.Result{ArtistsEnriched: 4, Retried: 3},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "retried:        3 earlier misses") {
		t.Errorf("summary is missing the retry line:\n%s", buf.String())
	}
}

// TestItemViewJSONCarriesTheTotals: the totals ride beside their numbers in the item
// JSON, so a caller editing one can read it back.
func TestItemViewJSONCarriesTheTotals(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(toItemView(&model.ItemView{PID: "t1", TrackNo: 7, TrackTotal: 12, DiscNo: 1, DiscTotal: 2}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"track":7,"trackTotal":12`, `"disc":1,"discTotal":2`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json = %s, want it to carry %s", b, want)
		}
	}
}

// TestAnalyzeResultReportsTheFingerprintPath: a pass reports the files fpcalc failed on,
// the partial reads it kept, and the files whose measurement failed, in its JSON and its
// summary, so a pass that quietly fell back to the pure-Go fingerprint does not read as
// a clean Chromaprint one.
func TestAnalyzeResultReportsTheFingerprintPath(t *testing.T) {
	t.Parallel()
	res := &waxbin.AnalyzeResult{Result: analyze.Result{
		Analyzed: 9, FingerprintFallbacks: 2, FingerprintPartialReads: 3, MeasureFailed: 1,
	}}
	b, err := json.Marshal(toAnalyzeView(res))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"fingerprintFallbacks":2`, `"fingerprintPartialReads":3`, `"measureFailed":1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("json = %s\nwant it to carry %s", b, want)
		}
	}

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := renderAnalyzeResult(cmd, &globals{}, res); err != nil {
		t.Fatalf("render: %v", err)
	}
	if got := lineWith(t, buf.String(), "fallback:"); !strings.Contains(got, "2") {
		t.Errorf("fallback line = %q, want the 2 files fpcalc failed on", got)
	}
	if got := lineWith(t, buf.String(), "partial:"); !strings.Contains(got, "3") {
		t.Errorf("partial line = %q, want the 3 partial reads kept", got)
	}
}

// TestEpisodeViewsCarryTheTranscriptFlag: a listed episode says whether a transcript is
// stored, as the detail view always did, under the same key.
func TestEpisodeViewsCarryTheTranscriptFlag(t *testing.T) {
	t.Parallel()
	ep := &model.Episode{PID: "e1", Title: "One", State: model.StatePresent, HasTranscript: true}
	for name, v := range map[string]any{
		"list":   toEpisodeViews([]*model.Episode{ep})[0],
		"detail": toEpisodeDetailView(&model.EpisodeDetail{Episode: ep}),
	} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), `"hasTranscript":true`) {
			t.Errorf("%s view = %s, want hasTranscript true", name, b)
		}
	}
}
