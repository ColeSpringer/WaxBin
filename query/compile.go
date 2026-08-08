package query

import (
	"fmt"
	"strings"
	"time"

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
	// ValueSub lowers a bound value before comparison: the compiler emits it in
	// place of the value's ? placeholder, so an identity handle compares an
	// internal id column against a subquery resolving the caller's pid, instead of
	// comparing a per-row correlated subquery against that pid. That is what lets
	// the planner drive the comparison off the id column's index rather than
	// visiting every row, and it evaluates the pid lookup once per query instead of
	// once per row. It must contain exactly one ? and, like Expr, is emitted
	// verbatim, so it must never contain caller-supplied text.
	//
	// Only is, isNot, in, notIn, isPresent, and isMissing are accepted on such a
	// column, and sorting by one is rejected: the rest either carry no value to lower
	// or would compare an internal id by text or by range, which is not a question an
	// identity handle answers, and ORDER BY would sort by internal rowid. The two
	// presence operators emit no value at all, so they compile to a direct
	// IS NULL / IS NOT NULL on the id column.
	//
	// notIn is a deny-list, matching compileSetCond's, and diverges from isNot twice on
	// purpose: a stale pid excludes nothing, and a row with no value is kept. A stored
	// deny-list goes stale one entry at a time, so one bad entry must not empty the
	// result, while isNot names a single subject and keeps its own reading. Prefer it
	// to Not{Cond{field, in, ...}}, which collapses to zero rows on one stale pid.
	ValueSub string
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
	// LimitMode/LimitSeed pass the query's limit interpretation through to the
	// evaluator (the store); the compiler validates the combination (see
	// validateLimitMode) but does not lower it to SQL itself.
	LimitMode LimitMode
	LimitSeed int64
	// NeedsUser is set when any field referenced in Where or Sorts carries
	// Column.NeedsUser, telling the caller to splice the per-user join and bind a
	// user id before executing.
	NeedsUser bool
}

const likeEscape = '\\'

const (
	// maxInValues caps one set-membership condition, matching idBatchSize in
	// store/sqlite/rollups.go. A caller needing more should chunk as ItemsByPIDs does.
	maxInValues = 500
	// maxBindArgs is SQLite's variable ceiling. maxInValues bounds a condition, not a
	// statement, so enough capped conditions still cross it (see CompileAt).
	maxBindArgs = 32766
	// bindReserve is headroom for binds the compiler never sees: the user-join id, a
	// keyset cursor's two, the limit. Without it a query landing on the ceiling
	// passes here and dies at execution anyway.
	bindReserve = 64
)

// Compile lowers a Query to parameterized SQL fragments against fields,
// anchoring the relative-time operators (inTheLast/notInTheLast) at time.Now.
// Every call re-anchors "now", so a stored rule evaluated per read stays fresh.
func Compile(q Query, fields Fields) (*Compiled, error) {
	return CompileAt(q, fields, time.Now())
}

// CompileAt is Compile with an explicit "now" anchor for the relative-time
// operators, so an evaluation can be pinned (tests, snapshots).
func CompileAt(q Query, fields Fields, now time.Time) (*Compiled, error) {
	if err := validateLimitMode(q); err != nil {
		return nil, err
	}
	c := &Compiled{Limit: q.Limit, Offset: q.Offset, LimitMode: q.LimitMode, LimitSeed: q.LimitSeed}

	if q.Where != nil {
		var sb strings.Builder
		if err := compileNode(q.Where, fields, &sb, &c.Args, &c.NeedsUser, now.UnixNano()); err != nil {
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
					fmt.Sprintf("cannot sort by a set field %q", s.Field))
			}
			if col.ValueSub != "" {
				// The Expr of a lowered column is an internal id, so ordering by it
				// would order by rowid: an arbitrary, non-reproducible sequence a
				// caller would read as alphabetical. Sort by the display field instead.
				return nil, waxerr.New(waxerr.CodeInvalid, "query.Compile",
					fmt.Sprintf("cannot sort by an identity field %q", s.Field))
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

	// Checked here because the statement total is only in hand once every condition
	// has compiled; otherwise it surfaces as an opaque driver error.
	if limit := maxBindArgs - bindReserve; len(c.Args) > limit {
		return nil, waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("query binds %d values, over the %d limit", len(c.Args), limit))
	}

	return c, nil
}

