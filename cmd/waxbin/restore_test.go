package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/internal/testaudio"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/waxerr"
)

// runRestore runs `restore <backup> --force` against db the way main does, ending
// any maintenance hand-off afterwards. It passes no root: restore's own --root flag
// shadows the global one.
func runRestore(t *testing.T, db, backup string) error {
	t.Helper()
	g := &globals{}
	cmd := newRootCmd(g)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--db", db, "restore", backup, "--force"})
	err := cmd.ExecuteContext(context.Background())
	g.cleanup()
	return err
}

// backupWithUser builds a catalog holding one named user and backs it up.
func backupWithUser(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "b.db")})
	if err != nil {
		t.Fatalf("open backup source: %v", err)
	}
	defer lib.Close()
	if _, err := lib.CreateUser(ctx, name); err != nil {
		t.Fatalf("create user: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := lib.Backup(ctx, backup, port.BackupOptions{}); err != nil {
		t.Fatalf("backup: %v", err)
	}
	return backup
}

// serveCatalog opens db read-write under opts, advertising a socket, and serves it
// until the test ends.
func serveCatalog(t *testing.T, opts waxbin.Options) *waxbin.Library {
	t.Helper()
	ctx := context.Background()
	opts.IPCSocket = testsock.Path(t)
	lib, err := waxbin.Open(ctx, opts)
	if err != nil {
		t.Fatalf("open served catalog: %v", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- lib.Serve(sctx, opts.IPCSocket) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = lib.Close()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		px, err := proxy.Dial(opts.IPCSocket)
		if err == nil {
			perr := px.Ping(ctx)
			_ = px.Close()
			if perr == nil {
				return lib
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hasUser(users []*model.User, name string) bool {
	for _, u := range users {
		if u.Name == name {
			return true
		}
	}
	return false
}

// TestRestoreUnderAServerTakesTheHandoff: with a server holding the catalog, restore
// suspends it through the hand-off, swaps the file, and the server reopens on the
// restored catalog when the command ends.
func TestRestoreUnderAServerTakesTheHandoff(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		Roots: []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}}})
	if _, err := lib.CreateUser(context.Background(), "BeforeRestore"); err != nil {
		t.Fatal(err)
	}

	if err := runRestore(t, db, backupWithUser(t, "FromBackup")); err != nil {
		t.Fatalf("restore under a server: %v", err)
	}
	users, err := lib.Users(context.Background())
	if err != nil {
		t.Fatalf("the server did not come back: %v", err)
	}
	if !hasUser(users, "FromBackup") || hasUser(users, "BeforeRestore") {
		t.Fatalf("server users after the restore = %+v, want the backup's", users)
	}
}

// TestRestoreRefusesALiveOwner: a foreground process holding the catalog is not a
// server that can hand it off, so restore refuses rather than replace the file under it.
func TestRestoreRefusesALiveOwner(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	db := filepath.Join(t.TempDir(), "catalog.db")
	owner, err := waxbin.Open(context.Background(), waxbin.Options{DBPath: db})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := runRestore(t, db, backupWithUser(t, "FromBackup")); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("restore under a live owner = %v, want CodeConflict", err)
	}
	if users, err := owner.Users(context.Background()); err != nil || hasUser(users, "FromBackup") {
		t.Fatalf("owner's users = %+v (err %v), want the catalog untouched", users, err)
	}
}

