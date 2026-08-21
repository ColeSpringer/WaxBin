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

// TestExportNSPRejectsInCond pins the same fail-closed behaviour for set membership:
// Navidrome has no `in`, so the whole document is rejected rather than dropping the
// condition.
func TestExportNSPRejectsInCond(t *testing.T) {
	q := query.New(query.EntityItems).WhereValues("artist", query.OpIn, "A", "B").Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("ExportNSP of an in cond: want CodeUnsupported, got %v", err)
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

// nspExportCases is the shared export table: every query this file expects
// ExportNSP to render or refuse, in one place, so the properties below all run
// over the same set and a case added for one is checked by the others.
func nspExportCases(t *testing.T) map[string]query.Query {
	t.Helper()
	imported := func(doc string) query.Query {
		q, err := ImportNSP([]byte(doc))
		if err != nil {
			t.Fatalf("fixture %s: %v", doc, err)
		}
		return q
	}
	negated := func(c query.Cond) query.Query {
		return query.New(query.EntityItems).WhereNode(query.Not{Node: c}).Build()
	}
	return map[string]query.Query{
		"clean and":            imported(`{"all":[{"is":{"artist":"Radiohead"}},{"contains":{"title":"karma"}}],"sort":"title","order":"desc","limit":50}`),
		"clean nested any":     imported(`{"any":[{"is":{"genre":"Jazz"}},{"all":[{"gt":{"year":2000}},{"notContains":{"album":"live"}}]}]}`),
		"clean user state":     imported(`{"all":[{"gt":{"rating":3}},{"is":{"starred":true}},{"gt":{"playcount":0}}]}`),
		"clean relative dates": imported(`{"all":[{"inTheLast":{"lastPlayed":30}},{"notInTheLast":{"dateAdded":7}}]}`),
		"clean random":         imported(`{"all":[{"is":{"artist":"X"}}],"sort":"random","limit":25}`),
		"clean date sort":      imported(`{"all":[{"is":{"artist":"X"}}],"sort":"dateAdded","order":"desc","limit":50}`),
		"clean range":          imported(`{"any":[{"isNot":{"artist":"X"}},{"inTheRange":{"year":[1990,1999]}},{"startsWith":{"album":"The"}}],"sort":"year","order":"asc","limit":25}`),
		"clean whole star":     query.New(query.EntityItems).Where("rating", query.OpGt, 80).Build(),
		"clean offset":         query.New(query.EntityItems).Where("artist", query.OpIs, "X").Limit(10).Offset(5).Build(),
		"clean empty rule":     query.New(query.EntityItems).Build(),
		"tag field":            query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "happy").Build(),
		"in operator":          query.New(query.EntityItems).WhereValues("artist", query.OpIn, "A", "B").Build(),
		"is present":           query.New(query.EntityItems).WherePresence("title", query.OpIsPresent).Build(),
		"path field":           query.New(query.EntityItems).Where("path", query.OpContains, "x").Build(),
		"fractional star":      query.New(query.EntityItems).Where("rating", query.OpGt, 73).Build(),
		"fractional star range": query.New(query.EntityItems).
			WhereRange("rating", query.OpInRange, 73, 80).Build(),
		"rating contains":        query.New(query.EntityItems).Where("rating", query.OpContains, 60).Build(),
		"rating notContains":     negated(query.Cond{Field: "rating", Op: query.OpContains, Value: 60}),
		"partial day":            query.New(query.EntityItems).Where("last_played", query.OpInTheLast, nspDayNS+1).Build(),
		"absolute date op":       query.New(query.EntityItems).Where("last_played", query.OpAfter, int64(1)).Build(),
		"unsupported negation":   negated(query.Cond{Field: "artist", Op: query.OpIs, Value: "x"}),
		"notContains on date":    negated(query.Cond{Field: "added", Op: query.OpContains, Value: "x"}),
		"notContains bad field":  negated(query.Cond{Field: "path", Op: query.OpContains, Value: "x"}),
		"unsupported sort field": query.New(query.EntityItems).Where("artist", query.OpIs, "X").OrderBy("path", false).Build(),
		"minutes budget":         query.New(query.EntityItems).Limit(60).LimitBy(query.LimitMinutes).Build(),
		"seeded random":          query.New(query.EntityItems).Limit(25).LimitBy(query.LimitRandom).Seed(42).Build(),
		"random with sorts": {Entity: query.EntityItems, Limit: 25, LimitMode: query.LimitRandom,
			Sorts: []query.Sort{{Field: "title"}}},
		"mixed group": query.New(query.EntityItems).Where("artist", query.OpIs, "Radiohead").
			Where("tag.MOOD", query.OpIs, "happy").Build(),
		"emptied nested group": query.New(query.EntityItems).Where("artist", query.OpIs, "X").
			WhereNode(query.Or{Nodes: []query.Node{
				query.Cond{Field: "tag.A", Op: query.OpIs, Value: "1"},
				query.Cond{Field: "tag.B", Op: query.OpIs, Value: "2"},
			}}).Build(),
		"nothing survives": query.New(query.EntityItems).Where("tag.A", query.OpIs, "1").
			Where("tag.B", query.OpIs, "2").Build(),
		"extra sorts": query.New(query.EntityItems).Where("artist", query.OpIs, "X").
			OrderBy("artist", false).OrderBy("year", true).Build(),
		"entity tracks": query.New(query.EntityTracks).Where("artist", query.OpIs, "X").Build(),
		"entity files":  query.New(query.EntityFiles).Where("artist", query.OpIs, "X").Build(),
		"random without limit": query.New(query.EntityItems).Where("artist", query.OpIs, "X").
			LimitBy(query.LimitRandom).Build(),
	}
}

