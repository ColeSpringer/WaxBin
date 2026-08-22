package sqlite

import (
	"sort"
	"testing"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playlist"
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
