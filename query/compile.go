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
}

// FieldMap whitelists the logical fields a query may reference for one entity.
// A field absent from the map is rejected at compile time, keeping untrusted
// field names out of SQL.
type FieldMap map[string]Column

// Compiled is the SQL fragment set produced from a Query, ready for a caller to
// splice into a full statement with bound Args.
type Compiled struct {
	Where   string // boolean expression, or "" for "match all"
	Args    []any  // positional bind values for Where
	OrderBy string // comma-separated ordering, or ""
	Limit   int    // 0 == none
	Offset  int    // 0 == none
}

const likeEscape = '\\'

// Compile lowers a Query to parameterized SQL fragments against fm.
func Compile(q Query, fm FieldMap) (*Compiled, error) {
	c := &Compiled{Limit: q.Limit, Offset: q.Offset}

	if q.Where != nil {
		var sb strings.Builder
		if err := compileNode(q.Where, fm, &sb, &c.Args); err != nil {
			return nil, err
		}
		c.Where = sb.String()
	}

	if len(q.Sorts) > 0 {
		terms := make([]string, 0, len(q.Sorts))
		for _, s := range q.Sorts {
			col, ok := fm[s.Field]
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, "query.Compile",
					fmt.Sprintf("unknown sort field %q", s.Field))
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

func compileNode(n Node, fm FieldMap, sb *strings.Builder, args *[]any) error {
	switch v := n.(type) {
	case Cond:
		return compileCond(v, fm, sb, args)
	case And:
		return compileGroup(v.Nodes, "AND", "1=1", fm, sb, args)
	case Or:
		return compileGroup(v.Nodes, "OR", "1=0", fm, sb, args)
	case Not:
		if v.Node == nil {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile", "NOT with no child")
		}
		sb.WriteString("NOT (")
		if err := compileNode(v.Node, fm, sb, args); err != nil {
			return err
		}
		sb.WriteString(")")
		return nil
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unsupported node type %T", n))
	}
}

func compileGroup(nodes []Node, joiner, empty string, fm FieldMap, sb *strings.Builder, args *[]any) error {
	if len(nodes) == 0 {
		sb.WriteString(empty) // empty AND => always true; empty OR => always false
		return nil
	}
	sb.WriteString("(")
	for i, child := range nodes {
		if i > 0 {
			sb.WriteString(" " + joiner + " ")
		}
		if err := compileNode(child, fm, sb, args); err != nil {
			return err
		}
	}
	sb.WriteString(")")
	return nil
}

func compileCond(c Cond, fm FieldMap, sb *strings.Builder, args *[]any) error {
	col, ok := fm[c.Field]
	if !ok {
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unknown field %q", c.Field))
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
