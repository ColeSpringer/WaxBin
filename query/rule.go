package query

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/colespringer/waxbin/waxerr"
)

// RuleVersion is the current rule-document schema version. It is an envelope
// version independent of the CLI's schemaVersion, so persisted rule docs can
// evolve on their own cadence.
const RuleVersion = 1

// RuleDoc is the on-disk/JSON form of a Query, wrapped in a version envelope.
type RuleDoc struct {
	Version int   `json:"version"`
	Query   Query `json:"query"`
}

// ParseRule decodes a versioned JSON rule document into a Query.
func ParseRule(data []byte) (Query, error) {
	var doc RuleDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return Query{}, waxerr.Wrap(waxerr.CodeInvalid, "query.ParseRule", err)
	}
	if doc.Version == 0 {
		return Query{}, waxerr.New(waxerr.CodeInvalid, "query.ParseRule",
			"missing rule document version")
	}
	if doc.Version > RuleVersion {
		return Query{}, waxerr.New(waxerr.CodeInvalid, "query.ParseRule",
			fmt.Sprintf("rule document version %d is newer than supported version %d",
				doc.Version, RuleVersion))
	}
	return doc.Query, nil
}

// MarshalRule encodes a Query as a versioned JSON rule document.
func MarshalRule(q Query) ([]byte, error) {
	data, err := json.Marshal(RuleDoc{Version: RuleVersion, Query: q})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "query.MarshalRule", err)
	}
	return data, nil
}

// nodeEnvelope is the wire shape for the Node union. The "type" discriminator
// selects which fields are meaningful.
type nodeEnvelope struct {
	Type   string            `json:"type"`
	Field  string            `json:"field,omitempty"`
	Op     Op                `json:"op,omitempty"`
	Value  json.RawMessage   `json:"value,omitempty"`
	Values []json.RawMessage `json:"values,omitempty"`
	Nodes  []json.RawMessage `json:"nodes,omitempty"`
	Node   json.RawMessage   `json:"node,omitempty"`
}

// MarshalJSON renders the Query, encoding the Node tree as tagged envelopes.
func (q Query) MarshalJSON() ([]byte, error) {
	type alias struct {
		Entity Entity          `json:"entity"`
		Where  json.RawMessage `json:"where,omitempty"`
		Sorts  []Sort          `json:"sorts,omitempty"`
		Limit  int             `json:"limit,omitempty"`
		Offset int             `json:"offset,omitempty"`
	}
	a := alias{Entity: q.Entity, Sorts: q.Sorts, Limit: q.Limit, Offset: q.Offset}
	if q.Where != nil {
		raw, err := marshalNode(q.Where)
		if err != nil {
			return nil, err
		}
		a.Where = raw
	}
	return json.Marshal(a)
}

// UnmarshalJSON parses a Query, decoding the Node tree from tagged envelopes.
func (q *Query) UnmarshalJSON(data []byte) error {
	var a struct {
		Entity Entity          `json:"entity"`
		Where  json.RawMessage `json:"where"`
		Sorts  []Sort          `json:"sorts"`
		Limit  int             `json:"limit"`
		Offset int             `json:"offset"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	q.Entity = a.Entity
	q.Sorts = a.Sorts
	q.Limit = a.Limit
	q.Offset = a.Offset
	if len(a.Where) > 0 && string(a.Where) != "null" {
		n, err := unmarshalNode(a.Where)
		if err != nil {
			return err
		}
		q.Where = n
	}
	return nil
}

func marshalNode(n Node) (json.RawMessage, error) {
	switch v := n.(type) {
	case Cond:
		env := nodeEnvelope{Type: "cond", Field: v.Field, Op: v.Op}
		if v.Value != nil {
			raw, err := json.Marshal(v.Value)
			if err != nil {
				return nil, err
			}
			env.Value = raw
		}
		for _, val := range v.Values {
			raw, err := json.Marshal(val)
			if err != nil {
				return nil, err
			}
			env.Values = append(env.Values, raw)
		}
		return json.Marshal(env)
	case And:
		return marshalGroup("and", v.Nodes)
	case Or:
		return marshalGroup("or", v.Nodes)
	case Not:
		child, err := marshalNode(v.Node)
		if err != nil {
			return nil, err
		}
		return json.Marshal(nodeEnvelope{Type: "not", Node: child})
	default:
		return nil, waxerr.New(waxerr.CodeInternal, "query.marshalNode",
			fmt.Sprintf("unsupported node type %T", n))
	}
}

func marshalGroup(typ string, nodes []Node) (json.RawMessage, error) {
	env := nodeEnvelope{Type: typ}
	for _, child := range nodes {
		raw, err := marshalNode(child)
		if err != nil {
			return nil, err
		}
		env.Nodes = append(env.Nodes, raw)
	}
	return json.Marshal(env)
}

// decodeScalar decodes a JSON scalar preserving integer precision: numbers
// become int64 when integral (so nanosecond timestamps and large int64 bounds
// survive), else float64. Plain json.Unmarshal into any would force float64 and
// silently round values above 2^53.
func decodeScalar(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "query.unmarshalNode", err)
	}
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i, nil
		}
		if f, err := n.Float64(); err == nil {
			return f, nil
		}
		return n.String(), nil
	}
	return v, nil
}

func unmarshalNode(data json.RawMessage) (Node, error) {
	var env nodeEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInvalid, "query.unmarshalNode", err)
	}
	switch env.Type {
	case "cond":
		c := Cond{Field: env.Field, Op: env.Op}
		if len(env.Value) > 0 {
			v, err := decodeScalar(env.Value)
			if err != nil {
				return nil, err
			}
			c.Value = v
		}
		for _, raw := range env.Values {
			v, err := decodeScalar(raw)
			if err != nil {
				return nil, err
			}
			c.Values = append(c.Values, v)
		}
		return c, nil
	case "and", "or":
		nodes := make([]Node, 0, len(env.Nodes))
		for _, raw := range env.Nodes {
			child, err := unmarshalNode(raw)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, child)
		}
		if env.Type == "and" {
			return And{Nodes: nodes}, nil
		}
		return Or{Nodes: nodes}, nil
	case "not":
		child, err := unmarshalNode(env.Node)
		if err != nil {
			return nil, err
		}
		return Not{Node: child}, nil
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, "query.unmarshalNode",
			fmt.Sprintf("unknown node type %q", env.Type))
	}
}
