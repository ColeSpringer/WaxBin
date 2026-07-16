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
	opt      OpenOptions // normalized open options, retained so Reopen rebuilds the same DSNs
	read     *sql.DB     // read pool (reopened in place by Reopen)
	write    *sql.DB     // single write connection (nil when read-only)
	wmu      sync.Mutex  // serializes write transactions; also guards closed
	closed   bool        // guarded by wmu
	lock     *writeLock  // held advisory lock (nil when read-only)
	readOnly bool
	owner    string
	log      *slog.Logger

	thumbMem *thumbCache // in-process cache of generated thumbnails (see art.go)

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

	s := &Store{
		path: opt.Path, opt: opt, readOnly: opt.ReadOnly, owner: opt.Owner, log: log,
		thumbMem: newThumbCache(thumbCacheMax),
	}

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

// Close releases the read/write connections and the advisory lock, and closes
// in-process change listeners so their range loops terminate. It first attempts a
// WAL checkpoint so the main DB file is self-contained for backups and read-only
// consumers.
func (s *Store) Close() error { return s.teardown(true) }

// Suspend is Close for a maintenance-mode hand-off: it checkpoints, releases the
// lock, and closes the connections, but KEEPS the in-process change subscribers
// registered so an embedder's subscription survives the hand-off and resumes
// delivering after Reopen. (A full Close would close those channels, terminating
// the embedder's range loop with no way to re-establish it.)
func (s *Store) Suspend() error { return s.teardown(false) }

// teardown closes the store, optionally closing change subscribers. closeSubs is
// true for a full Close and false for a maintenance Suspend.
func (s *Store) teardown(closeSubs bool) error {
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

	if closeSubs {
		// Close in-process change listeners so their range loops terminate.
		s.closeSubscribers()
	}
	// The pinned data_version connection is drawn from the read pool; drop it either
	// way. DataVersion re-pins a fresh one lazily after a reopen.
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

// Reopen re-acquires the write lock and reopens the connections of a Store that
// was Closed for a maintenance-mode hand-off, restoring it in place so every
// subsystem that still holds this *Store keeps working. It is the inverse of Close
// for the read-write path; a read-only store cannot be reopened this way, and a
// store that is already open is a no-op.
//
// The lock re-acquire retries with bounded backoff because a foreground process
// may still be releasing the flock as the hand-off ends. migrate runs again so a
// restore/rebuild that replaced the DB file mid-hand-off is brought current; the
// rest mirrors Open's read-write reconciliation.
func (s *Store) Reopen(ctx context.Context) error {
	const op = "store.Reopen"
	if s.readOnly {
		return waxerr.New(waxerr.CodeUnsupported, op, "a read-only store cannot be reopened")
	}
	s.wmu.Lock()
	closed := s.closed
	s.wmu.Unlock()
	if !closed {
		return nil
	}

	// Acquire the lock and open the connections without holding wmu: the retry can
	// sleep, and the reconciliation steps below take wmu themselves via writeTx.
	lock, err := acquireWriteLockRetry(ctx, s.opt.Path+".waxlock", s.opt.Owner, s.opt.IPCSocket)
	if err != nil {
		return err
	}
	wdb, err := openDB(ctx, rwDSN(s.opt), 1)
	if err != nil {
		_ = lock.release()
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	rdb, err := openDB(ctx, readDSN(s.opt), s.opt.ReadPoolSize)
	if err != nil {
		_ = wdb.Close()
		_ = lock.release()
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	s.wmu.Lock()
	if !s.closed {
		// Raced with a concurrent Reopen/Open that already restored the store; drop
		// the connections and lock we just took.
		s.wmu.Unlock()
		_ = rdb.Close()
		_ = wdb.Close()
		_ = lock.release()
		return nil
	}
	s.lock, s.write, s.read = lock, wdb, rdb
	s.closed = false
	s.wmu.Unlock()

	// The store is open again; run the same post-open reconciliation as Open. On any
	// failure, Close tears the half-restored store back down so it stays cleanly
	// closed (Close is safe here because s.closed is now false).
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return err
	}
	if n, err := s.ReclaimOrphans(ctx, nowNS()); err != nil {
		_ = s.Close()
		return err
	} else if n > 0 {
		s.log.Info("reclaimed orphaned jobs on reopen", "count", n)
	}
	if n, err := s.recoverOrganize(ctx); err != nil {
		_ = s.Close()
		return err
	} else if n > 0 {
		s.log.Info("recovered interrupted organize moves on reopen", "count", n)
	}
	if err := s.ensureDefaultUser(ctx); err != nil {
		_ = s.Close()
		return err
	}
	return nil
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
