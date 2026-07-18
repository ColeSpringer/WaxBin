package query

import (
	"fmt"
	"strings"

	"github.com/colespringer/waxbin/waxerr"
)

// Kind classifies a logical field's value domain so the compiler can pick the
// right SQL semantics (e.g. text presence checks empty-string, LIKE only
// applies to text).
type Kind int

const (
	KindText Kind = iota
	KindInt
	KindReal
	KindTime // stored as an integer (unix ns / ms); ordered comparisons allowed
)

// Column maps a logical field to a SQL expression (a column reference or a safe
// expression composed only of trusted identifiers). The Expr is emitted into
// SQL verbatim, so it must never contain caller-supplied text.
type Column struct {
	Expr string
	Kind Kind
	// NeedsUser marks a column whose Expr references a per-user join alias (a
	// user's play_state). A query touching such a column must be executed with a
	// user bound: the caller splices the join and prepends the user id to the
	// args. The compiler surfaces this via Compiled.NeedsUser rather than
	// resolving a user itself, keeping the query package store-agnostic.
	NeedsUser bool
	// Set, when non-nil, marks a set-membership column whose predicates compile to
	// a correlated EXISTS subquery (see SetColumn) rather than a scalar comparison.
	// Expr and Kind are ignored when Set is non-nil.
	Set *SetColumn
}

// SetColumn describes a set-membership field: one whose value lives in a related
// table (a row per value, keyed back to the item) rather than a column on the item
// row, so a predicate over it compiles to a correlated EXISTS subquery. It is how a
// dynamic tag.<KEY> field is filtered.
//
// SECURITY INVARIANT (load-bearing): Sub is a trusted SQL fragment built only from
// constant identifiers, but its ? placeholders bind values from Args. Args holds the
// canonical tag key, which the tag-key rules legally permit to contain SQL
// metacharacters such as a quote, a semicolon, or "--", so it is never inlined into
// Sub. Every ? placeholder in Sub binds one value from Args in order, and all of them
// must precede ValueExpr's comparison, because the compiler appends Args before the
// operator's value or pattern arg. A resolver that put a trailing placeholder in Sub
// would silently corrupt the arg order.
type SetColumn struct {
	// Sub is a correlated subquery selecting the rows for this item, for example
	// "SELECT 1 FROM item_tag itq WHERE itq.item_id = pi.id AND itq.key = ?".
	Sub string
	// ValueExpr is the value column compared inside Sub, for example "itq.value". It
	// must reference the table alias Sub binds (itq above). The compiler emits the
	// predicate as "<Sub> AND <ValueExpr> <op>", so the two share one scope and a
	// mismatched alias would be a SQL error.
	ValueExpr string
	// Args holds the bind values for Sub's placeholders (the canonical key). It is
	// never inlined into SQL.
	Args []any
}

// Fields resolves a logical field name to its Column. It is the compiler's field
// whitelist: an unresolved field is rejected, keeping untrusted names out of SQL. A
// static FieldMap satisfies it directly; a resolver that also handles dynamic fields
// (e.g. tag.<KEY>) wraps a FieldMap and adds cases, but must stay fail-closed
// (return false for anything it does not recognize).
type Fields interface {
	Column(field string) (Column, bool)
}

// FieldMap whitelists the logical fields a query may reference for one entity.
// A field absent from the map is rejected at compile time, keeping untrusted
// field names out of SQL.
type FieldMap map[string]Column

// Column looks up a static field, satisfying Fields.
func (m FieldMap) Column(f string) (Column, bool) { c, ok := m[f]; return c, ok }

// Compiled is the SQL fragment set produced from a Query, ready for a caller to
// splice into a full statement with bound Args.
type Compiled struct {
	Where   string // boolean expression, or "" for "match all"
	Args    []any  // positional bind values for Where
	OrderBy string // comma-separated ordering, or ""
	Limit   int    // 0 == none
	Offset  int    // 0 == none
	// NeedsUser is set when any field referenced in Where or Sorts carries
	// Column.NeedsUser, telling the caller to splice the per-user join and bind a
	// user id before executing.
	NeedsUser bool
}