// TestCheckNSPExportOwnsTheAnswer is the guard on the shared walk: whatever
// ExportNSP says about a query, CheckNSPExport's first gap says the same thing,
// and a query it renders has no gaps at all. One table owns the answer, so a
// caller listing the whole refusal can never drift from the sentence the strict
// export returns.
func TestCheckNSPExportOwnsTheAnswer(t *testing.T) {
	for name, q := range nspExportCases(t) {
		rep := CheckNSPExport(q)
		if rep.Direction != NSPDirExport {
			t.Errorf("%s: direction = %q, want export", name, rep.Direction)
		}
		for i, g := range rep.All() {
			if g.Kind == "" || g.Reason == "" {
				t.Errorf("%s: gap %d = %+v, want a kind and a reason", name, i, g)
			}
		}
		_, err := ExportNSP(q)
		switch {
		case err == nil && !rep.OK():
			t.Errorf("%s: ExportNSP rendered it but the report holds %d gaps", name, len(rep.Gaps))
		case err != nil && rep.OK():
			t.Errorf("%s: ExportNSP refused (%v) but the report is OK", name, err)
		case err != nil:
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Errorf("%s: want CodeUnsupported, got %v", name, err)
			}
			if got, want := err.Error(), nspErr(rep.Gaps[0].Reason).Error(); got != want {
				t.Errorf("%s: ExportNSP said %q, the first gap says %q", name, got, want)
			}
		}
	}
}

// TestExportNSPNotContainsOnDateField pins the message fix: the Not arm consults
// the date map before declaring a field unsupported, so a notContains on added
// reports the operator restriction it actually hit rather than a missing field.
func TestExportNSPNotContainsOnDateField(t *testing.T) {
	q := query.New(query.EntityItems).
		WhereNode(query.Not{Node: query.Cond{Field: "added", Op: query.OpContains, Value: "x"}}).Build()
	rep := CheckNSPExport(q)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Kind != NSPGapOperator {
		t.Fatalf("gaps = %+v, want one operator gap", rep.Gaps)
	}
	if !strings.Contains(rep.Gaps[0].Reason, "inTheLast") {
		t.Errorf("reason = %q, want the date-field operator restriction", rep.Gaps[0].Reason)
	}
}

