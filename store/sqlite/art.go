package sqlite

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// thumbCacheMax bounds the in-process thumbnail cache by entry count. Generated
// thumbnails are small (a few KB to tens of KB at typical box sizes), so a few
// hundred entries is a modest, predictable memory footprint.
const thumbCacheMax = 256

// thumbCache is a bounded in-process LRU of generated thumbnails keyed by (source
// hash, size). It sits in front of the thumb_cache table: it serves a read-only store,
// which cannot persist to the table and would otherwise regenerate on every request,
// and it saves a read-write store a SQL round-trip and re-decode for a hot cover. It
// is safe for concurrent use.
type thumbCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List // front = most recently used; values are *thumbEntry
	items map[thumbKey]*list.Element
}

type thumbKey struct {
	hash string
	size int
}

type thumbEntry struct {
	key  thumbKey
	blob model.ArtBlob // Bytes is treated as immutable once cached
}

func newThumbCache(max int) *thumbCache {
	return &thumbCache{max: max, ll: list.New(), items: map[thumbKey]*list.Element{}}
}

// get returns a cached thumbnail blob and marks it most-recently-used. A nil cache
// (defensive) is a permanent miss. The returned blob's Bytes is a private copy: the
// cache shares no backing array with callers, so a caller that mutates the bytes
// cannot corrupt the cached entry (or another caller's view of it).
func (c *thumbCache) get(hash string, size int) (model.ArtBlob, bool) {
	if c == nil {
		return model.ArtBlob{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[thumbKey{hash, size}]; ok {
		c.ll.MoveToFront(el)
		return cloneArtBlob(el.Value.(*thumbEntry).blob), true
	}
	return model.ArtBlob{}, false
}

// cloneArtBlob returns a copy of b whose Bytes shares no backing array with the
// original, so the cache and its callers cannot mutate each other's bytes.
func cloneArtBlob(b model.ArtBlob) model.ArtBlob {
	if b.Bytes != nil {
		b.Bytes = append([]byte(nil), b.Bytes...)
	}
	return b
}

// put inserts or refreshes a thumbnail, evicting the least-recently-used entries
// past the bound. It stores a private copy of the blob's bytes so a later mutation of
// the caller's slice cannot reach the cache.
func (c *thumbCache) put(hash string, size int, blob model.ArtBlob) {
	if c == nil {
		return
	}
	stored := cloneArtBlob(blob)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := thumbKey{hash, size}
	if el, ok := c.items[key]; ok {
		el.Value.(*thumbEntry).blob = stored
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&thumbEntry{key: key, blob: stored})
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*thumbEntry).key)
	}
}

// attachArtTxChanged maps a track/book item's cover onto the 'track' art slot
// (keyed by the item id) and reports whether the mapping changed, for the
// music/audiobook write paths that emit a delta only on a real change. See
// attachEntityArtTxChanged for the shared body.
func attachArtTxChanged(ctx context.Context, tx *sql.Tx, itemID int64, img *model.ArtImage) (bool, error) {
	return attachEntityArtTxChanged(ctx, tx, "track", itemID, img)
}

// insertArtSourceTx dedups a decoded/probed cover into the content-addressed
// art_source store (keyed by content hash), a no-op when the source is already
// present. It is the single art-blob writer shared by the front-cover attach and the
// role-scoped entity-art set.
func insertArtSourceTx(ctx context.Context, tx *sql.Tx, img *model.ArtImage) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO art_source(hash, format, width, height, size, data, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		img.Hash, img.Format, img.Width, img.Height, len(img.Data), img.Data, nowNS())
	return err
}

// attachEntityArtTx is the error-only wrapper over attachEntityArtTxChanged for the
// callers that do not need the changed signal.
func attachEntityArtTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, img *model.ArtImage) error {
	_, err := attachEntityArtTxChanged(ctx, tx, entityType, entityID, img)
	return err
}

