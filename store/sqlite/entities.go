package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
)

// resolveAndLinkEntities resolves the normalized entities a scanned track
// implies (artist, album-artist, release_group, album, genres), links them onto
// the track/item, and refreshes the item's FTS row. New entities are emitted to
// the change_log so delta consumers can update browse/facet caches. It runs
// inside the PutScannedTrack write transaction.
func resolveAndLinkEntities(ctx context.Context, tx *sql.Tx, itemID int64, tr model.Track, filePath []byte) error {
	artistID, err := resolveArtist(ctx, tx, tr.Artist)
	if err != nil {
		return err
	}
	// The album-artist anchors the release group; fall back to the track artist
	// when a track carries no explicit album-artist (the common single-artist case).
	albumArtistName := tr.AlbumArtist
	if strings.TrimSpace(albumArtistName) == "" {
		albumArtistName = tr.Artist
	}
	albumArtistID, err := resolveArtist(ctx, tx, albumArtistName)
	if err != nil {
		return err
	}

	albumID, err := resolveAlbumChain(ctx, tx, tr, albumArtistName, albumArtistID, filePath)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE track SET artist_id=?, album_artist_id=?, album_id=? WHERE item_id=?",
		nullInt64(artistID), nullInt64(albumArtistID), nullInt64(albumID), itemID); err != nil {
		return err
	}

	if err := syncItemGenres(ctx, tx, itemID, tr.Genre); err != nil {
		return err
	}
	return syncSearchFTS(ctx, tx, itemID, tr)
}

// resolveAlbumChain resolves the release_group and album for a track, returning
// the album id (0 when the track has no album title, or no artist to anchor the
// group on).
func resolveAlbumChain(ctx context.Context, tx *sql.Tx, tr model.Track, albumArtistName string, albumArtistID int64, filePath []byte) (int64, error) {
	artistMatchKey := identity.MatchKey(albumArtistName)
	if artistMatchKey == "" {
		// No artist at all (fully untagged): do not group. A title-only release
		// group would collide unrelated untagged albums (e.g. two different
		// "Greatest Hits"), so the track stays ungrouped until an artist is known.
		return 0, nil
	}
	rgKey := identity.ReleaseGroupKey("", artistMatchKey, tr.Album)
	if rgKey == "" {
		return 0, nil // non-album single: not grouped
	}
	rgID, err := resolveReleaseGroup(ctx, tx, rgKey, tr.Album, albumArtistID)
	if err != nil {
		return 0, err
	}
	folder := filepath.Dir(string(filePath))
	albumKey := identity.AlbumKey("", rgKey, tr.Year, 0, folder)
	return resolveAlbum(ctx, tx, albumKey, rgID, tr)
}

// resolveArtist finds-or-creates an artist by normalized match key, returning
// its id (0 when name is blank). A newly created artist is logged to change_log.
func resolveArtist(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, nil
	}
	mk := identity.MatchKey(name)
	if mk == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM artist WHERE match_key = ?", mk).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO artist(pid, name, sort_key, match_key) VALUES (?,?,?,?)",
		string(pid), name, model.SortKey(name), mk)
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "artist", pid, model.OpCreate)
}

// resolveReleaseGroup finds-or-creates a release group by its identity key.
func resolveReleaseGroup(ctx context.Context, tx *sql.Tx, key, title string, primaryArtistID int64) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM release_group WHERE match_key = ?", key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO release_group(pid, title, sort_key, primary_artist_id, type, match_key) VALUES (?,?,?,?,?,?)",
		string(pid), title, model.SortKey(title), nullInt64(primaryArtistID), "album", key)
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "release_group", pid, model.OpCreate)
}

// resolveAlbum finds-or-creates a specific release/edition by its identity key.
func resolveAlbum(ctx context.Context, tx *sql.Tx, key string, releaseGroupID int64, tr model.Track) (int64, error) {
	if key == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM album WHERE match_key = ?", key).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO album(pid, release_group_id, title, sort_key, year, match_key) VALUES (?,?,?,?,?,?)",
		string(pid), nullInt64(releaseGroupID), tr.Album, model.SortKey(tr.Album), nullInt(tr.Year), key)
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "album", pid, model.OpCreate)
}

// resolveGenre finds-or-creates a genre/mood entity by (facet, match key).
func resolveGenre(ctx context.Context, tx *sql.Tx, facet model.GenreFacet, name string) (int64, error) {
	mk := identity.MatchKey(name)
	if mk == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM genre WHERE facet = ? AND match_key = ?", string(facet), mk).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	pid := model.NewPID()
	r, err := tx.ExecContext(ctx,
		"INSERT INTO genre(pid, facet, name, match_key, sort_key) VALUES (?,?,?,?,?)",
		string(pid), string(facet), name, mk, model.SortKey(name))
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "genre", pid, model.OpCreate)
}

