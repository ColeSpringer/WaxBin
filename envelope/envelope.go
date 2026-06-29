// Package envelope is the version envelope for persisted, re-parsed artifacts
// (query rule documents, organization profile definitions, export manifests).
// Every such artifact is wrapped in a self-describing {kind, version, payload}
// header so it can be validated on read and evolve on its own cadence,
// independent of the storage schema version and the CLI's output schemaVersion.
// A reader rejects an unknown kind or a version newer than it understands rather
// than silently misinterpreting the payload.
package envelope

import (
	"encoding/json"
	"strconv"

	"github.com/colespringer/waxbin/waxerr"
)

// Envelope is the on-disk wrapper. Payload stays raw until the caller decodes it
// against the kind/version it just validated.
type Envelope struct {
	Kind    string          `json:"kind"`
	Version int             `json:"version"`
	Payload json.RawMessage `json:"payload"`
}

const op = "envelope"

// Wrap serializes payload inside a versioned envelope of the given kind.
func Wrap(kind string, version int, payload any) ([]byte, error) {
	if kind == "" || version < 1 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "envelope needs a kind and a version >= 1")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, op, err)
	}
	data, err := json.Marshal(Envelope{Kind: kind, Version: version, Payload: raw})
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, op, err)
	}
	return data, nil
}

// Unwrap parses an envelope, checks the kind matches and the version is within
// [1, maxVersion], and returns the still-raw payload plus its version. A wrong
// kind or a future version is a CodeInvalid error.
func Unwrap(data []byte, wantKind string, maxVersion int) (json.RawMessage, int, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, 0, waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	if e.Kind != wantKind {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op,
			"expected artifact kind "+wantKind+", got "+strOrNone(e.Kind))
	}
	if e.Version < 1 {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op, "missing or invalid envelope version")
	}
	if e.Version > maxVersion {
		return nil, 0, waxerr.New(waxerr.CodeInvalid, op,
			versionMsg(wantKind, e.Version, maxVersion))
	}
	return e.Payload, e.Version, nil
}

// Decode is Unwrap followed by decoding the payload into T. It is the common
// path; use Unwrap directly when a per-version payload shape must be selected.
func Decode[T any](data []byte, wantKind string, maxVersion int) (T, int, error) {
	var out T
	raw, version, err := Unwrap(data, wantKind, maxVersion)
	if err != nil {
		return out, 0, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, 0, waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	return out, version, nil
}

func strOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func versionMsg(kind string, got, max int) string {
	return "artifact " + kind + " version " + strconv.Itoa(got) +
		" is newer than supported version " + strconv.Itoa(max)
}
