package model

import "github.com/colespringer/waxbin/query"

// PlaylistKind distinguishes an explicit ordered list from a query evaluated on
// read.
type PlaylistKind string

const (
	PlaylistStatic PlaylistKind = "static" // an explicit, ordered item list
	PlaylistSmart  PlaylistKind = "smart"  // a query rule evaluated on read
)

// Valid reports whether k is a known playlist kind.
func (k PlaylistKind) Valid() bool { return k == PlaylistStatic || k == PlaylistSmart }

// PlaylistVisibility controls who sees a playlist. The schema is already
// multi-user; richer sharing and ACLs are deferred past v1.0.
type PlaylistVisibility string

const (
	VisibilityPrivate PlaylistVisibility = "private" // only the owner
	VisibilityShared  PlaylistVisibility = "shared"  // visible to every user
)

// Valid reports whether v is a known visibility (empty defaults to private).
func (v PlaylistVisibility) Valid() bool {
	return v == "" || v == VisibilityPrivate || v == VisibilityShared
}

// Playlist is a user's playlist: a static ordered list or a smart query rule.
// Rule is set only for smart playlists; ItemCount is the stored member count for
// static playlists (a smart playlist's membership is computed on read).
type Playlist struct {
	PID        PID
	Name       string
	OwnerPID   PID
	OwnerName  string
	Kind       PlaylistKind
	Visibility PlaylistVisibility
	Rule       *query.Query
	ItemCount  int
	CreatedAt  int64
	UpdatedAt  int64
}