// attachEntityArtTxChanged dedups a front-cover image into the content-addressed art
// store and maps it to one entity (entity_type, entity_id). It backs every cover
// ingest: a track/book item ('track'), a podcast feed ('podcast'), and an episode
// ('episode'). Album art is derived on read from current track maps, so a re-cover,
// retag, or delete cannot leave a stale album mapping behind. The write is
// idempotent: when the entity already maps this exact cover it does nothing (and
// reports false), so it can run on every scan/sync without churn. A nil/empty image
// is a no-op; a missing read does not mean the art was removed. It touches the
// front role and nothing else: a scan or feed re-sync must not clobber the
// back/booklet/... slots a user set through SetItemArt/SetEntityArt.
func attachEntityArtTxChanged(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, img *model.ArtImage) (bool, error) {
	if img == nil || len(img.Data) == 0 || img.Hash == "" {
		return false, nil
	}
	var curHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT source_hash FROM art_map WHERE entity_type = ? AND entity_id = ? AND role = 'front'",
		entityType, entityID).Scan(&curHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if curHash.Valid && curHash.String == img.Hash {
		return false, nil
	}
	// Re-point this entity's front cover through the shared slot writer; an entity
	// has exactly one image per role. When the old cover loses its last referencing
	// map row it becomes an orphaned source for GCArt.
	if err := setEntityArtRoleTx(ctx, tx, entityType, entityID, string(model.ArtRoleFront), img); err != nil {
		return false, err
	}
	return true, nil
}

// artLevel is one rung of the resolution fallback chain: an entity type and its
// internal id.
type artLevel struct {
	typ string
	id  int64
}

// ResolveArt resolves art for an entity in one role. The front role walks the
// fallback chain from the requested level up toward the root (track -> album ->
// release_group -> artist -> genre) and answers from the first level that has a
// front cover; every other role resolves at the requested level alone, since an
// ancestor's back cover or booklet would be misleading for a descendant. An empty
// role means front. size selects the output: a non-positive size returns the
// original source; a positive size returns a thumbnail scaled to fit a square box
// with that maximum side. Generated thumbnails are cached and never upscaled. A
// source that cannot be decoded for thumbnailing is returned as-is. The blob
// carries the answering Level and, for an album answered from a member track's
// cover, Derived. CodeNotFound means no consulted level has art in that role.
func (s *Store) ResolveArt(ctx context.Context, ref model.EntityRef, role model.ArtRole, size int) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	if !ref.Type.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
	}
	if role == "" {
		role = model.ArtRoleFront
	}
	if !role.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art role: "+string(role))
	}
	chain, err := s.artChain(ctx, ref)
	if err != nil {
		return nil, err
	}
	// Non-front roles never inherit from an ancestor: truncate the chain to the
	// requested entity itself (always the first level artChain builds).
	if role != model.ArtRoleFront && len(chain) > 1 {
		chain = chain[:1]
	}
	hash, level, derived, ok, err := s.artInChain(ctx, chain, role)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no "+string(role)+" art for "+string(ref.Type)+":"+string(ref.PID))
	}

	srcData, srcFormat, srcW, srcH, err := s.artSource(ctx, hash)
	if err != nil {
		return nil, err
	}
	blob := &model.ArtBlob{Bytes: srcData, Format: srcFormat, Width: srcW, Height: srcH, SourceHash: hash,
		Level: model.ArtEntity(level), Derived: derived}

	// Original requested, or a source already within the box: serve the source.
	longest := max(srcW, srcH)
	if size <= 0 || (longest > 0 && longest <= size) {
		return blob, nil
	}
	// Dimensions unknown (an undecodable or exotic source, e.g. an AVIF/HEIC cover
	// with no pure-Go decoder): there is nothing to scale, so serve the original.
	if longest == 0 {
		return blob, nil
	}
	thumb, err := s.thumbnail(ctx, hash, srcData, srcFormat, srcW, srcH, size)
	if err != nil {
		return nil, err
	}
	// The thumbnail cache is keyed by (source, size) alone; the same cached bytes
	// can answer different levels (a track's own cover vs a sibling resolving it
	// through the album), so the level is stamped per request, never cached.
	thumb.Level, thumb.Derived = model.ArtEntity(level), derived
	return thumb, nil
}

