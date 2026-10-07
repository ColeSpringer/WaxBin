// Package playlist is the consumer-facing playlist service: static and smart
// playlist CRUD plus M3U8 import/export. A static playlist is an explicit ordered
// item list; a smart playlist stores a query rule that the shared query engine
// evaluates on read. Database work lives in store/sqlite behind the Store port.
package playlist

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the playlist service needs (satisfied by store/sqlite).
type Store interface {
	CreatePlaylist(ctx context.Context, name string, ownerPID model.PID, kind model.PlaylistKind, vis model.PlaylistVisibility, rule *query.Query) (model.PID, error)
	CreatePlaylistWithItems(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility, itemPIDs []model.PID) (model.PID, error)
	PlaylistByPID(ctx context.Context, pid model.PID) (*model.Playlist, error)
	ListPlaylists(ctx context.Context, ownerPID model.PID) ([]*model.Playlist, error)
	DeletePlaylist(ctx context.Context, pid model.PID) error
	RenamePlaylist(ctx context.Context, pid model.PID, name string) error
	SetPlaylistVisibility(ctx context.Context, pid model.PID, vis model.PlaylistVisibility) error
	SetPlaylistRule(ctx context.Context, pid model.PID, rule query.Query) error
	SetPlaylistOwner(ctx context.Context, pid, ownerPID model.PID) error
	TransferPlaylists(ctx context.Context, fromPID, toPID model.PID) (int, error)
	PlaylistItems(ctx context.Context, pid model.PID, userPID model.PID) ([]*model.ItemView, error)
	CountPlaylistItems(ctx context.Context, pid model.PID, userPID model.PID, narrow query.Node) (int, error)
	AddPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error
	SetPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error
	RemovePlaylistItem(ctx context.Context, pid model.PID, itemPID model.PID) error
	RemovePlaylistItemAt(ctx context.Context, pid model.PID, index int, expect model.PID) error
	RemovePlaylistItemsAt(ctx context.Context, pid model.PID, indexes []int, expect []model.PID) error
	ItemsByPlaylistPath(ctx context.Context, path string) ([]*model.ItemView, error)
	UserByPID(ctx context.Context, pid model.PID) (*model.User, error)
}

// Service exposes playlist operations to consumers.
type Service struct{ store Store }

// New builds a playlist service over a store.
func New(store Store) *Service { return &Service{store: store} }

// CreateStatic creates an empty static playlist owned by ownerPID (empty =
// default user).
func (s *Service) CreateStatic(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility) (model.PID, error) {
	return s.store.CreatePlaylist(ctx, name, ownerPID, model.PlaylistStatic, vis, nil)
}

// CreateSmart creates a smart playlist whose membership is the rule evaluated on
// read.
func (s *Service) CreateSmart(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility, rule query.Query) (model.PID, error) {
	return s.store.CreatePlaylist(ctx, name, ownerPID, model.PlaylistSmart, vis, &rule)
}

// List returns the playlists visible to ownerPID (own plus shared).
func (s *Service) List(ctx context.Context, ownerPID model.PID) ([]*model.Playlist, error) {
	return s.store.ListPlaylists(ctx, ownerPID)
}

// Get returns one playlist's metadata.
func (s *Service) Get(ctx context.Context, pid model.PID) (*model.Playlist, error) {
	return s.store.PlaylistByPID(ctx, pid)
}

// Items returns a playlist's members: a static list's stored order, or a smart rule
// evaluated on read. If a smart rule references a per-user field such as rating,
// starred, or play_count, it evaluates against userPID's play_state, so one playlist
// yields different membership per user. An empty userPID selects the default user.
// The user is bound at read time and never stored in the rule.
func (s *Service) Items(ctx context.Context, pid model.PID, userPID model.PID) ([]*model.ItemView, error) {
	return s.store.PlaylistItems(ctx, pid, userPID)
}

