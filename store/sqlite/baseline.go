package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io/fs"
	"sync"

	"github.com/colespringer/waxbin/waxerr"
)

// The baseline guard. Pre-1.0 the v1 baseline is edited in place rather than
// migrated, so a catalog created by an earlier build can lack columns this build
// writes to while still reporting schema v1. Stamping the baseline's fingerprint
// into the catalog turns that into a refusal at open instead of a "no such column"
// from whichever write reaches the new column first.

// baselineFingerprint identifies the v1 baseline fsys ships: SHA-256 over its SQL
// as written, truncated to four bytes so it fits PRAGMA user_version.
//
// Version 1 specifically, never the highest version: once 0002 lands, hashing the
// highest would compare hash(0002) against a correctly migrated v1 catalog holding
// hash(0001) and refuse the catalog it should be migrating.
func baselineFingerprint(fsys fs.FS) (int32, error) {
	const op = "store.baseline"
	ms, err := loadMigrations(fsys)
	if err != nil {
		return 0, err
	}
	if len(ms) == 0 || ms[0].version != 1 {
		return 0, waxerr.New(waxerr.CodeInternal, op, "migration stream has no version-1 baseline")
	}
	sum := sha256.Sum256([]byte(ms[0].sql))
	return int32(binary.BigEndian.Uint32(sum[:4])), nil
}

// buildBaseline is this build's fingerprint, computed once per process.
var buildBaseline = sync.OnceValues(func() (int32, error) { return baselineFingerprint(migrationsFS) })

// BaselineFingerprint is the fingerprint this build stamps into a catalog it
// creates, exported so port can check a backup before restoring it.
func BaselineFingerprint() (int32, error) { return buildBaseline() }

// baselineStamp reads the fingerprint a catalog was built from. An unstamped
// catalog reads 0, which is already a mismatch.
func baselineStamp(ctx context.Context, q queryer) (int32, error) {
	var v int64
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.baseline", err)
	}
	return int32(v), nil
}

// setBaselineStamp records fp on a freshly created catalog. It joins the enclosing
// transaction, so the stamp and the schema_migrations row commit together. The
// pragma takes no bound parameters, hence the formatted value.
func setBaselineStamp(ctx context.Context, tx *sql.Tx, fp int32) error {
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", fp)); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.baseline", err)
	}
	return nil
}

// staleBaselineError names the cure as well as the cause: there is no migration to
// run, so the catalog has to be re-created.
func staleBaselineError(op string, got, want int32) error {
	return waxerr.New(waxerr.CodeUnsupported, op, fmt.Sprintf(
		"catalog was built from a different schema baseline (catalog %08x, this build %08x); "+
			"pre-1.0 the baseline is edited in place rather than migrated, so re-create the catalog "+
			"with `waxbin db reset` and then `waxbin rebuild`", uint32(got), uint32(want)))
}