// TestRestoreWaitsOutAServerJob: a server mid-job refuses the hand-off, and restore
// passes that refusal on unchanged, leaving the catalog alone.
func TestRestoreWaitsOutAServerJob(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	root := t.TempDir()
	db := filepath.Join(t.TempDir(), "catalog.db")
	if err := os.WriteFile(filepath.Join(root, "a.mp3"), testaudio.BuildMP3("One", "Artist", "Album", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	asked, release := make(chan struct{}, 1), make(chan struct{})
	blocking := &enrich.Mock{ProviderName: "slow", Caps: enrich.CapLyrics,
		EnrichFunc: func(context.Context, enrich.Request) (*enrich.Candidate, error) {
			asked <- struct{}{}
			<-release
			return nil, nil
		}}
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		Roots:               []config.Root{{Path: root, Mode: model.ModeManaged, Profile: "waxbin-native"}},
		EnrichmentProviders: []enrich.Provider{blocking}})
	if _, err := lib.Scan(ctx, waxbin.ScanRequest{}); err != nil {
		t.Fatal(err)
	}
	jobPID, err := lib.StartEnrich(ctx, waxbin.EnrichOptions{})
	if err != nil {
		t.Fatalf("start enrich: %v", err)
	}
	<-asked

	err = runRestore(t, db, backupWithUser(t, "FromBackup"))
	close(release)
	if !waxerr.Is(err, waxerr.CodeConflict) || !strings.Contains(err.Error(), "a background job is running; retry after it completes") {
		t.Fatalf("restore during a server job = %v, want the running-job refusal", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		job, err := lib.Job(ctx, jobPID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != model.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the enrichment job did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if users, err := lib.Users(ctx); err != nil || hasUser(users, "FromBackup") {
		t.Fatalf("users = %+v (err %v), want the catalog untouched", users, err)
	}
}

// TestRefusedRestoreLeavesTheServerAlone: a restore refused for its own arguments (no
// --force over an existing catalog, a backup that is not there) is refused before the
// hand-off, so the server keeps running and its consumers see no reopen.
func TestRefusedRestoreLeavesTheServerAlone(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "catalog.db")
	var reopens atomic.Int32
	lib := serveCatalog(t, waxbin.Options{DBPath: db,
		OnReopen: func(context.Context, waxbin.ReopenEvent) { reopens.Add(1) }})
	head, err := lib.LatestChangeSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backup := backupWithUser(t, "FromBackup")
	for name, args := range map[string][]string{
		"no --force":     {"--db", db, "restore", backup},
		"missing backup": {"--db", db, "restore", filepath.Join(t.TempDir(), "missing.db"), "--force"},
	} {
		g := &globals{}
		cmd := newRootCmd(g)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(ctx)
		g.cleanup()
		if err == nil {
			t.Fatalf("%s: restore succeeded, want a refusal", name)
		}
	}
	if n := reopens.Load(); n != 0 {
		t.Fatalf("refused restores cycled the server %d times", n)
	}
	if now, err := lib.LatestChangeSeq(ctx); err != nil || now != head {
		t.Fatalf("feed head = %d (err %v), want it still at %d", now, err, head)
	}
}

// TestRestoreReportsAServerThatCannotReopen: a backup that validates but that the
// server then fails to reopen leaves it closed, and the restore says so instead of
// reporting success.
func TestRestoreReportsAServerThatCannotReopen(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	db := filepath.Join(t.TempDir(), "catalog.db")
	serveCatalog(t, waxbin.Options{DBPath: db})
	backup := backupWithUser(t, "FromBackup")
	raw, err := sql.Open("sqlite", "file:"+backup)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA foreign_keys = OFF; DROP TABLE user"); err != nil {
		t.Fatalf("breaking the backup: %v", err)
	}
	_ = raw.Close()

	err = runRestore(t, db, backup)
	if err == nil || !strings.Contains(err.Error(), "could not reopen") {
		t.Fatalf("restore of a catalog the server cannot reopen = %v, want that reported", err)
	}
}

// TestRestoreRelocatesOnlyToAFolder: `restore --root` takes a relative path from where the
// command runs, and refuses a folder that is not there unless --allow-absent says it is
// mounted later.
func TestRestoreRelocatesOnlyToAFolder(t *testing.T) {
	t.Setenv("WAXBIN_CONFIG", "")
	ctx := context.Background()
	source := t.TempDir()
	lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: filepath.Join(t.TempDir(), "src.db"),
		Roots: []config.Root{{Path: source, Mode: model.ModeInPlace}}})
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := lib.Backup(ctx, backup, port.BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := lib.Close(); err != nil {
		t.Fatal(err)
	}
	restore := func(db string, args ...string) error {
		cmd := newRootCmd(&globals{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", db, "restore", backup}, args...))
		return cmd.ExecuteContext(ctx)
	}
	rootOf := func(db string) string {
		t.Helper()
		lib, err := waxbin.Open(ctx, waxbin.Options{DBPath: db, ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer lib.Close()
		libs, err := lib.Libraries(ctx)
		if err != nil || len(libs) != 1 {
			t.Fatalf("libraries = %+v (err %v)", libs, err)
		}
		return libs[0].DisplayRoot
	}

	base := t.TempDir()
	missing := filepath.Join(base, "not mounted")
	db := filepath.Join(t.TempDir(), "a.db")
	if err := restore(db, "--root", missing); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("restore --root to a missing folder = %v, want CodeInvalid", err)
	}
	if err := restore(db, "--root", missing, "--allow-absent", "--force"); err != nil {
		t.Fatalf("restore --root --allow-absent: %v", err)
	}
	if got := rootOf(db); got != missing {
		t.Errorf("relocated to %s, want %s", got, missing)
	}

	if err := os.Mkdir(filepath.Join(base, "music"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	db = filepath.Join(t.TempDir(), "b.db")
	if err := restore(db, "--root", "music"); err != nil {
		t.Fatalf("restore --root music: %v", err)
	}
	if got := rootOf(db); got != filepath.Join(base, "music") {
		t.Errorf("relocated to %s, want the folder under the working directory", got)
	}
}
