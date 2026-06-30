package sqlite

import (
	"database/sql/driver"
	"encoding/binary"
	"hash/fnv"

	msqlite "modernc.org/sqlite"
)

// init registers WaxBin's custom SQLite scalar functions. modernc makes them
// available to every connection opened afterwards, and init runs before any Open,
// so the read pool and write connection both see them.
func init() {
	// wb_shuffle(seed, key) is a deterministic hash used to order a "random"
	// discovery-list browse by a consumer-supplied seed. WaxBin is session-less, so
	// the seed is what keeps a paginated shuffle stable across pages: the same seed
	// yields the same order, and keyset pagination resumes after the cursor's hash.
	msqlite.MustRegisterDeterministicScalarFunction("wb_shuffle", 2, wbShuffle)
}

// wbShuffle returns a non-negative 63-bit FNV-1a hash of (seed, key). Masking off
// the sign bit keeps ORDER BY and the keyset cursor comparison unambiguous (all
// values are non-negative integers), and determinism lets SQLite use it in both
// the ORDER BY and the WHERE keyset of the same paginated query.
func wbShuffle(_ *msqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	var seed int64
	if v, ok := args[0].(int64); ok {
		seed = v
	}
	var key string
	switch v := args[1].(type) {
	case string:
		key = v
	case []byte:
		key = string(v)
	}
	h := fnv.New64a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seed))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64() & 0x7fffffffffffffff), nil
}