// CountItems returns how many of a playlist's members also match narrow, without
// hydrating them. A nil narrow counts every member. The contract is exact: the result
// equals the number of rows Items(pid, userPID) returns that satisfy narrow, for both
// playlist kinds and whatever limit or offset a smart rule carries.
//
// Three "playlist count" semantics now exist, and a static playlist holding A twice
// plus B reports 3, 2, 3:
//
//	model.Playlist.ItemCount  COUNT(*) entries    static only, no user, no narrow
//	the playlist facet bucket COUNT(DISTINCT)     narrowed, owner-scoped, static only
//	CountItems                COUNT(*) entries    narrowed, both kinds, per-user
//
// COUNT(*) is what the stated contract requires here, so this is documentation rather
// than a semantics change.
//
// This is the single-playlist answer; the facet is the fan-out answer. A caller
// looping this over many playlists should use Library.Facet with read.GroupPlaylist
// and the same narrow as the query instead, which returns the narrowed count for every
// visible static playlist in one query, owner-scoped to that caller. The split is
// intentional rather than redundant: the facet excludes smart playlists, which store no
// membership rows, and it counts distinct items where this counts entries.
//
// One case cannot be exact. A random-limited rule with no seed draws a fresh order per
// evaluation, so with a non-nil narrow the count evaluates one draw and a following
// Items call is a different draw. With a nil narrow the answer is min(matches, limit)
// and stays stable, and a seeded or budget-mode rule is deterministic.
func (s *Service) CountItems(ctx context.Context, pid model.PID, userPID model.PID, narrow query.Node) (int, error) {
	return s.store.CountPlaylistItems(ctx, pid, userPID, narrow)
}

// Delete removes a playlist.
func (s *Service) Delete(ctx context.Context, pid model.PID) error {
	return s.store.DeletePlaylist(ctx, pid)
}

// Rename sets a playlist's name.
func (s *Service) Rename(ctx context.Context, pid model.PID, name string) error {
	return s.store.RenamePlaylist(ctx, pid, name)
}

// SetVisibility changes a playlist's visibility.
func (s *Service) SetVisibility(ctx context.Context, pid model.PID, vis model.PlaylistVisibility) error {
	return s.store.SetPlaylistVisibility(ctx, pid, vis)
}

// SetOwner moves a playlist to another user (empty selects the default user); an
// unknown playlist or user is CodeNotFound. Who sees a private playlist follows its
// owner, while a smart rule over per-user state still evaluates for its reader.
func (s *Service) SetOwner(ctx context.Context, pid, ownerPID model.PID) error {
	return s.store.SetPlaylistOwner(ctx, pid, ownerPID)
}

// TransferPlaylists moves every playlist fromPID owns to toPID and returns how many
// moved, the step before a user is retired.
func (s *Service) TransferPlaylists(ctx context.Context, fromPID, toPID model.PID) (int, error) {
	return s.store.TransferPlaylists(ctx, fromPID, toPID)
}

// SetRule replaces a smart playlist's rule in place. The pid is stable across
// the edit; membership follows on the next read (rules are evaluated on read).
// The rule is validated like CreateSmart's; a static playlist is rejected.
func (s *Service) SetRule(ctx context.Context, pid model.PID, rule query.Query) error {
	return s.store.SetPlaylistRule(ctx, pid, rule)
}

// Add appends items to a static playlist.
func (s *Service) Add(ctx context.Context, pid model.PID, itemPIDs ...model.PID) error {
	return s.store.AddPlaylistItems(ctx, pid, itemPIDs)
}

// Set replaces a static playlist's contents (reorder/replace).
func (s *Service) Set(ctx context.Context, pid model.PID, itemPIDs []model.PID) error {
	return s.store.SetPlaylistItems(ctx, pid, itemPIDs)
}

// Remove drops every occurrence of an item from a static playlist.
func (s *Service) Remove(ctx context.Context, pid model.PID, itemPID model.PID) error {
	return s.store.RemovePlaylistItem(ctx, pid, itemPID)
}

// RemoveAt drops the entry at one index of a static playlist's listing (0 is the first
// entry Items returns), so a single occurrence of a duplicated item can go. Later
// entries move up one index. A non-empty expect is the item the caller saw at that
// index: an entry holding another one, or none, is refused with CodeConflict. A
// negative index is CodeInvalid and an unguarded one past the end CodeNotFound.
func (s *Service) RemoveAt(ctx context.Context, pid model.PID, index int, expect model.PID) error {
	return s.store.RemovePlaylistItemAt(ctx, pid, index, expect)
}

// RemoveAtMany drops the entries at several indexes in one step, each index naming the
// listing as it stood before any entry went. expect is nil, or the item each index must
// still hold (an empty pid skips that check). Any refusal removes nothing.
func (s *Service) RemoveAtMany(ctx context.Context, pid model.PID, indexes []int, expect []model.PID) error {
	return s.store.RemovePlaylistItemsAt(ctx, pid, indexes, expect)
}