// syncItemGenres replaces an item's genre set from a raw (possibly multi-valued)
// genre tag. Replace semantics so a retag that drops a genre updates the links.
func syncItemGenres(ctx context.Context, tx *sql.Tx, itemID int64, rawGenre string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_genre WHERE item_id = ?", itemID); err != nil {
		return err
	}
	for _, name := range identity.SplitGenres(rawGenre) {
		gid, err := resolveGenre(ctx, tx, model.FacetGenre, name)
		if err != nil {
			return err
		}
		if gid == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO item_genre(item_id, genre_id) VALUES (?, ?)", itemID, gid); err != nil {
			return err
		}
	}
	return nil
}

// syncSearchFTS rebuilds the item's metadata FTS row (rowid == item id). The
// table is writer-maintained with no triggers, so the delete-then-insert keeps
// it consistent inside the mutating transaction.
func syncSearchFTS(ctx context.Context, tx *sql.Tx, itemID int64, tr model.Track) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_fts WHERE rowid = ?", itemID); err != nil {
		return err
	}
	var title string
	if err := tx.QueryRowContext(ctx, "SELECT title FROM playable_item WHERE id = ?", itemID).Scan(&title); err != nil {
		return err
	}
	artist := strings.TrimSpace(tr.Artist + " " + tr.AlbumArtist)
	_, err := tx.ExecContext(ctx,
		"INSERT INTO search_fts(rowid, kind, title, subtitle, artist, album, extra) VALUES (?,?,?,?,?,?,?)",
		itemID, string(model.KindTrack), title, "", artist, tr.Album, tr.Genre)
	return err
}

// RefreshRollups recomputes every maintained rollup from the base tables in one
// transaction. The derived-data consistency check compares the stored rows
// against the same recompute.
func (s *Store) RefreshRollups(ctx context.Context) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		return rebuildRollups(ctx, tx, nowNS())
	})
}

func rebuildRollups(ctx context.Context, tx *sql.Tx, now int64) error {
	stmts := []string{
		"DELETE FROM artist_rollup",
		"DELETE FROM release_group_rollup",
		"DELETE FROM genre_rollup",
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, artistRollupRebuild, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, releaseGroupRollupRebuild, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, genreRollupRebuild, now); err != nil {
		return err
	}
	return nil
}

// The rebuild statements join each entity to its tracks and the tracks' primary
// files so durations sum from the real audio rows. COUNT(DISTINCT item) tolerates
// the LEFT JOINs (an entity with no tracks rolls up to zero, not one).
const artistRollupRebuild = `
INSERT INTO artist_rollup(artist_id, release_group_count, track_count, total_duration_ms, updated_at)
SELECT a.id,
       (SELECT COUNT(*) FROM release_group rg WHERE rg.primary_artist_id = a.id),
       COUNT(DISTINCT t.item_id),
       COALESCE(SUM(f.duration_ms), 0),
       ?
FROM artist a
LEFT JOIN track t      ON t.artist_id = a.id
LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
LEFT JOIN file f       ON f.id = pf.file_id
GROUP BY a.id`

const releaseGroupRollupRebuild = `
INSERT INTO release_group_rollup(release_group_id, track_count, total_duration_ms, updated_at)
SELECT rg.id,
       COUNT(DISTINCT t.item_id),
       COALESCE(SUM(f.duration_ms), 0),
       ?
FROM release_group rg
LEFT JOIN album al     ON al.release_group_id = rg.id
LEFT JOIN track t      ON t.album_id = al.id
LEFT JOIN item_file pf ON pf.item_id = t.item_id AND pf.role = 'primary'
LEFT JOIN file f       ON f.id = pf.file_id
GROUP BY rg.id`

const genreRollupRebuild = `
INSERT INTO genre_rollup(genre_id, track_count, total_duration_ms, updated_at)
SELECT g.id,
       COUNT(DISTINCT ig.item_id),
       COALESCE(SUM(f.duration_ms), 0),
       ?
FROM genre g
LEFT JOIN item_genre ig ON ig.genre_id = g.id
LEFT JOIN item_file pf  ON pf.item_id = ig.item_id AND pf.role = 'primary'
LEFT JOIN file f        ON f.id = pf.file_id
GROUP BY g.id`
