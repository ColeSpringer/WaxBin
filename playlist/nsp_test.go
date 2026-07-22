package playlist

import (
	"fmt"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

func TestImportNSPBasic(t *testing.T) {
	data := []byte(`{"all":[{"is":{"artist":"Radiohead"}},{"contains":{"title":"karma"}}],"sort":"title","order":"desc","limit":50}`)
	q, err := ImportNSP(data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if q.Entity != query.EntityItems {
		t.Errorf("entity = %q, want items", q.Entity)
	}
	and, ok := q.Where.(query.And)
	if !ok || len(and.Nodes) != 2 {
		t.Fatalf("where = %T (%+v), want And of 2", q.Where, q.Where)
	}
	if len(q.Sorts) != 1 || q.Sorts[0].Field != "title" || !q.Sorts[0].Desc {
		t.Errorf("sorts = %+v, want title desc", q.Sorts)
	}
	if q.Limit != 50 {
		t.Errorf("limit = %d, want 50", q.Limit)
	}
}

func TestImportNSPAnyAndNested(t *testing.T) {
	data := []byte(`{"any":[{"is":{"genre":"Jazz"}},{"all":[{"gt":{"year":2000}},{"notContains":{"album":"live"}}]}]}`)
	q, err := ImportNSP(data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	or, ok := q.Where.(query.Or)
	if !ok || len(or.Nodes) != 2 {
		t.Fatalf("where = %T, want Or of 2", q.Where)
	}
	inner, ok := or.Nodes[1].(query.And)
	if !ok || len(inner.Nodes) != 2 {
		t.Fatalf("nested = %T, want And of 2", or.Nodes[1])
	}
	if _, ok := inner.Nodes[1].(query.Not); !ok {
		t.Errorf("notContains did not map to Not: %T", inner.Nodes[1])
	}
}

func TestImportNSPRejectsUnsupported(t *testing.T) {
	cases := map[string]string{
		"relative op": `{"all":[{"inTheLast":{"year":30}}]}`,
		"unknown op":  `{"all":[{"inPlaylist":{"title":"x"}}]}`,
		"bad field":   `{"all":[{"is":{"comment":"x"}}]}`,
		"no root":     `{"limit":10}`,
		"bad sort":    `{"all":[{"is":{"title":"x"}}],"sort":"comment"}`,
	}
	for name, doc := range cases {
		if _, err := ImportNSP([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("%s: want CodeUnsupported, got %v", name, err)
		}
	}
}

func TestImportNSPIgnoresNameAndComment(t *testing.T) {
	// Navidrome writes playlist metadata (name/comment) at the top level; these do not
	// affect membership, so importing must succeed and ignore them rather than reject an
	// otherwise-representable document.
	data := []byte(`{"name":"My Mix","comment":"road trip","all":[{"is":{"artist":"Radiohead"}}]}`)
	q, err := ImportNSP(data)
	if err != nil {
		t.Fatalf("import with name/comment: %v", err)
	}
	and, ok := q.Where.(query.And)
	if !ok || len(and.Nodes) != 1 {
		t.Fatalf("where = %T, want And of 1 (name/comment ignored, rule preserved)", q.Where)
	}
	// A genuinely semantics-affecting key WaxBin cannot represent is still rejected.
	if _, err := ImportNSP([]byte(`{"limitPercent":50,"all":[{"is":{"artist":"X"}}]}`)); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("limitPercent: want CodeUnsupported, got %v", err)
	}
}

func TestImportNSPStrictLimitOffsetOrder(t *testing.T) {
	// limit/offset/order are handled consistently: a malformed value on any of them
	// rejects the whole import rather than being silently discarded on some and errored
	// on others.
	cases := map[string]string{
		"bad limit":  `{"all":[{"is":{"title":"x"}}],"limit":"notanumber"}`,
		"bad offset": `{"all":[{"is":{"title":"x"}}],"offset":"notanumber"}`,
		"bad order":  `{"all":[{"is":{"title":"x"}}],"sort":"title","order":123}`,
	}
	for name, doc := range cases {
		if _, err := ImportNSP([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("%s: want CodeUnsupported, got %v", name, err)
		}
	}
	// Well-formed limit/offset/order still import cleanly.
	q, err := ImportNSP([]byte(`{"all":[{"is":{"title":"x"}}],"sort":"title","order":"desc","limit":10,"offset":5}`))
	if err != nil {
		t.Fatalf("well-formed import: %v", err)
	}
	if q.Limit != 10 || q.Offset != 5 || len(q.Sorts) != 1 || !q.Sorts[0].Desc {
		t.Errorf("parsed q = %+v, want limit 10 offset 5 sort title desc", q)
	}
}

func TestNSPRoundTrip(t *testing.T) {
	orig := []byte(`{"any":[{"isNot":{"artist":"X"}},{"inTheRange":{"year":[1990,1999]}},{"startsWith":{"album":"The"}}],"sort":"year","order":"asc","limit":25}`)
	q1, err := ImportNSP(orig)
	if err != nil {
		t.Fatalf("import1: %v", err)
	}
	out, err := ExportNSP(q1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	q2, err := ImportNSP(out)
	if err != nil {
		t.Fatalf("import2: %v", err)
	}
	// Compare via the canonical rule marshal so the two queries must be equivalent.
	b1, err := query.MarshalRule(q1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := query.MarshalRule(q2)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Errorf("round-trip diverged:\n q1=%s\n q2=%s", b1, b2)
	}
}

func TestNSPUserStateFields(t *testing.T) {
	// Navidrome's per-user fields (rating/starred/playcount) map to WaxBin's
	// user-state query fields, so a rating/starred/playcount rule imports and
	// round-trips. The user is bound at read time, never in the rule doc.
	data := []byte(`{"all":[{"gt":{"rating":3}},{"is":{"starred":true}},{"gt":{"playcount":0}}]}`)
	q, err := ImportNSP(data)
	if err != nil {
		t.Fatalf("import user-state nsp: %v", err)
	}
	and, ok := q.Where.(query.And)
	if !ok || len(and.Nodes) != 3 {
		t.Fatalf("where = %T, want And of 3", q.Where)
	}
	// The fields lowered to the WaxBin user-state field names, and rating scaled from
	// Navidrome's 0-to-5 scale to WaxBin's 0-to-100 one (3 stars becomes 60).
	wantFields := map[string]bool{"rating": true, "starred": true, "play_count": true}
	for _, n := range and.Nodes {
		c, ok := n.(query.Cond)
		if !ok {
			t.Fatalf("node = %T, want Cond", n)
		}
		if !wantFields[c.Field] {
			t.Errorf("unexpected field %q", c.Field)
		}
		delete(wantFields, c.Field)
		if c.Field == "rating" {
			if f, _ := asFloat(c.Value); f != 60 {
				t.Errorf("rating value = %v, want 60 (3 stars * %d)", c.Value, nspRatingScale)
			}
		}
	}
	if len(wantFields) != 0 {
		t.Errorf("missing mapped fields: %v", wantFields)
	}

	// Full round-trip equivalence through the canonical rule marshal (60 -> 3 stars
	// on export -> 60 again on re-import).
	out, err := ExportNSP(q)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	q2, err := ImportNSP(out)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	b1, _ := query.MarshalRule(q)
	b2, _ := query.MarshalRule(q2)
	if string(b1) != string(b2) {
		t.Errorf("user-state round-trip diverged:\n q1=%s\n q2=%s", b1, b2)
	}
}

// TestExportNSPRejectsTagCond confirms a custom-tag predicate cannot round-trip to a
// Navidrome smart playlist: .nsp has no custom-tag concept, so ExportNSP faithfully
// rejects the whole document (CodeUnsupported) rather than dropping the tag filter.
func TestExportNSPRejectsTagCond(t *testing.T) {
	q := query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "happy").Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("ExportNSP of a tag.* cond: want CodeUnsupported, got %v", err)
	}
}

func TestNSPRatingScaleAndLastPlayed(t *testing.T) {
	// lastPlayed maps only through the RELATIVE operators; an absolute date rule
	// holds a date string WaxBin's nanosecond column cannot compare against, so it
	// stays (deliberately) unsupported, not silently mis-mapped.
	if _, err := ImportNSP([]byte(`{"all":[{"before":{"lastPlayed":"2023-01-01"}}]}`)); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("lastPlayed import: want CodeUnsupported, got %v", err)
	}

	// A WaxBin rating that is a whole star exports cleanly (80/100 -> 4 stars).
	// (gte/lte have no .nsp operator, so gt is used here.)
	whole := query.New(query.EntityItems).Where("rating", query.OpGt, 80).Build()
	out, err := ExportNSP(whole)
	if err != nil {
		t.Fatalf("export whole-star rating: %v", err)
	}
	if !strings.Contains(string(out), `"rating": 4`) {
		t.Errorf("rating 80 should export as 4 stars, got: %s", out)
	}

	// A rating that is not a whole star has no faithful 0-to-5 representation, so
	// export rejects it rather than emitting a mismatched value.
	frac := query.New(query.EntityItems).Where("rating", query.OpGt, 73).Build()
	if _, err := ExportNSP(frac); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export rating 73 (not a whole star): want CodeUnsupported, got %v", err)
	}
}

func TestNSPRelativeDates(t *testing.T) {
	// inTheLast/notInTheLast on the date fields map to WaxBin's relative-time
	// operators with the day count converted to a nanosecond window.
	data := []byte(`{"all":[{"inTheLast":{"lastPlayed":30}},{"notInTheLast":{"dateAdded":7}}]}`)
	q, err := ImportNSP(data)
	if err != nil {
		t.Fatalf("import relative dates: %v", err)
	}
	and, ok := q.Where.(query.And)
	if !ok || len(and.Nodes) != 2 {
		t.Fatalf("where = %T, want And of 2", q.Where)
	}
	c0 := and.Nodes[0].(query.Cond)
	if c0.Field != "last_played" || c0.Op != query.OpInTheLast || c0.Value != 30*nspDayNS {
		t.Errorf("cond 0 = %+v, want last_played inTheLast 30d in ns", c0)
	}
	c1 := and.Nodes[1].(query.Cond)
	if c1.Field != "added" || c1.Op != query.OpNotInTheLast || c1.Value != 7*nspDayNS {
		t.Errorf("cond 1 = %+v, want added notInTheLast 7d in ns", c1)
	}

	// Full round-trip: export back to days, re-import, same canonical rule.
	out, err := ExportNSP(q)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	q2, err := ImportNSP(out)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	b1, _ := query.MarshalRule(q)
	b2, _ := query.MarshalRule(q2)
	if string(b1) != string(b2) {
		t.Errorf("relative-date round-trip diverged:\n q1=%s\n q2=%s", b1, b2)
	}

	// Still rejected: absolute operators on date fields, fractional/non-positive
	// day counts, the relative ops on a non-date field, and a day count whose
	// nanosecond window would overflow int64 (the wrap can land on a positive,
	// plausible-looking but wrong window, so the bound rejects before it).
	rejected := map[string]string{
		"absolute after":         `{"all":[{"after":{"dateAdded":"2023-01-01"}}]}`,
		"absolute is":            `{"all":[{"is":{"lastPlayed":"2023-01-01"}}]}`,
		"fractional days":        `{"all":[{"inTheLast":{"lastPlayed":1.5}}]}`,
		"zero days":              `{"all":[{"inTheLast":{"lastPlayed":0}}]}`,
		"negative days":          `{"all":[{"notInTheLast":{"dateAdded":-3}}]}`,
		"non-numeric days":       `{"all":[{"inTheLast":{"lastPlayed":"thirty"}}]}`,
		"relative non-date":      `{"all":[{"inTheLast":{"year":30}}]}`,
		"relative on artist":     `{"all":[{"notInTheLast":{"artist":30}}]}`,
		"overflow negative-wrap": `{"all":[{"inTheLast":{"lastPlayed":200000}}]}`,
		"overflow positive-wrap": `{"all":[{"notInTheLast":{"dateAdded":320000}}]}`,
		"absurd days":            `{"all":[{"inTheLast":{"lastPlayed":1e30}}]}`,
	}
	for name, doc := range rejected {
		if _, err := ImportNSP([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("%s: want CodeUnsupported, got %v", name, err)
		}
	}

	// The largest representable whole-day window (MaxInt64/nspDayNS days) still
	// imports: the overflow bound is exclusive, not a shrunken range.
	maxDays := int64(9223372036854775807) / nspDayNS
	big, err := ImportNSP([]byte(fmt.Sprintf(`{"all":[{"inTheLast":{"lastPlayed":%d}}]}`, maxDays)))
	if err != nil {
		t.Fatalf("import max-day window (%d days): %v", maxDays, err)
	}
	if c := big.Where.(query.And).Nodes[0].(query.Cond); c.Value != maxDays*nspDayNS {
		t.Errorf("max-day window = %v, want %d", c.Value, maxDays*nspDayNS)
	}

	// Export of a window that is not a whole number of days rejects (the
	// whole-star precedent), as does an absolute operator on a date field.
	partial := query.New(query.EntityItems).Where("last_played", query.OpInTheLast, nspDayNS+1).Build()
	if _, err := ExportNSP(partial); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export partial-day window: want CodeUnsupported, got %v", err)
	}
	abs := query.New(query.EntityItems).Where("last_played", query.OpAfter, int64(1)).Build()
	if _, err := ExportNSP(abs); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export absolute op on date field: want CodeUnsupported, got %v", err)
	}
}

