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
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the playlist service needs (satisfied by store/sqlite).
type Store interface {
	CreatePlaylist(ctx context.Context, name string, ownerPID model.PID, kind model.PlaylistKind, vis model.PlaylistVisibility, rule *query.Query) (model.PID, error)
	PlaylistByPID(ctx context.Context, pid model.PID) (*model.Playlist, error)
	ListPlaylists(ctx context.Context, ownerPID model.PID) ([]*model.Playlist, error)
	DeletePlaylist(ctx context.Context, pid model.PID) error
	RenamePlaylist(ctx context.Context, pid model.PID, name string) error
	SetPlaylistVisibility(ctx context.Context, pid model.PID, vis model.PlaylistVisibility) error
	SetPlaylistRule(ctx context.Context, pid model.PID, rule query.Query) error
	PlaylistItems(ctx context.Context, pid model.PID, userPID model.PID) ([]*model.ItemView, error)
	AddPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error
	SetPlaylistItems(ctx context.Context, pid model.PID, itemPIDs []model.PID) error
	RemovePlaylistItem(ctx context.Context, pid model.PID, itemPID model.PID) error
	RemovePlaylistItemAt(ctx context.Context, pid model.PID, position int) error
	ItemByPlaylistPath(ctx context.Context, path string) (*model.ItemView, error)
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

// RemoveAt drops a single occurrence of a static playlist by its position.
func (s *Service) RemoveAt(ctx context.Context, pid model.PID, position int) error {
	return s.store.RemovePlaylistItemAt(ctx, pid, position)
}

// ExportM3U8 writes a playlist's current members as an extended M3U (#EXTM3U)
// document: a #EXTINF metadata line plus the file path per item. Smart playlists
// export their evaluated membership, so the file is a static snapshot; that
// membership is evaluated for userPID (empty selects the default user) when the
// rule references per-user state.
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

// ImportResult reports an M3U8 import: the new playlist plus how many entries
// matched cataloged items and which paths did not.
type ImportResult struct {
	PlaylistPID    model.PID
	Matched        int
	Unmatched      int
	UnmatchedPaths []string
}

// ImportM3U8 creates a static playlist from an M3U8 document, matching each entry
// to a cataloged item by exact path or a unique relative-path suffix. Unmatched
// paths are skipped and reported, never invented; an empty file yields an empty
// playlist.
func (s *Service) ImportM3U8(ctx context.Context, name string, ownerPID model.PID, vis model.PlaylistVisibility, r io.Reader) (*ImportResult, error) {
	const op = "playlist.ImportM3U8"
	paths, err := parseM3U8(r)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	var matched []model.PID
	res := &ImportResult{}
	for _, p := range paths {
		it, err := s.store.ItemByPlaylistPath(ctx, p)
		if err != nil {
			if waxerr.Is(err, waxerr.CodeNotFound) {
				res.Unmatched++
				res.UnmatchedPaths = append(res.UnmatchedPaths, p)
				continue
			}
			return nil, err
		}
		matched = append(matched, it.PID)
	}

	pid, err := s.store.CreatePlaylist(ctx, name, ownerPID, model.PlaylistStatic, vis, nil)
	if err != nil {
		return nil, err
	}
	if len(matched) > 0 {
		if err := s.store.SetPlaylistItems(ctx, pid, matched); err != nil {
			return nil, err
		}
	}
	res.PlaylistPID = pid
	res.Matched = len(matched)
	return res, nil
}

// parseM3U8 extracts the file-path entries from an M3U8 document: every non-empty
// line that is not a directive (#-prefixed). Both extended (#EXTM3U) and plain
// path-only playlists parse the same way.
func parseM3U8(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // tolerate long path lines
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
