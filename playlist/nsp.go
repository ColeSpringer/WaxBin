package playlist

import (
	"encoding/json"
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
// map cleanly (text and integer) are listed; a date/relative field, or any field not
// here, is rejected. WaxBin has no relative-date operator, so Navidrome's date
// fields (with inTheLast/before/after over dates) cannot round-trip and are excluded.
var nspFieldToWB = map[string]string{
	"title":       "title",
	"album":       "album",
	"artist":      "artist",
	"albumartist": "albumartist",
	"genre":       "genre",
	"year":        "year",
	"tracknumber": "track_no",
	"discnumber":  "disc_no",
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
	if raw, ok := top["sort"]; ok {
		var field string
		if err := json.Unmarshal(raw, &field); err != nil {
			return query.Query{}, nspErr("nsp: bad sort")
		}
		wb, ok := nspFieldToWB[strings.ToLower(field)]
		if !ok {
			return query.Query{}, nspErr("nsp: unsupported sort field: " + field)
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

	// Compile-validate so an operator/field/value combination the engine rejects
	// fails the whole import rather than producing an un-runnable playlist.
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
		wb, ok := nspFieldToWB[strings.ToLower(field)]
		if !ok {
			return nil, nspErr("nsp: unsupported field: " + field)
		}
		return nspCond(op, wb, rawVal)
	}
	return nil, nspErr("nsp: empty operator")
}

// nspCond builds a condition (or a negated one) for a leaf operator.
func nspCond(op, field string, rawVal json.RawMessage) (query.Node, error) {
	if op == "inTheRange" {
		var vals []any
		if err := json.Unmarshal(rawVal, &vals); err != nil || len(vals) != 2 {
			return nil, nspErr("nsp: inTheRange needs a [low, high] array")
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
	return query.Cond{Field: field, Op: wbOp, Value: v}, nil
}

// ExportNSP renders a WaxBin item query as a Navidrome .nsp document. It rejects
// (all-or-nothing) any node, operator, or field with no faithful .nsp representation.
func ExportNSP(q query.Query) ([]byte, error) {
	group, err := nspExportRoot(q.Where)
	if err != nil {
		return nil, err
	}
	if len(q.Sorts) > 0 {
		s := q.Sorts[0]
		nsp, ok := wbFieldToNSP[s.Field]
		if !ok {
			return nil, nspErr("nsp: unsupported sort field: " + s.Field)
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

// nspExportCond renders a single condition as `{"<op>": {"<field>": <value>}}`.
func nspExportCond(c query.Cond) (map[string]any, error) {
	field, ok := wbFieldToNSP[c.Field]
	if !ok {
		return nil, nspErr("nsp: unsupported field: " + c.Field)
	}
	op, ok := wbOpToNSP[c.Op]
	if !ok {
		return nil, nspErr("nsp: unsupported operator: " + string(c.Op))
	}
	if c.Op == query.OpInRange {
		return map[string]any{op: map[string]any{field: c.Values}}, nil
	}
	return map[string]any{op: map[string]any{field: c.Value}}, nil
}
