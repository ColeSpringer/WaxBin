// Package sqlite is WaxBin's SQLite-only DataStore. It implements model.Catalog
// and model.JobStore over modernc.org/sqlite (pure Go, no CGO) and owns the
// write ownership model: a single write connection serialized behind a mutex
// (the in-process write coordinator) plus an OS advisory flock on a lockfile
// for cross-process/cross-container ownership.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	_ "modernc.org/sqlite"
)

const driverName = "sqlite"

// OpenOptions configures a Store.
type OpenOptions struct {
	Path      string // catalog DB path (local filesystem only)
	ReadOnly  bool   // read-only consumers take no lock and never migrate
	Owner     string // write-owner identity recorded in the lockfile and jobs
	IPCSocket string // optional IPC socket path advertised in the lockfile
	Logger    *slog.Logger

	BusyTimeoutMS int
	CacheSizeKB   int
	MmapSizeBytes int64
	ReadPoolSize  int
}

// Store is the SQLite-backed catalog. It is safe for concurrent use: writes go
// through the single coordinated write connection; reads use a connection pool.
type Store struct {
	path     string
	read     *sql.DB    // read pool (set once in Open; never reassigned)
	write    *sql.DB    // single write connection (nil when read-only)
	wmu      sync.Mutex // serializes write transactions; also guards closed
	closed   bool       // guarded by wmu
	lock     *writeLock // held advisory lock (nil when read-only)
	readOnly bool
	owner    string
	log      *slog.Logger

	subMu sync.Mutex                     // guards subs
	subs  map[chan model.Change]struct{} // in-process change_log listeners

	dvMu   sync.Mutex // guards dvConn
	dvConn *sql.Conn  // pinned connection for PRAGMA data_version polling
}

// Open opens (creating if needed) the catalog at opt.Path. A read-write open
// acquires the exclusive flock, runs migrations, and reclaims orphaned jobs; a
// read-only open requires an existing DB, takes no lock, and never writes.
func Open(ctx context.Context, opt OpenOptions) (*Store, error) {
	const op = "store.Open"
	if strings.TrimSpace(opt.Path) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "empty db path")
	}
	if opt.BusyTimeoutMS <= 0 {
		opt.BusyTimeoutMS = 10000
	}
	if opt.ReadPoolSize <= 0 {
		opt.ReadPoolSize = 8
	}
	if opt.CacheSizeKB <= 0 {
		opt.CacheSizeKB = 16000 // ~16 MiB page cache
	}
	if opt.MmapSizeBytes <= 0 {
		opt.MmapSizeBytes = 256 << 20 // 256 MiB mmap window
	}
	log := opt.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	s := &Store{path: opt.Path, readOnly: opt.ReadOnly, owner: opt.Owner, log: log}

	if opt.ReadOnly {
		if _, err := os.Stat(opt.Path); err != nil {
			return nil, waxerr.Wrapf(waxerr.CodeNotFound, op, err, "opening read-only %s", opt.Path)
		}
		rdb, err := openDB(ctx, roDSN(opt), opt.ReadPoolSize)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		s.read = rdb
		if err := s.verifyReadable(ctx); err != nil {
			_ = s.Close()
			return nil, err
		}
		return s, nil
	}

	lock, err := acquireWriteLock(opt.Path+".waxlock", opt.Owner, opt.IPCSocket, nowNS())
	if err != nil {
		return nil, err
	}
	s.lock = lock

	wdb, err := openDB(ctx, rwDSN(opt), 1)
	if err != nil {
		_ = lock.release()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	s.write = wdb

	rdb, err := openDB(ctx, readDSN(opt), opt.ReadPoolSize)
	if err != nil {
		_ = s.Close()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	s.read = rdb

	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}

	// We hold the exclusive flock, so any job still marked running belongs to a
	// dead prior owner: reclaim it (flock-based liveness, no PID checks).
	if n, err := s.ReclaimOrphans(ctx, nowNS()); err != nil {
		_ = s.Close()
		return nil, err
	} else if n > 0 {
		log.Info("reclaimed orphaned jobs on open", "count", n)
	}

	// Finish or roll back any move a prior owner crashed mid-flight (planned but
	// never committed/aborted). Same flock-liveness reasoning as job reclaim.
	if n, err := s.recoverOrganize(ctx); err != nil {
		_ = s.Close()
		return nil, err
	} else if n > 0 {
		log.Info("recovered interrupted organize moves on open", "count", n)
	}

	// Seed the default playback user so single-user setups need no configuration.
	if err := s.ensureDefaultUser(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}

	return s, nil
}

