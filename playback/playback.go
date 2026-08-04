// Package playback is the consumer-facing playback-state service. It coalesces
// high-frequency position ticks in memory and writes them on checkpoints
// (pause, seek, track change) or periodic Flush calls. Played status, ratings,
// stars, bookmarks, queue, and sessions pass through to the store.
package playback

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the playback service needs (satisfied by store/sqlite).
type Store interface {
	SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error
	MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error
	// SetRating/SetStar take an optional recorded time asOf (unix ns, nil = server
	// now); when supplied the store stamps the change in recorded time and enforces
	// recorded-time last-writer-wins, so an import or replayed offline toggle orders
	// correctly against an out-of-band change. Both report whether the write changed
	// anything: false means the store suppressed the change-feed delta because the
	// call was value-identical, cleared something never set, or lost to a stale
	// replay. SetProgress and MarkPlayed always write, so neither returns it.
	SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int, asOf *int64) (bool, error)
	SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool, asOf *int64) (bool, error)
	PlayStateFor(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error)
	// PlayStatesForItems is the bulk read behind StatesForItems: every user's
	// state for each given item, keyed by item pid, each slice ordered by user
	// pid; untouched and unknown items are absent.
	PlayStatesForItems(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.PlayState, error)
	// DefaultUser resolves the seeded default user, so the overlay can match a
	// position buffered under the empty-pid sentinel to its flushed row.
	DefaultUser(ctx context.Context) (*model.User, error)
	AddBookmark(ctx context.Context, userPID, itemPID model.PID, positionMS int64, label string) (model.PID, error)
	Bookmarks(ctx context.Context, userPID, itemPID model.PID) ([]model.Bookmark, error)
	DeleteBookmark(ctx context.Context, bookmarkPID model.PID) error
	SetQueue(ctx context.Context, userPID model.PID, itemPIDs []model.PID) error
	Queue(ctx context.Context, userPID model.PID) ([]*model.ItemView, error)
	StartSession(ctx context.Context, userPID, itemPID model.PID, client string) (model.PID, error)
	EndSession(ctx context.Context, sessionPID model.PID, msPlayed int64) error
}

// Service buffers playback progress and delegates the rest of playback state to
// the store.
type Service struct {
	store    Store
	mu       sync.Mutex
	pending  map[progressKey]tick // buffered resume positions awaiting a flush (mu)
	flushMu  sync.Mutex           // serializes the actual DB writes across flushes
	importer PlayStateImporter    // external play-state import seam (no-op default)
}

type progressKey struct {
	user model.PID
	item model.PID
}

// tick is one buffered resume position and when it was buffered. The time is for
// the overlay only, so an unflushed position comes back with a matching stamp
// instead of contradicting it; the write stamps at the store's own now.
type tick struct {
	positionMS int64
	atNS       int64
}

// New builds a playback service over a store.
func New(store Store) *Service {
	return &Service{store: store, pending: map[progressKey]tick{}, importer: noopImporter{}}
}

// Progress buffers a resume position without writing it. Call it on every tick;
// the position is persisted later by Checkpoint or Flush, so a stream of ticks
// collapses to one write. The newest position for an item wins.
func (s *Service) Progress(userPID, itemPID model.PID, positionMS int64) {
	s.mu.Lock()
	s.pending[progressKey{userPID, itemPID}] = tick{positionMS, time.Now().UnixNano()}
	s.mu.Unlock()
}

// Checkpoint persists an item's resume position immediately, usually on pause,
// seek, or track change. It writes through the same serialized path as Flush so a
// concurrent flush cannot overwrite the newer checkpoint with an older tick.
func (s *Service) Checkpoint(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	s.mu.Lock()
	s.pending[progressKey{userPID, itemPID}] = tick{positionMS, time.Now().UnixNano()}
	s.mu.Unlock()
	return s.flush(ctx)
}

// Flush persists every buffered position (the periodic checkpoint a consumer runs
// on a timer).
func (s *Service) Flush(ctx context.Context) error { return s.flush(ctx) }

// flush is the single serialized write path. flushMu prevents concurrent flushes
// from interleaving their snapshot and write phases. Every update lands in
// pending first, where the newest position wins, so a checkpoint cannot be
// replaced by an older buffered tick. Writes happen outside s.mu so incoming
// ticks do not block. On write errors, the batch keeps going, the first error is
// returned, and failed positions are re-queued unless a newer tick already
// arrived for that item. A CodeNotFound failure is the exception: the user or
// item is gone, a retry can never succeed, so the tick is dropped instead of
// re-queued (it would otherwise linger forever, resurfacing through the
// StatesForItems overlay on every read).
func (s *Service) flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	s.pending = map[progressKey]tick{}
	s.mu.Unlock()

	var firstErr error
	for k, t := range batch {
		if err := s.store.SetProgress(ctx, k.user, k.item, t.positionMS); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if waxerr.Is(err, waxerr.CodeNotFound) {
				continue
			}
			s.mu.Lock()
			if _, newer := s.pending[k]; !newer {
				s.pending[k] = t
			}
			s.mu.Unlock()
		}
	}
	return firstErr
}

// MarkPlayed records a play (and optionally that it finished).
func (s *Service) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	return s.store.MarkPlayed(ctx, userPID, itemPID, finished)
}

// SetRating sets (0..100) or clears (nil) a user's rating for an item. asOf (unix
// ns, nil = server now) records the change time so a replayed or imported rating
// orders by recorded-time last-writer-wins. It reports whether the write changed
// anything; false means no change-feed delta was appended (a value-identical
// re-rate, a clear of a rating never set, or a stale replay).
func (s *Service) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int, asOf *int64) (bool, error) {
	return s.store.SetRating(ctx, userPID, itemPID, rating, asOf)
}