// ExportM3U8 writes a playlist's current members as an extended M3U (#EXTM3U)
// document: a #EXTINF metadata line plus the file path per item. Smart playlists
// export their evaluated membership, so the file is a static snapshot; that
// membership is evaluated for userPID (empty selects the default user) when the
// rule references per-user state.
//
// A playlist's cover art is not exported: the format has no standard cover
// directive, and this document carries track metadata only. A consumer that wants
// the cover reads it through ResolveArt on the playlist reference instead.
func (s *Service) ExportM3U8(ctx context.Context, pid model.PID, w io.Writer, userPID model.PID) error {
	const op = "playlist.ExportM3U8"
	items, err := s.store.PlaylistItems(ctx, pid, userPID)
	if err != nil {
		return err
	}
	// M3U8 is line-based; a path containing a newline cannot be represented.
	// Reject it before writing a playlist that would split the path on re-import.
	for _, it := range items {
		if strings.ContainsAny(it.DisplayPath, "\r\n") {
			return waxerr.New(waxerr.CodeInvalid, op,
				"item path contains a newline and cannot be written to M3U8: "+string(it.PID))
		}
	}
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "#EXTM3U")
	for _, it := range items {
		fmt.Fprintf(bw, "#EXTINF:%d,%s\n", it.DurationMS/1000, m3uMeta(it.Artist, it.Title))
		fmt.Fprintln(bw, it.DisplayPath)
	}
	if err := bw.Flush(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

// m3uMeta renders the "#EXTINF" artist-title label with line breaks folded to
// spaces, since the directive is a single line.
func m3uMeta(artist, title string) string {
	folder := strings.NewReplacer("\r", " ", "\n", " ")
	return folder.Replace(artist) + " - " + folder.Replace(title)
}

// ImportResult reports an M3U8 import: the new playlist, how many lines became
// entries, how many joined the entry before them (Merged: the next file of the same
// item, such as a book's next part), and which paths matched nothing. The three counts
// add up to the document's path lines.
type ImportResult struct {
	PlaylistPID    model.PID
	Matched        int
	Merged         int
	Unmatched      int
	UnmatchedPaths []string
}

// CheckImport refuses what no import could create a playlist with (a blank name, an
// unknown visibility, an owner that is no user), so ImportM3U8 fails before it looks
// any entry up, and a preview fails where the import would.
func (s *Service) CheckImport(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility) error {
	const op = "playlist.ImportM3U8"
	if strings.TrimSpace(name) == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "playlist name is required")
	}
	if vis != "" && !vis.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "unknown visibility: "+string(vis))
	}
	_, err := s.store.UserByPID(ctx, ownerPID)
	return err
}

// M3UMatch is one path line of an M3U8 document and the cataloged item it names, nil
// when it names none or several its #EXTINF line cannot tell apart. Merged marks a line
// naming the item the line before it named through another of its files (a book's next
// part, a copy), which an import adds once for the run; a path given twice is a repeat
// and stays two entries.
type M3UMatch struct {
	Path   string
	Item   *model.ItemView
	Merged bool
}

// ResolveM3U8 matches every path line of an M3U8 document against the catalog in
// document order and writes nothing, so it answers on a read-only library and shows
// what an import would build. A path names the items behind its file through any of
// their files (an exact path, or a relative-path suffix); when it names several, as a
// cue rip's file names each of its tracks, the entry's #EXTINF label picks one.
func (s *Service) ResolveM3U8(ctx context.Context, r io.Reader) ([]M3UMatch, error) {
	return s.resolveM3U8(ctx, "playlist.ResolveM3U8", r)
}

func (s *Service) resolveM3U8(ctx context.Context, op string, r io.Reader) ([]M3UMatch, error) {
	entries, err := parseM3U8(r)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	out := make([]M3UMatch, 0, len(entries))
	for i, e := range entries {
		items, err := s.store.ItemsByPlaylistPath(ctx, e.path)
		if err != nil {
			return nil, err
		}
		m := M3UMatch{Path: e.path, Item: e.pick(items)}
		if i > 0 && m.Item != nil {
			prev := out[i-1]
			m.Merged = prev.Item != nil && prev.Item.PID == m.Item.PID && cleanPath(prev.Path) != cleanPath(m.Path)
		}
		out = append(out, m)
	}
	return out, nil
}