func TestExportNSPPartialPrunes(t *testing.T) {
	// A tag condition pruned out of an all group leaves the rest intact, and the
	// report names what went.
	mixed := query.New(query.EntityItems).Where("artist", query.OpIs, "Radiohead").
		Where("tag.MOOD", query.OpIs, "happy").Build()
	e, err := ExportNSPPartial(mixed)
	if err != nil {
		t.Fatalf("partial export of a mixed group: %v", err)
	}
	if len(e.Report.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", e.Report.Gaps)
	}
	if g := e.Report.Gaps[0]; g.Kind != NSPGapField || g.Field != "tag.MOOD" || g.Path != "/where/nodes/1" {
		t.Errorf("gap = %+v, want a field gap on tag.MOOD at /where/nodes/1", g)
	}
	if got := e.Report.Fields(); len(got) != 1 || got[0] != "tag.MOOD" {
		t.Errorf("Fields() = %v, want [tag.MOOD]", got)
	}
	if and, ok := e.Rule.Where.(query.And); !ok || len(and.Nodes) != 1 {
		t.Errorf("surviving rule = %+v, want an And of 1", e.Rule.Where)
	}
	if !strings.Contains(string(e.Data), "Radiohead") || strings.Contains(string(e.Data), "MOOD") {
		t.Errorf("document kept the wrong half:\n%s", e.Data)
	}

	// A nested any whose members all drop is dropped in turn: leaving it behind
	// would turn a dropped leaf into a rule that matches nothing.
	nested := query.New(query.EntityItems).Where("artist", query.OpIs, "X").
		WhereNode(query.Or{Nodes: []query.Node{
			query.Cond{Field: "tag.A", Op: query.OpIs, Value: "1"},
			query.Cond{Field: "tag.B", Op: query.OpIs, Value: "2"},
		}}).Build()
	e, err = ExportNSPPartial(nested)
	if err != nil {
		t.Fatalf("partial export of a nested group: %v", err)
	}
	if len(e.Report.Gaps) != 3 {
		t.Fatalf("gaps = %+v, want the two conditions and the group they emptied", e.Report.Gaps)
	}
	if g := e.Report.Gaps[2]; g.Kind != NSPGapShape || g.Path != "/where/nodes/1" {
		t.Errorf("last gap = %+v, want a shape gap on the emptied group", g)
	}
	if strings.Contains(string(e.Data), "any") {
		t.Errorf("emptied any group survived into the document:\n%s", e.Data)
	}

	// A rule with nothing left is a rule matching the whole library, which is not
	// a partial export of anything.
	empty := query.New(query.EntityItems).Where("tag.A", query.OpIs, "1").
		Where("tag.B", query.OpIs, "2").Build()
	if _, err := ExportNSPPartial(empty); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("partial export of an all-unmappable rule: want CodeUnsupported, got %v", err)
	}

	// A budget limit takes its value with it: rendering the 60 alone would say
	// sixty tracks where the rule said sixty minutes.
	budget := query.New(query.EntityItems).Where("artist", query.OpIs, "X").
		Limit(60).LimitBy(query.LimitMinutes).Build()
	e, err = ExportNSPPartial(budget)
	if err != nil {
		t.Fatalf("partial export of a minutes budget: %v", err)
	}
	if len(e.Report.Gaps) != 2 || e.Report.Gaps[0].Path != "/limitMode" || e.Report.Gaps[1].Path != "/limit" {
		t.Fatalf("gaps = %+v, want the mode and the limit that rode on it", e.Report.Gaps)
	}
	if e.Rule.LimitMode != query.LimitCount || e.Rule.Limit != 0 {
		t.Errorf("surviving rule = mode %q limit %d, want the budget gone entirely", e.Rule.LimitMode, e.Rule.Limit)
	}
	if strings.Contains(string(e.Data), "limit") {
		t.Errorf("the minutes value rode along as a track count:\n%s", e.Data)
	}

	// A pinned seed drops alone: sort "random" already conveys the shuffle.
	seeded := query.New(query.EntityItems).Where("artist", query.OpIs, "X").
		Limit(25).LimitBy(query.LimitRandom).Seed(42).Build()
	e, err = ExportNSPPartial(seeded)
	if err != nil {
		t.Fatalf("partial export of a seeded random: %v", err)
	}
	if e.Rule.LimitMode != query.LimitRandom || e.Rule.Limit != 25 || e.Rule.LimitSeed != 0 {
		t.Errorf("surviving rule = %+v, want the shuffle kept and the seed dropped", e.Rule)
	}
	if !strings.Contains(string(e.Data), `"sort": "random"`) {
		t.Errorf("document lost the shuffle:\n%s", e.Data)
	}
}

