package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file implements the enrich.Store port: the iteration queries that feed the
// enrichment pass, the transactional apply methods that persist provider results
// (MBID-first, lock-respecting, provenance-recording), the response cache, and the
// coverage report. Enrichment adds entity data and fills gaps; it never overwrites
// a tagged or locked field.

// enrichArtistBacksItems restricts artist enrichment to artists that actually back
// a track (as artist or album artist) or credit a book, so ghost artists left by a
// retag are not looked up.
const enrichArtistBacksItems = `(EXISTS (SELECT 1 FROM track t WHERE t.artist_id = a.id OR t.album_artist_id = a.id)
	OR EXISTS (SELECT 1 FROM item_contributor ic WHERE ic.artist_id = a.id))`

// enrichRGBacksItems restricts release-group enrichment to groups that back at
// least one track.
const enrichRGBacksItems = `EXISTS (SELECT 1 FROM album al JOIN track t ON t.album_id = al.id WHERE al.release_group_id = rg.id)`

// notEnriched returns the SQL predicate excluding already-enriched entities, or
// "1=1" for a forced run that re-enriches everything. idExpr is the entity's id
// column (book keys on item_id, not id).
func notEnriched(entityType, idExpr string, force bool) string {
	if force {
		return "1=1"
	}
	return "NOT EXISTS (SELECT 1 FROM entity_enrichment ee WHERE ee.entity_type = '" +
		entityType + "' AND ee.entity_id = " + idExpr + ")"
}

// ArtistsNeedingEnrichment returns the next keyset page of artists to enrich.
func (s *Store) ArtistsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int) ([]model.EnrichTarget, error) {
	const op = "store.ArtistsNeedingEnrichment"
	stmt := `SELECT a.id, a.pid, a.name, COALESCE(a.mbid,'')
		FROM artist a
		WHERE a.id > ? AND ` + enrichArtistBacksItems + ` AND ` + notEnriched(model.EnrichArtistType, "a.id", force) + `
		ORDER BY a.id LIMIT ?`
	rows, err := s.read.QueryContext(ctx, stmt, afterID, limitOr(limit))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichArtistType}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReleaseGroupsNeedingEnrichment returns the next keyset page of release groups to
// enrich, each with its primary-artist name. When includeRepFile is set it also
// resolves a representative member file (path + duration) for the AcoustID fallback;
// otherwise that correlated lookup is skipped entirely (the common path).
func (s *Store) ReleaseGroupsNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int, includeRepFile bool) ([]model.EnrichTarget, error) {
	const op = "store.ReleaseGroupsNeedingEnrichment"
	// The representative file's path and duration must come from ONE row, so a single
	// correlated subquery picks the file id (deterministically, lowest first) and the
	// join reads both columns from that same file. That keeps a path from ever pairing
	// with a duration read off a different file.
	repJoin, repCols := "", "X'', 0"
	if includeRepFile {
		repJoin = ` LEFT JOIN file rf ON rf.id = (
			SELECT pf.file_id FROM item_file pf
			JOIN track t ON t.item_id = pf.item_id
			JOIN album al ON al.id = t.album_id
			WHERE al.release_group_id = rg.id AND pf.role = 'primary'
			ORDER BY pf.file_id LIMIT 1)`
		repCols = "COALESCE(rf.path, X''), COALESCE(rf.duration_ms, 0)"
	}
	stmt := `SELECT rg.id, rg.pid, rg.title, COALESCE(rg.mbid,''), COALESCE(ar.name,''), ` + repCols + `
		FROM release_group rg
		LEFT JOIN artist ar ON ar.id = rg.primary_artist_id` + repJoin + `
		WHERE rg.id > ? AND ` + enrichRGBacksItems + ` AND ` + notEnriched(model.EnrichReleaseGroupType, "rg.id", force) + `
		ORDER BY rg.id LIMIT ?`
	rows, err := s.read.QueryContext(ctx, stmt, afterID, limitOr(limit))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichReleaseGroupType}
		var pid string
		var path []byte
		var durMS int64
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &t.ArtistName, &path, &durMS); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		t.FilePath = string(path)
		t.DurationSec = int(durMS / 1000)
		out = append(out, t)
	}
	return out, rows.Err()
}