const likeEscape = '\\'

// Compile lowers a Query to parameterized SQL fragments against fields.
func Compile(q Query, fields Fields) (*Compiled, error) {
	c := &Compiled{Limit: q.Limit, Offset: q.Offset}

	if q.Where != nil {
		var sb strings.Builder
		if err := compileNode(q.Where, fields, &sb, &c.Args, &c.NeedsUser); err != nil {
			return nil, err
		}
		c.Where = sb.String()
	}

	if len(q.Sorts) > 0 {
		terms := make([]string, 0, len(q.Sorts))
		for _, s := range q.Sorts {
			col, ok := fields.Column(s.Field)
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, "query.Compile",
					fmt.Sprintf("unknown sort field %q", s.Field))
			}
			if col.Set != nil {
				return nil, waxerr.New(waxerr.CodeInvalid, "query.Compile",
					fmt.Sprintf("cannot sort by a tag field %q", s.Field))
			}
			if col.NeedsUser {
				c.NeedsUser = true
			}
			dir := "ASC"
			if s.Desc {
				dir = "DESC"
			}
			terms = append(terms, col.Expr+" "+dir)
		}
		c.OrderBy = strings.Join(terms, ", ")
	}

	return c, nil
}

func compileNode(n Node, fields Fields, sb *strings.Builder, args *[]any, nu *bool) error {
	switch v := n.(type) {
	case Cond:
		return compileCond(v, fields, sb, args, nu)
	case And:
		return compileGroup(v.Nodes, "AND", "1=1", fields, sb, args, nu)
	case Or:
		return compileGroup(v.Nodes, "OR", "1=0", fields, sb, args, nu)
	case Not:
		if v.Node == nil {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile", "NOT with no child")
		}
		sb.WriteString("NOT (")
		if err := compileNode(v.Node, fields, sb, args, nu); err != nil {
			return err
		}
		sb.WriteString(")")
		return nil
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unsupported node type %T", n))
	}
}

func compileGroup(nodes []Node, joiner, empty string, fields Fields, sb *strings.Builder, args *[]any, nu *bool) error {
	if len(nodes) == 0 {
		sb.WriteString(empty) // empty AND => always true; empty OR => always false
		return nil
	}
	sb.WriteString("(")
	for i, child := range nodes {
		if i > 0 {
			sb.WriteString(" " + joiner + " ")
		}
		if err := compileNode(child, fields, sb, args, nu); err != nil {
			return err
		}
	}
	sb.WriteString(")")
	return nil
}

func compileCond(c Cond, fields Fields, sb *strings.Builder, args *[]any, nu *bool) error {
	col, ok := fields.Column(c.Field)
	if !ok {
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unknown field %q", c.Field))
	}
	// Any reference to a per-user column (in any operator, including presence
	// checks) means the statement needs the user join and bind.
	if col.NeedsUser {
		*nu = true
	}
	// A set-membership column (a tag.<KEY> field) compiles to a correlated EXISTS
	// subquery instead of a scalar comparison; its bind order is security-sensitive
	// (see SetColumn), so it lives in its own helper.
	if col.Set != nil {
		return compileSetCond(c, col.Set, sb, args)
	}

	switch c.Op {
	case OpIs:
		sb.WriteString(col.Expr + " = ?")
		*args = append(*args, c.Value)
	case OpIsNot:
		sb.WriteString(col.Expr + " <> ?")
		*args = append(*args, c.Value)
	case OpGt, OpAfter:
		sb.WriteString(col.Expr + " > ?")
		*args = append(*args, c.Value)
	case OpLt, OpBefore:
		sb.WriteString(col.Expr + " < ?")
		*args = append(*args, c.Value)
	case OpGte:
		sb.WriteString(col.Expr + " >= ?")
		*args = append(*args, c.Value)
	case OpLte:
		sb.WriteString(col.Expr + " <= ?")
		*args = append(*args, c.Value)
	case OpContains:
		sb.WriteString(likeExpr(col.Expr))
		*args = append(*args, "%"+likePattern(c.Value)+"%")
	case OpStartsWith:
		sb.WriteString(likeExpr(col.Expr))
		*args = append(*args, likePattern(c.Value)+"%")
	case OpEndsWith:
		sb.WriteString(likeExpr(col.Expr))
		*args = append(*args, "%"+likePattern(c.Value))
	case OpInRange:
		if len(c.Values) != 2 {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile",
				fmt.Sprintf("inTheRange on %q needs exactly 2 values", c.Field))
		}
		sb.WriteString(col.Expr + " BETWEEN ? AND ?")
		*args = append(*args, c.Values[0], c.Values[1])
	case OpIsPresent:
		if col.Kind == KindText {
			sb.WriteString("(" + col.Expr + " IS NOT NULL AND " + col.Expr + " <> '')")
		} else {
			sb.WriteString(col.Expr + " IS NOT NULL")
		}
	case OpIsMissing:
		if col.Kind == KindText {
			sb.WriteString("(" + col.Expr + " IS NULL OR " + col.Expr + " = '')")
		} else {
			sb.WriteString(col.Expr + " IS NULL")
		}
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unsupported operator %q", c.Op))
	}
	return nil
}

