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
type PID string

// String returns the PID as a plain string.
func (p PID) String() string { return string(p) }

// Valid reports whether p parses as a well-formed ULID.
func (p PID) Valid() bool {
	_, err := ulid.ParseStrict(string(p))
	return err == nil
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