// ArtRoles lists the artwork slots an entity holds at its own level, with no
// chain fallback: each stored role with its source's format, dimensions, and
// hash, in role order. An entity with no art returns an empty list, not an
// error, so a caller can distinguish "nothing stored" from "no such entity".
func (s *Store) ArtRoles(ctx context.Context, ref model.EntityRef) ([]model.ArtRoleInfo, error) {
	const op = "store.ArtRoles"
	if !ref.Type.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
	}
	// artChain resolves the pid to its internal id (erroring on an unknown entity);
	// the first level is always the requested entity itself.
	chain, err := s.artChain(ctx, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx,
		`SELECT m.role, s.format, s.width, s.height, s.hash
		 FROM art_map m JOIN art_source s ON s.hash = m.source_hash
		 WHERE m.entity_type = ? AND m.entity_id = ? ORDER BY m.role`,
		chain[0].typ, chain[0].id)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ArtRoleInfo
	for rows.Next() {
		var info model.ArtRoleInfo
		var role string
		if err := rows.Scan(&role, &info.Format, &info.Width, &info.Height, &info.SourceHash); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		info.Role = model.ArtRole(role)
		out = append(out, info)
	}
	return out, rows.Err()
}

// thumbnail returns a cached thumbnail for (source hash, size), generating and
// caching it on a miss. A generation failure (e.g. an exotic source format) falls
// back to the original source, served from the bytes/metadata the caller already
// loaded (no re-fetch).
func (s *Store) thumbnail(ctx context.Context, hash string, srcData []byte, srcFormat string, srcW, srcH, size int) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	// Check the in-process cache first. This serves a read-only store, which cannot
	// persist to thumb_cache and would otherwise regenerate on every request, and it
	// saves a read-write store the SQL round-trip and re-decode for a hot cover.
	if blob, ok := s.thumbMem.get(hash, size); ok {
		b := blob
		return &b, nil
	}

	var data []byte
	var format string
	var w, h int
	err := s.read.QueryRowContext(ctx,
		"SELECT data, format, width, height FROM thumb_cache WHERE source_hash = ? AND size = ?",
		hash, size).Scan(&data, &format, &w, &h)
	if err == nil {
		blob := model.ArtBlob{Bytes: data, Format: format, Width: w, Height: h, SourceHash: hash, Thumbnail: true}
		s.thumbMem.put(hash, size, blob)
		b := blob
		return &b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	thumb, tFormat, tw, th, gerr := art.Thumbnail(srcData, size)
	if gerr != nil {
		// The pure-Go decoders cannot handle this source (an undecodable or exotic
		// format such as AVIF/HEIC): serve the original unscaled rather than failing.
		s.log.Warn("art thumbnail generation failed; serving original", "hash", hash, "size", size, "err", gerr)
		return &model.ArtBlob{Bytes: srcData, Format: srcFormat, Width: srcW, Height: srcH, SourceHash: hash}, nil
	}

	blob := model.ArtBlob{Bytes: thumb, Format: tFormat, Width: tw, Height: th, SourceHash: hash, Thumbnail: true}
	s.thumbMem.put(hash, size, blob)

	// Best-effort cache write: a read-only library still serves the generated thumbnail
	// from the in-process cache above, just without persisting it to disk.
	if !s.readOnly {
		if err := s.writeTx(ctx, func(tx *sql.Tx) error {
			_, e := tx.ExecContext(ctx,
				`INSERT OR REPLACE INTO thumb_cache(source_hash, size, format, width, height, data, created_at)
				 VALUES (?,?,?,?,?,?,?)`, hash, size, tFormat, tw, th, thumb, nowNS())
			return e
		}); err != nil {
			s.log.Warn("caching thumbnail", "hash", hash, "size", size, "err", err)
		}
	}
	b := blob
	return &b, nil
}

