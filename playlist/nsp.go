package playlist

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Navidrome (.nsp) smart-playlist interop. A .nsp file is a JSON rule tree: an
// "all"/"any" group of leaves, each `{"<op>": {"<field>": <value>}}`, plus an
// optional sort/order/limit. ImportNSP/ExportNSP map it to and from WaxBin's query
// rule. Mapping is ALL-OR-NOTHING: an operator or field WaxBin cannot represent
// faithfully rejects the whole document (CodeUnsupported) rather than emitting a
// lossy partial that would silently drift on re-import.

// nspFieldToWB maps a Navidrome field name to a WaxBin query field. Only fields that
// map cleanly (text and integer) are listed here; the date fields live in
// nspDateFieldToWB with their own operator restriction, and any other field is
// rejected.
//
// The per-user fields that do map: starred (Navidrome's boolean to WaxBin's 0/1
// starred field) and playcount (an integer count, 1:1). rating maps too, but its
// value is scale-converted, since Navidrome rates 0 to 5 stars while WaxBin uses 0 to
// 100. A rating is multiplied by nspRatingScale on import and divided on export (see
// nspCond and nspExportCond). A WaxBin rating that is not a whole number of stars is
// rejected on export rather than written at the wrong scale. A mapped rule evaluates
// against the reading user, bound at read time and never persisted.
var nspFieldToWB = map[string]string{
	"title":       "title",
	"album":       "album",
	"artist":      "artist",
	"albumartist": "albumartist",
	"genre":       "genre",
	"year":        "year",
	"tracknumber": "track_no",
	"discnumber":  "disc_no",
	"rating":      "rating",
	"starred":     "starred",
	"playcount":   "play_count",
}

// nspDateFieldToWB maps Navidrome's date fields to WaxBin's nanosecond time fields.
// In a condition, only the relative operators map (inTheLast/notInTheLast, a whole
// number of days): WaxBin stores these fields as Unix nanoseconds while a Navidrome
// absolute rule (before/after/is) holds a date string ("2023-01-01"), so the
// absolute forms cannot round-trip faithfully and stay rejected (all-or-nothing)
// rather than quietly producing an always-true or always-empty predicate (an
// integer compared to a date string). See nspDateCond/nspExportDateCond for the
// restriction. As sort fields they map without restriction, since ordering a
// nanosecond column is exact; sort dateAdded desc is Navidrome's "recently added".
var nspDateFieldToWB = map[string]string{
	"lastplayed": "last_played",
	"dateadded":  "added",
}

// wbDateFieldToNSP is the reverse date-field map for export.
var wbDateFieldToNSP = func() map[string]string {
	m := make(map[string]string, len(nspDateFieldToWB))
	for k, v := range nspDateFieldToWB {
		m[v] = k
	}
	return m
}()

// nspDayNS is one day's span in nanoseconds: the unit conversion between a
// Navidrome relative-date value (days) and a WaxBin relative-time window (ns).
const nspDayNS = int64(24) * 60 * 60 * 1_000_000_000

// nspRatingScale bridges Navidrome's 0 to 5 star scale and WaxBin's 0 to 100 rating.
// A Navidrome value is multiplied by it on import and a WaxBin value divided by it on
// export. A 1:1 mapping would quietly mis-match: a Navidrome "more than 3 stars" rule
// would read as WaxBin "more than 3 out of 100" (nearly everything), and a WaxBin
// "rating at least 80" would export as "80 stars" (nothing).
const nspRatingScale = 20

// asFloat coerces a JSON-decoded (float64) or programmatically-built (int/int64)
// numeric value to a float64.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	default:
		return 0, false
	}
}

// scaleRatingIn converts a Navidrome rating value (0 to 5 stars) to WaxBin's 0 to 100
// scale.
func scaleRatingIn(v any) (any, error) {
	f, ok := asFloat(v)
	if !ok {
		return nil, nspErr("nsp: rating value must be numeric")
	}
	return f * nspRatingScale, nil
}

