package query_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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

func TestCompileNeedsUser(t *testing.T) {
	// A field map with one per-user column, mirroring the store's user-state fields.
	fm := query.FieldMap{
		"title":      {Expr: "pi.title", Kind: query.KindText},
		"year":       {Expr: "t.year", Kind: query.KindInt},
		"rating":     {Expr: "ps.rating", Kind: query.KindInt, NeedsUser: true},
		"play_count": {Expr: "COALESCE(ps.play_count, 0)", Kind: query.KindInt, NeedsUser: true},
	}

	// A query touching no per-user field must not flag NeedsUser, so the store skips
	// the user lookup and join entirely.
	plain, err := query.Compile(query.New(query.EntityItems).
		Where("title", query.OpContains, "x").Where("year", query.OpGte, 2000).Build(), fm)
	if err != nil {
		t.Fatalf("compile plain: %v", err)
	}
	if plain.NeedsUser {
		t.Error("plain query flagged NeedsUser")
	}

	// A per-user field in the WHERE (any operator, including presence) flags it.
	for _, tc := range []struct {
		name string
		q    query.Query
	}{
		{"where-cond", query.New(query.EntityItems).Where("rating", query.OpGte, 50).Build()},
		{"where-presence", query.New(query.EntityItems).WherePresence("rating", query.OpIsMissing).Build()},
		{"where-nested", query.New(query.EntityItems).WhereNode(query.Or{Nodes: []query.Node{
			query.Cond{Field: "title", Op: query.OpIs, Value: "a"},
			query.Not{Node: query.Cond{Field: "play_count", Op: query.OpGt, Value: 0}},
		}}).Build()},
		{"sort-only", query.New(query.EntityItems).OrderBy("rating", true).Build()},
	} {
		c, err := query.Compile(tc.q, fm)
		if err != nil {
			t.Fatalf("compile %s: %v", tc.name, err)
		}
		if !c.NeedsUser {
			t.Errorf("%s: NeedsUser not set for a per-user field reference", tc.name)
		}
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
		`{"kind":"waxbin.rule","version":1,"payload":{"entity":"items","where":{"type":"cond","field":"added","op":"after","value":%d}}}`, ns)

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

func TestCompileAtRelativeOps(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const window = int64(30) * 24 * 60 * 60 * 1_000_000_000 // 30 days in ns
	cutoff := now.UnixNano() - window

	c, err := query.CompileAt(query.New(query.EntityItems).
		Where("added", query.OpInTheLast, window).Build(), testFields, now)
	if err != nil {
		t.Fatalf("compile inTheLast: %v", err)
	}
	if c.Where != "pi.created_at >= ?" {
		t.Errorf("inTheLast where = %q", c.Where)
	}
	if !reflect.DeepEqual(c.Args, []any{cutoff}) {
		t.Errorf("inTheLast args = %#v, want [%d]", c.Args, cutoff)
	}

	// notInTheLast matches NULL: "not seen in the window" includes never-seen.
	c, err = query.CompileAt(query.New(query.EntityItems).
		Where("added", query.OpNotInTheLast, window).Build(), testFields, now)
	if err != nil {
		t.Fatalf("compile notInTheLast: %v", err)
	}
	if c.Where != "(pi.created_at IS NULL OR pi.created_at < ?)" {
		t.Errorf("notInTheLast where = %q", c.Where)
	}
	if !reflect.DeepEqual(c.Args, []any{cutoff}) {
		t.Errorf("notInTheLast args = %#v, want [%d]", c.Args, cutoff)
	}

	// Compile delegates to CompileAt with a fresh now: the two anchors differ.
	c2, err := query.Compile(query.New(query.EntityItems).
		Where("added", query.OpInTheLast, window).Build(), testFields)
	if err != nil {
		t.Fatalf("compile via Compile: %v", err)
	}
	if got := c2.Args[0].(int64); got <= cutoff {
		t.Errorf("Compile anchor %d not fresher than the pinned one %d", got, cutoff)
	}
}

func TestRelativeOpsKindGateAndBadValues(t *testing.T) {
	now := time.Now()
	cases := map[string]query.Query{
		"text field":       query.New(query.EntityItems).Where("title", query.OpInTheLast, int64(1)).Build(),
		"int field":        query.New(query.EntityItems).Where("year", query.OpNotInTheLast, int64(1)).Build(),
		"zero window":      query.New(query.EntityItems).Where("added", query.OpInTheLast, int64(0)).Build(),
		"negative window":  query.New(query.EntityItems).Where("added", query.OpInTheLast, int64(-5)).Build(),
		"string window":    query.New(query.EntityItems).Where("added", query.OpInTheLast, "30d").Build(),
		"missing window":   query.New(query.EntityItems).Where("added", query.OpInTheLast, nil).Build(),
		"fractional float": query.New(query.EntityItems).Where("added", query.OpNotInTheLast, 1.5).Build(),
	}
	for name, q := range cases {
		if _, err := query.CompileAt(q, testFields, now); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: want CodeInvalid, got %v", name, err)
		}
	}
}

