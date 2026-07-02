// Package playback is the consumer-facing playback-state service. It coalesces
// high-frequency position ticks in memory and writes them on checkpoints
// (pause, seek, track change) or periodic Flush calls. Played status, ratings,
// stars, bookmarks, queue, and sessions pass through to the store.
package playback

import (
	"context"
	"sync"

	"github.com/colespringer/waxbin/model"
)

// Store is the persistence the playback service needs (satisfied by store/sqlite).
type Store interface {
	SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error
	MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error
	SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error
	SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error
	PlayStateFor(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error)
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
	pending  map[progressKey]int64 // buffered resume positions awaiting a flush (mu)
	flushMu  sync.Mutex            // serializes the actual DB writes across flushes
	importer PlayStateImporter     // external play-state import seam (no-op default)
}

type progressKey struct {
	user model.PID
	item model.PID
}

// New builds a playback service over a store.
func New(store Store) *Service {
	return &Service{store: store, pending: map[progressKey]int64{}, importer: noopImporter{}}
}

// Progress buffers a resume position without writing it. Call it on every tick;
// the position is persisted later by Checkpoint or Flush, so a stream of ticks
// collapses to one write. The newest position for an item wins.
func (s *Service) Progress(userPID, itemPID model.PID, positionMS int64) {
	s.mu.Lock()
	s.pending[progressKey{userPID, itemPID}] = positionMS
	s.mu.Unlock()
}

// Checkpoint persists an item's resume position immediately, usually on pause,
// seek, or track change. It writes through the same serialized path as Flush so a
// concurrent flush cannot overwrite the newer checkpoint with an older tick.
func (s *Service) Checkpoint(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	s.mu.Lock()
	s.pending[progressKey{userPID, itemPID}] = positionMS
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
// arrived for that item.
func (s *Service) flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	batch := s.pending
	s.pending = map[progressKey]int64{}
	s.mu.Unlock()

	var firstErr error
	for k, pos := range batch {
		if err := s.store.SetProgress(ctx, k.user, k.item, pos); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			s.mu.Lock()
			if _, newer := s.pending[k]; !newer {
				s.pending[k] = pos
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

// SetRating sets (0..100) or clears (nil) a user's rating for an item.
func (s *Service) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error {
	return s.store.SetRating(ctx, userPID, itemPID, rating)
}

// SetStar stars or unstars an item for a user.
func (s *Service) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error {
	return s.store.SetStar(ctx, userPID, itemPID, starred)
}

// State returns a user's playback state for an item, overlaying any buffered (not
// yet flushed) resume position so a same-process reader sees its own latest tick.
func (s *Service) State(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error) {
	st, err := s.store.PlayStateFor(ctx, userPID, itemPID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if pos, ok := s.pending[progressKey{userPID, itemPID}]; ok {
		st.PositionMS = pos
	}
	s.mu.Unlock()
	return st, nil
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
