package query_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// tagResolver mirrors the store's tag-aware Fields resolver for storage-agnostic
// compiler tests: it resolves the static fields plus tag.<KEY> to a correlated EXISTS
// over item_tag, binding the (uppercased) key rather than inlining it. The store's real
// resolver additionally canonicalizes and rejects reserved keys; those checks are
// exercised in the store package.
type tagResolver struct{ static query.FieldMap }

func (r tagResolver) Column(field string) (query.Column, bool) {
	if c, ok := r.static[field]; ok {
		return c, true
	}
	const p = "tag."
	if len(field) > len(p) && field[:len(p)] == p {
		return query.Column{Set: &query.SetColumn{
			Sub:       "SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ?",
			ValueExpr: "itq.value",
			Args:      []any{strings.ToUpper(field[len(p):])},
		}}, true
	}
	return query.Column{}, false
}

func compileTag(t *testing.T, q query.Query) *query.Compiled {
	t.Helper()
	c, err := query.Compile(q, tagResolver{static: testFields})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func TestCompileTagEquality(t *testing.T) {
	c := compileTag(t, query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "happy").Build())
	want := "EXISTS (SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ? AND itq.value = ?)"
	if c.Where != want {
		t.Fatalf("where = %q\nwant %q", c.Where, want)
	}
	if !reflect.DeepEqual(c.Args, []any{"MOOD", "happy"}) {
		t.Fatalf("args = %#v, want [MOOD happy] (key first)", c.Args)
	}
}

func TestCompileTagIsNotIsComplementOfIs(t *testing.T) {
	isNot := compileTag(t, query.New(query.EntityItems).Where("tag.MOOD", query.OpIsNot, "happy").Build())
	if !strings.HasPrefix(isNot.Where, "NOT EXISTS (") {
		t.Fatalf("isNot should be a negated EXISTS: %s", isNot.Where)
	}
	// tag.X isNot V is exactly NOT (tag.X is V): same subquery, same args, negated.
	notIs := compileTag(t, query.New(query.EntityItems).
		WhereNode(query.Not{Node: query.Cond{Field: "tag.MOOD", Op: query.OpIs, Value: "happy"}}).Build())
	if !reflect.DeepEqual(isNot.Args, notIs.Args) {
		t.Fatalf("isNot args %#v differ from NOT(is) args %#v", isNot.Args, notIs.Args)
	}
	if !strings.Contains(notIs.Where, "NOT (EXISTS (") {
		t.Fatalf("NOT(is) should wrap the EXISTS: %s", notIs.Where)
	}
}

func TestCompileTagPresence(t *testing.T) {
	present := compileTag(t, query.New(query.EntityItems).WherePresence("tag.MOOD", query.OpIsPresent).Build())
	if !strings.Contains(present.Where, "AND itq.value <> ''") || !strings.HasPrefix(present.Where, "EXISTS (") {
		t.Fatalf("isPresent must guard on a non-empty value: %s", present.Where)
	}
	if !reflect.DeepEqual(present.Args, []any{"MOOD"}) {
		t.Fatalf("isPresent args = %#v, want [MOOD]", present.Args)
	}
	missing := compileTag(t, query.New(query.EntityItems).WherePresence("tag.MOOD", query.OpIsMissing).Build())
	if !strings.HasPrefix(missing.Where, "NOT EXISTS (") || !strings.Contains(missing.Where, "AND itq.value <> ''") {
		t.Fatalf("isMissing must be a negated non-empty EXISTS: %s", missing.Where)
	}
}

func TestCompileTagContainsEscapesAndWraps(t *testing.T) {
	c := compileTag(t, query.New(query.EntityItems).Where("tag.MOOD", query.OpContains, "50%").Build())
	if !strings.Contains(c.Where, "itq.value LIKE ? ESCAPE '\\'") {
		t.Fatalf("contains should reuse the scalar LIKE expr: %s", c.Where)
	}
	// Key first, then the wildcard-wrapped, LIKE-escaped pattern.
	if !reflect.DeepEqual(c.Args, []any{"MOOD", `%50\%%`}) {
		t.Fatalf("args = %#v, want [MOOD %%50\\%%%%]", c.Args)
	}
}

func TestCompileTagTwoCondsIndependentScopes(t *testing.T) {
	c := compileTag(t, query.New(query.EntityItems).
		Where("tag.MOOD", query.OpIs, "happy").
		Where("tag.TEMPO", query.OpIs, "fast").Build())
	if n := strings.Count(c.Where, "EXISTS ("); n != 2 {
		t.Fatalf("want two independent EXISTS subqueries, got %d in %s", n, c.Where)
	}
	// Each EXISTS is its own correlated subquery (its itq alias is scoped to it), so the
	// two conds do not interfere; args interleave key-then-value at each cond's position.
	if !reflect.DeepEqual(c.Args, []any{"MOOD", "happy", "TEMPO", "fast"}) {
		t.Fatalf("args = %#v, want [MOOD happy TEMPO fast]", c.Args)
	}
}

func TestCompileTagRejectsOrderedOperators(t *testing.T) {
	for _, q := range []query.Query{
		query.New(query.EntityItems).Where("tag.YEAR", query.OpGt, "2000").Build(),
		query.New(query.EntityItems).WhereRange("tag.YEAR", query.OpInRange, "1", "9").Build(),
	} {
		if _, err := query.Compile(q, tagResolver{static: testFields}); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Fatalf("ordered operator on a tag field should be CodeInvalid, got %v", err)
		}
	}
}

func TestCompileTagSortRejected(t *testing.T) {
	q := query.New(query.EntityItems).OrderBy("tag.MOOD", false).Build()
	if _, err := query.Compile(q, tagResolver{static: testFields}); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("sorting by a tag field should be CodeInvalid, got %v", err)
	}
}

func TestCompileTagKeyIsBoundNeverInlined(t *testing.T) {
	// A key that legally contains SQL metacharacters must never reach the SQL text.
	c := compileTag(t, query.New(query.EntityItems).Where("tag.WEIRD'KEY", query.OpIs, "v").Build())
	if strings.Contains(c.Where, "WEIRD'KEY") {
		t.Fatalf("the tag key must be bound, not inlined into SQL: %s", c.Where)
	}
	if c.Args[0] != "WEIRD'KEY" {
		t.Fatalf("the tag key must be the leading bind arg, got %#v", c.Args)
	}
}

func TestRuleRoundTripTagCond(t *testing.T) {
	q := query.New(query.EntityItems).
		Where("tag.MOOD", query.OpIs, "happy").
		Where("tag.TEMPO", query.OpContains, "fast").Build()
	data, err := query.MarshalRule(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	res := tagResolver{static: testFields}
	c1, _ := query.Compile(q, res)
	c2, err := query.Compile(got, res)
	if err != nil {
		t.Fatalf("compile parsed: %v", err)
	}
	if c1.Where != c2.Where || !reflect.DeepEqual(c1.Args, c2.Args) {
		t.Fatalf("tag cond round-trip mismatch:\n in:  %s %#v\n out: %s %#v", c1.Where, c1.Args, c2.Where, c2.Args)
	}
}
