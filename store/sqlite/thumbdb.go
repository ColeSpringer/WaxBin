package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colespringer/waxbin/waxerr"
	msqlite "modernc.org/sqlite"
)

// The generated thumbnails live in a file of their own beside the catalog, attached to
// every catalog connection as the thumbs schema, so one write coordinator and one read
// pool serve both and a reopen attaches it again with its connections. A backup (VACUUM
// INTO copies main alone), a VACUUM of the catalog and the baseline fingerprint never
// touch it, and a reset or a restore leaves it: a thumbnail is keyed by its source's
// content hash, so it serves any catalog holding that source, and GCArt sweeps the rest.
// A transaction writing both schemas is atomic per file, not across them, which a cache
// never needs.

// ThumbFile is the thumbnail file beside the catalog at dbPath.
func ThumbFile(dbPath string) string { return dbPath + ".thumbs" }

// thumbLayoutVersion is the user_version a file laid out by this build carries. A file
// with another (0 is the mark noteThumbsDamage leaves) is replaced by the next writer.
const thumbLayoutVersion = 1

// thumbPragmas come before the table: auto_vacuum only takes on a file with no table
// yet. Incremental, so a prune's freed pages go back in short steps of their own
// (reclaimThumbPages) rather than all in the prune's commit.
const thumbPragmas = `PRAGMA thumbs.auto_vacuum = INCREMENTAL;
PRAGMA thumbs.journal_mode = WAL;`

// thumbTable is the one table. The image bytes come last, so a read of the small columns
// (the census, the age prune) stops before a row's overflow pages.
const thumbTable = `CREATE TABLE IF NOT EXISTS thumbs.thumb (
  source_hash TEXT    NOT NULL,  -- the art_source it was generated from
  size        INTEGER NOT NULL,  -- ladder rung in px (art.Rung)
  format      TEXT    NOT NULL,  -- jpeg|png
  width       INTEGER NOT NULL,
  height      INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  data        BLOB    NOT NULL,
  PRIMARY KEY (source_hash, size)
)`

// errThumbLayout is ready's answer to a file holding a thumb table this build did not lay
// out, or one marked damaged.
var errThumbLayout = errors.New("the thumbnail file is not this build's layout")

// thumbsRole is which pool a thumbsConnector serves.
type thumbsRole int

const (
	thumbsReader   thumbsRole = iota // a read-write store's read pool
	thumbsWriter                     // a read-write store's write connection
	thumbsReadOnly                   // a read-only store's pool
)

// thumbsConnector opens a catalog connection with the thumbnail file attached: the file
// itself for a read-write store, laid out when it has no table yet; the file read-only
// for a read-only store, or an empty in-memory twin when there is none, so it persists
// nothing. The writer, which connects first at an open and a reopen, moves aside a file
// that is damaged or not this build's layout and starts a fresh one. A file still
// unusable gets the twin, with one warning, since a cache must never keep the catalog
// from opening; on the writer it also clears stored, so nothing is written where no
// reader would find it, until reattachThumbs finds the file usable.
type thumbsConnector struct {
	driver.Connector
	file   string
	role   thumbsRole
	stored *atomic.Bool // the writer's: whether its connection holds the file
	warn   func(msg string, args ...any)
	once   *sync.Once
}

func newThumbsConnector(base driver.Connector, file string, role thumbsRole, stored *atomic.Bool, warn func(string, ...any)) thumbsConnector {
	return thumbsConnector{Connector: base, file: file, role: role, stored: stored, warn: warn, once: new(sync.Once)}
}