// validateLimitMode enforces the limit-mode contract at compile time, failing
// closed on an unknown mode so a doc written by a future binary is rejected
// rather than silently evaluated as unlimited.
func validateLimitMode(q Query) error {
	const op = "query.Compile"
	switch q.LimitMode {
	case LimitCount:
		if q.LimitSeed != 0 {
			return waxerr.New(waxerr.CodeInvalid, op,
				"limitSeed requires a random, minutes, or megabytes limit mode")
		}
	case LimitRandom:
		if q.Limit <= 0 {
			return waxerr.New(waxerr.CodeInvalid, op, "limit mode random requires a positive limit")
		}
		if len(q.Sorts) > 0 {
			return waxerr.New(waxerr.CodeInvalid, op,
				"limit mode random cannot be combined with sorts (the shuffle is the order)")
		}
	case LimitMinutes, LimitMegabytes:
		if q.Limit <= 0 {
			return waxerr.New(waxerr.CodeInvalid, op,
				fmt.Sprintf("limit mode %s requires a positive limit", q.LimitMode))
		}
		if q.LimitSeed != 0 && len(q.Sorts) > 0 {
			return waxerr.New(waxerr.CodeInvalid, op,
				"limitSeed on a budget mode requires empty sorts (the seed supplies the order)")
		}
	default:
		return waxerr.New(waxerr.CodeInvalid, op,
			fmt.Sprintf("unknown limit mode %q", q.LimitMode))
	}
	return nil
}

func compileNode(n Node, fields Fields, sb *strings.Builder, args *[]any, nu *bool, nowNS int64) error {
	switch v := n.(type) {
	case Cond:
		return compileCond(v, fields, sb, args, nu, nowNS)
	case And:
		return compileGroup(v.Nodes, "AND", "1=1", fields, sb, args, nu, nowNS)
	case Or:
		return compileGroup(v.Nodes, "OR", "1=0", fields, sb, args, nu, nowNS)
	case Not:
		if v.Node == nil {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile", "NOT with no child")
		}
		sb.WriteString("NOT (")
		if err := compileNode(v.Node, fields, sb, args, nu, nowNS); err != nil {
			return err
		}
		sb.WriteString(")")
		return nil
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unsupported node type %T", n))
	}
}

func compileGroup(nodes []Node, joiner, empty string, fields Fields, sb *strings.Builder, args *[]any, nu *bool, nowNS int64) error {
	if len(nodes) == 0 {
		sb.WriteString(empty) // empty AND => always true; empty OR => always false
		return nil
	}
	sb.WriteString("(")
	for i, child := range nodes {
		if i > 0 {
			sb.WriteString(" " + joiner + " ")
		}
		if err := compileNode(child, fields, sb, args, nu, nowNS); err != nil {
			return err
		}
	}
	sb.WriteString(")")
	return nil
}

