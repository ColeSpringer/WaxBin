// Package sqlite is WaxBin's SQLite-only DataStore. It implements model.Catalog
// and model.JobStore over modernc.org/sqlite (pure Go, no CGO) and owns the
// write ownership model: a single write connection serialized behind a mutex
// (the in-process write coordinator) plus an OS advisory flock on a lockfile
// for cross-process/cross-container ownership.
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

	// AllowStaleBaseline downgrades the stale-baseline refusal to a logged warning so
	// the catalog can be read for salvage. Read-only opens only: a read-write open is
	// refused regardless, since the write path is where a missing column costs data.
	AllowStaleBaseline bool

	// SecretCipher, when set, seals secret-table values at rest. When nil the
	// store keeps secrets in plaintext (standalone CLI). Like Logger, it is a live
	// injected object held on the Store and preserved across Reopen.
	SecretCipher model.SecretCipher
	// SecretKeyID labels the key/epoch a sealed value was written under, so a
	// rotation can be told apart from the prior generation. Defaults to "1" when a
	// cipher is set without one; ignored when no cipher is configured.
	SecretKeyID string

	BusyTimeoutMS int
	CacheSizeKB   int
	MmapSizeBytes int64
	ReadPoolSize  int
}

// Store is the SQLite-backed catalog. It is safe for concurrent use: writes go
// through the single coordinated write connection; reads use a connection pool.
type Store struct {
	path string
	opt  OpenOptions // normalized open options, retained so Reopen rebuilds the same DSNs
	// gen is the connection set in use, read through rdb and wdb. Reopen publishes a new
	// one whole, so a reader never sees half of a reopen, and a suspend or close publishes
	// closedGen.
	gen    atomic.Pointer[generation]
	wmu    sync.Mutex // serializes write transactions; also guards closed
	closed bool       // guarded by wmu
	// handoffMu serializes Suspend, Close and Reopen, so a call made while a reopen runs
	// takes effect after it.
	handoffMu sync.Mutex
	// stopMu guards closing, the Close calls waiting for handoffMu, and stopReopen, which
	// cancels the Reopen in flight, so a Close never waits out a reopen's lock retry.
	stopMu     sync.Mutex
	closing    int
	stopReopen context.CancelCauseFunc
	// suspended mirrors closed for the work a method does ahead of its transaction, which
	// runs off wmu.
	suspended atomic.Bool
	readOnly  bool
	// allowStale warns instead of refusing on a baseline mismatch; read-only opens
	// only (migrate never consults it).
	allowStale bool
	owner      string
	log        *slog.Logger

	cipher      model.SecretCipher // seals/opens secret-table values (nil = plaintext)
	cipherKeyID string             // key/epoch label stamped into a sealed value

	rstmts stmtCache // prepared hot reads on the read pool

	examined examinedMem // the oversized pictures examined lately, see art.go

	thumbMem    *thumbCache  // in-process cache of generated thumbnails (see art.go)
	thumbFail   *thumbCache  // in-process record of sources that failed to generate
	thumbFlight *thumbFlight // one generation per (source, rung), however many callers want it

	subMu sync.Mutex                     // guards subs
	subs  map[chan model.Change]struct{} // in-process change_log listeners

	// dvConn is the pinned connection DataVersion reads PRAGMA data_version from, dvRaw
	// the last value it read there (-1 while none is pinned), and dvOut the counter
	// DataVersion returns, bumped whenever the pragma moves or the connection is
	// replaced. dvMu guards all three.
	dvMu   sync.Mutex
	dvConn *sql.Conn
	dvRaw  int64
	dvOut  int64

	// roFile is the catalog file a read-only store opened, so DataVersion can tell when
	// a restore renamed another file over the path.
	roFile os.FileInfo

	// mark is what Suspend left for Reopen to compare against, kept until a reopen
	// succeeds. wmu guards it.
	mark *suspendMark
}