// scaleRatingOut converts a WaxBin rating value (0 to 100) back to Navidrome's 0 to 5
// scale. It rejects a value that is not a whole number of stars rather than writing a
// fractional or mismatched star count.
func scaleRatingOut(v any) (any, error) {
	f, ok := asFloat(v)
	if !ok {
		return nil, nspErr("nsp: rating value must be numeric")
	}
	n := int64(f)
	if float64(n) != f || n%nspRatingScale != 0 {
		return nil, nspErr(fmt.Sprintf(
			"nsp: WaxBin rating %v is not a whole star (a multiple of %d) and has no Navidrome 0-5 equivalent", v, nspRatingScale))
	}
	return n / nspRatingScale, nil
}

// wbFieldToNSP is the reverse map for export, built from nspFieldToWB.
var wbFieldToNSP = func() map[string]string {
	m := make(map[string]string, len(nspFieldToWB))
	for k, v := range nspFieldToWB {
		m[v] = k
	}
	return m
}()

// nspOpToWB maps a Navidrome leaf operator to a WaxBin operator. notContains has no
// direct WaxBin operator and is handled specially (wrapped in a Not).
var nspOpToWB = map[string]query.Op{
	"is":         query.OpIs,
	"isNot":      query.OpIsNot,
	"contains":   query.OpContains,
	"startsWith": query.OpStartsWith,
	"endsWith":   query.OpEndsWith,
	"gt":         query.OpGt,
	"lt":         query.OpLt,
	"before":     query.OpBefore,
	"after":      query.OpAfter,
	"inTheRange": query.OpInRange,
}

// wbOpToNSP is the reverse map for export.
var wbOpToNSP = func() map[query.Op]string {
	m := make(map[query.Op]string, len(nspOpToWB))
	for k, v := range nspOpToWB {
		m[v] = k
	}
	return m
}()

func nspErr(msg string) error { return waxerr.New(waxerr.CodeUnsupported, "playlist.nsp", msg) }

// ImportNSP parses a Navidrome .nsp document into a WaxBin item query. It rejects
// (all-or-nothing) any operator or field WaxBin cannot represent.
func ImportNSP(data []byte) (query.Query, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return query.Query{}, waxerr.Wrap(waxerr.CodeInvalid, "playlist.nsp", err)
	}

	var root query.Node
	haveRoot := false
	for key, raw := range top {
		switch key {
		case "sort", "order", "limit", "offset":
			continue
		case "name", "comment":
			// Navidrome playlist metadata that does not affect membership. Ignore it; the
			// WaxBin playlist name and visibility are supplied separately. Rejecting these
			// would turn away common .nsp files that are otherwise representable.
			continue
		case "all", "any":
			if haveRoot {
				return query.Query{}, nspErr("nsp: multiple root groups")
			}
			node, err := nspGroup(key, raw)
			if err != nil {
				return query.Query{}, err
			}
			root, haveRoot = node, true
		default:
			// A semantics-affecting key WaxBin cannot represent (e.g. limitPercent) is
			// rejected all-or-nothing rather than silently dropped, which would let the
			// imported playlist drift from the original.
			return query.Query{}, nspErr("nsp: unsupported top-level key: " + key)
		}
	}
	if !haveRoot {
		return query.Query{}, nspErr("nsp: missing all/any root group")
	}

	q := query.Query{Entity: query.EntityItems, Where: root}
	// limit/offset parse before the sort block: the random-sort mapping below
	// checks the parsed limit value, not just whether the key is present.
	if raw, ok := top["limit"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return query.Query{}, nspErr("nsp: bad limit")
		}
		q.Limit = n
	}
	if raw, ok := top["offset"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return query.Query{}, nspErr("nsp: bad offset")
		}
		q.Offset = n
	}
	if raw, ok := top["sort"]; ok {
		var field string
		if err := json.Unmarshal(raw, &field); err != nil {
			return query.Query{}, nspErr("nsp: bad sort")
		}
		if lower := strings.ToLower(field); lower == "random" {
			// Navidrome's random sort maps to WaxBin's random limit mode: a seeded
			// shuffle, drawn fresh per evaluation (no seed is persisted). WaxBin
			// requires a positive limit with that mode, since "everything,
			// shuffled" is a playback concern rather than a selection, so a random
			// sort with no limit (or a zero/negative one) stays unsupported
			// instead of importing a query every downstream compile would reject.
			// "order" is ignored: asc/desc of a shuffle is meaningless.
			if q.Limit <= 0 {
				return query.Query{}, nspErr("nsp: sort random requires a positive limit")
			}
			q.LimitMode = query.LimitRandom
		} else {
			wb, ok := nspFieldToWB[lower]
			if !ok {
				// The date fields sort too (WaxBin's added/last_played are plain
				// time columns): "recently added" is sort dateAdded desc.
				if wb, ok = nspDateFieldToWB[lower]; !ok {
					return query.Query{}, nspErr("nsp: unsupported sort field: " + field)
				}
			}
			desc := false
			if o, ok := top["order"]; ok {
				var ord string
				if err := json.Unmarshal(o, &ord); err != nil {
					return query.Query{}, nspErr("nsp: bad order")
				}
				desc = strings.EqualFold(ord, "desc")
			}
			q.Sorts = []query.Sort{{Field: wb, Desc: desc}}
		}
	}

	// Marshal-validate so a node shape the rule codec cannot represent fails the
	// whole import. No compilation happens here: field, entity, and limit-mode
	// validation runs where the rule is stored (CreatePlaylist and
	// SetPlaylistRule compile against the store's field whitelist), and since
	// the import maps only fields and combinations it knows, those checks serve
	// as a backstop rather than the primary barrier.
	if _, err := query.MarshalRule(q); err != nil {
		return query.Query{}, err
	}
	return q, nil
}

