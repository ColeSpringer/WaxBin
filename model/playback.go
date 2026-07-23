package model

// User is a playback identity. Every catalog has a default user, so single-user
// deployments can use playback state without extra setup.
type User struct {
	ID        int64
	PID       PID
	Name      string
	IsDefault bool
	CreatedAt int64 // unix nanoseconds
}

// PlayState is one user's playback state for one item: resume position, played/
// finished flags, play count, rating, and star. Rating is 0..100 (HasRating
// distinguishes an explicit 0 from unset); Starred carries the star with its set
// time for recency ordering. The changed-at stamps record when the star or
// rating last changed value, a clear included, so they survive an unstar or a
// rating clear; a value-identical set never bumps them. They carry what a sync
// replay guard needs to order a local change against a remote one.
type PlayState struct {
	UserPID          PID
	ItemPID          PID
	PositionMS       int64
	Played           bool
	Finished         bool
	PlayCount        int
	Rating           int
	HasRating        bool
	Starred          bool
	StarredAt        int64 // unix nanoseconds; 0 when not starred
	LastPlayedAt     int64 // unix nanoseconds
	RatingChangedAt  int64 // unix nanoseconds; 0 = rating never changed
	StarredChangedAt int64 // unix nanoseconds; 0 = star never changed
	UpdatedAt        int64 // unix nanoseconds
}

// Bookmark is a labeled position within an item (audiobooks/podcasts).
type Bookmark struct {
	PID        PID
	ItemPID    PID
	PositionMS int64
	Label      string
	CreatedAt  int64 // unix nanoseconds
}

// PlaySession is one play of an item, the history that feeds stats.
type PlaySession struct {
	PID       PID
	ItemPID   PID
	StartedAt int64 // unix nanoseconds
	EndedAt   int64 // unix nanoseconds; 0 while open
	MsPlayed  int64
	Client    string
}

// RatingBounds clamps a rating into the canonical 0..100 range.
func RatingBounds(r int) int {
	if r < 0 {
		return 0
	}
	if r > 100 {
		return 100
	}
	return r
}
