package query_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

var testFields = query.FieldMap{
	"title":  {Expr: "pi.title", Kind: query.KindText},
	"artist": {Expr: "t.artist", Kind: query.KindText},
	"year":   {Expr: "t.year", Kind: query.KindInt},
	"added":  {Expr: "pi.created_at", Kind: query.KindTime},
}

func TestCompileBasicAnd(t *testing.T) {
	q := query.New(query.EntityItems).
		Where("title", query.OpContains, "drive").
		Where("year", query.OpGte, 2000).
		OrderBy("title", false).
		Limit(10).
		Build()

	c, err := query.Compile(q, testFields)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(c.Where, "pi.title LIKE ?") || !strings.Contains(c.Where, "t.year >= ?") {
		t.Fatalf("unexpected where: %s", c.Where)
	}
	if c.OrderBy != "pi.title ASC" {
		t.Fatalf("order by = %q", c.OrderBy)
	}
	if c.Limit != 10 {
		t.Fatalf("limit = %d", c.Limit)
	}
	want := []any{"%drive%", 2000}
	if !reflect.DeepEqual(c.Args, want) {
		t.Fatalf("args = %#v, want %#v", c.Args, want)
	}
}

func TestCompileRejectsUnknownField(t *testing.T) {
	q := query.New(query.EntityItems).Where("evil; DROP TABLE", query.OpIs, "x").Build()
	_, err := query.Compile(q, testFields)
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for unknown field, got %v", err)
	}
}

func TestCompileEscapesLikeMetacharacters(t *testing.T) {
	q := query.New(query.EntityItems).Where("title", query.OpContains, "50%_off").Build()
	c, err := query.Compile(q, testFields)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got := c.Args[0].(string)
	if got != `%50\%\_off%` {
		t.Fatalf("escaped pattern = %q", got)
	}
	if !strings.Contains(c.Where, `ESCAPE '\'`) {
		t.Fatalf("missing ESCAPE clause: %s", c.Where)
	}
}

func TestCompilePresenceTextVsInt(t *testing.T) {
	c, err := query.Compile(query.New(query.EntityItems).
		WherePresence("title", query.OpIsMissing).Build(), testFields)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.Where, "IS NULL OR pi.title = ''") {
		t.Fatalf("text isMissing should check empty string: %s", c.Where)
	}

	c, err = query.Compile(query.New(query.EntityItems).
		WherePresence("year", query.OpIsPresent).Build(), testFields)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.Where, "= ''") {
		t.Fatalf("int isPresent should not check empty string: %s", c.Where)
	}
}

func TestCompileOrNot(t *testing.T) {
	q := query.New(query.EntityItems).WhereNode(query.Or{Nodes: []query.Node{
		query.Cond{Field: "artist", Op: query.OpIs, Value: "A"},
		query.Not{Node: query.Cond{Field: "year", Op: query.OpLt, Value: 1990}},
	}}).Build()
	c, err := query.Compile(q, testFields)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !strings.Contains(c.Where, " OR ") || !strings.Contains(c.Where, "NOT (") {
		t.Fatalf("unexpected boolean shape: %s", c.Where)
	}
}

func TestRuleRoundTrip(t *testing.T) {
	q := query.New(query.EntityItems).
		Where("title", query.OpContains, "x").
		WhereNode(query.Or{Nodes: []query.Node{
			query.Cond{Field: "year", Op: query.OpGte, Value: float64(2000)},
			query.Not{Node: query.Cond{Field: "artist", Op: query.OpIs, Value: "VA"}},
		}}).
		OrderBy("year", true).
		Limit(5).
		Build()

	data, err := query.MarshalRule(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Identical compiled SQL is the useful equivalence here.
	c1, _ := query.Compile(q, testFields)
	c2, err := query.Compile(got, testFields)
	if err != nil {
		t.Fatalf("compile parsed: %v", err)
	}
	if c1.Where != c2.Where || c1.OrderBy != c2.OrderBy || c1.Limit != c2.Limit {
		t.Fatalf("round-trip mismatch:\n in:  %+v\n out: %+v", c1, c2)
	}
}

func TestRuleInt64Precision(t *testing.T) {
	const ns int64 = 1_700_000_000_123_456_789 // a nanosecond timestamp > 2^53
	doc := fmt.Sprintf(
		`{"version":1,"query":{"entity":"items","where":{"type":"cond","field":"added","op":"after","value":%d}}}`, ns)

	q, err := query.ParseRule([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := query.Compile(q, testFields)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, ok := c.Args[0].(int64)
	if !ok {
		t.Fatalf("bound value decoded as %T, want int64 (float64 would lose precision)", c.Args[0])
	}
	if got != ns {
		t.Fatalf("bound value = %d, want %d", got, ns)
	}
}

func TestParseRuleRejectsFutureVersion(t *testing.T) {
	_, err := query.ParseRule([]byte(`{"version":999,"query":{"entity":"items"}}`))
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for newer version, got %v", err)
	}
}
