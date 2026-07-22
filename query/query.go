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
	EntityItems  Entity = "items"  // every playable_item (track, book, ...)
	EntityTracks Entity = "tracks" // music tracks only (excludes books)
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

	// Relative-time operators, under Navidrome's names so .nsp rules map 1:1.
	// The condition value is a positive int64 window in nanoseconds, only
	// KindTime fields accept them, and "now" is anchored at compile time (see
	// CompileAt). OpInTheLast matches a timestamp within the window ending now;
	// OpNotInTheLast matches the complement, which includes NULL, so "not
	// played in the last 30 days" also matches never-played.
	OpInTheLast    Op = "inTheLast"
	OpNotInTheLast Op = "notInTheLast"
)

// LimitMode selects how Query.Limit is interpreted at evaluation time. The
// zero value is the plain row-count cap; the other modes are how a smart list
// says "25 random tracks", "an hour of music", or "what fits on the device".
type LimitMode string

const (
	// LimitCount is the default: Limit caps the row count in sorted order.
	// It marshals as the absent field; "count" is accepted on parse.
	LimitCount LimitMode = ""
	// LimitRandom returns Limit rows drawn by a seeded shuffle. LimitSeed pins
	// the order (0 = a fresh order per evaluation); Sorts are rejected with
	// this mode, since the shuffle is the order.
	LimitRandom LimitMode = "random"
	// LimitMinutes accumulates rows in order until adding the next row would
	// exceed Limit minutes of playtime, excluding that row and stopping. A row
	// with no measurable playtime is skipped rather than counted as free.
	LimitMinutes LimitMode = "minutes"
	// LimitMegabytes accumulates rows in order until adding the next row would
	// exceed Limit megabytes (10^6 bytes), excluding that row and stopping. A
	// row's cost is the summed size of all its backing files (every part of a
	// multi-file book). A virtual track carved from a shared single-file rip
	// counts the whole rip file's size once per included track, so a shared rip
	// over-counts and a device budget under-fills rather than overflowing,
	// which is the safe way to be wrong. A fileless row costs nothing
	// measurable and is skipped.
	LimitMegabytes LimitMode = "megabytes"
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
	// LimitMode interprets Limit (count/random/minutes/megabytes; see the
	// LimitMode constants). Additive to the version-1 rule document: an older
	// binary parsing a mode-carrying doc drops the mode but still honors Limit,
	// so it evaluates the plain row cap. A "random 25" rule comes back as the
	// first 25 in sort order and a "60 minutes" rule as 60 rows. That silent
	// drift is accepted pre-1.0.
	LimitMode LimitMode `json:"limitMode,omitempty"`
	// LimitSeed pins the shuffle order of a random/budget evaluation (0 = a
	// fresh order per evaluation). Only meaningful with a non-count LimitMode;
	// CompileAt rejects it on the count mode.
	LimitSeed int64 `json:"limitSeed,omitempty"`
}

// Builder is a fluent constructor. Conditions added via Where are combined with
// AND; use WhereNode to add arbitrary And/Or/Not subtrees.
type Builder struct {
	entity    Entity
	ands      []Node
	sorts     []Sort
	limit     int
	offset    int
	limitMode LimitMode
	limitSeed int64
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

// LimitBy sets how Limit is interpreted (count/random/minutes/megabytes).
func (b *Builder) LimitBy(mode LimitMode) *Builder { b.limitMode = mode; return b }

// Seed pins the shuffle order of a random/budget-mode evaluation (0 = fresh
// per evaluation).
func (b *Builder) Seed(seed int64) *Builder { b.limitSeed = seed; return b }

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
		Entity:    b.entity,
		Where:     where,
		Sorts:     b.sorts,
		Limit:     b.limit,
		Offset:    b.offset,
		LimitMode: b.limitMode,
		LimitSeed: b.limitSeed,
	}
}