func TestNSPRandomSortAndLimitModes(t *testing.T) {
	// sort "random" (with a limit) maps to the random limit mode, not a Sort.
	q, err := ImportNSP([]byte(`{"all":[{"is":{"artist":"X"}}],"sort":"random","limit":25}`))
	if err != nil {
		t.Fatalf("import sort random: %v", err)
	}
	if q.LimitMode != query.LimitRandom || q.Limit != 25 || len(q.Sorts) != 0 {
		t.Errorf("imported = mode %q limit %d sorts %v, want random/25/none", q.LimitMode, q.Limit, q.Sorts)
	}

	// It round-trips: export renders sort "random" again.
	out, err := ExportNSP(q)
	if err != nil {
		t.Fatalf("export random: %v", err)
	}
	q2, err := ImportNSP(out)
	if err != nil {
		t.Fatalf("re-import random: %v", err)
	}
	if q2.LimitMode != query.LimitRandom || q2.Limit != 25 {
		t.Errorf("re-imported = mode %q limit %d, want random/25", q2.LimitMode, q2.Limit)
	}

	// A random sort with no limit, or a zero/negative one, has no WaxBin
	// representation ("everything, shuffled" is a playback concern; random
	// requires a positive limit) and rejects all-or-nothing instead of importing
	// a query every downstream compile refuses.
	for name, doc := range map[string]string{
		"no limit":       `{"all":[{"is":{"artist":"X"}}],"sort":"random"}`,
		"zero limit":     `{"all":[{"is":{"artist":"X"}}],"sort":"random","limit":0}`,
		"negative limit": `{"all":[{"is":{"artist":"X"}}],"sort":"random","limit":-5}`,
	} {
		if _, err := ImportNSP([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("sort random with %s: want CodeUnsupported, got %v", name, err)
		}
	}

	// Budget modes and a pinned seed have no .nsp representation, and neither
	// does the (compile-invalid) random+sorts hybrid, whose sort would otherwise
	// silently overwrite the shuffle on export.
	budget := query.New(query.EntityItems).Limit(60).LimitBy(query.LimitMinutes).Build()
	if _, err := ExportNSP(budget); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export minutes mode: want CodeUnsupported, got %v", err)
	}
	seeded := query.New(query.EntityItems).Limit(25).LimitBy(query.LimitRandom).Seed(42).Build()
	if _, err := ExportNSP(seeded); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export seeded random: want CodeUnsupported, got %v", err)
	}
	hybrid := query.Query{Entity: query.EntityItems, Limit: 25, LimitMode: query.LimitRandom,
		Sorts: []query.Sort{{Field: "title"}}}
	if _, err := ExportNSP(hybrid); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export random+sorts hybrid: want CodeUnsupported, got %v", err)
	}
}