// nspPartialRefusals names the cases whose whole condition tree drops, which is
// the one thing ExportNSPPartial refuses. Naming them rather than tolerating any
// refusal is what keeps the invariant below honest: a regression that made a case
// like "minutes budget" refuse would otherwise skip the case it exists to pin.
func nspPartialRefusals() map[string]bool {
	return map[string]bool{
		"tag field":             true,
		"in operator":           true,
		"is present":            true,
		"path field":            true,
		"fractional star":       true,
		"fractional star range": true,
		"rating contains":       true,
		"rating notContains":    true,
		"partial day":           true,
		"absolute date op":      true,
		"unsupported negation":  true,
		"notContains on date":   true,
		"notContains bad field": true,
		"nothing survives":      true,
	}
}

// TestExportNSPPartialRuleDescribesTheDocument is the invariant that makes the
// lossy path honest: the rule a partial export hands back is exactly the rule
// its document describes, so a caller can show it before writing the file.
func TestExportNSPPartialRuleDescribesTheDocument(t *testing.T) {
	refuses := nspPartialRefusals()
	for name, q := range nspExportCases(t) {
		e, err := ExportNSPPartial(q)
		if (err != nil) != refuses[name] {
			t.Errorf("%s: partial export err = %v, want refusal: %v", name, err, refuses[name])
		}
		if err != nil {
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Errorf("%s: partial refusal = %v, want CodeUnsupported", name, err)
			}
			continue
		}
		again, err := ExportNSP(e.Rule)
		if err != nil {
			t.Errorf("%s: the surviving rule does not export cleanly: %v", name, err)
			continue
		}
		if string(again) != string(e.Data) {
			t.Errorf("%s: rule and document disagree:\n rule renders %s\n partial wrote %s", name, again, e.Data)
		}
	}
}

// TestNSPExportIdempotent is the round-trip property: a document WaxBin exports
// re-imports to a rule that exports to the same document. It is what catches an
// exporter emitting something the importer turns away.
func TestNSPExportIdempotent(t *testing.T) {
	for name, q := range nspExportCases(t) {
		first, err := ExportNSP(q)
		if err != nil {
			if strings.HasPrefix(name, "clean ") {
				t.Errorf("%s: strict export refused a case the table calls clean: %v", name, err)
			}
			continue
		}
		back, err := ImportNSP(first)
		if err != nil {
			t.Errorf("%s: WaxBin exported a document it refuses to import: %v\n%s", name, err, first)
			continue
		}
		second, err := ExportNSP(back)
		if err != nil {
			t.Errorf("%s: re-export of the round-tripped rule refused: %v", name, err)
			continue
		}
		if string(second) != string(first) {
			t.Errorf("%s: round trip diverged:\n first=%s\n second=%s", name, first, second)
		}
	}
}

// TestExportNSPExtraSorts pins the behaviour change: a two-term sort is a real
// stored rule, .nsp has one sort key, and an export that quietly kept the first
// term handed back a differently ordered playlist.
func TestExportNSPExtraSorts(t *testing.T) {
	q := query.New(query.EntityItems).OrderBy("artist", false).OrderBy("year", true).Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("export of a two-term sort: want CodeUnsupported, got %v", err)
	}
	e, err := ExportNSPPartial(q)
	if err != nil {
		t.Fatalf("partial export of a two-term sort: %v", err)
	}
	if len(e.Report.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", e.Report.Gaps)
	}
	if g := e.Report.Gaps[0]; g.Kind != NSPGapSort || g.Path != "/sorts/1" || g.Field != "year" {
		t.Errorf("gap = %+v, want a sort gap on year at /sorts/1", g)
	}
	if len(e.Rule.Sorts) != 1 || e.Rule.Sorts[0].Field != "artist" {
		t.Errorf("surviving sorts = %+v, want artist alone", e.Rule.Sorts)
	}
	if !strings.Contains(string(e.Data), `"sort": "artist"`) {
		t.Errorf("document lost the first sort term:\n%s", e.Data)
	}
	// The extra terms are reported even when the first one is itself unmappable,
	// since the two are separate causes.
	both := query.New(query.EntityItems).OrderBy("path", false).OrderBy("year", true).Build()
	if rep := CheckNSPExport(both); len(rep.Gaps) != 2 {
		t.Errorf("gaps = %+v, want the unmappable field and the extra term", rep.Gaps)
	}
}

