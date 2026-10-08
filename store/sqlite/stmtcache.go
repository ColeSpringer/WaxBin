package sqlite

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"sync"
)

// stmtCacheMax bounds the statements kept prepared: a query's text varies with the
// shape of its filter, so the set a long-running server sees is open-ended. An item-view
// statement holds 50 to 60 KB of SQLite's memory on each pool connection that has run it,
// so the bound keeps the cache near 2 MB a connection, an eighth of the page cache each
// connection keeps by default.
const stmtCacheMax = 32

// stmtCache holds prepared statements on the read pool, keyed by their SQL text, so a hot
// read skips parsing and planning its statement on every call. Preparing allocates
// heavily, and modernc's allocator is one lock for the whole process, so an uncached read
// serializes every reader behind it (BenchmarkReadScaling). database/sql prepares a
// cached statement again on each pool connection that first runs it.
//
// Past the limit the least recently used statement goes, closed once no reader holds
// it. Reopen swaps the read pool and tells the cache (swap), which drops the old pool's
// statements; a reader still holding the old pool runs its query there directly rather
// than disturbing the cache. The lock is held neither while waiting for a connection,
// since a reader releasing its statement may be holding one, nor while a statement
// closes.
type stmtCache struct {
	limit int // 0 means stmtCacheMax
	mu    sync.Mutex
	db    *sql.DB
	byKey map[string]*cachedStmt
	lru   list.List // *cachedStmt, most recently used first
}

type cachedStmt struct {
	query string
	stmt  *sql.Stmt
	users int
	gone  bool // out of the cache; closed when its last user is done
	elem  *list.Element
}

// queryContext runs query through its cached statement, or directly on db when the
// statement cannot be prepared, which then reports why.
func (c *stmtCache) queryContext(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	e, err := c.acquire(ctx, db, query)
	if err != nil {
		return db.QueryContext(ctx, query, args...)
	}
	// The rows keep the statement alive past a close, so it is released at once.
	defer c.release(e)
	return e.stmt.QueryContext(ctx, args...)
}

// queryRowContext is queryContext for a single row.
func (c *stmtCache) queryRowContext(ctx context.Context, db *sql.DB, query string, args ...any) *sql.Row {
	e, err := c.acquire(ctx, db, query)
	if err != nil {
		return db.QueryRowContext(ctx, query, args...)
	}
	defer c.release(e)
	return e.stmt.QueryRowContext(ctx, args...)
}

func (c *stmtCache) acquire(ctx context.Context, db *sql.DB, query string) (*cachedStmt, error) {
	var stale []*sql.Stmt
	defer func() {
		for _, st := range stale {
			_ = st.Close()
		}
	}()
	c.mu.Lock()
	if c.db == nil {
		c.db = db
	}
	if c.db != db {
		c.mu.Unlock()
		return nil, errForeignPool
	}
	if e, ok := c.byKey[query]; ok {
		e.users++
		c.lru.MoveToFront(e.elem)
		c.mu.Unlock()
		return e, nil
	}
	c.mu.Unlock()

	st, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock() // deferred last, so it runs before the stale statements close
	if c.db != db {
		// The pool was swapped while this one prepared: use it once and let it go.
		return &cachedStmt{query: query, stmt: st, users: 1, gone: true}, nil
	}
	if e, ok := c.byKey[query]; ok {
		// Another reader cached the same text first.
		stale = append(stale, st)
		e.users++
		c.lru.MoveToFront(e.elem)
		return e, nil
	}
	if c.byKey == nil {
		c.byKey = map[string]*cachedStmt{}
	}
	e := &cachedStmt{query: query, stmt: st, users: 1}
	e.elem = c.lru.PushFront(e)
	c.byKey[query] = e
	limit := c.limit
	if limit <= 0 {
		limit = stmtCacheMax
	}
	for c.lru.Len() > limit {
		stale = c.removeLocked(c.lru.Back().Value.(*cachedStmt), stale)
	}
	return e, nil
}

// errForeignPool is acquire's answer to a reader holding a pool the cache has moved on
// from; the reader runs its query on that pool itself.
var errForeignPool = errors.New("statement cache: the pool is not the one it holds")

// swap points the cache at a new read pool, dropping the statements prepared on the old
// one. Reopen calls it where it swaps the pool.
func (c *stmtCache) swap(db *sql.DB) {
	c.mu.Lock()
	stale := c.dropLocked(nil)
	c.db = db
	c.mu.Unlock()
	for _, st := range stale {
		_ = st.Close()
	}
}

func (c *stmtCache) release(e *cachedStmt) {
	c.mu.Lock()
	e.users--
	done := e.gone && e.users == 0
	c.mu.Unlock()
	if done {
		_ = e.stmt.Close()
	}
}

// removeLocked takes e out of the cache, adding its statement to stale when no reader
// holds it; otherwise its last reader closes it.
func (c *stmtCache) removeLocked(e *cachedStmt, stale []*sql.Stmt) []*sql.Stmt {
	c.lru.Remove(e.elem)
	delete(c.byKey, e.query)
	e.gone = true
	if e.users == 0 {
		stale = append(stale, e.stmt)
	}
	return stale
}

func (c *stmtCache) dropLocked(stale []*sql.Stmt) []*sql.Stmt {
	for c.lru.Len() > 0 {
		stale = c.removeLocked(c.lru.Back().Value.(*cachedStmt), stale)
	}
	return stale
}