func TestNSPDateSortFields(t *testing.T) {
	// Navidrome's common "recently added" playlist (sort dateAdded desc) maps to
	// a WaxBin sort over the added time field; lastPlayed sorts the same way.
	q, err := ImportNSP([]byte(`{"all":[{"is":{"artist":"X"}}],"sort":"dateAdded","order":"desc","limit":50}`))
	if err != nil {
		t.Fatalf("import sort dateAdded: %v", err)
	}
	if len(q.Sorts) != 1 || q.Sorts[0].Field != "added" || !q.Sorts[0].Desc {
		t.Errorf("sorts = %+v, want added desc", q.Sorts)
	}
	q2, err := ImportNSP([]byte(`{"all":[{"is":{"artist":"X"}}],"sort":"lastPlayed","order":"asc"}`))
	if err != nil {
		t.Fatalf("import sort lastPlayed: %v", err)
	}
	if len(q2.Sorts) != 1 || q2.Sorts[0].Field != "last_played" || q2.Sorts[0].Desc {
		t.Errorf("sorts = %+v, want last_played asc", q2.Sorts)
	}

	// Round-trip: the date sort exports back and re-imports equivalently.
	out, err := ExportNSP(q)
	if err != nil {
		t.Fatalf("export date sort: %v", err)
	}
	back, err := ImportNSP(out)
	if err != nil {
		t.Fatalf("re-import date sort: %v", err)
	}
	b1, _ := query.MarshalRule(q)
	b2, _ := query.MarshalRule(back)
	if string(b1) != string(b2) {
		t.Errorf("date-sort round-trip diverged:\n q1=%s\n q2=%s", b1, b2)
	}
}

func TestExportNSPRejectsUnsupported(t *testing.T) {
	// isPresent has no .nsp representation.
	q := query.New(query.EntityItems).WherePresence("title", query.OpIsPresent).Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export isPresent: want CodeUnsupported, got %v", err)
	}
	// A field WaxBin has but .nsp does not map (path).
	q = query.New(query.EntityItems).Where("path", query.OpContains, "x").Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export path: want CodeUnsupported, got %v", err)
	}
}