// TestExportNSPEntity separates the two entities that would round-trip to
// something else: tracks exports faithfully and comes back wider, files is not a
// playlist of items at all.
func TestExportNSPEntity(t *testing.T) {
	tracks := query.New(query.EntityTracks).Where("artist", query.OpIs, "X").Build()
	if _, err := ExportNSP(tracks); err != nil {
		t.Fatalf("export of a tracks rule: %v", err)
	}
	rep := CheckNSPExport(tracks)
	if !rep.OK() {
		t.Errorf("tracks entity blocked the export: %+v", rep.Gaps)
	}
	if len(rep.Notes) != 1 || rep.Notes[0].Kind != NSPGapEntity || rep.Notes[0].Path != "/entity" {
		t.Errorf("notes = %+v, want one entity note at /entity", rep.Notes)
	}

	files := query.New(query.EntityFiles).Where("artist", query.OpIs, "X").Build()
	if _, err := ExportNSP(files); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("export of a files rule: want CodeUnsupported, got %v", err)
	}
	e, err := ExportNSPPartial(files)
	if err != nil {
		t.Fatalf("partial export of a files rule: %v", err)
	}
	if e.Rule.Entity != query.EntityItems {
		t.Errorf("surviving entity = %q, want items (the entity was what dropped)", e.Rule.Entity)
	}

	// Every other value, the zero one included, stays silent: this boundary
	// reports round-trip drift, it does not validate the entity.
	if rep := CheckNSPExport(query.Query{}); !rep.OK() || len(rep.Notes) != 0 {
		t.Errorf("zero entity reported %+v / %+v, want silence", rep.Gaps, rep.Notes)
	}
}

// TestExportNSPRandomWithoutLimit closes the hole where the strict exporter
// emitted a document the strict importer refuses.
func TestExportNSPRandomWithoutLimit(t *testing.T) {
	q := query.New(query.EntityItems).LimitBy(query.LimitRandom).Build()
	if _, err := ExportNSP(q); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Fatalf("export of random with no limit: want CodeUnsupported, got %v", err)
	}
	e, err := ExportNSPPartial(q)
	if err != nil {
		t.Fatalf("partial export of random with no limit: %v", err)
	}
	if e.Rule.LimitMode != query.LimitCount {
		t.Errorf("surviving mode = %q, want the shuffle dropped", e.Rule.LimitMode)
	}
	if strings.Contains(string(e.Data), "random") {
		t.Errorf("document kept a shuffle ImportNSP refuses:\n%s", e.Data)
	}
}

// nspImportCases is the shared import table, the mirror of nspExportCases: every
// document this file expects ImportNSP to read or refuse.
func nspImportCases() map[string]string {
	return map[string]string{
		"clean and":            `{"all":[{"is":{"artist":"Radiohead"}},{"contains":{"title":"karma"}}],"sort":"title","order":"desc","limit":50}`,
		"clean nested any":     `{"any":[{"is":{"genre":"Jazz"}},{"all":[{"gt":{"year":2000}},{"notContains":{"album":"live"}}]}]}`,
		"clean user state":     `{"all":[{"gt":{"rating":3}},{"is":{"starred":true}},{"gt":{"playcount":0}}]}`,
		"clean relative dates": `{"all":[{"inTheLast":{"lastPlayed":30}},{"notInTheLast":{"dateAdded":7}}]}`,
		"clean random":         `{"all":[{"is":{"artist":"X"}}],"sort":"random","limit":25}`,
		"clean date sort":      `{"all":[{"is":{"artist":"X"}}],"sort":"dateAdded","order":"desc","limit":50}`,
		"clean metadata":       `{"name":"My Mix","comment":"road trip","all":[{"is":{"artist":"Radiohead"}}]}`,
		"clean empty group":    `{"all":[]}`,
		"unsupported field":    `{"all":[{"is":{"comment":"x"}}]}`,
		"unknown operator":     `{"all":[{"inPlaylist":{"title":"x"}}]}`,
		"relative on non-date": `{"all":[{"inTheLast":{"year":30}}]}`,
		"absolute date op":     `{"all":[{"before":{"lastPlayed":"2023-01-01"}}]}`,
		"fractional days":      `{"all":[{"inTheLast":{"lastPlayed":1.5}}]}`,
		"absurd days":          `{"all":[{"inTheLast":{"lastPlayed":1e30}}]}`,
		"absurd negative days": `{"all":[{"inTheLast":{"lastPlayed":-1e30}}]}`,
		"rating contains":      `{"all":[{"contains":{"rating":3}}]}`,
		"rating notContains":   `{"all":[{"notContains":{"rating":3}}]}`,
		"unsupported sort":     `{"all":[{"is":{"title":"x"}}],"sort":"comment"}`,
		"unsupported key":      `{"limitPercent":50,"all":[{"is":{"artist":"X"}}]}`,
		"random no limit":      `{"all":[{"is":{"artist":"X"}}],"sort":"random"}`,
		"no root":              `{"limit":10}`,
		"two rules in one":     `{"all":[{"is":{"artist":"x","album":"y"}}]}`,
		"bare leaf document":   `{"is":{"artist":"x","album":"y"}}`,
		"bad limit":            `{"all":[{"is":{"title":"x"}}],"limit":"notanumber"}`,
		"bad offset":           `{"all":[{"is":{"title":"x"}}],"offset":"notanumber"}`,
		"bad order":            `{"all":[{"is":{"title":"x"}}],"sort":"title","order":123}`,
		"bad rating value":     `{"all":[{"is":{"rating":"good"}}]}`,
		"group not an array":   `{"all":{"is":{"artist":"x"}}}`,
		"multiple roots":       `{"all":[{"is":{"artist":"x"}}],"any":[{"is":{"genre":"Jazz"}}]}`,
		"all unmappable":       `{"all":[{"is":{"comment":"x"}},{"is":{"bitrate":320}}]}`,
	}
}

