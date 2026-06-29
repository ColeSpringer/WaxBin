package sqlite

import (
	"context"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Subscribe registers an in-process listener for change_log deltas. Mutating
// transactions publish their rows after commit, so an embedded consumer can
// update caches without polling. The returned channel is buffered; a slow
// listener drops events instead of blocking the writer and recovers by replaying
// from ChangesSince. The cancel func unsubscribes and closes the channel.
//
// Cross-process consumers (the CLI watcher, a separate WaxDeck container) cannot
// use this bus; they tail DataVersion instead.
//
// Consumer contract: subscribe first, then prime the cursor with ChangesSince.
// A transaction that commits between Subscribe and the initial pull may not be
// delivered on the bus, but it is in change_log and the pull will see it.
// Subscribing after the pull would leave a gap.
func (s *Store) Subscribe() (<-chan model.Change, func()) {
	ch := make(chan model.Change, 256)
	s.subMu.Lock()
	if s.subs == nil {
		s.subs = map[chan model.Change]struct{}{}
	}
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()

	cancel := func() {
		s.subMu.Lock()
		defer s.subMu.Unlock()
		// Only this closure closes ch, and only if Close has not already done so
		// (Close removes it from the set first), so there is never a double close.
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}

// closeSubscribers closes and drops every listener channel, called from Close so
// a same-process consumer's range loop terminates when the store goes away.
func (s *Store) closeSubscribers() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		close(ch)
	}
	s.subs = nil
}

// hasSubscribers reports whether any in-process listener is registered, so the
// writer can skip the post-commit publish entirely when no one is listening.
func (s *Store) hasSubscribers() bool {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	return len(s.subs) > 0
}

// publishSince reads the change_log rows committed after preSeq and fans them out
// to subscribers. Sends are non-blocking: a full channel means that listener is
// behind and must full-resync, so dropping is correct.
func (s *Store) publishSince(ctx context.Context, preSeq int64) {
	rows, err := s.write.QueryContext(ctx,
		"SELECT seq, ts, entity_type, entity_pid, op FROM change_log WHERE seq > ? ORDER BY seq", preSeq)
	if err != nil {
		s.log.Warn("change publish read", "err", err)
		return
	}
	var changes []model.Change
	for rows.Next() {
		var c model.Change
		var pid string
		if err := rows.Scan(&c.Seq, &c.TS, &c.EntityType, &pid, &c.Op); err != nil {
			s.log.Warn("change publish scan", "err", err)
			rows.Close()
			return
		}
		c.EntityPID = model.PID(pid)
		changes = append(changes, c)
	}
	// A row-iteration error means the delta set is incomplete; don't publish a
	// partial set (which would look authoritative). Subscribers recover via the
	// data_version move + ChangesSince.
	if err := rows.Err(); err != nil {
		s.log.Warn("change publish iterate", "err", err)
		rows.Close()
		return
	}
	rows.Close()
	if len(changes) == 0 {
		return
	}

	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		for _, c := range changes {
			select {
			case ch <- c:
			default: // listener behind; it recovers via ChangesSince
			}
		}
	}
}

// maxChangeSeq returns the current highest change_log seq on the write
// connection, captured before a transaction so publishSince can find its rows.
func (s *Store) maxChangeSeq(ctx context.Context) int64 {
	var seq int64
	_ = s.write.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM change_log").Scan(&seq)
	return seq
}

// DataVersion returns SQLite's PRAGMA data_version. The value changes between two
// reads when another connection commits, letting a separate process poll it and
// call ChangesSince only when needed. Because data_version is connection-relative,
// it must be read from one pinned connection; a rotating pool would compare
// unrelated baselines.
func (s *Store) DataVersion(ctx context.Context) (int64, error) {
	const op = "store.DataVersion"
	s.dvMu.Lock()
	defer s.dvMu.Unlock()
	if s.dvConn == nil {
		conn, err := s.read.Conn(ctx)
		if err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		s.dvConn = conn
	}
	var v int64
	if err := s.dvConn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
		// Discard a broken pinned connection so the next call re-acquires a fresh
		// one rather than failing forever on the same dead conn.
		_ = s.dvConn.Close()
		s.dvConn = nil
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return v, nil
}

// closeDataVersionConn releases the pinned data_version connection back to the
// pool, called from Close.
func (s *Store) closeDataVersionConn() {
	s.dvMu.Lock()
	defer s.dvMu.Unlock()
	if s.dvConn != nil {
		_ = s.dvConn.Close()
		s.dvConn = nil
	}
}