// nspGroup parses an all/any group (a JSON array of rules) into an And/Or node.
func nspGroup(key string, raw json.RawMessage) (query.Node, error) {
	var rules []json.RawMessage
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, nspErr("nsp: " + key + " must be an array")
	}
	nodes := make([]query.Node, 0, len(rules))
	for _, r := range rules {
		n, err := nspRule(r)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	if key == "any" {
		return query.Or{Nodes: nodes}, nil
	}
	return query.And{Nodes: nodes}, nil
}

// nspRule parses one rule element: a nested group or an operator leaf.
func nspRule(raw json.RawMessage) (query.Node, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) != 1 {
		return nil, nspErr("nsp: each rule needs exactly one operator or group")
	}
	for key, val := range m {
		if key == "all" || key == "any" {
			return nspGroup(key, val)
		}
		return nspLeaf(key, val)
	}
	return nil, nspErr("nsp: empty rule")
}

// nspLeaf parses an operator leaf `{"<op>": {"<field>": <value>}}`.
func nspLeaf(op string, val json.RawMessage) (query.Node, error) {
	var fv map[string]json.RawMessage
	if err := json.Unmarshal(val, &fv); err != nil || len(fv) != 1 {
		return nil, nspErr("nsp: operator " + op + " needs exactly one field")
	}
	for field, rawVal := range fv {
		if wb, ok := nspDateFieldToWB[strings.ToLower(field)]; ok {
			return nspDateCond(op, wb, field, rawVal)
		}
		wb, ok := nspFieldToWB[strings.ToLower(field)]
		if !ok {
			return nil, nspErr("nsp: unsupported field: " + field)
		}
		return nspCond(op, wb, rawVal)
	}
	return nil, nspErr("nsp: empty operator")
}

// nspDateCond builds a relative-time condition for a Navidrome date field. The .nsp
// value is a number of days; WaxBin's operators take a nanosecond window, so the
// value converts on the way in (and back out in nspExportDateCond). Only
// inTheLast/notInTheLast map; any other operator on a date field is rejected (see
// nspDateFieldToWB), as is a fractional or non-positive day count, which would have
// no faithful export.
func nspDateCond(op, wbField, nspField string, rawVal json.RawMessage) (query.Node, error) {
	var wbOp query.Op
	switch op {
	case "inTheLast":
		wbOp = query.OpInTheLast
	case "notInTheLast":
		wbOp = query.OpNotInTheLast
	default:
		return nil, nspErr("nsp: only inTheLast/notInTheLast are supported on " + nspField)
	}
	var days float64
	if err := json.Unmarshal(rawVal, &days); err != nil {
		return nil, nspErr("nsp: " + op + " needs a number of days")
	}
	// Bound before converting: past MaxInt64/nspDayNS (about 106,751 days, or
	// 292 years) the nanosecond multiply below would overflow int64 and silently
	// wrap, in part of that range to a positive and plausible-looking window,
	// and a float that large makes the int64 conversion itself
	// implementation-defined.
	if days > float64(math.MaxInt64/nspDayNS) {
		return nil, nspErr(fmt.Sprintf("nsp: %s window of %v days is too large", op, days))
	}
	n := int64(days)
	if float64(n) != days || n <= 0 {
		return nil, nspErr(fmt.Sprintf("nsp: %s needs a positive whole number of days, got %v", op, days))
	}
	return query.Cond{Field: wbField, Op: wbOp, Value: n * nspDayNS}, nil
}