// artSource loads a source image's bytes and metadata by hash.
func (s *Store) artSource(ctx context.Context, hash string) ([]byte, string, int, int, error) {
	const op = "store.ResolveArt"
	var data []byte
	var format string
	var w, h int
	err := s.read.QueryRowContext(ctx,
		"SELECT data, format, width, height FROM art_source WHERE hash = ?", hash).Scan(&data, &format, &w, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", 0, 0, waxerr.New(waxerr.CodeNotFound, op, "art source missing: "+hash)
	}
	if err != nil {
		return nil, "", 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return data, format, w, h, nil
}

// artInChain returns the source hash of the first chain level that has art in the
// requested role, with that level and whether the answer was derived; ok=false
// when none does. The role filter is what keeps a level's back/booklet slots from
// answering a front-cover walk (any-role rows used to win by rowid order). The
// primary key holds one row per (entity, role), so a direct lookup needs no
// ordering. Album front art with no direct entry is derived from the album's
// track covers, so it is always a cover currently carried by a track in that
// album, never a stale denormalized mapping left by a re-cover, cross-album
// retag, or deletion; the member ORDER BY rowid keeps the pick stable. The
// derivation applies to the front cover alone: the other roles answer from a
// durable row at the entity's own level or not at all, matching the public
// contract that non-front roles never look past the requested entity.
func (s *Store) artInChain(ctx context.Context, chain []artLevel, role model.ArtRole) (hash string, level string, derived, ok bool, err error) {
	const op = "store.ResolveArt"
	for _, lv := range chain {
		if lv.id == 0 {
			continue
		}
		qerr := s.read.QueryRowContext(ctx,
			"SELECT source_hash FROM art_map WHERE entity_type = ? AND entity_id = ? AND role = ?",
			lv.typ, lv.id, string(role)).Scan(&hash)
		if qerr == nil {
			return hash, lv.typ, false, true, nil
		}
		if !errors.Is(qerr, sql.ErrNoRows) {
			return "", "", false, false, waxerr.Wrap(waxerr.CodeIO, op, qerr)
		}
		if lv.typ == string(model.ArtAlbum) && role == model.ArtRoleFront {
			derr := s.read.QueryRowContext(ctx,
				`SELECT tm.source_hash FROM art_map tm JOIN track t ON t.item_id = tm.entity_id
				 WHERE tm.entity_type = 'track' AND tm.role = 'front' AND t.album_id = ?
				 ORDER BY tm.rowid LIMIT 1`, lv.id).Scan(&hash)
			if derr == nil {
				return hash, lv.typ, true, true, nil
			}
			if !errors.Is(derr, sql.ErrNoRows) {
				return "", "", false, false, waxerr.Wrap(waxerr.CodeIO, op, derr)
			}
		}
	}
	return "", "", false, false, nil
}

// artChain builds the resolution fallback chain for a reference: the requested
// level first, then its ancestors up to the genre root, each as (type, internal
// id). Missing ancestors are dropped, so the chain only contains resolvable levels.
// A returned chain is never empty: every branch resolves the requested entity
// itself before appending ancestors, and an unknown pid or type errors instead.
// ResolveArt's non-front truncation (chain[:1]) and ArtRoles' own-level read
// (chain[0]) both rely on that.
func (s *Store) artChain(ctx context.Context, ref model.EntityRef) ([]artLevel, error) {
	const op = "store.ResolveArt"
	switch ref.Type {
	case model.ArtTrack:
		return s.trackArtChain(ctx, ref.PID)
	case model.ArtAlbum:
		albumID, err := s.idByPID(ctx, "album", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return s.albumArtChain(ctx, albumID)
	case model.ArtReleaseGroup:
		rgID, err := s.idByPID(ctx, "release_group", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return s.releaseGroupArtChain(ctx, rgID)
	case model.ArtArtist:
		artistID, err := s.idByPID(ctx, "artist", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtArtist), artistID}}, nil
	case model.ArtGenre:
		genreID, err := s.idByPID(ctx, "genre", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtGenre), genreID}}, nil
	case model.ArtEpisode:
		return s.episodeArtChain(ctx, ref.PID)
	case model.ArtPodcast:
		podID, err := s.idByPID(ctx, "podcast", ref.PID, op)
		if err != nil {
			return nil, err
		}
		return []artLevel{{string(model.ArtPodcast), podID}}, nil
	case model.ArtPlaylist:
		plID, err := s.idByPID(ctx, "playlist", ref.PID, op)
		if err != nil {
			return nil, err
		}
		// One rung on purpose: a playlist has no ancestor, so even a front cover has
		// nowhere to fall back to.
		return []artLevel{{string(model.ArtPlaylist), plID}}, nil
	}
	return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
}

