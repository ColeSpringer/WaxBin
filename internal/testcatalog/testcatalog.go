// Package testcatalog hands tests an empty migrated catalog. The schema is built once per
// test binary and copied, since building it through a race-built SQLite costs about a
// quarter of a second of CPU per catalog.
package testcatalog

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/colespringer/waxbin/store/sqlite"
)

// seed holds the bytes of a migrated catalog that no open has used yet: a first open
// leaves its default user and that user's change row behind, and a copy of those would
// hand every test the same user pid, so they are taken out and each catalog's own first
// open makes them, as it would in a catalog it created.
var seed = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "waxbin-seed")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "seed.db")
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: path, Owner: "seed"})
	if err != nil {
		return nil, err
	}
	// Close checkpoints the WAL, so the one file carries the whole catalog.
	if err := st.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec("DELETE FROM user; DELETE FROM change_log; DELETE FROM sqlite_sequence")
	// The last connection to close checkpoints the deletes into the file.
	if err = errors.Join(err, db.Close()); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// Seed puts an empty migrated catalog at path when nothing is there yet, where a first
// read-write open would create one, and returns path. An existing file is left alone, so
// a helper that also reopens catalogs can seed every path it is handed. A read-only open
// of a missing catalog must not be seeded: it fails, and a test may mean it to.
func Seed(tb testing.TB, path string) string {
	tb.Helper()
	if err := seedAt(path); err != nil {
		tb.Fatalf("seed %s: %v", path, err)
	}
	return path
}

// seedAt writes the seed to path unless something is there. The file appears whole or
// not at all, so two openers of one fresh path never write under each other, and a WAL
// with no catalog beside it is refused rather than replayed onto the seed.
func seedAt(path string) error {
	if path == "" {
		return errors.New("no path")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := os.Lstat(path + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("a WAL is there without its catalog")
	}
	blob, err := seed()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".seed-*")
	if err != nil {
		return err
	}
	// A temp file a scanner still holds open lingers in the test's own folder and goes
	// with it, which is no reason to fail the seed.
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, err = tmp.Write(blob)
	if err = errors.Join(err, tmp.Close()); err != nil {
		return err
	}
	// A link fails when the path exists, so only one seeder publishes it.
	if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}