// Close releases the read/write connections and the advisory lock. It first
// attempts a WAL checkpoint so the main DB file is self-contained for backups and
// read-only consumers.
func (s *Store) Close() error {
	// Mark closed under wmu so an in-flight writeTx (which holds wmu for its whole
	// duration and checks closed) cannot be mid-transaction here; checkpoint while
	// still holding it. The connection fields are not nil'd; a racing reader hits
	// a closed *sql.DB and gets an error rather than a nil-pointer dereference.
	s.wmu.Lock()
	if s.closed {
		s.wmu.Unlock()
		return nil
	}
	s.closed = true
	if s.write != nil {
		_, _ = s.write.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	s.wmu.Unlock()

	// Close in-process change listeners so their range loops terminate.
	s.closeSubscribers()
	s.closeDataVersionConn()

	var errs []error
	if s.write != nil {
		errs = append(errs, s.write.Close())
	}
	if s.read != nil {
		errs = append(errs, s.read.Close())
	}
	if s.lock != nil {
		errs = append(errs, s.lock.release())
	}
	return errors.Join(errs...)
}

// ReadOnly reports whether the store was opened read-only.
func (s *Store) ReadOnly() bool { return s.readOnly }

// OwnerInfo returns the current lockfile owner metadata (read-write opens only).
func (s *Store) OwnerInfo() (OwnerInfo, error) {
	return readOwnerInfo(s.path + ".waxlock")
}

// writeTx runs fn inside a single serialized write transaction. It is the only
// path that mutates the database (the write-coordinator). fn must not retain the
// *sql.Tx beyond the call.
func (s *Store) writeTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.readOnly || s.write == nil {
		return waxerr.New(waxerr.CodeUnsupported, "store.writeTx", "library opened read-only")
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return waxerr.New(waxerr.CodeUnsupported, "store.writeTx", "store is closed")
	}

	// With in-process listeners, snapshot the change_log head so rows appended by
	// this transaction can be published after commit. The common CLI path has no
	// subscribers and pays no publish cost.
	notify := s.hasSubscribers()
	var preSeq int64
	if notify {
		preSeq = s.maxChangeSeq(ctx)
	}

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.writeTx", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.writeTx", err)
	}
	if notify {
		// Publish committed rows from a background context. If the caller's context
		// is canceled just after commit, in-process listeners should still receive
		// the deltas instead of waiting for a later DataVersion poll.
		s.publishSince(context.Background(), preSeq)
	}
	return nil
}

func openDB(ctx context.Context, dsn string, maxConns int) (*sql.DB, error) {
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(0)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// commonPragmas are the per-connection pragmas shared by every DSN (the writer
// additionally sets journal_mode + synchronous).
func commonPragmas(opt OpenOptions) []string {
	return []string{
		pragma("busy_timeout", fmt.Sprint(opt.BusyTimeoutMS)),
		pragma("foreign_keys", "ON"),
		pragma("temp_store", "MEMORY"),
		pragma("cache_size", fmt.Sprint(-opt.CacheSizeKB)), // negative => KiB
		pragma("mmap_size", fmt.Sprint(opt.MmapSizeBytes)),
	}
}

// rwDSN is the DSN for the single write connection: full pragma set including
// WAL and NORMAL synchronous.
func rwDSN(opt OpenOptions) string {
	p := append([]string{pragma("journal_mode", "WAL"), pragma("synchronous", "NORMAL")}, commonPragmas(opt)...)
	return "file:" + opt.Path + "?" + strings.Join(p, "&")
}

// readDSN is the DSN for the read pool against a writable DB file (same process
// as the writer). It does not set journal_mode (inherited from the file header).
func readDSN(opt OpenOptions) string {
	return "file:" + opt.Path + "?" + strings.Join(commonPragmas(opt), "&")
}

// roDSN is the DSN for a read-only consumer process.
func roDSN(opt OpenOptions) string {
	return "file:" + opt.Path + "?mode=ro&" + strings.Join(commonPragmas(opt), "&")
}

func pragma(name, value string) string { return "_pragma=" + name + "(" + value + ")" }

func nowNS() int64 { return time.Now().UnixNano() }