func TestLimitModeValidation(t *testing.T) {
	now := time.Now()
	bad := map[string]query.Query{
		"unknown mode":          query.New(query.EntityItems).Limit(5).LimitBy("bytes").Build(),
		"random without limit":  query.New(query.EntityItems).LimitBy(query.LimitRandom).Build(),
		"random with sorts":     query.New(query.EntityItems).Limit(5).LimitBy(query.LimitRandom).OrderBy("title", false).Build(),
		"minutes without limit": query.New(query.EntityItems).LimitBy(query.LimitMinutes).Build(),
		"seed on count":         query.New(query.EntityItems).Limit(5).Seed(7).Build(),
		"seed + sorts budget":   query.New(query.EntityItems).Limit(5).LimitBy(query.LimitMegabytes).Seed(7).OrderBy("title", false).Build(),
	}
	for name, q := range bad {
		if _, err := query.CompileAt(q, testFields, now); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%s: want CodeInvalid, got %v", name, err)
		}
	}

	good := map[string]query.Query{
		"random":              query.New(query.EntityItems).Limit(25).LimitBy(query.LimitRandom).Build(),
		"random seeded":       query.New(query.EntityItems).Limit(25).LimitBy(query.LimitRandom).Seed(42).Build(),
		"minutes sorted":      query.New(query.EntityItems).Limit(60).LimitBy(query.LimitMinutes).OrderBy("title", false).Build(),
		"megabytes seeded":    query.New(query.EntityItems).Limit(700).LimitBy(query.LimitMegabytes).Seed(9).Build(),
		"plain count + limit": query.New(query.EntityItems).Limit(10).Build(),
	}
	for name, q := range good {
		c, err := query.CompileAt(q, testFields, now)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		// The compiler passes the mode and seed through for the evaluator.
		if c.LimitMode != q.LimitMode || c.LimitSeed != q.LimitSeed {
			t.Errorf("%s: Compiled mode/seed = %q/%d, want %q/%d",
				name, c.LimitMode, c.LimitSeed, q.LimitMode, q.LimitSeed)
		}
	}
}

func TestLimitModeRuleRoundTrip(t *testing.T) {
	q := query.New(query.EntityItems).
		Where("added", query.OpInTheLast, int64(86_400_000_000_000)).
		Limit(25).LimitBy(query.LimitRandom).Seed(42).
		Build()
	data, err := query.MarshalRule(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := query.ParseRule(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.LimitMode != query.LimitRandom || got.LimitSeed != 42 || got.Limit != 25 {
		t.Errorf("round-trip = mode %q seed %d limit %d, want random/42/25",
			got.LimitMode, got.LimitSeed, got.Limit)
	}
	cond, ok := got.Where.(query.Cond)
	if !ok || cond.Op != query.OpInTheLast || cond.Value != int64(86_400_000_000_000) {
		t.Errorf("round-tripped cond = %+v, want inTheLast 1d window", got.Where)
	}

	// "count" is accepted as the explicit spelling of the default mode.
	doc := `{"kind":"waxbin.rule","version":1,"payload":{"entity":"items","limit":5,"limitMode":"count"}}`
	got, err = query.ParseRule([]byte(doc))
	if err != nil {
		t.Fatalf("parse count-mode doc: %v", err)
	}
	if got.LimitMode != query.LimitCount {
		t.Errorf("limitMode = %q, want the normalized default", got.LimitMode)
	}

	// A pre-mode rule document (no limitMode/limitSeed) still parses with the
	// defaults, guarding the additive-fields contract.
	old := `{"kind":"waxbin.rule","version":1,"payload":{"entity":"items","where":{"type":"cond","field":"title","op":"is","value":"x"},"limit":5}}`
	got, err = query.ParseRule([]byte(old))
	if err != nil {
		t.Fatalf("parse old doc: %v", err)
	}
	if got.LimitMode != query.LimitCount || got.LimitSeed != 0 || got.Limit != 5 {
		t.Errorf("old doc = mode %q seed %d limit %d, want defaults + limit 5",
			got.LimitMode, got.LimitSeed, got.Limit)
	}
}

func TestRelativeOpRejectedOnTagField(t *testing.T) {
	// A set-membership (tag) column has no time semantics; the relative ops are
	// rejected there like every other ordered operator.
	fm := fieldsWithTag{}
	q := query.New(query.EntityItems).Where("tag.MOOD", query.OpInTheLast, int64(1)).Build()
	if _, err := query.CompileAt(q, fm, time.Now()); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("inTheLast on a tag field: want CodeInvalid, got %v", err)
	}
}

// fieldsWithTag resolves tag.* to a set column, mirroring the store's resolver shape.
type fieldsWithTag struct{}

func (fieldsWithTag) Column(f string) (query.Column, bool) {
	if strings.HasPrefix(f, "tag.") {
		return query.Column{Set: &query.SetColumn{
			Sub:       "SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ?",
			ValueExpr: "itq.value",
			Args:      []any{strings.TrimPrefix(f, "tag.")},
		}}, true
	}
	c, ok := testFields[f]
	return c, ok
}

func TestParseRuleRejectsFutureVersion(t *testing.T) {
	_, err := query.ParseRule([]byte(`{"kind":"waxbin.rule","version":999,"payload":{"entity":"items"}}`))
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for newer version, got %v", err)
	}
}

func TestParseRuleRejectsWrongKind(t *testing.T) {
	_, err := query.ParseRule([]byte(`{"kind":"something.else","version":1,"payload":{"entity":"items"}}`))
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for a mismatched artifact kind, got %v", err)
	}
}
