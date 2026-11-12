package model

import (
	"crypto/rand"
	"sync"

	"github.com/oklog/ulid/v2"
)

// PID is an opaque, sortable public identifier surfaced for every entity. It is
// a ULID rendered as its 26-character Crockford base32 string at the boundary;
// the storage layer may keep a BLOB(16) internal representation. A PID is
// assigned once and preserved for the life of the entity (see package identity).
//
// A PID is matched as text, in the upper-case spelling NewPID writes. A ULID decodes
// in either case, but nothing here decodes one, so the lower-case spelling of an
// entity's PID names nothing.
type PID string

// String returns the PID as a plain string.
func (p PID) String() string { return string(p) }

// Valid reports whether p is a PID as the catalog writes one: a well-formed ULID in
// its upper-case spelling. Another spelling of the same ULID is not valid, since it
// would match no entity and, adopted for a new one, would be a second id for the first.
func (p PID) Valid() bool {
	id, err := ulid.ParseStrict(string(p))
	return err == nil && id.String() == string(p)
}

var (
	pidMu sync.Mutex
	// Monotonic entropy seeded from crypto/rand keeps PIDs strictly increasing
	// within a millisecond, preserving lexicographic == creation order.
	pidEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewPID mints a fresh, time-ordered PID. It is safe for concurrent use.
func NewPID() PID {
	pidMu.Lock()
	defer pidMu.Unlock()
	return PID(ulid.MustNew(ulid.Now(), pidEntropy).String())
}