// episodeArtChain resolves a podcast episode's chain: episode -> podcast. The
// episode's own artwork wins; a feed image is the fallback for an episode without
// one.
func (s *Store) episodeArtChain(ctx context.Context, pid model.PID) ([]artLevel, error) {
	const op = "store.ResolveArt"
	var itemID, podcastID int64
	err := s.read.QueryRowContext(ctx,
		`SELECT pi.id, ep.podcast_id FROM playable_item pi
		 JOIN episode ep ON ep.item_id = pi.id WHERE pi.pid = ?`, string(pid)).Scan(&itemID, &podcastID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such episode: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return []artLevel{
		{string(model.ArtEpisode), itemID},
		{string(model.ArtPodcast), podcastID},
	}, nil
}

// trackArtChain resolves a track's full chain: track -> album -> release_group ->
// artist -> genre. The artist level is the release group's primary artist, falling
// back to the track's album artist then artist; the genre level is the item's first
// genre.
func (s *Store) trackArtChain(ctx context.Context, pid model.PID) ([]artLevel, error) {
	const op = "store.ResolveArt"
	var itemID int64
	var kind string
	var albumID, albumArtistID, artistID sql.NullInt64
	err := s.read.QueryRowContext(ctx,
		`SELECT pi.id, pi.kind, t.album_id, t.album_artist_id, t.artist_id
		 FROM playable_item pi LEFT JOIN track t ON t.item_id = pi.id WHERE pi.pid = ?`, string(pid)).
		Scan(&itemID, &kind, &albumID, &albumArtistID, &artistID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	chain := []artLevel{{string(model.ArtTrack), itemID}}
	// A book stores its cover at the item level (the 'track' art_map slot, keyed by
	// the item id), then falls back to its author artist and first genre. It has no
	// album/release-group rungs, so resolve it directly and return.
	if kind == string(model.KindBook) {
		var authorID sql.NullInt64
		if err := s.read.QueryRowContext(ctx,
			"SELECT author_id FROM book WHERE item_id = ?", itemID).Scan(&authorID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if authorID.Valid {
			chain = append(chain, artLevel{string(model.ArtArtist), authorID.Int64})
		}
		if gid := s.firstItemGenre(ctx, itemID); gid != 0 {
			chain = append(chain, artLevel{string(model.ArtGenre), gid})
		}
		return chain, nil
	}
	var rgID, primaryArtistID int64
	if albumID.Valid {
		chain = append(chain, artLevel{string(model.ArtAlbum), albumID.Int64})
		rgID, primaryArtistID, err = s.albumParents(ctx, albumID.Int64)
		if err != nil {
			return nil, err
		}
		if rgID != 0 {
			chain = append(chain, artLevel{string(model.ArtReleaseGroup), rgID})
		}
	}
	// Artist level: prefer the release group's primary artist, then album artist,
	// then track artist.
	artLevelID := primaryArtistID
	if artLevelID == 0 && albumArtistID.Valid {
		artLevelID = albumArtistID.Int64
	}
	if artLevelID == 0 && artistID.Valid {
		artLevelID = artistID.Int64
	}
	if artLevelID != 0 {
		chain = append(chain, artLevel{string(model.ArtArtist), artLevelID})
	}
	if gid := s.firstItemGenre(ctx, itemID); gid != 0 {
		chain = append(chain, artLevel{string(model.ArtGenre), gid})
	}
	return chain, nil
}

func (s *Store) albumArtChain(ctx context.Context, albumID int64) ([]artLevel, error) {
	chain := []artLevel{{string(model.ArtAlbum), albumID}}
	rgID, artistID, err := s.albumParents(ctx, albumID)
	if err != nil {
		return nil, err
	}
	if rgID != 0 {
		chain = append(chain, artLevel{string(model.ArtReleaseGroup), rgID})
	}
	if artistID != 0 {
		chain = append(chain, artLevel{string(model.ArtArtist), artistID})
	}
	return chain, nil
}

func (s *Store) releaseGroupArtChain(ctx context.Context, rgID int64) ([]artLevel, error) {
	chain := []artLevel{{string(model.ArtReleaseGroup), rgID}}
	var artistID sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT primary_artist_id FROM release_group WHERE id = ?", rgID).Scan(&artistID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if artistID.Valid {
		chain = append(chain, artLevel{string(model.ArtArtist), artistID.Int64})
	}
	return chain, nil
}

// albumParents returns an album's release-group id and that group's primary-artist
// id (each 0 when absent).
func (s *Store) albumParents(ctx context.Context, albumID int64) (rgID, artistID int64, err error) {
	var rg sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT release_group_id FROM album WHERE id = ?", albumID).Scan(&rg); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if !rg.Valid {
		return 0, 0, nil
	}
	var artist sql.NullInt64
	if err := s.read.QueryRowContext(ctx,
		"SELECT primary_artist_id FROM release_group WHERE id = ?", rg.Int64).Scan(&artist); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return rg.Int64, 0, waxerr.Wrap(waxerr.CodeIO, "store.ResolveArt", err)
	}
	if artist.Valid {
		return rg.Int64, artist.Int64, nil
	}
	return rg.Int64, 0, nil
}