// SetStar stars or unstars an item for a user. asOf (unix ns, nil = server now)
// records the flip time so a replayed or imported star orders by recorded-time
// last-writer-wins. It reports whether the write changed anything; false means no
// change-feed delta was appended (a re-star, an unstar of an unstarred item, or a
// stale replay).
func (s *Service) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool, asOf *int64) (bool, error) {
	return s.store.SetStar(ctx, userPID, itemPID, starred, asOf)
}

// State returns a user's playback state for an item, overlaying any buffered (not
// yet flushed) resume position, and its stamp, so a same-process reader sees its
// own latest tick.
func (s *Service) State(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error) {
	st, err := s.store.PlayStateFor(ctx, userPID, itemPID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if t, ok := s.pending[progressKey{userPID, itemPID}]; ok {
		st.PositionMS, st.LastProgressAt = t.positionMS, t.atNS
	}
	s.mu.Unlock()
	return st, nil
}

// StatesForItems returns every user's playback state for each of the given
// items, keyed by item pid with each slice ordered by user pid, overlaying
// buffered (not yet flushed) resume positions: a buffered position replaces the
// flushed one on its state, and a (user, item) pair that has only a buffered
// position gets a position-only state synthesized, so a same-process reader
// never misses the window between a tick and its flush. Only this process's
// buffer is visible; a second process reads flushed state alone. Untouched and
// unknown items are absent from the flushed read, but a synthesized entry is
// not existence-checked: an item deleted after this process buffered a tick for
// it can surface until the next flush (which discards a not-found tick). That
// errs on the side the overlay exists for; a keep-or-drop consumer briefly sees
// "in use" rather than ever missing a live pair.
func (s *Service) StatesForItems(ctx context.Context, itemPIDs []model.PID) (map[model.PID][]model.PlayState, error) {
	states, err := s.store.PlayStatesForItems(ctx, itemPIDs)
	if err != nil {
		return nil, err
	}
	requested := make(map[model.PID]bool, len(itemPIDs))
	for _, pid := range itemPIDs {
		requested[pid] = true
	}
	s.mu.Lock()
	pending := make(map[progressKey]tick)
	needDefault := false
	for k, t := range s.pending {
		if requested[k.item] {
			pending[k] = t
			if k.user == "" {
				needDefault = true
			}
		}
	}
	s.mu.Unlock()
	if len(pending) == 0 {
		return states, nil
	}
	// A position buffered under the empty-pid default-user sentinel must match
	// the default user's flushed row (which carries the real pid), not synthesize
	// a duplicate state beside it. A caller mixing the sentinel and the explicit
	// pid for the same item has two buffer slots already; whichever resolves last
	// wins here, exactly as unordered flushes would.
	if needDefault {
		u, err := s.store.DefaultUser(ctx)
		if err != nil {
			return nil, err
		}
		resolved := make(map[progressKey]tick, len(pending))
		for k, t := range pending {
			if k.user == "" {
				k.user = u.PID
			}
			resolved[k] = t
		}
		pending = resolved
	}
	if states == nil {
		states = make(map[model.PID][]model.PlayState)
	}
	for k, t := range pending {
		list := states[k.item]
		hit := false
		for i := range list {
			if list[i].UserPID == k.user {
				list[i].PositionMS, list[i].LastProgressAt = t.positionMS, t.atNS
				hit = true
				break
			}
		}
		if !hit {
			list = append(list, model.PlayState{UserPID: k.user, ItemPID: k.item,
				PositionMS: t.positionMS, LastProgressAt: t.atNS})
			sort.Slice(list, func(i, j int) bool { return list[i].UserPID < list[j].UserPID })
		}
		states[k.item] = list
	}
	return states, nil
}

// AddBookmark records a labeled position within an item.
func (s *Service) AddBookmark(ctx context.Context, userPID, itemPID model.PID, positionMS int64, label string) (model.PID, error) {
	return s.store.AddBookmark(ctx, userPID, itemPID, positionMS, label)
}

// Bookmarks lists a user's bookmarks for an item.
func (s *Service) Bookmarks(ctx context.Context, userPID, itemPID model.PID) ([]model.Bookmark, error) {
	return s.store.Bookmarks(ctx, userPID, itemPID)
}

// RemoveBookmark deletes a bookmark by pid.
func (s *Service) RemoveBookmark(ctx context.Context, bookmarkPID model.PID) error {
	return s.store.DeleteBookmark(ctx, bookmarkPID)
}

// SetQueue replaces a user's persistent play queue.
func (s *Service) SetQueue(ctx context.Context, userPID model.PID, itemPIDs []model.PID) error {
	return s.store.SetQueue(ctx, userPID, itemPIDs)
}

// Queue returns a user's play queue in order.
func (s *Service) Queue(ctx context.Context, userPID model.PID) ([]*model.ItemView, error) {
	return s.store.Queue(ctx, userPID)
}

// StartSession opens a play session, returning its pid for EndSession.
func (s *Service) StartSession(ctx context.Context, userPID, itemPID model.PID, client string) (model.PID, error) {
	return s.store.StartSession(ctx, userPID, itemPID, client)
}

// EndSession closes a session with the milliseconds played.
func (s *Service) EndSession(ctx context.Context, sessionPID model.PID, msPlayed int64) error {
	return s.store.EndSession(ctx, sessionPID, msPlayed)
}