// TestCheckNSPImportOwnsTheAnswer is the import half of the one-table guard:
// whatever ImportNSP says about a document, CheckNSPImport's first gap says the
// same thing, and a document it reads has no gaps at all.
func TestCheckNSPImportOwnsTheAnswer(t *testing.T) {
	for name, doc := range nspImportCases() {
		rep, cerr := CheckNSPImport([]byte(doc))
		if cerr != nil {
			t.Errorf("%s: check failed on a parseable document: %v", name, cerr)
			continue
		}
		if rep.Direction != NSPDirImport {
			t.Errorf("%s: direction = %q, want import", name, rep.Direction)
		}
		_, err := ImportNSP([]byte(doc))
		switch {
		case err == nil && !rep.OK():
			t.Errorf("%s: ImportNSP read it but the report holds %d gaps", name, len(rep.Gaps))
		case err != nil && rep.OK():
			t.Errorf("%s: ImportNSP refused (%v) but the report is OK", name, err)
		case err != nil:
			if !waxerr.Is(err, waxerr.CodeUnsupported) {
				t.Errorf("%s: want CodeUnsupported, got %v", name, err)
			}
			if got, want := err.Error(), nspErr(rep.Gaps[0].Reason).Error(); got != want {
				t.Errorf("%s: ImportNSP said %q, the first gap says %q", name, got, want)
			}
		}
	}

	// Unparseable JSON is the one failure that is not a gap.
	rep, err := CheckNSPImport([]byte(`{not json`))
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("malformed JSON: want CodeInvalid, got %v", err)
	}
	if len(rep.All()) != 0 {
		t.Errorf("malformed JSON reported %+v, want no gaps", rep.All())
	}
}