func compileCond(c Cond, fields Fields, sb *strings.Builder, args *[]any, nu *bool, nowNS int64) error {
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
	// Decided once here, so the three column helpers below can assume a validated,
	// non-empty list.
	if c.Op == OpIn || c.Op == OpNotIn {
		if err := checkSetValues(c); err != nil {
			return err
		}
		if len(c.Values) == 0 {
			// IN () is a syntax error, and an empty list is a shape real callers reach
			// (a user who follows no shows), so it gets an answer rather than an error.
			if c.Op == OpIn {
				sb.WriteString("1=0")
			} else {
				sb.WriteString("1=1")
			}
			return nil
		}
	}
	// A set-membership column (a tag.<KEY> field) compiles to a correlated EXISTS
	// subquery instead of a scalar comparison; its bind order is security-sensitive
	// (see SetColumn), so it lives in its own helper.
	if col.Set != nil {
		return compileSetCond(c, col.Set, sb, args)
	}
	// A value-lowered column (an identity handle) compares an internal id against a
	// subquery resolving the caller's pid, and accepts only the operators that
	// comparison can answer; see Column.ValueSub.
	if col.ValueSub != "" {
		return compileValueSubCond(c, col, sb, args)
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
	case OpIn, OpNotIn:
		if c.Op == OpNotIn {
			sb.WriteString(col.Expr + " NOT")
		} else {
			sb.WriteString(col.Expr)
		}
		sb.WriteString(" IN " + placeholderList(len(c.Values)))
		*args = append(*args, c.Values...)
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
	case OpInTheLast, OpNotInTheLast:
		if col.Kind != KindTime {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile",
				fmt.Sprintf("%s on %q requires a time field", c.Op, c.Field))
		}
		window, err := windowNS(c)
		if err != nil {
			return err
		}
		if c.Op == OpInTheLast {
			sb.WriteString(col.Expr + " >= ?")
		} else {
			// The complement includes NULL: "not played in the last 30 days"
			// includes never-played (see the store field map's NULL contract).
			sb.WriteString("(" + col.Expr + " IS NULL OR " + col.Expr + " < ?)")
		}
		*args = append(*args, nowNS-window)
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("unsupported operator %q", c.Op))
	}
	return nil
}

// compileValueSubCond compiles a condition against a value-lowered column (an
// identity handle such as artist_pid) by emitting Column.ValueSub in place of the
// value's placeholder, so the comparison reads "<id column> = (SELECT id ... WHERE
// pid = ?)". The pid still binds as a positional arg inside ValueSub, exactly once
// per query rather than once per row.
//
// isNot against a pid no entity holds matches nothing rather than everything,
// because the lowered value is NULL and `x <> NULL` is NULL. That differs from an
// unlowered isNot, which compares text and so matches every row carrying a different
// pid. It is the one behavior change lowering makes, and it is the more useful
// answer of the two: "items not by this artist" over an artist that does not exist
// is a question with no meaningful subject. `is` against a nonexistent pid matched
// nothing before and still does.
func compileValueSubCond(c Cond, col Column, sb *strings.Builder, args *[]any) error {
	switch c.Op {
	case OpIs:
		sb.WriteString(col.Expr + " = " + col.ValueSub)
		*args = append(*args, c.Value)
	case OpIsNot:
		sb.WriteString(col.Expr + " <> " + col.ValueSub)
		*args = append(*args, c.Value)
	case OpIn:
		if len(c.Values) == 1 {
			// SQLite reads `x IN ((SELECT ...))` as the subquery form rather than a
			// one-element list, and that plans as a scan. Arity 1 emits exactly what `is`
			// emits, which is indexed and means the same thing.
			sb.WriteString(col.Expr + " = " + col.ValueSub)
			*args = append(*args, c.Values[0])
			return nil
		}
		// The subquery repeats per value on purpose, and it is not free. Measured: the
		// collapsed form (`IN (SELECT id FROM podcast WHERE pid IN (?,?))`) plans as
		// SCAN pi at 2 and at 500 values; this seeks at both. It costs 27KB/~2ms to
		// prepare at 500 against 1KB/~0.25ms, and nothing at 2. The seek scales with
		// the catalog, the parse with a subscription count.
		subs := make([]string, len(c.Values))
		for i := range subs {
			subs[i] = col.ValueSub
		}
		sb.WriteString(col.Expr + " IN (" + strings.Join(subs, ", ") + ")")
		*args = append(*args, c.Values...)
	case OpNotIn:
		// A deny-list (see Column.ValueSub). IS NOT is null-safe, so a stale pid lowers
		// to NULL and its term reads true for every row instead of taking the condition
		// to UNKNOWN; the IS NULL disjunct then covers a valueless row meeting one. The
		// outer parens are required: compileGroup joins children with a bare AND, so a
		// fragment holding a top-level OR must parenthesize itself.
		sb.WriteString("(" + col.Expr + " IS NULL OR (")
		for i := range c.Values {
			if i > 0 {
				sb.WriteString(" AND ")
			}
			sb.WriteString(col.Expr + " IS NOT " + col.ValueSub)
		}
		sb.WriteString("))")
		*args = append(*args, c.Values...)
	case OpIsPresent:
		// No value to lower: the id column being non-NULL is the whole question, and
		// it beats the unlowered form, which had to run a subquery per row to
		// discover the same thing.
		sb.WriteString(col.Expr + " IS NOT NULL")
	case OpIsMissing:
		sb.WriteString(col.Expr + " IS NULL")
	default:
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("operator %q not supported on an identity field %q", c.Op, c.Field))
	}
	return nil
}

