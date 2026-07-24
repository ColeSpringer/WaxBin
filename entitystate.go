package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// This file exposes the per-user entity play-state surface on the Library facade: star
// and rating writes scoped to a catalog entity (artist/release_group/album/genre) and
// their read-backs. It is the entity-scoped twin of the item playback surface reached
// through Playback(), but goes straight to the store rather than through a service:
// item play-state has a buffering service in front of it because a resume position is a
// stream of coalesced ticks, while an entity star is a single durable write with nothing
// to buffer.

// SetEntityStar stars or unstars a catalog entity for a user, recording the star time
// for recency ordering of the starred list. asOf (unix nanoseconds, nil = server now) is
// the recorded flip time; when supplied the engine enforces recorded-time
// last-writer-wins, so a replayed offline toggle cannot undo a later out-of-band change,
// exactly as item SetStar does. A value-identical call is a silent no-op that preserves
// the stored star time.
func (l *Library) SetEntityStar(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, starred bool, asOf *int64) error {
	return l.store.SetEntityStar(ctx, userPID, kind, entityPID, starred, asOf)
}

// SetEntityRating sets (0..100) or clears (rating nil) a user's rating for a catalog
// entity. asOf carries the recorded change time and enforces recorded-time
// last-writer-wins; see SetEntityStar.
func (l *Library) SetEntityRating(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, rating *int, asOf *int64) error {
	return l.store.SetEntityRating(ctx, userPID, kind, entityPID, rating, asOf)
}

// EntityPlayState returns a user's star/rating state for one catalog entity. An unknown
// user or entity pid is CodeNotFound (like PlayStateFor); an entity the user has never
// starred or rated reads back zero-valued, not an error.
func (l *Library) EntityPlayState(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID) (*model.EntityPlayState, error) {
	return l.store.EntityPlayState(ctx, userPID, kind, entityPID)
}

// StarredEntities lists a user's starred entities of one kind, most-recently-starred
// first. Pair it with EntityByPIDs to hydrate display names in one batch.
func (l *Library) StarredEntities(ctx context.Context, userPID model.PID, kind model.MergeEntity) ([]model.EntityPlayState, error) {
	return l.store.StarredEntities(ctx, userPID, kind)
}