func TestImportNSPPartialPrunes(t *testing.T) {
	// An unmappable leaf is pruned out of the group and named, in the document's
	// own vocabulary and at its own pointer.
	imp, err := ImportNSPPartial([]byte(`{"all":[{"is":{"artist":"Radiohead"}},{"is":{"comment":"x"}}]}`))
	if err != nil {
		t.Fatalf("partial import: %v", err)
	}
	if len(imp.Report.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want one", imp.Report.Gaps)
	}
	if g := imp.Report.Gaps[0]; g.Kind != NSPGapField || g.Field != "comment" || g.Path != "/all/1" {
		t.Errorf("gap = %+v, want a field gap on comment at /all/1", g)
	}
	and, ok := imp.Rule.Where.(query.And)
	if !ok || len(and.Nodes) != 1 {
		t.Fatalf("rule = %+v, want an And of 1", imp.Rule.Where)
	}

	// A nested group whose members all drop is dropped in turn.
	imp, err = ImportNSPPartial([]byte(
		`{"all":[{"is":{"artist":"X"}},{"any":[{"is":{"comment":"a"}},{"is":{"bitrate":320}}]}]}`))
	if err != nil {
		t.Fatalf("partial import of a nested group: %v", err)
	}
	if len(imp.Report.Gaps) != 3 {
		t.Fatalf("gaps = %+v, want the two leaves and the group they emptied", imp.Report.Gaps)
	}
	if g := imp.Report.Gaps[2]; g.Kind != NSPGapShape || g.Path != "/all/1/any" {
		t.Errorf("last gap = %+v, want a shape gap on the emptied group", g)
	}
	if and, ok := imp.Rule.Where.(query.And); !ok || len(and.Nodes) != 1 {
		t.Errorf("rule = %+v, want the emptied group gone", imp.Rule.Where)
	}

	// A top-level key WaxBin cannot represent is a real gap, so a partial import
	// may drop it, and the rule it built still holds the rest.
	imp, err = ImportNSPPartial([]byte(`{"limitPercent":50,"all":[{"is":{"artist":"X"}}]}`))
	if err != nil {
		t.Fatalf("partial import of limitPercent: %v", err)
	}
	if len(imp.Report.Gaps) != 1 || imp.Report.Gaps[0].Path != "/limitPercent" {
		t.Errorf("gaps = %+v, want one at /limitPercent", imp.Report.Gaps)
	}

	// A random sort with no limit drops the shuffle rather than the rule.
	imp, err = ImportNSPPartial([]byte(`{"all":[{"is":{"artist":"X"}}],"sort":"random"}`))
	if err != nil {
		t.Fatalf("partial import of an unlimited shuffle: %v", err)
	}
	if imp.Rule.LimitMode != query.LimitCount || len(imp.Rule.Sorts) != 0 {
		t.Errorf("rule = %+v, want the shuffle dropped and nothing put in its place", imp.Rule)
	}

	// A document with nothing left is a document matching the whole library.
	if _, err := ImportNSPPartial([]byte(`{"all":[{"is":{"comment":"x"}}]}`)); !waxerr.Is(err, waxerr.CodeUnsupported) {
		t.Errorf("partial import of an all-unmappable document: want CodeUnsupported, got %v", err)
	}
}

