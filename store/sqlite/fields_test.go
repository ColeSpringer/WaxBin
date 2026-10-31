package sqlite

import (
	"reflect"
	"sort"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playlist"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// TestNoUndeclaredFieldAliases is the guard on the alias relation. Two itemFields
// entries sharing an Expr are one column under two names as far as the compiler is
// concerned, and every surface that reads a stored field name back out (the .nsp
// exporter today) resolves through model.QueryFieldAliases to know that. A fifth pair
// added straight to the map would evaluate fine and export as an unsupported field, so
// it fails here instead.
//
// Set fields are skipped: their Expr is empty by contract, so genre_pid,
// credit_artist_pid, and playlist_pid would otherwise read as one group.
func TestNoUndeclaredFieldAliases(t *testing.T) {
	t.Parallel()
	byExpr := map[string][]string{}
	for name, col := range itemFields {
		if col.Set != nil {
			continue
		}
		byExpr[col.Expr] = append(byExpr[col.Expr], name)
	}
	for expr, names := range byExpr {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		canon := ""
		for _, n := range names {
			if model.CanonicalQueryField(n) == n {
				canon = n
			}
		}
		if canon == "" {
			t.Errorf("fields %v share the expression %q with no canonical spelling among them", names, expr)
			continue
		}
		for _, n := range names {
			if itemFields[n] != itemFields[canon] {
				t.Errorf("%q shares an expression with %q but not the rest of its column: %+v vs %+v",
					n, canon, itemFields[n], itemFields[canon])
			}
		}
		want := model.QueryFieldSpellings(canon)
		sort.Strings(want)
		if len(want) != len(names) {
			t.Errorf("fields %v share the expression %q; model.QueryFieldAliases declares %v", names, expr, want)
			continue
		}
		for i := range want {
			if want[i] != names[i] {
				t.Errorf("fields %v share the expression %q; model.QueryFieldAliases declares %v", names, expr, want)
				break
			}
		}
	}
}

// TestDeclaredAliasesResolve pins the other direction: every declared alias and every
// canonical spelling is a real query field, and the pair compiles to the same column.
// A rename on one side of the map alone would leave a rule holding a field the engine
// no longer knows.
func TestDeclaredAliasesResolve(t *testing.T) {
	t.Parallel()
	for alias, canon := range model.QueryFieldAliases() {
		a, ok := itemFields[alias]
		if !ok {
			t.Errorf("declared alias %q is not a query field", alias)
			continue
		}
		c, ok := itemFields[canon]
		if !ok {
			t.Errorf("canonical spelling %q of alias %q is not a query field", canon, alias)
			continue
		}
		// The whole Column, not just Expr and Kind: a hand-written alias that copied
		// those two and dropped NeedsUser would compile without the play_state join and
		// read an unbound alias, and starred is both a user-state column and an .nsp
		// field, so the shape is reachable.
		if a != c {
			t.Errorf("%q and %q are declared aliases but compile differently: %+v vs %+v", alias, canon, a, c)
		}
	}
}

// TestNSPExportableFieldsAreQueryFields catches an .nsp map naming a field the engine
// dropped or renamed. The exporter would go on refusing rules nobody could have written
// while reading as though it supported them. playlist imports model and query and
// nothing here, so this test-only import is not a cycle.
func TestNSPExportableFieldsAreQueryFields(t *testing.T) {
	t.Parallel()
	fields := playlist.NSPExportableFields()
	if len(fields) == 0 {
		t.Fatal("NSPExportableFields is empty")
	}
	for _, f := range fields {
		if _, ok := itemFields[f]; !ok {
			t.Errorf("nsp exports %q, which is not a query field", f)
		}
	}
}

// TestPerUserFieldsAreExactlyTheNine pins the fields that read the calling user's play
// state and that QueryNeedsUser answers for each field by that flag alone, so a tenth
// per-user field cannot arrive without a host caching rule results hearing of it.
func TestPerUserFieldsAreExactlyTheNine(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	var got []string
	for name, col := range itemFields {
		if col.NeedsUser {
			got = append(got, name)
		}
		needs, err := st.QueryNeedsUser(query.New(query.EntityItems).Where(name, query.OpIsPresent, nil).Build())
		if err != nil {
			t.Errorf("%s isPresent: %v", name, err)
			continue
		}
		if needs != col.NeedsUser {
			t.Errorf("QueryNeedsUser(%s isPresent) = %v, want %v", name, needs, col.NeedsUser)
		}
	}
	sort.Strings(got)
	want := []string{"finished", "last_played", "last_progress", "play_count", "played", "position_ms",
		"rating", "starred", "starred_at"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("per-user fields = %v, want %v", got, want)
	}
}

// TestQueryNeedsUserFollowsSortsToo: a rule that only orders by a per-user field still
// reads per-user state, and one nesting the field under a negation does too.
func TestQueryNeedsUserFollowsSortsToo(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	for _, tc := range []struct {
		name string
		q    query.Query
		want bool
	}{
		{"no rule", query.New(query.EntityItems).Build(), false},
		{"title sort", query.New(query.EntityItems).OrderBy("title", false).Build(), false},
		{"rating sort", query.New(query.EntityItems).OrderBy("rating", true).Build(), true},
		{"negated play count", query.New(query.EntityItems).
			WhereNode(query.Not{Node: query.Cond{Field: "play_count", Op: query.OpGt, Value: 0}}).Build(), true},
		{"tag filter", query.New(query.EntityItems).Where("tag.MOOD", query.OpIs, "calm").Build(), false},
	} {
		got, err := st.QueryNeedsUser(tc.q)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: QueryNeedsUser = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestQueryNeedsUserRefusesAnUnknownField: a rule that does not compile has no answer, so
// it is refused with the compiler's error rather than read as user-agnostic.
func TestQueryNeedsUserRefusesAnUnknownField(t *testing.T) {
	t.Parallel()
	st, _ := entityFixture(t)
	for _, q := range []query.Query{
		query.New(query.EntityItems).Where("bogus", query.OpIs, 1).Build(),
		query.New(query.EntityItems).OrderBy("bogus", false).Build(),
		query.New(query.Entity("albums")).Build(),
	} {
		if _, err := st.QueryNeedsUser(q); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("QueryNeedsUser(%+v) = %v, want CodeInvalid", q, err)
		}
	}
}
