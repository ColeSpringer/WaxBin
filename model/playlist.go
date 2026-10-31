package model

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

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
// static playlists (a smart playlist's membership is computed on read). HasArt
// reports whether the playlist carries its own front cover, so a list read can tell
// which playlists have one without a resolve call each. Description is the owner's
// note on the list, as CheckPlaylistDescription stores it.
type Playlist struct {
	PID         PID
	Name        string
	Description string
	OwnerPID    PID
	OwnerName   string
	Kind        PlaylistKind
	Visibility  PlaylistVisibility
	Rule        *query.Query
	ItemCount   int
	HasArt      bool
	CreatedAt   int64
	UpdatedAt   int64
}

// PathCandidates is what one playlist path names: the items behind the files it names,
// or, past the lookup's limit, Overflow and no items.
type PathCandidates struct {
	Items    []*ItemView
	Overflow bool
}

// lineBreaks reads every line break a description may arrive with as a line feed.
var lineBreaks = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u0085", "\n", "\u2028", "\n", "\u2029", "\n")

// MaxPlaylistDescriptionBytes bounds a playlist's description.
const MaxPlaylistDescriptionBytes = 4096

// CheckPlaylistDescription returns a description as a playlist stores it, trimmed and
// with every line break (CRLF, CR, next line, line or paragraph separator) read as a line
// feed, or refuses it (CodeInvalid) when it is not UTF-8, runs past
// MaxPlaylistDescriptionBytes, or holds a character feed text could not keep (see
// CapText): a tab or other control, or a bidirectional embedding, override or isolate.
// The marks and joiners prose needs stay.
func CheckPlaylistDescription(text string) (string, error) {
	const op = "model.CheckPlaylistDescription"
	text = strings.TrimSpace(lineBreaks.Replace(text))
	switch {
	case !utf8.ValidString(text):
		return "", waxerr.New(waxerr.CodeInvalid, op, "a playlist description must be valid UTF-8")
	case len(text) > MaxPlaylistDescriptionBytes:
		return "", waxerr.New(waxerr.CodeInvalid, op,
			fmt.Sprintf("a playlist description is at most %d bytes, got %d", MaxPlaylistDescriptionBytes, len(text)))
	}
	if i := strings.IndexFunc(text, func(r rune) bool { return r != '\n' && escapedRune(r) && !keptInText(r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(text[i:])
		return "", waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("a playlist description cannot hold %U", r))
	}
	return text, nil
}
