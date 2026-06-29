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
	artistID, err := resolveArtist(ctx, tx, tr.Artist, tr.MBArtistID)
	if err != nil {
		return err
	}
	// The album-artist anchors the release group; fall back to the track artist
	// when a track carries no explicit album-artist (the common single-artist case).
	albumArtistName, albumArtistMBID := tr.AlbumArtist, tr.MBAlbumArtistID
	if strings.TrimSpace(albumArtistName) == "" {
		albumArtistName, albumArtistMBID = tr.Artist, tr.MBArtistID
	}
	albumArtistID, err := resolveArtist(ctx, tx, albumArtistName, albumArtistMBID)
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

	if err := syncItemGenres(ctx, tx, itemID, tr.Genres, tr.Genre); err != nil {
		return err
	}
	return syncSearchFTS(ctx, tx, itemID, tr)
}

// resolveAlbumChain resolves the release_group and album for a track, returning
// the album id (0 when the track has no album title, or no artist to anchor the
// group on). MusicBrainz ids, when present, key the group/release directly
// (MBID-first), so two heuristic guesses for one release unify on the same id.
func resolveAlbumChain(ctx context.Context, tx *sql.Tx, tr model.Track, albumArtistName string, albumArtistID int64, filePath []byte) (int64, error) {
	artistMatchKey := identity.MatchKey(albumArtistName)
	if artistMatchKey == "" && tr.MBReleaseGroupID == "" {
		// No artist and no MBID (fully untagged): do not group. A title-only
		// release group would collide unrelated untagged albums (e.g. two
		// different "Greatest Hits"), so the track stays ungrouped until an
		// artist or MBID is known.
		return 0, nil
	}
	rgKey := identity.ReleaseGroupKey(tr.MBReleaseGroupID, artistMatchKey, tr.Album)
	if rgKey == "" {
		return 0, nil // non-album single: not grouped
	}
	rgID, err := resolveReleaseGroup(ctx, tx, rgKey, tr.Album, albumArtistID, tr.MBReleaseGroupID)
	if err != nil {
		return 0, err
	}
	folder := filepath.Dir(string(filePath))
	// Key the album by release group, year, and folder, not disc total. Multi-disc
	// albums are often tagged inconsistently, and including disc_total would split
	// one edition into separate album rows. The folder already disambiguates
	// editions; disc_total is still recorded for display. A MusicBrainz release id
	// keys the album directly when present.
	albumKey := identity.AlbumKey(tr.MBReleaseID, rgKey, tr.Year, 0, folder)
	return resolveAlbum(ctx, tx, albumKey, rgID, tr)
}

// resolveArtist finds-or-creates an artist by normalized match key, returning
// its id (0 when name is blank). Artists dedup by name (MBID-first unification of
// same-named artists is enrichment's job); a known MBID is recorded on the new
// row so enrichment and Subsonic artist info have it. A new artist is logged.
func resolveArtist(ctx context.Context, tx *sql.Tx, name, mbid string) (int64, error) {
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
		"INSERT INTO artist(pid, name, sort_key, match_key, mbid) VALUES (?,?,?,?,?)",
		string(pid), name, model.SortKey(name), mk, nullStr(strings.TrimSpace(mbid)))
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
func resolveReleaseGroup(ctx context.Context, tx *sql.Tx, key, title string, primaryArtistID int64, mbid string) (int64, error) {
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
		"INSERT INTO release_group(pid, title, sort_key, primary_artist_id, type, match_key, mbid) VALUES (?,?,?,?,?,?,?)",
		string(pid), title, model.SortKey(title), nullInt64(primaryArtistID), "album", key, nullStr(strings.TrimSpace(mbid)))
	if err != nil {
		return 0, err
	}
	id, err = r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, appendChange(ctx, tx, "release_group", pid, model.OpCreate)
}

// resolveAlbum finds-or-creates a specific release/edition by its identity key,
// recording its disc total and MusicBrainz release id when known.
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
		"INSERT INTO album(pid, release_group_id, title, sort_key, year, disc_total, mbid, match_key) VALUES (?,?,?,?,?,?,?,?)",
		string(pid), nullInt64(releaseGroupID), tr.Album, model.SortKey(tr.Album), nullInt(tr.Year),
		nullInt(tr.DiscTotal), nullStr(strings.TrimSpace(tr.MBReleaseID)), key)
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

// syncItemGenres replaces an item's genre set. The adapter usually supplies
// already-split genres; rawGenre is re-split as a fallback (e.g. a single tag
// holding "Rock; Pop"). Replace semantics so a retag that drops a genre updates
// the links. Duplicates are removed by match key, preserving first-seen casing.
func syncItemGenres(ctx context.Context, tx *sql.Tx, itemID int64, genres []string, rawGenre string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_genre WHERE item_id = ?", itemID); err != nil {
		return err
	}
	names := dedupGenres(genres)
	if len(names) == 0 {
		names = identity.SplitGenres(rawGenre)
	}
	for _, name := range names {
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

// dedupGenres splits any residual separators in already-listed genres and
// removes duplicates by match key, preserving first-seen display casing. A
// provider that returns one "Rock/Pop" element is still normalized to two.
func dedupGenres(genres []string) []string {
	var out []string
	seen := make(map[string]bool, len(genres))
	for _, g := range genres {
		for _, name := range identity.SplitGenres(g) {
			mk := identity.MatchKey(name)
			if mk == "" || seen[mk] {
				continue
			}
			seen[mk] = true
			out = append(out, name)
		}
	}
	return out
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
