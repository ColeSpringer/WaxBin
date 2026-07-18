package playlist

import (
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
	// lastPlayed is a Navidrome date field; WaxBin stores nanoseconds and has no
	// relative-date operator, so it is (deliberately) unsupported, not silently
	// mis-mapped.
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