func cleanPath(p string) string { return filepath.Clean(filepath.FromSlash(p)) }

// ImportM3U8 creates a static playlist from an M3U8 document as ResolveM3U8 matches it,
// once CheckImport passes. Unmatched paths are skipped and reported, never invented, a
// merged line adds no entry, and an empty file yields an empty playlist. The playlist
// and its entries are one write, so a failure leaves no playlist behind.
func (s *Service) ImportM3U8(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility, r io.Reader) (*ImportResult, error) {
	if err := s.CheckImport(ctx, name, ownerPID, vis); err != nil {
		return nil, err
	}
	matches, err := s.resolveM3U8(ctx, "playlist.ImportM3U8", r)
	if err != nil {
		return nil, err
	}
	res := &ImportResult{}
	var matched []model.PID
	for _, m := range matches {
		switch {
		case m.Item == nil:
			res.Unmatched++
			res.UnmatchedPaths = append(res.UnmatchedPaths, m.Path)
		case m.Merged:
			res.Merged++
		default:
			matched = append(matched, m.Item.PID)
		}
	}
	pid, err := s.store.CreatePlaylistWithItems(ctx, name, ownerPID, vis, matched)
	if err != nil {
		return nil, err
	}
	res.PlaylistPID = pid
	res.Matched = len(matched)
	return res, nil
}

// m3uEntry is one path line of an M3U8 document with the #EXTINF line before it.
type m3uEntry struct {
	path    string
	seconds int // the stated length, -1 when none
	label   string
}

// pick chooses among the items an entry's path names: the only one, else the one its
// label names as ExportM3U8 writes it ("artist - title") or by its title alone, the
// stated length breaking a tie between two of one name. It picks nothing when that
// leaves none or several.
func (e m3uEntry) pick(items []*model.ItemView) *model.ItemView {
	if len(items) == 1 {
		return items[0]
	}
	key := identity.MatchKey(e.label)
	if key == "" {
		return nil
	}
	var named []*model.ItemView
	for _, it := range items {
		if identity.MatchKey(m3uMeta(it.Artist, it.Title)) == key || identity.MatchKey(it.Title) == key {
			named = append(named, it)
		}
	}
	if len(named) > 1 && e.seconds >= 0 {
		var timed []*model.ItemView
		for _, it := range named {
			if d := it.DurationMS/1000 - int64(e.seconds); d >= -1 && d <= 1 {
				timed = append(timed, it)
			}
		}
		named = timed
	}
	if len(named) == 1 {
		return named[0]
	}
	return nil
}

// parseM3U8 extracts the file-path entries from an M3U8 document: every non-empty
// line that is not a directive (#-prefixed), each with the #EXTINF line before it.
// Both extended (#EXTM3U) and plain path-only playlists parse the same way, and a
// byte-order mark (Notepad writes one) is not part of the first line.
func parseM3U8(r io.Reader) ([]m3uEntry, error) {
	var out []m3uEntry
	next := m3uEntry{seconds: -1}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // tolerate long path lines
	first := true
	for sc.Scan() {
		line := sc.Text()
		if first {
			line, first = strings.TrimPrefix(line, "\ufeff"), false
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXTINF:"):
			next = parseExtInf(strings.TrimPrefix(line, "#EXTINF:"))
		case strings.HasPrefix(line, "#"):
		default:
			next.path = line
			out = append(out, next)
			next = m3uEntry{seconds: -1}
		}
	}
	return out, sc.Err()
}

// parseExtInf reads "length[ attributes],label". The label starts after the first comma
// outside a quoted attribute value; a length that is not a number, or is negative (the
// format's unknown), is no length.
func parseExtInf(info string) m3uEntry {
	head, label := info, ""
	quoted := false
	for i, r := range info {
		if r == '"' {
			quoted = !quoted
		} else if r == ',' && !quoted {
			head, label = info[:i], info[i+1:]
			break
		}
	}
	e := m3uEntry{seconds: -1, label: strings.TrimSpace(label)}
	if f := strings.Fields(head); len(f) > 0 {
		if n, err := strconv.ParseFloat(f[0], 64); err == nil && n >= 0 {
			e.seconds = int(n)
		}
	}
	return e
}
