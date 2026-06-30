package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// attachArtTx dedups an item's cover image into the content-addressed art store
// and maps it to the track. Album art is derived on read from current track maps,
// so a re-cover, retag, or delete cannot leave a stale album mapping behind. The
// write is idempotent: when the track already maps this exact cover it does
// nothing, so it can run on every scan (catching a directory cover added after the
// audio was first indexed) without churn. A nil/empty image is a no-op; a missing
// read does not mean the art was removed.
func attachArtTx(ctx context.Context, tx *sql.Tx, itemID int64, img *model.ArtImage) error {
	if img == nil || len(img.Data) == 0 || img.Hash == "" {
		return nil
	}
	// Idempotence: an unchanged cover writes nothing, so this is safe to run on every
	// scan (catching a directory cover added after the audio was first indexed)
	// without churning a no-op rescan.
	var curHash sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT source_hash FROM art_map WHERE entity_type = 'track' AND entity_id = ? AND role = 'front' LIMIT 1",
		itemID).Scan(&curHash); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if curHash.Valid && curHash.String == img.Hash {
		return nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO art_source(hash, format, width, height, size, data, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		img.Hash, img.Format, img.Width, img.Height, len(img.Data), img.Data, nowNS()); err != nil {
		return err
	}
	// Re-point this track's cover; a track has exactly one front cover. Album art is
	// derived on read from the album's current track covers (see artInChain), so a
	// re-cover, retag, or delete cannot leave a stale album map. When the old cover
	// loses its last referencing track it becomes an orphaned source for GCArt.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = 'track' AND entity_id = ?", itemID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO art_map(entity_type, entity_id, source_hash, role, priority)
		 VALUES ('track', ?, ?, 'front', 0)`, itemID, img.Hash)
	return err
}

// artLevel is one rung of the resolution fallback chain: an entity type and its
// internal id.
type artLevel struct {
	typ string
	id  int64
}

// ResolveArt resolves art for an entity, walking the fallback chain from the
// requested level up toward the root (track -> album -> release_group -> artist ->
// genre) and returning the first level that has art. size selects the output: a
// non-positive size returns the original source; a positive size returns a
// thumbnail scaled to fit a square box with that maximum side. Generated
// thumbnails are cached and never upscaled. A source that cannot be decoded for
// thumbnailing is returned as-is. CodeNotFound means no level in the chain has art.
func (s *Store) ResolveArt(ctx context.Context, ref model.EntityRef, size int) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	if !ref.Type.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
	}
	chain, err := s.artChain(ctx, ref)
	if err != nil {
		return nil, err
	}
	hash, ok, err := s.artInChain(ctx, chain)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no art for "+string(ref.Type)+":"+string(ref.PID))
	}

	srcData, srcFormat, srcW, srcH, err := s.artSource(ctx, hash)
	if err != nil {
		return nil, err
	}
	blob := &model.ArtBlob{Bytes: srcData, Format: srcFormat, Width: srcW, Height: srcH, SourceHash: hash}

	// Original requested, or a source already within the box (or undecodable, hence
	// zero-sized): serve the source.
	longest := srcW
	if srcH > srcW {
		longest = srcH
	}
	if size <= 0 || longest == 0 || longest <= size {
		return blob, nil
	}
	return s.thumbnail(ctx, hash, srcData, srcFormat, srcW, srcH, size)
}

// thumbnail returns a cached thumbnail for (source hash, size), generating and
// caching it on a miss. A generation failure (e.g. an exotic source format) falls
// back to the original source, served from the bytes/metadata the caller already
// loaded (no re-fetch).
func (s *Store) thumbnail(ctx context.Context, hash string, srcData []byte, srcFormat string, srcW, srcH, size int) (*model.ArtBlob, error) {
	const op = "store.ResolveArt"
	var data []byte
	var format string
	var w, h int
	err := s.read.QueryRowContext(ctx,
		"SELECT data, format, width, height FROM thumb_cache WHERE source_hash = ? AND size = ?",
		hash, size).Scan(&data, &format, &w, &h)
	if err == nil {
		return &model.ArtBlob{Bytes: data, Format: format, Width: w, Height: h, SourceHash: hash, Thumbnail: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	thumb, tFormat, tw, th, gerr := art.Thumbnail(srcData, size)
	if gerr != nil {
		// Undecodable source: serve the original unscaled rather than failing, reusing
		// the bytes and metadata already in hand.
		s.log.Warn("art thumbnail generation failed; serving original", "hash", hash, "size", size, "err", gerr)
		return &model.ArtBlob{Bytes: srcData, Format: srcFormat, Width: srcW, Height: srcH, SourceHash: hash}, nil
	}

	// Best-effort cache write: a read-only library still serves the generated
	// thumbnail, just without persisting it.
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
	return &model.ArtBlob{Bytes: thumb, Format: tFormat, Width: tw, Height: th, SourceHash: hash, Thumbnail: true}, nil
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

// artInChain returns the source hash of the first chain level that has art, or
// ok=false when none does. A level's direct entity art is looked up in art_map
// (track covers, and release_group/artist/genre/album art populated by enrichment).
// Album art with no direct entry is derived from the album's track covers, so it
// is always one of the covers currently carried by a track in that album, never a
// stale denormalized mapping left by a re-cover, cross-album retag, or deletion.
func (s *Store) artInChain(ctx context.Context, chain []artLevel) (string, bool, error) {
	const op = "store.ResolveArt"
	for _, lv := range chain {
		if lv.id == 0 {
			continue
		}
		var hash string
		err := s.read.QueryRowContext(ctx,
			`SELECT source_hash FROM art_map WHERE entity_type = ? AND entity_id = ?
			 ORDER BY priority, rowid LIMIT 1`, lv.typ, lv.id).Scan(&hash)
		if err == nil {
			return hash, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if lv.typ == string(model.ArtAlbum) {
			derr := s.read.QueryRowContext(ctx,
				`SELECT tm.source_hash FROM art_map tm JOIN track t ON t.item_id = tm.entity_id
				 WHERE tm.entity_type = 'track' AND tm.role = 'front' AND t.album_id = ?
				 ORDER BY tm.priority, tm.rowid LIMIT 1`, lv.id).Scan(&hash)
			if derr == nil {
				return hash, true, nil
			}
			if !errors.Is(derr, sql.ErrNoRows) {
				return "", false, waxerr.Wrap(waxerr.CodeIO, op, derr)
			}
		}
	}
	return "", false, nil
}

// artChain builds the resolution fallback chain for a reference: the requested
// level first, then its ancestors up to the genre root, each as (type, internal
// id). Missing ancestors are dropped, so the chain only contains resolvable levels.
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
	}
	return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown art entity type: "+string(ref.Type))
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

// GCArt reclaims orphaned art: map rows whose entity is gone, then source images
// no longer referenced by any map, cascading to their cached thumbnails. It
// returns the number of source images and thumbnails removed. It is the repair for
// the orphan counts VerifyDerived reports.
func (s *Store) GCArt(ctx context.Context) (sources, thumbnails int, err error) {
	const op = "store.GCArt"
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		// Drop polymorphic map rows whose backing entity no longer exists.
		for _, m := range []struct{ typ, table string }{
			{"track", "playable_item"}, {"album", "album"},
			{"release_group", "release_group"}, {"artist", "artist"}, {"genre", "genre"},
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
