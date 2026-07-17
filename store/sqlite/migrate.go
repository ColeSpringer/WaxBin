package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/waxerr"
)

//go:embed migrations
var migrationsFS embed.FS

// SchemaVersion is the highest migration version this build ships. A read-only
// open against a newer DB is refused (it may rely on schema the binary lacks).
const SchemaVersion = 1

type migration struct {
	version int
	name    string
	sql     string
}

// migrate brings the catalog up to SchemaVersion, applying each pending
// migration in its own transaction. If the DB already holds data (version > 0),
// it is byte-copied to a backup via VACUUM INTO before the first migration.
func (s *Store) migrate(ctx context.Context) error {
	const op = "store.migrate"
	if _, err := s.write.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	current, err := s.currentVersion(ctx)
	if err != nil {
		return err
	}
	// A catalog newer than this build (e.g. after a downgrade) has no pending
	// migrations, so guard explicitly. An older read-write binary must not write
	// to a schema it does not understand. Mirrors verifyReadable.
	if current > SchemaVersion {
		return waxerr.New(waxerr.CodeUnsupported, op,
			fmt.Sprintf("catalog schema v%d is newer than this build supports (v%d)", current, SchemaVersion))
	}

	all, err := loadMigrations(migrationsFS)
	if err != nil {
		return err
	}
	var pending []migration
	for _, m := range all {
		if m.version > current {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	if current > 0 {
		backup := fmt.Sprintf("%s.pre-migrate-%d.bak", s.path, current)
		if _, err := s.write.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return waxerr.Wrapf(waxerr.CodeIO, op, err, "backing up to %s before migrate", backup)
		}
		// The pre-migrate backup carries the secret table, so restrict it like the
		// live catalog.
		s.restrictSecretFiles(backup)
		s.log.Info("backed up catalog before migration", "to", backup, "from_version", current)
	}

	for _, m := range pending {
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return waxerr.Wrapf(waxerr.CodeIO, op, err, "applying migration %04d_%s", m.version, m.name)
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)",
				m.version, m.name, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		s.log.Info("applied migration", "version", m.version, "name", m.name)
	}
	return nil
}

// verifyReadable ensures a read-only open is not against a DB newer than this
// build understands.
func (s *Store) verifyReadable(ctx context.Context) error {
	var v int
	err := s.read.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		// A missing schema_migrations table means an uninitialized DB; any other
		// failure (corruption, permissions, truncation) is a real I/O error and
		// must not be reported as "run init".
		if strings.Contains(err.Error(), "no such table") {
			return waxerr.New(waxerr.CodeInvalid, "store.Open",
				"catalog is not initialized (run `waxbin init`)")
		}
		return waxerr.Wrap(waxerr.CodeIO, "store.Open", err)
	}
	if v > SchemaVersion {
		return waxerr.New(waxerr.CodeUnsupported, "store.Open",
			fmt.Sprintf("catalog schema v%d is newer than this build supports (v%d)", v, SchemaVersion))
	}
	return nil
}

// CatalogVersion returns the catalog's current applied migration version using
// the read pool, so it is safe on a read-only open (which never migrates).
// Returns 0 for an uninitialized catalog. doctor reports the catalog's actual
// version, distinct from this build's SchemaVersion, so a read-only diagnostic
// against an older catalog does not fail on a table from a pending migration.
func (s *Store) CatalogVersion(ctx context.Context) (int, error) {
	var v int
	err := s.read.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return 0, nil
		}
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.CatalogVersion", err)
	}
	return v, nil
}

func (s *Store) currentVersion(ctx context.Context) (int, error) {
	var v int
	if err := s.write.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.migrate", err)
	}
	return v, nil
}

// loadMigrations reads the migration stream from fsys (the embedded FS outside
// of tests). A migration is either a single "NNNN_name.sql" file or an
// "NNNN_name" directory of .sql files split by purpose. A directory is one
// migration all the same: its files run in filename order inside the same
// transaction and record a single schema_migrations row. The v1 baseline uses
// the directory shape; post-1.0 changes are expected to be single files.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "store.migrate", err)
	}
	var ms []migration
	for _, e := range entries {
		var ver int
		var name, sql string
		switch {
		case e.IsDir():
			var err error
			if ver, name, err = parseMigrationName(e.Name()); err != nil {
				return nil, err
			}
			if sql, err = readMigrationDir(fsys, path.Join("migrations", e.Name())); err != nil {
				return nil, err
			}
		case strings.HasSuffix(e.Name(), ".sql"):
			var err error
			if ver, name, err = parseMigrationName(strings.TrimSuffix(e.Name(), ".sql")); err != nil {
				return nil, err
			}
			data, err := fs.ReadFile(fsys, path.Join("migrations", e.Name()))
			if err != nil {
				return nil, waxerr.Wrap(waxerr.CodeInternal, "store.migrate", err)
			}
			sql = string(data)
		default:
			continue
		}
		ms = append(ms, migration{version: ver, name: name, sql: sql})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, waxerr.New(waxerr.CodeInternal, "store.migrate",
				fmt.Sprintf("duplicate migration version %d (%s, %s)",
					ms[i].version, ms[i-1].name, ms[i].name))
		}
	}
	return ms, nil
}

// readMigrationDir concatenates a directory migration's .sql files in filename
// order (fs.ReadDir sorts), so the numeric prefix inside the directory controls
// table-creation order without carrying version meaning.
//
// Files are joined with "\n;\n" so a file boundary is always a statement
// boundary: a final statement missing its ';' ends at its own file instead of
// merging into the next one, and no statement can span two files. The newline
// keeps the ';' out of a trailing line comment, and the extra ';' after a
// well-terminated file is an empty statement SQLite skips.
func readMigrationDir(fsys fs.FS, dir string) (string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return "", waxerr.Wrap(waxerr.CodeInternal, "store.migrate", err)
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return "", waxerr.Wrap(waxerr.CodeInternal, "store.migrate", err)
		}
		b.Write(data)
		b.WriteString("\n;\n")
	}
	if b.Len() == 0 {
		return "", waxerr.New(waxerr.CodeInternal, "store.migrate",
			"migration directory holds no .sql files: "+dir)
	}
	return b.String(), nil
}

// parseMigrationName splits "0001_init" into (1, "init"). The caller strips any
// ".sql" suffix first.
func parseMigrationName(base string) (int, string, error) {
	i := strings.IndexByte(base, '_')
	if i <= 0 {
		return 0, "", waxerr.New(waxerr.CodeInternal, "store.migrate",
			"bad migration name: "+base)
	}
	ver, err := strconv.Atoi(base[:i])
	if err != nil {
		return 0, "", waxerr.Wrapf(waxerr.CodeInternal, "store.migrate", err,
			"bad migration version in %s", base)
	}
	return ver, base[i+1:], nil
}