// nspCond builds a condition (or a negated one) for a leaf operator. A rating value
// is scaled up from Navidrome's 0-to-5 scale to WaxBin's 0-to-100 one. field is the
// resolved WaxBin name, so "rating" here is WaxBin's rating field.
func nspCond(op, field string, rawVal json.RawMessage) (query.Node, error) {
	if op == "inTheRange" {
		var vals []any
		if err := json.Unmarshal(rawVal, &vals); err != nil || len(vals) != 2 {
			return nil, nspErr("nsp: inTheRange needs a [low, high] array")
		}
		if field == "rating" {
			for i := range vals {
				sv, err := scaleRatingIn(vals[i])
				if err != nil {
					return nil, err
				}
				vals[i] = sv
			}
		}
		return query.Cond{Field: field, Op: query.OpInRange, Values: vals}, nil
	}
	var v any
	if err := json.Unmarshal(rawVal, &v); err != nil {
		return nil, nspErr("nsp: bad value for " + op)
	}
	if op == "notContains" {
		return query.Not{Node: query.Cond{Field: field, Op: query.OpContains, Value: v}}, nil
	}
	wbOp, ok := nspOpToWB[op]
	if !ok {
		return nil, nspErr("nsp: unsupported operator: " + op)
	}
	if field == "rating" {
		sv, err := scaleRatingIn(v)
		if err != nil {
			return nil, err
		}
		v = sv
	}
	return query.Cond{Field: field, Op: wbOp, Value: v}, nil
}

// ExportNSP renders a WaxBin item query as a Navidrome .nsp document. It rejects
// (all-or-nothing) any node, operator, or field with no faithful .nsp representation.
func ExportNSP(q query.Query) ([]byte, error) {
	group, err := nspExportRoot(q.Where)
	if err != nil {
		return nil, err
	}
	if q.LimitMode != query.LimitCount || q.LimitSeed != 0 {
		// Only the unseeded random mode maps back (Navidrome's sort "random"); the
		// budget modes (minutes/megabytes) and a pinned seed have no .nsp
		// representation and reject the whole export.
		if q.LimitMode != query.LimitRandom || q.LimitSeed != 0 {
			return nil, nspErr("nsp: limit mode/seed has no .nsp representation")
		}
		// Random plus Sorts is not even a valid WaxBin query (compile rejects the
		// combination), and rendering it would let the sort block below silently
		// overwrite the shuffle, so it is rejected here too.
		if len(q.Sorts) > 0 {
			return nil, nspErr("nsp: random limit mode combined with sorts is not exportable")
		}
		group["sort"] = "random"
	}
	if len(q.Sorts) > 0 {
		s := q.Sorts[0]
		nsp, ok := wbFieldToNSP[s.Field]
		if !ok {
			// The date fields export as sorts too (mirroring the import).
			if nsp, ok = wbDateFieldToNSP[s.Field]; !ok {
				return nil, nspErr("nsp: unsupported sort field: " + s.Field)
			}
		}
		group["sort"] = nsp
		if s.Desc {
			group["order"] = "desc"
		} else {
			group["order"] = "asc"
		}
	}
	if q.Limit > 0 {
		group["limit"] = q.Limit
	}
	if q.Offset > 0 {
		group["offset"] = q.Offset
	}
	return json.MarshalIndent(group, "", "  ")
}

// nspExportRoot renders the top-level rule as an all/any group (wrapping a bare leaf).
func nspExportRoot(n query.Node) (map[string]any, error) {
	switch node := n.(type) {
	case nil:
		return map[string]any{"all": []any{}}, nil
	case query.And, query.Or:
		return nspExportNode(node)
	default:
		leaf, err := nspExportNode(node)
		if err != nil {
			return nil, err
		}
		return map[string]any{"all": []any{leaf}}, nil
	}
}