// generation is one set of connections an open or a reopen took together: the pools and
// the write lock. A suspend or close takes the set down as a whole.
type generation struct {
	read  *sql.DB
	write *sql.DB    // nil when read-only
	lock  *writeLock // nil when read-only
}

func (g *generation) closePools() error {
	var errs []error
	if g.write != nil {
		errs = append(errs, g.write.Close())
	}
	return errors.Join(append(errs, g.read.Close())...)
}

// closedGen is what a suspended or closed store publishes in place of its connections:
// one handle that refuses every call with the CodeUnsupported a write gets, so a read
// during a hand-off is told the store is closed rather than failing as I/O. It is built
// at package init, so the goroutine its pool runs never belongs to a synctest bubble.
var closedGen = func() *generation {
	db := sql.OpenDB(refusing{})
	return &generation{read: db, write: db}
}()

// refusing is the connector behind closedGen's handle.
type refusing struct{}

func (refusing) Connect(context.Context) (driver.Conn, error) { return nil, errStoreClosed() }
func (refusing) Driver() driver.Driver                        { return refusing{} }
func (refusing) Open(string) (driver.Conn, error)             { return nil, errStoreClosed() }

func errStoreClosed() error { return waxerr.New(waxerr.CodeUnsupported, "sqlite", "store is closed") }

// rdb is the read pool.
func (s *Store) rdb() *sql.DB { return s.gen.Load().read }

// wdb is the single write connection, nil while a read-only store is open.
func (s *Store) wdb() *sql.DB { return s.gen.Load().write }

