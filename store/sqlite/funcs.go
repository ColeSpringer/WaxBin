package sqlite

import (
	"database/sql/driver"
	"encoding/binary"
	"hash/fnv"

	"github.com/colespringer/waxbin/identity"
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
	// wb_sole_credit(credit, key) reports whether a music credit names one artist alone:
	// it splits to one name, or the whole credit folds to key, the match key of the
	// artist a credit resolving as one entity is named for (an artist's derived sort
	// spelling, entityspelling.go).
	msqlite.MustRegisterDeterministicScalarFunction("wb_sole_credit", 2, wbSoleCredit)
}

func wbSoleCredit(_ *msqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	credit, key := sqlText(args[0]), sqlText(args[1])
	if len(identity.SplitPerformerCredit(credit)) == 1 || identity.MatchKey(credit) == key {
		return int64(1), nil
	}
	return int64(0), nil
}

// sqlText reads a text argument, "" for any other type.
func sqlText(v driver.Value) string {
	switch v := v.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
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
	key := sqlText(args[1])
	h := fnv.New64a()
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(seed))
	_, _ = h.Write(b[:])
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64() & 0x7fffffffffffffff), nil
}