func (c thumbsConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if err := c.attach(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (c thumbsConnector) attach(ctx context.Context, conn driver.Conn) error {
	ex, okEx := conn.(driver.ExecerContext)
	q, okQ := conn.(driver.QueryerContext)
	if !okEx || !okQ {
		return errors.New("the sqlite connection runs no statements")
	}
	if err := c.attachSome(ctx, ex, q); err != nil {
		return err
	}
	// An attached database takes SQLite's default sync, FULL, not the DSN's NORMAL.
	return execConn(ctx, ex, "PRAGMA thumbs.synchronous = NORMAL")
}

func (c thumbsConnector) attachSome(ctx context.Context, ex driver.ExecerContext, q driver.QueryerContext) error {
	if c.role == thumbsReadOnly {
		if _, err := os.Stat(c.file); errors.Is(err, fs.ErrNotExist) {
			return attachTwin(ctx, ex)
		}
	}
	err := c.attachFile(ctx, ex, q)
	if err != nil && c.role == thumbsWriter && (damaged(err) || errors.Is(err, errThumbLayout)) {
		if merr := c.moveAside(); merr == nil {
			c.warn("the thumbnail file was damaged or not this build's; moved it aside and started a fresh one",
				"path", c.file, "aside", c.file+".unusable", "err", err)
			err = c.attachFile(ctx, ex, q)
		} else {
			c.warn("the thumbnail file is damaged or not this build's and cannot be moved aside",
				"path", c.file, "err", merr)
		}
	}
	if c.stored != nil {
		c.stored.Store(err == nil)
	}
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	c.once.Do(func() {
		c.warn("the thumbnail file cannot be used; thumbnails are not stored", "path", c.file, "err", err)
	})
	return attachTwin(ctx, ex)
}

// attachFile attaches the file and readies it, detaching it again when it cannot be used.
func (c thumbsConnector) attachFile(ctx context.Context, ex driver.ExecerContext, q driver.QueryerContext) error {
	mode := "rwc"
	if c.role == thumbsReadOnly {
		mode = "ro"
	}
	if err := execConn(ctx, ex, "ATTACH DATABASE ? AS thumbs", fileURI(c.file)+"?mode="+mode); err != nil {
		return err
	}
	if err := c.ready(ctx, ex, q); err != nil {
		_ = execConn(ctx, ex, "DETACH DATABASE thumbs")
		return err
	}
	return nil
}

// damaged reports whether err says the file is no database or a corrupt one, rather than
// one that could not be opened, which a move aside would not cure.
func damaged(err error) bool {
	var se *msqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff
	return code == sqliteNotADB || code == sqliteCorrupt
}

// SQLite's primary result codes for a file that is no database and for a corrupt one.
const (
	sqliteCorrupt = 11
	sqliteNotADB  = 26
)

// moveAside sets the unusable file aside as <file>.unusable, replacing an earlier one. Its
// WAL and shared memory go first: a WAL left beside a fresh file would be replayed into it.
func (c thumbsConnector) moveAside() error {
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(c.file + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return os.Rename(c.file, c.file+".unusable")
}

// ready checks the attached file: this build's table and version are ready, no table is
// laid out (a read-only store cannot, so for it such a file is unusable), and a table
// under another version is errThumbLayout.
func (c thumbsConnector) ready(ctx context.Context, ex driver.ExecerContext, q driver.QueryerContext) error {
	tables, version, err := readLayout(ctx, ex, q)
	if err != nil {
		return err
	}
	switch {
	case tables > 0 && version == thumbLayoutVersion:
		return nil
	case tables > 0:
		return errThumbLayout
	case c.role == thumbsReadOnly:
		return errors.New("the thumbnail file holds no thumbnail table")
	}
	return layOut(ctx, ex)
}

// readLayout reads whether the attached file holds the thumb table and the version it was
// laid out under, in one read transaction, so a layout another connection commits
// meanwhile is seen whole or not at all. The version is a pragma of its own: the
// pragma_user_version function reads the catalog's whatever schema it is named under.
func readLayout(ctx context.Context, ex driver.ExecerContext, q driver.QueryerContext) (tables, version int64, err error) {
	if err := execConn(ctx, ex, "BEGIN"); err != nil {
		return 0, 0, err
	}
	defer func() { _ = execConn(context.WithoutCancel(ctx), ex, "COMMIT") }()
	if tables, err = queryInt(ctx, q, "SELECT COUNT(*) FROM thumbs.sqlite_master WHERE type = 'table' AND name = 'thumb'"); err != nil {
		return 0, 0, err
	}
	version, err = queryInt(ctx, q, "PRAGMA thumbs.user_version")
	return tables, version, err
}

func queryInt(ctx context.Context, q driver.QueryerContext, stmt string) (int64, error) {
	rows, err := q.QueryContext(ctx, stmt, nil)
	if err != nil {
		return 0, err
	}
	dest := make([]driver.Value, 1)
	err = rows.Next(dest)
	_ = rows.Close()
	if err != nil && err != io.EOF {
		return 0, err
	}
	n, _ := dest[0].(int64)
	return n, nil
}

// layOut lays out an empty attached file, the table and its version in one transaction.
func layOut(ctx context.Context, ex driver.ExecerContext) error {
	if err := execConn(ctx, ex, thumbPragmas); err != nil {
		return err
	}
	if err := execConn(ctx, ex, "BEGIN"); err != nil {
		return err
	}
	for _, stmt := range []string{thumbTable, fmt.Sprintf("PRAGMA thumbs.user_version = %d", thumbLayoutVersion)} {
		if err := execConn(ctx, ex, stmt); err != nil {
			_ = execConn(context.WithoutCancel(ctx), ex, "ROLLBACK")
			return err
		}
	}
	return execConn(ctx, ex, "COMMIT")
}

func attachTwin(ctx context.Context, ex driver.ExecerContext) error {
	if err := execConn(ctx, ex, "ATTACH DATABASE ':memory:' AS thumbs"); err != nil {
		return err
	}
	return layOut(ctx, ex)
}

func execConn(ctx context.Context, ex driver.ExecerContext, q string, args ...any) error {
	named := make([]driver.NamedValue, len(args))
	for i, a := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	_, err := ex.ExecContext(ctx, q, named)
	return err
}

// fileURI is path as an SQLite file: URI, escaping what its parser would read as URI
// syntax.
func fileURI(path string) string {
	return "file:" + strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

// noteThumbsDamage takes err from a statement on the thumbnail file and reports whether
// it says the file is damaged. The first time, the store stops storing and reading
// thumbnails there, and a store whose write connection holds the file stamps it so the
// next read-write open moves it aside; until then everything that reads the cache whole
// reports it damaged rather than failing. A read-only store cannot stamp it.
func (s *Store) noteThumbsDamage(ctx context.Context, err error) bool {
	if !damaged(err) {
		return false
	}
	if s.thumbsDamaged.Swap(true) {
		return true
	}
	if !s.thumbsStored.Swap(false) {
		s.log.Warn("the thumbnail file is damaged; thumbnails are not read from it",
			"path", ThumbFile(s.path), "err", err)
		return true
	}
	s.log.Warn("the thumbnail file is damaged; thumbnails are not stored until the next read-write open replaces it",
		"path", ThumbFile(s.path), "err", err)
	if serr := s.writeTx(context.WithoutCancel(ctx), func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "PRAGMA thumbs.user_version = 0")
		return e
	}); serr != nil {
		s.log.Warn("marking the damaged thumbnail file", "path", ThumbFile(s.path), "err", serr)
	}
	return true
}

// errThumbsDamaged is the answer of a call that needs the thumbnail file once it is known
// to be damaged.
func errThumbsDamaged(op string) error {
	return waxerr.New(waxerr.CodeIO, op, "the thumbnail file is damaged; the next read-write open replaces it")
}

// thumbsReattachEvery is how often a thumbnail write retries a file the write connection
// could not attach.
const thumbsReattachEvery = time.Minute

// reattachThumbs attaches the thumbnail file to the write connection again when an earlier
// attach left it on the in-memory twin, as a passing failure does (a busy file, too many
// open files): at most once a minute for a thumbnail write, and on every call when force
// is set, as the maintenance calls that act on the file set it. It reports whether the
// write connection holds the file.
func (s *Store) reattachThumbs(ctx context.Context, force bool) bool {
	if s.thumbsStored.Load() {
		return true
	}
	if s.readOnly || s.thumbsDamaged.Load() {
		return false
	}
	now := nowNS()
	last := s.thumbsReattached.Load()
	if !force && (now-last < int64(thumbsReattachEvery) || !s.thumbsReattached.CompareAndSwap(last, now)) {
		return false
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	g := s.gen.Load()
	if s.closed || s.thumbsStored.Load() || g.write == nil {
		return s.thumbsStored.Load()
	}
	conn, err := g.write.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.Raw(func(dc any) error {
		ex, ok := dc.(driver.ExecerContext)
		if !ok {
			return nil
		}
		if err := execConn(ctx, ex, "DETACH DATABASE thumbs"); err != nil {
			return err
		}
		return g.thumbs.attach(ctx, dc.(driver.Conn))
	})
	return s.thumbsStored.Load()
}

// thumbReclaimStep is how many free pages one reclaim step gives back.
const thumbReclaimStep = 1000

// reclaimThumbPages gives the thumbnail file's free pages back to the filesystem, step
// pages at a time, each step a transaction of its own so a large prune never holds the one
// writer for the whole move, and then checkpoints passively so the file can shrink. It
// returns how many steps it took.
func (s *Store) reclaimThumbPages(ctx context.Context, step int) (int, error) {
	steps := 0
	for {
		var free int64
		err := s.writeTx(ctx, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, "PRAGMA thumbs.freelist_count").Scan(&free); err != nil || free == 0 {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA thumbs.incremental_vacuum(%d)", step))
			return err
		})
		if err != nil {
			return steps, err
		}
		if free == 0 {
			break
		}
		steps++
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if !s.closed {
		_, _ = s.wdb().ExecContext(ctx, "PRAGMA thumbs.wal_checkpoint(PASSIVE)")
	}
	return steps, nil
}