// BooksNeedingEnrichment returns audiobooks to enrich. It requires a non-empty
// mbid: MusicBrainz text search for audiobooks is unreliable, so a book is only
// enriched when it carries an explicit release id. A catalog with no book mbids
// therefore yields nothing and costs no lookups.
func (s *Store) BooksNeedingEnrichment(ctx context.Context, force bool, afterID int64, limit int) ([]model.EnrichTarget, error) {
	const op = "store.BooksNeedingEnrichment"
	stmt := `SELECT b.item_id, pi.pid, pi.title, COALESCE(b.mbid,''), COALESCE(b.author,'')
		FROM book b JOIN playable_item pi ON pi.id = b.item_id
		WHERE b.item_id > ? AND b.mbid IS NOT NULL AND b.mbid <> '' AND ` + notEnriched(model.EnrichBookType, "b.item_id", force) + `
		ORDER BY b.item_id LIMIT ?`
	rows, err := s.read.QueryContext(ctx, stmt, afterID, limitOr(limit))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EnrichTarget
	for rows.Next() {
		t := model.EnrichTarget{Type: model.EnrichBookType}
		var pid string
		if err := rows.Scan(&t.ID, &pid, &t.Name, &t.MBID, &t.ArtistName); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		t.PID = model.PID(pid)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountEntitiesNeedingEnrichment totals the artists, release groups, and books the
// pass would process, so the heartbeat can report a real ratio.
func (s *Store) CountEntitiesNeedingEnrichment(ctx context.Context, force bool) (int, error) {
	const op = "store.CountEntitiesNeedingEnrichment"
	var total int
	for _, q := range []string{
		`SELECT COUNT(*) FROM artist a WHERE ` + enrichArtistBacksItems + ` AND ` + notEnriched(model.EnrichArtistType, "a.id", force),
		`SELECT COUNT(*) FROM release_group rg WHERE ` + enrichRGBacksItems + ` AND ` + notEnriched(model.EnrichReleaseGroupType, "rg.id", force),
		`SELECT COUNT(*) FROM book b WHERE b.mbid IS NOT NULL AND b.mbid <> '' AND ` + notEnriched(model.EnrichBookType, "b.item_id", force),
	} {
		var n int
		if err := s.read.QueryRowContext(ctx, q).Scan(&n); err != nil {
			return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		total += n
	}
	return total, nil
}

// ApplyArtistEnrichment persists one artist's resolved data: MBID (only when the
// artist has none, so a tagged id is never overwritten), aliases, and directed
// relations to other catalog artists. A no-match still writes the marker so the
// artist is not retried every run.
func (s *Store) ApplyArtistEnrichment(ctx context.Context, in model.ArtistEnrichment) error {
	const op = "store.ApplyArtistEnrichment"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if !in.Matched {
			return markEnrichedTx(ctx, tx, model.EnrichArtistType, in.ArtistID, false, "")
		}
		if in.MBID != "" {
			if _, err := tx.ExecContext(ctx,
				"UPDATE artist SET mbid = ? WHERE id = ? AND (mbid IS NULL OR mbid = '')", in.MBID, in.ArtistID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := insertAliasesTx(ctx, tx, in.ArtistID, in.SortName, in.Aliases); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := insertRelationsTx(ctx, tx, in.ArtistID, in.Relations); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := markEnrichedTx(ctx, tx, model.EnrichArtistType, in.ArtistID, true, in.MBID); err != nil {
			return err
		}
		return appendChange(ctx, tx, "artist", in.PID, model.OpUpdate)
	})
}

// insertAliasesTx adds an artist's alternate names, including the MusicBrainz
// sort-name, ignoring duplicates (UNIQUE(artist_id, name)).
func insertAliasesTx(ctx context.Context, tx *sql.Tx, artistID int64, sortName string, aliases []string) error {
	names := aliases
	if strings.TrimSpace(sortName) != "" {
		names = append([]string{sortName}, names...)
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO artist_alias(artist_id, name, sort_key, is_primary) VALUES (?,?,?,0)",
			artistID, name, model.SortKey(name)); err != nil {
			return err
		}
	}
	return nil
}

// insertRelationsTx links an artist to other catalog artists identified by MBID.
// Targets not present in the catalog are skipped (no stub artists are created), so
// relations only ever connect entities the user actually has.
func insertRelationsTx(ctx context.Context, tx *sql.Tx, srcID int64, rels []model.ArtistRelationInput) error {
	for _, r := range rels {
		if r.TargetMBID == "" {
			continue
		}
		var dstID int64
		err := tx.QueryRowContext(ctx, "SELECT id FROM artist WHERE mbid = ?", r.TargetMBID).Scan(&dstID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if dstID == srcID {
			continue
		}
		// Orient the edge: normally enriched(src) -> target(dst); an inbound relation
		// (MusicBrainz reported it from the far end) reverses it so the stored
		// direction is consistent regardless of which artist was enriched.
		src, dst := srcID, dstID
		if r.Inbound {
			src, dst = dstID, srcID
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO artist_relation(src_id, dst_id, kind) VALUES (?,?,?)",
			src, dst, r.Kind); err != nil {
			return err
		}
	}
	return nil
}

// ApplyReleaseGroupEnrichment persists one release group's resolved data: MBID
// (unless it collides with another group's, deferred to the merge gate), type,
// genres added to member items that have none (respecting genre locks, recording
// enrichment provenance), and the Cover Art Archive front cover. Touched genre
// rollups are maintained so db verify stays clean.
func (s *Store) ApplyReleaseGroupEnrichment(ctx context.Context, in model.ReleaseGroupEnrichment) error {
	const op = "store.ApplyReleaseGroupEnrichment"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if !in.Matched {
			return markEnrichedTx(ctx, tx, model.EnrichReleaseGroupType, in.ReleaseGroupID, false, "")
		}
		if err := setReleaseGroupMBIDTx(ctx, tx, s.log, in.ReleaseGroupID, in.MBID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if in.Type != "" {
			if _, err := tx.ExecContext(ctx, "UPDATE release_group SET type = ? WHERE id = ?", in.Type, in.ReleaseGroupID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		aff := newAffectedRollups()
		if err := populateReleaseGroupGenresTx(ctx, tx, in.ReleaseGroupID, in.Genres, aff); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if in.Art != nil {
			if err := attachEntityArtTx(ctx, tx, string(model.ArtReleaseGroup), in.ReleaseGroupID, in.Art); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if !aff.empty() {
			if err := maintainRollupsTx(ctx, tx, aff, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := markEnrichedTx(ctx, tx, model.EnrichReleaseGroupType, in.ReleaseGroupID, true, in.MBID); err != nil {
			return err
		}
		return appendChange(ctx, tx, "release_group", in.PID, model.OpUpdate)
	})
}

// setReleaseGroupMBIDTx sets a release group's MBID only when it has none and the
// id is not already held by another group. A collision means two heuristic groups
// resolved to one MBID; unifying them is the merge primitive's job (a later gate),
// so here it is logged and left, never forced into a duplicate key.
func setReleaseGroupMBIDTx(ctx context.Context, tx *sql.Tx, log logger, rgID int64, mbid string) error {
	if mbid == "" {
		return nil
	}
	var other int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM release_group WHERE mbid = ? AND id <> ?", mbid, rgID).Scan(&other)
	if err == nil {
		log.Warn("enrichment: release-group MBID already used by another group; leaving unmerged", "mbid", mbid, "rg", rgID, "other", other)
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE release_group SET mbid = ? WHERE id = ? AND (mbid IS NULL OR mbid = '')", mbid, rgID)
	return err
}

// populateReleaseGroupGenresTx attaches the release group's genres to member items
// that carry no genre and whose genre field is not locked, recording enrichment
// provenance and collecting the touched genres for rollup maintenance. It never
// overwrites a tagged or user genre.
func populateReleaseGroupGenresTx(ctx context.Context, tx *sql.Tx, rgID int64, genres []string, aff *affectedRollups) error {
	if len(genres) == 0 {
		return nil
	}
	gids := make([]int64, 0, len(genres))
	names := make([]string, 0, len(genres))
	for _, name := range genres {
		gid, err := resolveGenre(ctx, tx, model.FacetGenre, name)
		if err != nil {
			return err
		}
		if gid != 0 {
			gids = append(gids, gid)
			names = append(names, name)
			aff.genres[gid] = true
		}
	}
	if len(gids) == 0 {
		return nil
	}
	// The denormalized track.genre feeds the item display, the `--genre` query
	// filter, and (on the next scan) the FTS row; set it too so an enrichment genre
	// is visible everywhere the facet/browse item_genre links already surface it,
	// not only in genre browse.
	genreDisplay := strings.Join(names, "; ")
	// Member items with no genre and no genre lock.
	rows, err := tx.QueryContext(ctx, `SELECT pi.id, pi.pid
		FROM track t JOIN album al ON al.id = t.album_id JOIN playable_item pi ON pi.id = t.item_id
		WHERE al.release_group_id = ?
		  AND NOT EXISTS (SELECT 1 FROM item_genre ig WHERE ig.item_id = t.item_id)
		  AND NOT EXISTS (SELECT 1 FROM field_provenance fp WHERE fp.item_id = t.item_id AND fp.field = 'genre' AND fp.locked = 1)`, rgID)
	if err != nil {
		return err
	}
	type memberItem struct {
		id  int64
		pid model.PID
	}
	var items []memberItem
	for rows.Next() {
		var id int64
		var pid string
		if err := rows.Scan(&id, &pid); err != nil {
			rows.Close()
			return err
		}
		items = append(items, memberItem{id, model.PID(pid)})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	now := nowNS()
	for _, it := range items {
		for _, gid := range gids {
			if _, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO item_genre(item_id, genre_id) VALUES (?,?)", it.id, gid); err != nil {
				return err
			}
		}
		// Fill the denormalized display column only when empty (never overwriting a
		// tag; the member query already excluded items that carry a genre).
		if _, err := tx.ExecContext(ctx,
			"UPDATE track SET genre = ? WHERE item_id = ? AND (genre IS NULL OR genre = '')", genreDisplay, it.id); err != nil {
			return err
		}
		// Record that genres came from enrichment so future organize/enrichment
		// respects them. The value stays empty (genres are multi-valued via
		// item_genre); the row exists to carry the source and enable a lock.
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?, 'genre', 'enrichment', 0, ?)
			ON CONFLICT(item_id, field) DO UPDATE SET source = 'enrichment', updated_at = excluded.updated_at`,
			it.id, now); err != nil {
			return err
		}
		if err := appendChange(ctx, tx, "item", it.pid, model.OpUpdate); err != nil {
			return err
		}
	}
	return nil
}

// ApplyBookEnrichment fills an audiobook's external identifiers and publisher from
// a MusicBrainz release, only where the field is currently empty so a tagged value
// is never overwritten.
func (s *Store) ApplyBookEnrichment(ctx context.Context, in model.BookEnrichment) error {
	const op = "store.ApplyBookEnrichment"
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		if !in.Matched {
			return markEnrichedTx(ctx, tx, model.EnrichBookType, in.BookItemID, false, "")
		}
		// Fill-when-empty for each field.
		for _, f := range []struct {
			col, val string
		}{
			{"asin", in.ASIN}, {"isbn", in.ISBN}, {"publisher", in.Publisher},
		} {
			if strings.TrimSpace(f.val) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"UPDATE book SET "+f.col+" = ? WHERE item_id = ? AND ("+f.col+" = '' OR "+f.col+" IS NULL)",
				f.val, in.BookItemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := markEnrichedTx(ctx, tx, model.EnrichBookType, in.BookItemID, true, in.MBID); err != nil {
			return err
		}
		return appendChange(ctx, tx, "item", in.PID, model.OpUpdate)
	})
}

// markEnrichedTx upserts the sparse enrichment marker for an entity.
func markEnrichedTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, matched bool, mbid string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(entity_type, entity_id) DO UPDATE SET
		  provider = excluded.provider, matched = excluded.matched, mbid = excluded.mbid, enriched_at = excluded.enriched_at`,
		entityType, entityID, "musicbrainz", boolInt(matched), nullStr(strings.TrimSpace(mbid)), nowNS())
	return err
}

// EnrichmentCacheGet returns a cached provider payload by key.
func (s *Store) EnrichmentCacheGet(ctx context.Context, key string) ([]byte, bool, error) {
	var payload []byte
	err := s.read.QueryRowContext(ctx, "SELECT payload FROM enrichment_cache WHERE cache_key = ?", key).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, waxerr.Wrap(waxerr.CodeIO, "store.EnrichmentCacheGet", err)
	}
	return payload, true, nil
}

// EnrichmentCachePut stores a provider payload under key, replacing any prior value.
func (s *Store) EnrichmentCachePut(ctx context.Context, key string, payload []byte) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO enrichment_cache(cache_key, payload, fetched_at) VALUES (?,?,?)
			 ON CONFLICT(cache_key) DO UPDATE SET payload = excluded.payload, fetched_at = excluded.fetched_at`,
			key, payload, nowNS())
		return err
	})
}

// EnrichmentCoverage reports how many entities of each type have been enriched.
func (s *Store) EnrichmentCoverage(ctx context.Context) (model.EnrichmentCoverage, error) {
	const op = "store.EnrichmentCoverage"
	var cov model.EnrichmentCoverage
	rows, err := s.read.QueryContext(ctx,
		"SELECT entity_type, COUNT(*), COALESCE(SUM(matched),0) FROM entity_enrichment GROUP BY entity_type")
	if err != nil {
		return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ string
		var count, matched int
		if err := rows.Scan(&typ, &count, &matched); err != nil {
			return cov, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		cov.Matched += matched
		switch typ {
		case model.EnrichArtistType:
			cov.Artists = count
		case model.EnrichReleaseGroupType:
			cov.ReleaseGroups = count
		case model.EnrichBookType:
			cov.Books = count
		}
	}
	return cov, rows.Err()
}

// limitOr defaults a non-positive limit to a sane batch cap.
func limitOr(limit int) int {
	if limit <= 0 {
		return 100
	}
	return limit
}

// logger is the minimal logging surface the tx helpers need (satisfied by
// *slog.Logger).
type logger interface {
	Warn(msg string, args ...any)
}