// TestImportNSPPartialRefusesMalformed pins the asymmetry with the export side:
// a broken document is not an unmappable one, and pruning it would turn a rule
// the person wrote into one nobody can see is missing.
func TestImportNSPPartialRefusesMalformed(t *testing.T) {
	broken := map[string]string{
		"two fields in one rule": `{"all":[{"is":{"artist":"x","album":"y"}}]}`,
		"bare leaf document":     `{"is":{"artist":"x","album":"y"}}`,
		"limit with no root":     `{"limit":10}`,
		"group not an array":     `{"all":{"is":{"artist":"x"}}}`,
		"bad limit":              `{"all":[{"is":{"title":"x"}}],"limit":"notanumber"}`,
	}
	for name, doc := range broken {
		if _, err := ImportNSPPartial([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("%s: want CodeUnsupported, got %v", name, err)
		}
		rep, err := CheckNSPImport([]byte(doc))
		if err != nil {
			t.Fatalf("%s: check: %v", name, err)
		}
		found := false
		for _, g := range rep.Gaps {
			if g.Kind == NSPGapMalformed {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: gaps = %+v, want one marked malformed", name, rep.Gaps)
		}
	}
}

// TestNSPRatingSubstringOpsRejected pins the one place the rating scale bridge
// cannot be applied. Every other operator on rating converts between 0-to-5 and
// 0-to-100, but a substring match does not survive a numeric conversion, so
// scaling it would silently change which ratings match and not scaling it would
// compare a 0-to-5 value against a 0-to-100 column. Both directions refuse.
func TestNSPRatingSubstringOpsRejected(t *testing.T) {
	for _, op := range []query.Op{query.OpContains, query.OpStartsWith, query.OpEndsWith} {
		q := query.New(query.EntityItems).Where("rating", op, 60).Build()
		rep := CheckNSPExport(q)
		if len(rep.Gaps) != 1 || rep.Gaps[0].Kind != NSPGapOperator || rep.Gaps[0].Field != "rating" {
			t.Errorf("export %s on rating: gaps = %+v, want one operator gap", op, rep.Gaps)
		}
	}
	neg := query.New(query.EntityItems).
		WhereNode(query.Not{Node: query.Cond{Field: "rating", Op: query.OpContains, Value: 60}}).Build()
	if rep := CheckNSPExport(neg); len(rep.Gaps) != 1 || rep.Gaps[0].Kind != NSPGapOperator {
		t.Errorf("export notContains on rating: gaps = %+v, want one operator gap", rep.Gaps)
	}

	for _, doc := range []string{
		`{"all":[{"contains":{"rating":3}}]}`,
		`{"all":[{"startsWith":{"rating":3}}]}`,
		`{"all":[{"endsWith":{"rating":3}}]}`,
		`{"all":[{"notContains":{"rating":3}}]}`,
	} {
		if _, err := ImportNSP([]byte(doc)); !waxerr.Is(err, waxerr.CodeUnsupported) {
			t.Errorf("import %s: want CodeUnsupported, got %v", doc, err)
		}
		rep, err := CheckNSPImport([]byte(doc))
		if err != nil {
			t.Fatalf("check %s: %v", doc, err)
		}
		// The leaf is the only member of its group, so the group it empties is
		// reported after it; the leaf's own gap is what this checks.
		if len(rep.Gaps) == 0 || rep.Gaps[0].Kind != NSPGapOperator || rep.Gaps[0].Field != "rating" {
			t.Errorf("import %s: gaps = %+v, want an operator gap on rating first", doc, rep.Gaps)
		}
	}

	// The numeric operators still scale, so this is a rule about substring
	// matching and not about the rating field.
	if rep := CheckNSPExport(query.New(query.EntityItems).Where("rating", query.OpGt, 60).Build()); !rep.OK() {
		t.Errorf("gt on rating stopped mapping: %+v", rep.Gaps)
	}
}

// TestExportNSPModeAndSeedAreSeparateGaps pins that the two drop independently:
// one sentence covering both claimed the mode was unrepresentable on a document
// that kept it.
func TestExportNSPModeAndSeedAreSeparateGaps(t *testing.T) {
	both := query.New(query.EntityItems).Limit(60).LimitBy(query.LimitMinutes).Seed(7).Build()
	rep := CheckNSPExport(both)
	if len(rep.Gaps) != 3 {
		t.Fatalf("gaps = %+v, want the mode, the seed and the limit that rode on the mode", rep.Gaps)
	}
	if rep.Gaps[0].Path != "/limitMode" || rep.Gaps[1].Path != "/limitSeed" || rep.Gaps[2].Path != "/limit" {
		t.Errorf("gap paths = %q/%q/%q, want /limitMode /limitSeed /limit",
			rep.Gaps[0].Path, rep.Gaps[1].Path, rep.Gaps[2].Path)
	}

	// A seeded shuffle keeps the mode, so its one gap must talk about the seed
	// alone: the document it produces still says sort "random".
	seeded := query.New(query.EntityItems).Limit(25).LimitBy(query.LimitRandom).Seed(42).Build()
	rep = CheckNSPExport(seeded)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Path != "/limitSeed" {
		t.Fatalf("gaps = %+v, want the seed alone", rep.Gaps)
	}
	if strings.Contains(rep.Gaps[0].Reason, "mode") {
		t.Errorf("reason = %q, but the mode survives into the document", rep.Gaps[0].Reason)
	}
}

// TestNSPGapPathEscapesDocumentKeys covers the one pointer segment that is not
// ours: a top-level key the document supplied. Unescaped, a key holding a "/"
// reads as two segments and a rule editor following Path lands somewhere else.
func TestNSPGapPathEscapesDocumentKeys(t *testing.T) {
	rep, err := CheckNSPImport([]byte(`{"all":[{"is":{"artist":"x"}}],"limit/percent":50,"a~b":1}`))
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	got := make(map[string]bool, len(rep.Gaps))
	for _, g := range rep.Gaps {
		got[g.Path] = true
	}
	for _, want := range []string{"/limit~1percent", "/a~0b"} {
		if !got[want] {
			t.Errorf("paths = %v, want one at %q", got, want)
		}
	}
}