// nspExportNode renders one node (group or leaf) as an .nsp object.
func nspExportNode(n query.Node) (map[string]any, error) {
	switch node := n.(type) {
	case query.And:
		arr, err := nspExportChildren(node.Nodes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"all": arr}, nil
	case query.Or:
		arr, err := nspExportChildren(node.Nodes)
		if err != nil {
			return nil, err
		}
		return map[string]any{"any": arr}, nil
	case query.Not:
		// The only Not shape .nsp can express is notContains.
		if c, ok := node.Node.(query.Cond); ok && c.Op == query.OpContains {
			field, ok := wbFieldToNSP[c.Field]
			if !ok {
				return nil, nspErr("nsp: unsupported field: " + c.Field)
			}
			return map[string]any{"notContains": map[string]any{field: c.Value}}, nil
		}
		return nil, nspErr("nsp: unsupported negation (only notContains maps)")
	case query.Cond:
		return nspExportCond(node)
	default:
		return nil, nspErr("nsp: unsupported rule node")
	}
}

func nspExportChildren(nodes []query.Node) ([]any, error) {
	arr := make([]any, 0, len(nodes))
	for _, n := range nodes {
		obj, err := nspExportNode(n)
		if err != nil {
			return nil, err
		}
		arr = append(arr, obj)
	}
	return arr, nil
}

// nspExportCond renders a single condition as `{"<op>": {"<field>": <value>}}`. A
// rating value is scaled back down from WaxBin's 0-to-100 scale to Navidrome's 0-to-5
// one, rejecting a value that is not a whole star rather than writing a mismatched one.
func nspExportCond(c query.Cond) (map[string]any, error) {
	if nspField, ok := wbDateFieldToNSP[c.Field]; ok {
		return nspExportDateCond(c, nspField)
	}
	field, ok := wbFieldToNSP[c.Field]
	if !ok {
		return nil, nspErr("nsp: unsupported field: " + c.Field)
	}
	op, ok := wbOpToNSP[c.Op]
	if !ok {
		return nil, nspErr("nsp: unsupported operator: " + string(c.Op))
	}
	if c.Op == query.OpInRange {
		vals := c.Values
		if c.Field == "rating" {
			vals = make([]any, len(c.Values))
			for i, x := range c.Values {
				sv, err := scaleRatingOut(x)
				if err != nil {
					return nil, err
				}
				vals[i] = sv
			}
		}
		return map[string]any{op: map[string]any{field: vals}}, nil
	}
	val := c.Value
	if c.Field == "rating" {
		sv, err := scaleRatingOut(c.Value)
		if err != nil {
			return nil, err
		}
		val = sv
	}
	return map[string]any{op: map[string]any{field: val}}, nil
}

// nspExportDateCond renders a relative-time condition back to .nsp days. A window
// that is not a whole number of days has no .nsp representation and rejects the
// export (the whole-star rating precedent), as does any non-relative operator on a
// date field.
func nspExportDateCond(c query.Cond, nspField string) (map[string]any, error) {
	var op string
	switch c.Op {
	case query.OpInTheLast:
		op = "inTheLast"
	case query.OpNotInTheLast:
		op = "notInTheLast"
	default:
		return nil, nspErr("nsp: only inTheLast/notInTheLast are supported on " + nspField)
	}
	// The window is read as int64 directly rather than through asFloat: a window
	// past about 104 days exceeds 2^53 ns and would silently round in a float64.
	var ns int64
	switch v := c.Value.(type) {
	case int64:
		ns = v
	case int:
		ns = int64(v)
	case float64:
		ns = int64(v)
		if float64(ns) != v {
			return nil, nspErr("nsp: " + op + " window must be a whole nanosecond count")
		}
	default:
		return nil, nspErr("nsp: " + op + " window must be numeric")
	}
	if ns <= 0 || ns%nspDayNS != 0 {
		return nil, nspErr(fmt.Sprintf(
			"nsp: %s window %v ns is not a whole number of days and has no .nsp equivalent", op, c.Value))
	}
	return map[string]any{op: map[string]any{nspField: ns / nspDayNS}}, nil
}
