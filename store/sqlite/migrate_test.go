package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func mapFile(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

func TestLoadMigrationsDirectoryConcatenatesInNameOrder(t *testing.T) {
	fsys := fstest.MapFS{
		// Deliberately declared out of order; fs.ReadDir sorts by filename.
		"migrations/0001_init/02_second.sql": mapFile("-- two\n"),
		"migrations/0001_init/01_first.sql":  mapFile("-- one\n"),
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d migrations, want 1", len(ms))
	}
	if ms[0].version != 1 || ms[0].name != "init" {
		t.Errorf("got version=%d name=%q, want 1/init", ms[0].version, ms[0].name)
	}
	one, two := strings.Index(ms[0].sql, "-- one"), strings.Index(ms[0].sql, "-- two")
	if one < 0 || two < 0 || one > two {
		t.Errorf("directory files concatenated out of order: one@%d two@%d in %q", one, two, ms[0].sql)
	}
}

// TestDirectoryMigrationFileBoundariesAreStatementBoundaries executes a
// directory migration whose first file ends without ';' (and whose second ends
// in a trailing line comment): the join separator must terminate each file's
// last statement rather than letting it merge into the next file.
func TestDirectoryMigrationFileBoundariesAreStatementBoundaries(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_init/01_a.sql": mapFile("CREATE TABLE a (x INTEGER)"),
		"migrations/0001_init/02_b.sql": mapFile("CREATE TABLE b (y INTEGER);\n-- trailing comment"),
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), ms[0].sql); err != nil {
		t.Fatalf("executing concatenated migration: %v\nsql:\n%s", err, ms[0].sql)
	}
	for _, table := range []string{"a", "b"} {
		var n int
		if err := db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("table %q missing after directory migration", table)
		}
	}
}

func TestLoadMigrationsDirectorySkipsNonSQL(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_init/01_first.sql": mapFile("-- one\n"),
		"migrations/0001_init/README.md":    mapFile("not sql"),
		"migrations/0001_init/sub/x.sql":    mapFile("-- nested, skipped\n"),
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 1 {
		t.Fatalf("got %d migrations, want 1", len(ms))
	}
	if strings.Contains(ms[0].sql, "not sql") || strings.Contains(ms[0].sql, "nested") {
		t.Errorf("non-.sql or nested content leaked into migration SQL: %q", ms[0].sql)
	}
}

func TestLoadMigrationsEmptyDirectoryErrors(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/0001_init": &fstest.MapFile{Mode: fs.ModeDir},
	}
	if _, err := loadMigrations(fsys); err == nil ||
		!strings.Contains(err.Error(), "no .sql files") {
		t.Fatalf("want a no-.sql-files error for an empty migration directory, got %v", err)
	}

	// A directory holding only non-SQL files is just as empty.
	fsys = fstest.MapFS{
		"migrations/0001_init/notes.txt": mapFile("x"),
	}
	if _, err := loadMigrations(fsys); err == nil ||
		!strings.Contains(err.Error(), "no .sql files") {
		t.Fatalf("want a no-.sql-files error for a directory with no .sql, got %v", err)
	}
}

func TestLoadMigrationsDuplicateVersionErrors(t *testing.T) {
	// The duplicate spans both shapes: a directory and a single file.
	fsys := fstest.MapFS{
		"migrations/0001_init/01_first.sql": mapFile("-- one\n"),
		"migrations/0001_other.sql":         mapFile("-- other\n"),
	}
	if _, err := loadMigrations(fsys); err == nil ||
		!strings.Contains(err.Error(), "duplicate migration version 1") {
		t.Fatalf("want a duplicate-version error, got %v", err)
	}
}

func TestLoadMigrationsSortsNumerically(t *testing.T) {
	// Unpadded names order lexicographically as 10 < 2; version sorting must be
	// numeric.
	fsys := fstest.MapFS{
		"migrations/10_ten.sql": mapFile("-- ten\n"),
		"migrations/2_two.sql":  mapFile("-- two\n"),
	}
	ms, err := loadMigrations(fsys)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(ms) != 2 || ms[0].version != 2 || ms[1].version != 10 {
		t.Fatalf("got %+v, want versions [2 10]", ms)
	}
}

func TestLoadMigrationsSkipsAndRejectsBadNames(t *testing.T) {
	// A stray non-.sql file at the top level is ignored.
	fsys := fstest.MapFS{
		"migrations/0001_init.sql": mapFile("-- ok\n"),
		"migrations/README.md":     mapFile("ignored"),
	}
	ms, err := loadMigrations(fsys)
	if err != nil || len(ms) != 1 {
		t.Fatalf("got %d migrations, err=%v; want 1 migration, nil", len(ms), err)
	}

	// A .sql file without the NNNN_ prefix is a loud failure, not a skip.
	for name, wantMsg := range map[string]string{
		"migrations/0002.sql":  "bad migration name",
		"migrations/abc_x.sql": "bad migration version",
	} {
		fsys := fstest.MapFS{name: mapFile("-- x\n")}
		if _, err := loadMigrations(fsys); err == nil ||
			!strings.Contains(err.Error(), wantMsg) {
			t.Errorf("%s: want %q error, got %v", name, wantMsg, err)
		}
	}
}

// TestEmbeddedMigrationsMatchSchemaVersion pins the shipped stream to the build
// constant: the highest embedded migration version must equal SchemaVersion.
// Pre-1.0 that stream is exactly one directory migration, 0001_init, because
// schema changes are edited into the baseline rather than appended. This test
// fails first when someone appends a 0002 without bumping SchemaVersion, or
// bumps the constant without shipping the migration.
func TestEmbeddedMigrationsMatchSchemaVersion(t *testing.T) {
	ms, err := loadMigrations(migrationsFS)
	if err != nil {
		t.Fatalf("loadMigrations(embedded): %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	if got := ms[len(ms)-1].version; got != SchemaVersion {
		t.Errorf("highest embedded migration is v%d but SchemaVersion is %d", got, SchemaVersion)
	}
	if SchemaVersion == 1 && (len(ms) != 1 || ms[0].name != "init") {
		t.Errorf("pre-release stream must be the single 0001_init baseline; got %d migrations", len(ms))
	}
	if strings.TrimSpace(ms[0].sql) == "" {
		t.Error("baseline migration is empty")
	}
}
