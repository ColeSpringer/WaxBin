// Package query is WaxBin's shared selection engine. It provides one typed
// query model for CLI filters, organize selection, and read-side features.
// Queries are built programmatically or parsed from versioned JSON rule
// documents and compiled to parameterized SQL against a field whitelist. There
// is no free-text DSL.
//
// The package is storage-agnostic: it knows operators and a logical field model
// but not the schema. A caller supplies a FieldMap (logical field -> SQL
// expression) at compile time, which is what keeps untrusted field names from
// ever reaching SQL.
package query

// Entity is the top-level thing a query selects.
type Entity string

const (
	EntityItems  Entity = "items"  // playable_item (+ track) rows
	EntityTracks Entity = "tracks" // alias for music items
	EntityFiles  Entity = "files"  // file rows
)

// Op is a comparison operator. The set mirrors the plan's operator vocabulary.
type Op string

const (
	OpIs         Op = "is"
	OpIsNot      Op = "isNot"
	OpContains   Op = "contains"
	OpStartsWith Op = "startsWith"
	OpEndsWith   Op = "endsWith"
	OpGt         Op = "gt"
	OpLt         Op = "lt"
	OpGte        Op = "gte"
	OpLte        Op = "lte"
	OpInRange    Op = "inTheRange" // Values[0]..Values[1] inclusive
	OpBefore     Op = "before"     // time/ordinal <
	OpAfter      Op = "after"      // time/ordinal >
	OpIsPresent  Op = "isPresent"  // non-null and (for text) non-empty
	OpIsMissing  Op = "isMissing"  // null or (for text) empty
)

// Node is one element of a boolean condition tree: a Cond leaf or an And/Or/Not
// combinator.
type Node interface{ isNode() }

// Cond is a single field comparison.
type Cond struct {
	Field  string `json:"field"`
	Op     Op     `json:"op"`
	Value  any    `json:"value,omitempty"`  // single-value operators
	Values []any  `json:"values,omitempty"` // range operators (inTheRange)
}

// And is a conjunction; an empty And matches everything.
type And struct {
	Nodes []Node `json:"nodes"`
}

// Or is a disjunction; an empty Or matches nothing.
type Or struct {
	Nodes []Node `json:"nodes"`
}

// Not negates its child.
type Not struct {
	Node Node `json:"node"`
}

func (Cond) isNode() {}
func (And) isNode()  {}
func (Or) isNode()   {}
func (Not) isNode()  {}

// Sort is one ordering term. Sorts are applied in slice order.
type Sort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// Query is a complete, storage-agnostic selection.
type Query struct {
	Entity Entity `json:"entity"`
	Where  Node   `json:"where,omitempty"`
	Sorts  []Sort `json:"sorts,omitempty"`
	Limit  int    `json:"limit,omitempty"`  // 0 == no limit
	Offset int    `json:"offset,omitempty"` // 0 == no offset
}

// Builder is a fluent constructor. Conditions added via Where are combined with
// AND; use WhereNode to add arbitrary And/Or/Not subtrees.
type Builder struct {
	entity Entity
	ands   []Node
	sorts  []Sort
	limit  int
	offset int
}

// New starts a Builder for the given entity.
func New(entity Entity) *Builder { return &Builder{entity: entity} }

// Where appends a single-value condition to the implicit top-level AND.
func (b *Builder) Where(field string, op Op, value any) *Builder {
	b.ands = append(b.ands, Cond{Field: field, Op: op, Value: value})
	return b
}

// WhereRange appends a range condition (e.g. OpInRange) to the top-level AND.
func (b *Builder) WhereRange(field string, op Op, lo, hi any) *Builder {
	b.ands = append(b.ands, Cond{Field: field, Op: op, Values: []any{lo, hi}})
	return b
}

// WherePresence appends a presence condition (OpIsPresent / OpIsMissing).
func (b *Builder) WherePresence(field string, op Op) *Builder {
	b.ands = append(b.ands, Cond{Field: field, Op: op})
	return b
}

// WhereNode appends an arbitrary node to the top-level AND.
func (b *Builder) WhereNode(n Node) *Builder {
	if n != nil {
		b.ands = append(b.ands, n)
	}
	return b
}

// OrderBy appends an ordering term.
func (b *Builder) OrderBy(field string, desc bool) *Builder {
	b.sorts = append(b.sorts, Sort{Field: field, Desc: desc})
	return b
}

// Limit sets the row cap (0 == unlimited).
func (b *Builder) Limit(n int) *Builder { b.limit = n; return b }

// Offset sets the row offset.
func (b *Builder) Offset(n int) *Builder { b.offset = n; return b }

// Build materializes the Query. A single top-level condition is unwrapped;
// multiple are wrapped in an And.
func (b *Builder) Build() Query {
	var where Node
	switch len(b.ands) {
	case 0:
		where = nil
	case 1:
		where = b.ands[0]
	default:
		where = And{Nodes: b.ands}
	}
	return Query{
		Entity: b.entity,
		Where:  where,
		Sorts:  b.sorts,
		Limit:  b.limit,
		Offset: b.offset,
	}
}