// settle waits up to bound for g's connections to close, polling the pools, and returns
// how many are still open. A closed pool closes an in-use connection when its user
// releases it, so this is the wait for the reads still running on g.
func (g *generation) settle(bound time.Duration) int {
	deadline := time.Now().Add(bound)
	for {
		n := g.read.Stats().OpenConnections
		if g.write != nil {
			n += g.write.Stats().OpenConnections
		}
		if n == 0 || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openPools opens the write connection and the read pool over lock, releasing the lock
// when either fails.
func (s *Store) openPools(ctx context.Context, op string, lock *writeLock) (*generation, error) {
	w, err := openDB(ctx, rwDSN(s.opt), 1)
	if err != nil {
		_ = lock.release()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	r, err := openDB(ctx, readDSN(s.opt), s.opt.ReadPoolSize)
	if err != nil {
		_ = w.Close()
		_ = lock.release()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return &generation{read: r, write: w, lock: lock}, nil
}

// suspendMark records the catalog a suspend closed: its file, and the change feed's
// head then (-1 when it could not be read). replaced is set once a reopen has found
// another catalog, so a retry after a failed reopen still says so.
type suspendMark struct {
	file     os.FileInfo
	head     int64
	replaced bool
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

	keyID := opt.SecretKeyID
	if opt.SecretCipher != nil {
		if keyID == "" {
			keyID = defaultSecretKeyID
		}
		if err := validateSecretKeyID(keyID); err != nil {
			return nil, err
		}
	}

	s := &Store{
		path: opt.Path, opt: opt, readOnly: opt.ReadOnly, owner: opt.Owner, log: log,
		allowStale: opt.AllowStaleBaseline,
		cipher:     opt.SecretCipher, cipherKeyID: keyID,
		thumbMem:    newThumbCache(thumbCacheMax, thumbCacheBytes),
		thumbFail:   newThumbCache(thumbFailMax, thumbFailBytes),
		thumbFlight: newThumbFlight(),
		dvRaw:       -1,
	}

	if opt.ReadOnly {
		fi, err := os.Stat(opt.Path)
		if err != nil {
			return nil, waxerr.Wrapf(waxerr.CodeNotFound, op, err, "opening read-only %s", opt.Path)
		}
		// Windows reads a file's identity at its first comparison, so compare now, while
		// the file at the path is the one being opened.
		_ = os.SameFile(fi, fi)
		s.roFile = fi
		r, err := openDB(ctx, roDSN(opt), opt.ReadPoolSize)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		s.gen.Store(&generation{read: r})
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
	g, err := s.openPools(ctx, op, lock)
	if err != nil {
		return nil, err
	}
	s.gen.Store(g)

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

	// Restrict the secret-bearing files to owner-only. In plaintext mode this is
	// the only at-rest protection a stored password has; in ciphered mode it is
	// defense-in-depth. Best-effort: a filesystem without Unix perms just warns.
	s.restrictSecretFiles()

	return s, nil
}

// Close releases the read/write connections and the advisory lock, and closes
// in-process change listeners so their range loops terminate. It first attempts a
// WAL checkpoint so the main DB file is self-contained for backups and read-only
// consumers. A read still running keeps its connection until it finishes: the
// checkpoint waits for one holding data it has not copied yet, up to the busy timeout,
// and Close then waits up to two seconds for the rest before it lets the lock go, so the
// catalog file is normally closed when another owner can take it. A Reopen in flight is
// stopped first, rather than waited out.
func (s *Store) Close() error {
	s.stopMu.Lock()
	s.closing++
	if s.stopReopen != nil {
		s.stopReopen(errClosing)
	}
	s.stopMu.Unlock()
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	s.stopMu.Lock()
	s.closing--
	s.stopMu.Unlock()
	return s.teardown(true)
}

// errClosing is the cause a Close cancels a reopen in flight with.
var errClosing = errors.New("the store is closing")

// Suspend is Close for a maintenance-mode hand-off: it checkpoints, releases the
// lock, and closes the connections, but keeps the in-process change subscribers
// registered so an embedder's subscription survives the hand-off and resumes
// delivering after Reopen. (A full Close would close those channels, terminating
// the embedder's range loop with no way to re-establish it.) It also records the file
// and the feed's head, for Reopen to tell the same catalog from a replaced one.
func (s *Store) Suspend() error {
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	return s.teardown(false)
}

// teardown closes the store, optionally closing change subscribers. closeSubs is
// true for a full Close and false for a maintenance Suspend. The caller holds
// handoffMu.
func (s *Store) teardown(closeSubs bool) error {
	// Mark closed under wmu so an in-flight writeTx (which holds wmu for its whole
	// duration and checks closed) cannot be mid-transaction here; checkpoint while
	// still holding it. A reader that took the old pool before the swap below runs on it
	// until the pools close (its snapshot can hold the checkpoint up), and one that comes
	// in after the swap gets closedGen's refusal.
	s.wmu.Lock()
	if s.closed {
		// A suspended store keeps its subscribers and its mark, so a Close now still has
		// both to drop; a repeated Suspend keeps them.
		if closeSubs {
			s.mark = nil
		}
		s.wmu.Unlock()
		if closeSubs {
			s.closeSubscribers()
		}
		return nil
	}
	s.closed = true
	s.suspended.Store(true)
	g := s.gen.Swap(closedGen)
	if g.write != nil {
		if !closeSubs && s.mark == nil {
			s.mark = s.markLocked(g.write)
		}
		_, _ = g.write.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)")
	}
	s.wmu.Unlock()

	if closeSubs {
		// Close in-process change listeners so their range loops terminate.
		s.closeSubscribers()
	}
	// The pools close first, so DataVersion cannot pin another connection, and then the
	// pinned one is released to its closed pool, which closes it. DataVersion re-pins a
	// fresh one lazily after a reopen. Reads still running hold the catalog file open
	// until they finish, so the lock goes only after them: another owner that finds it
	// free can replace the file.
	err := g.closePools()
	s.closeDataVersionConn()
	if n := g.settle(settleBound); n > 0 {
		s.log.Warn("catalog connections still open after closing the store", "open", n, "waited", settleBound)
	}
	return errors.Join(err, g.lock.release())
}

// settleBound is how long a suspend or a close waits for the reads still running on the
// closed pools to finish.
var settleBound = 2 * time.Second

// Reopen re-acquires the write lock and reopens the connections of a Store that
// was Closed for a maintenance-mode hand-off, restoring it in place so every
// subsystem that still holds this *Store keeps working. It is the inverse of Close
// for the read-write path; a read-only store cannot be reopened this way, and a
// store that is already open is a no-op. A Suspend or another Reopen made while it
// runs waits for it to finish; a Close stops it, and it returns CodeCanceled.
//
// The lock re-acquire retries with bounded backoff because a foreground process
// may still be releasing the flock as the hand-off ends. migrate runs again so a
// restore/rebuild that replaced the DB file mid-hand-off is brought current; the
// rest mirrors Open's read-write reconciliation. The result says whether it reopened
// and whether the catalog is still the one Suspend closed (see ReopenResult).
func (s *Store) Reopen(ctx context.Context) (_ ReopenResult, err error) {
	const op = "store.Reopen"
	if s.readOnly {
		return ReopenResult{}, waxerr.New(waxerr.CodeUnsupported, op, "a read-only store cannot be reopened")
	}
	s.handoffMu.Lock()
	defer s.handoffMu.Unlock()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	s.stopMu.Lock()
	if s.closing > 0 {
		s.stopMu.Unlock()
		return ReopenResult{}, waxerr.New(waxerr.CodeCanceled, op, errClosing.Error())
	}
	s.stopReopen = stop
	s.stopMu.Unlock()
	defer func() {
		s.stopMu.Lock()
		s.stopReopen = nil
		s.stopMu.Unlock()
		if err != nil && context.Cause(ctx) == errClosing {
			err = waxerr.Classify(waxerr.CodeCanceled, op, err)
		}
	}()
	s.wmu.Lock()
	closed := s.closed
	s.wmu.Unlock()
	if !closed {
		return ReopenResult{}, nil
	}

	// Acquire the lock and open the connections without holding wmu: the retry can
	// sleep, and the reconciliation steps below take wmu themselves via writeTx.
	lock, err := acquireWriteLockRetry(ctx, s.opt.Path+".waxlock", s.opt.Owner, s.opt.IPCSocket)
	if err != nil {
		return ReopenResult{}, err
	}
	g, err := s.openPools(ctx, op, lock)
	if err != nil {
		return ReopenResult{}, err
	}

	s.wmu.Lock()
	s.gen.Store(g)
	s.rstmts.swap(g.read)
	replaced := s.catchUpLocked(ctx)
	s.closed = false
	s.suspended.Store(false)
	s.wmu.Unlock()

	// The store is open again; run the same post-open reconciliation as Open. On any
	// failure the half-restored store is suspended again, keeping its subscribers and
	// the mark, so a later Reopen can still finish the job.
	if err := s.migrate(ctx); err != nil {
		_ = s.teardown(false)
		return ReopenResult{}, err
	}
	if n, err := s.ReclaimOrphans(ctx, nowNS()); err != nil {
		_ = s.teardown(false)
		return ReopenResult{}, err
	} else if n > 0 {
		s.log.Info("reclaimed orphaned jobs on reopen", "count", n)
	}
	if n, err := s.recoverOrganize(ctx); err != nil {
		_ = s.teardown(false)
		return ReopenResult{}, err
	} else if n > 0 {
		s.log.Info("recovered interrupted organize moves on reopen", "count", n)
	}
	if err := s.ensureDefaultUser(ctx); err != nil {
		_ = s.teardown(false)
		return ReopenResult{}, err
	}
	s.wmu.Lock()
	s.mark = nil
	s.wmu.Unlock()
	return ReopenResult{Reopened: true, Replaced: replaced}, nil
}

// markLocked records the catalog a suspend is about to close, read over its write
// connection w. The caller holds wmu.
func (s *Store) markLocked(w *sql.DB) *suspendMark {
	m := &suspendMark{head: -1}
	if err := w.QueryRowContext(context.Background(),
		"SELECT COALESCE(MAX(seq), 0) FROM change_log").Scan(&m.head); err != nil {
		m.head = -1
	}
	if fi, err := os.Stat(s.path); err == nil {
		// Windows reads a file's identity at its first comparison, so compare now,
		// while the file at the path is the one being closed.
		_ = os.SameFile(fi, fi)
		m.file = fi
	}
	return m
}

// catchUpLocked compares the reopened catalog with the suspend's mark and reports
// whether it was replaced: another file at the path, a feed that went back, or new
// rows already pruned away. Otherwise the feed ran on, and the rows another process
// wrote meanwhile, never published here, go to subscribers now. The caller holds wmu,
// so no write lands between the head read here and the rows published.
func (s *Store) catchUpLocked(ctx context.Context) bool {
	m := s.mark
	if m == nil {
		// Closed without a suspend, so there is nothing to compare against.
		m = &suspendMark{head: -1}
		s.mark = m
	}
	if !m.replaced {
		m.replaced = s.replacedSince(ctx, m)
	}
	return m.replaced
}

// replacedSince reports whether the catalog now open differs from the one m recorded,
// publishing the rows written since m when it does not.
func (s *Store) replacedSince(ctx context.Context, m *suspendMark) bool {
	if m.file == nil || m.head < 0 {
		return true
	}
	if fi, err := os.Stat(s.path); err != nil || !os.SameFile(m.file, fi) {
		return true
	}
	var head, oldest int64
	if err := s.wdb().QueryRowContext(ctx,
		"SELECT COALESCE(MAX(seq), 0), COALESCE(MIN(seq), 0) FROM change_log").Scan(&head, &oldest); err != nil {
		return true
	}
	if head < m.head || (head > m.head && oldest > m.head+1) {
		return true
	}
	if head > m.head && s.hasSubscribers() {
		s.publishSince(ctx, m.head)
	}
	m.head = head
	return false
}

// ReopenResult reports what a Reopen found. Reopened is false for a store that was
// already open. Replaced reports that the catalog is not the one Suspend closed, or
// that its feed does not run on from where it stopped; when it is false, the rows
// another process wrote in between have been published to subscribers.
type ReopenResult struct {
	Reopened bool
	Replaced bool
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
// writable is writeTx's refusal, for the work a method does ahead of its transaction, so
// a read-only or closed store answers a caller the same way whatever that work was.
func (s *Store) writable() error {
	if s.readOnly {
		return waxerr.New(waxerr.CodeUnsupported, "store.writeTx", "library opened read-only")
	}
	if s.suspended.Load() {
		return waxerr.New(waxerr.CodeUnsupported, "store.writeTx", "store is closed")
	}
	return nil
}

func (s *Store) writeTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.readOnly {
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

	tx, err := s.wdb().BeginTx(ctx, nil)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "store.writeTx", err)
	}
	// A panic in fn would otherwise keep the one write connection checked out for good.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
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

// lastNS is the last value nowNS returned.
var lastNS atomic.Int64

// nowNS is the store's clock: the wall clock in Unix nanoseconds, never at or below a
// value it already returned in this process. Windows' clock ticks every 15.6ms, so
// back-to-back writes can read the same nanosecond, and an equal stamp is worse than a
// skewed one: browse recency and the sync staleness test order by these stamps, a
// write-back settled at one fill's stamp must see a later fill as newer, and the owed
// sweep's instant must fall strictly between the markers written before and after it.
// The skew is nanoseconds while the wall clock moves forward; after it steps back,
// stamps hold at the old time until it catches up. Stamps from separate processes are
// not ordered, as the wall clock's never were.
func nowNS() int64 {
	wall := wallNS()
	for {
		last := lastNS.Load()
		ns := max(wall, last+1)
		if lastNS.CompareAndSwap(last, ns) {
			return ns
		}
	}
}

// wallNS is the wall clock nowNS floors, a variable so a test can stop it.
var wallNS = func() int64 { return time.Now().UnixNano() }