// firstItemGenre returns the item's lowest-id genre, or 0 when it has none.
func (s *Store) firstItemGenre(ctx context.Context, itemID int64) int64 {
	var gid int64
	err := s.read.QueryRowContext(ctx,
		"SELECT genre_id FROM item_genre WHERE item_id = ? ORDER BY genre_id LIMIT 1", itemID).Scan(&gid)
	if err != nil {
		return 0
	}
	return gid
}

// idByPID resolves an entity pid to its rowid in the named table.
func (s *Store) idByPID(ctx context.Context, table string, pid model.PID, op string) (int64, error) {
	var id int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such "+table+": "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}

// deleteEntityArtTx drops every art_map row an entity holds, in all roles. A delete
// path must call this rather than leave the rows for GCArt: the entity tables use a
// plain INTEGER PRIMARY KEY, and SQLite hands a deleted row's id to the next insert,
// so a surviving map row would show a dead entity's cover on whatever live entity
// inherits its id, and GC would keep the row because the id exists again. It is the
// same reasoning deleteItemCascade applies to entity_enrichment markers. The source
// image left behind is still GC's to reclaim.
func deleteEntityArtTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = ? AND entity_id = ?", entityType, entityID)
	return err
}

// GCArt reclaims orphaned art: map rows whose entity is gone, then source images
// no longer referenced by any map, cascading to their cached thumbnails. It
// returns the number of source images and thumbnails removed. It is the repair for
// the orphan counts VerifyDerived reports.
func (s *Store) GCArt(ctx context.Context) (sources, thumbnails int, err error) {
	const op = "store.GCArt"
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Drop polymorphic map rows whose backing entity no longer exists. This
		// slot->table list and verify.go's liveArtSourceQ arms are the same set and
		// must stay in lockstep: a slot missing on either side leaves a persistent
		// verify complaint that --fix cannot clear (verify counts a source orphaned
		// while GC keeps it, or GC never removes the dead rows keeping it alive).
		// Roles never enter into it; both sides are deliberately role-agnostic.
		for _, m := range []struct{ typ, table string }{
			{"track", "playable_item"}, {"album", "album"},
			{"release_group", "release_group"}, {"artist", "artist"}, {"genre", "genre"},
			{"episode", "playable_item"}, {"podcast", "podcast"},
			{"playlist", "playlist"},
		} {
			if _, e := tx.ExecContext(ctx,
				"DELETE FROM art_map WHERE entity_type = ? AND entity_id NOT IN (SELECT id FROM "+m.table+")",
				m.typ); e != nil {
				return e
			}
		}
		// Count thumbnails about to be cascaded, then drop unreferenced sources.
		if e := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM thumb_cache WHERE source_hash NOT IN (SELECT source_hash FROM art_map)`).
			Scan(&thumbnails); e != nil {
			return e
		}
		r, e := tx.ExecContext(ctx,
			"DELETE FROM art_source WHERE hash NOT IN (SELECT source_hash FROM art_map)")
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		sources = int(n)
		return nil
	})
	if err != nil {
		return 0, 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return sources, thumbnails, nil
}