// windowNS extracts a relative-time operator's window: a positive integral
// nanosecond count. A rule-doc value arrives as int64 (decodeScalar preserves
// integer precision); a programmatic int or an integral float is accepted too,
// and anything else is rejected.
func windowNS(c Cond) (int64, error) {
	var w int64
	switch v := c.Value.(type) {
	case int64:
		w = v
	case int:
		w = int64(v)
	case float64:
		w = int64(v)
		if float64(w) != v {
			return 0, waxerr.New(waxerr.CodeInvalid, "query.Compile",
				fmt.Sprintf("%s on %q needs a whole nanosecond window, got %v", c.Op, c.Field, v))
		}
	default:
		return 0, waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("%s on %q needs an integer nanosecond window, got %T", c.Op, c.Field, c.Value))
	}
	if w <= 0 {
		return 0, waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("%s on %q needs a positive nanosecond window", c.Op, c.Field))
	}
	return w, nil
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
// unordered TEXT; the relative-time operators (inTheLast/notInTheLast) fall under the
// same rejection.
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
	case OpIn:
		sb.WriteString("EXISTS (" + set.Sub + " AND " + set.ValueExpr + " IN " + placeholderList(len(c.Values)) + ")")
		*args = append(*args, set.Args...)
		*args = append(*args, c.Values...)
	case OpNotIn:
		// Inherits isNot's deny-list semantics: an item carrying no such tag matches.
		sb.WriteString("NOT EXISTS (" + set.Sub + " AND " + set.ValueExpr + " IN " + placeholderList(len(c.Values)) + ")")
		*args = append(*args, set.Args...)
		*args = append(*args, c.Values...)
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
			fmt.Sprintf("operator %q not supported on a set field", c.Op))
	}
	return nil
}

// checkSetValues bounds a set-membership list and rejects a nil element, which
// decodes straight out of a rule doc ({"values":["a",null]}) and would compile to a
// NULL bind matching nothing. Rejecting it uniformly names the caller bug instead of
// dropping a value in silence.
func checkSetValues(c Cond) error {
	// Where(f, OpIn, v) fills Value, which these operators do not read, leaving an
	// empty Values that legitimately compiles to the match-nothing constant. Caught
	// here because the wrong answer is otherwise silent: the range operators need no
	// such guard, since their arity check already rejects an empty Values.
	if c.Value != nil {
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("%s on %q takes values, not value", c.Op, c.Field))
	}
	if len(c.Values) > maxInValues {
		return waxerr.New(waxerr.CodeInvalid, "query.Compile",
			fmt.Sprintf("%s on %q takes at most %d values, got %d", c.Op, c.Field, maxInValues, len(c.Values)))
	}
	for _, v := range c.Values {
		if v == nil {
			return waxerr.New(waxerr.CodeInvalid, "query.Compile",
				fmt.Sprintf("%s on %q has a null value", c.Op, c.Field))
		}
	}
	return nil
}

// placeholderList renders "(?, ?, ?)" for n values; n is always >= 1, since an empty
// list is answered with a constant before any column helper runs.
func placeholderList(n int) string {
	return "(" + strings.Repeat("?, ", n-1) + "?)"
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
