package playlist

import (
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