// compileSetCond compiles a condition against a set-membership column (a tag.<KEY>
// field) to a correlated EXISTS or NOT EXISTS over its related table. Set.Args (the
// canonical key, always bound) precede any operator value or pattern arg, matching the
// placeholder order in Set.Sub then ValueExpr. See the SetColumn security invariant.
//
// isNot is the complement of is (NOT EXISTS), so `tag.X isNot V` means "the item does
// not carry value V for key X". An item with no such tag matches, which diverges from
// scalar isNot (which drops NULLs) and is exactly the deny-list contract. Ordered
// operators (gt, lt, before, and the rest) are rejected because tag values are
// unordered TEXT.
func compileSetCond(c Cond, set *SetColumn, sb *strings.Builder, args *[]any) error {
	// presence guards on a non-empty value so isPresent/isMissing mean the same thing
	// the scalar text fields do (their presence check is `<> ''`).
	const nonEmpty = " <> ''"
	switch c.Op {
	case OpIsPresent:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + set.ValueExpr + nonEmpty + ")")
		*args = append(*args, set.Args...)
	case OpIsMissing:
		sb.WriteString("NOT EXISTS (" + set.Sub + " AND " + set.ValueExpr + nonEmpty + ")")
		*args = append(*args, set.Args...)
	case OpIs:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + set.ValueExpr + " = ?)")
		*args = append(*args, set.Args...)
		*args = append(*args, c.Value)
	case OpIsNot:
		sb.WriteString("NOT EXISTS (" + set.Sub + " AND " + set.ValueExpr + " = ?)")
		*args = append(*args, set.Args...)
		*args = append(*args, c.Value)
	case OpContains:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + likeExpr(set.ValueExpr) + ")")
		*args = append(*args, set.Args...)
		*args = append(*args, "%"+likePattern(c.Value)+"%")
	case OpStartsWith:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + likeExpr(set.ValueExpr) + ")")
		*args = append(*args, set.Args...)
		*args = append(*args, likePattern(c.Value)+"%")
	case OpEndsWith:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + likeExpr(set.ValueExpr) + ")")
		*args = append(*args, set.Args...)
		*args = append(*args, "%"+likePattern(c.Value))
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("operator %q not supported on a tag field", c.Op))
	}
	return nil
}

func likeExpr(expr string) string {
	return expr + " LIKE ? ESCAPE '" + string(likeEscape) + "'"
}

// likePattern stringifies a value and escapes LIKE metacharacters so a literal
// % or _ in user input matches itself.
func likePattern(v any) string {
	s := fmt.Sprint(v)
	r := strings.NewReplacer(
		string(likeEscape), string(likeEscape)+string(likeEscape),
		"%", string(likeEscape)+"%",
		"_", string(likeEscape)+"_",
	)
	return r.Replace(s)
}
