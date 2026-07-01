package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// MergeEntity collapses one loser entity onto the survivor. It is MergeEntities
// with a single loser; see that method for the full contract.
func (s *Store) MergeEntity(ctx context.Context, et model.MergeEntity, survivorPID, loserPID model.PID) (*model.MergeReport, error) {
	reports, err := s.MergeEntities(ctx, et, survivorPID, []model.PID{loserPID})
	if err != nil {
		return nil, err
	}
	return reports[0], nil
}

// MergeEntities collapses every loser entity onto the survivor in one
// transaction, so a failure on any loser rolls the whole batch back and never
// leaves a half-applied merge. For each loser it re-points every child row
// (tracks, albums, item_genre links, contributor credits, aliases, relations, art
// maps) onto the survivor, unions the MBID, release-group type, and enrichment
// marker when the survivor lacks one, recomputes the affected rollups, deletes the
// loser, and writes change_log deltas: a per-item update for every re-pointed item
// (so a delta-sync consumer sees the moved item/entity associations), a survivor
// update, and a loser delete. The survivor keeps its PID.
//
// This is the first-class merge primitive of Section 13. It backs the audit
// duplicate-entity repair and the late-enrichment unification of two heuristic
// release-groups or artists that resolve to one MBID.
//
// Merge works at the catalog level. It re-points foreign keys but does not rewrite
// file tags, the denormalized text columns (e.g. track.album_artist), or the match
// keys of dependent rows. Those dependent rows keep their existing keys, which stay
// internally consistent: a re-pointed release_group still carries the loser's
// derived match_key, and resolution keys off the unchanged track tags, so no
// duplicate is created. The merge survives a re-scan when it is MBID-anchored (the
// enrichment case), because the survivor inherits the loser's MBID below and
// identity is MBID-first. A purely heuristic merge with no MBID can be re-derived
// from the still-original tags on a later scan, so fix the tags or enable organize
// tag write-back to make it durable. Merging artists does not auto-collapse two
// same-titled release groups now sharing the survivor; run `merge release_group`
// for those.
func (s *Store) MergeEntities(ctx context.Context, et model.MergeEntity, survivorPID model.PID, loserPIDs []model.PID) ([]*model.MergeReport, error) {
	const op = "store.MergeEntities"
	if !et.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown merge entity type: "+string(et))
	}
	if len(loserPIDs) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no loser entities to merge")
	}
	for _, l := range loserPIDs {
		if l == survivorPID {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "survivor and loser are the same entity")
		}
	}
	table := string(et) // artist|release_group|album|genre; the table and art_map slot share the name
	reports := make([]*model.MergeReport, 0, len(loserPIDs))
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, loserPID := range loserPIDs {
			rep, err := mergeEntityTx(ctx, tx, et, table, survivorPID, loserPID)
			if err != nil {
				return err
			}
			reports = append(reports, rep)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reports, nil
}

// mergeEntityTx performs one merge inside the caller's transaction.
func mergeEntityTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity, table string, survivorPID, loserPID model.PID) (*model.MergeReport, error) {
	rep := &model.MergeReport{EntityType: et, Survivor: survivorPID, Loser: loserPID}
	sid, err := entityIDByPID(ctx, tx, table, survivorPID)
	if err != nil {
		return nil, err
	}
	lid, err := entityIDByPID(ctx, tx, table, loserPID)
	if err != nil {
		return nil, err
	}

	// Collect the items whose entity association is about to change, before the
	// re-point while the loser links still exist, so we can write a per-item
	// change_log delta for each. Re-linking an item emits an item update elsewhere
	// (see enrich.go); merge follows the same rule.
	affectedItems, err := affectedItemPIDs(ctx, tx, et, lid)
	if err != nil {
		return nil, err
	}

	aff := newAffectedRollups()
	var children int
	switch et {
	case model.MergeArtist:
		children, err = repointArtist(ctx, tx, sid, lid, aff)
	case model.MergeReleaseGroup:
		children, err = repointReleaseGroup(ctx, tx, sid, lid, aff)
	case model.MergeAlbum:
		children, err = repointAlbum(ctx, tx, sid, lid, aff)
	case model.MergeGenre:
		children, err = repointGenre(ctx, tx, sid, lid, aff)
	}
	if err != nil {
		return nil, err
	}
	rep.Children = children

	// Re-point the polymorphic art map (no FK, so it never cascades), keeping the
	// survivor's own art when it has any.
	if err := repointArtMap(ctx, tx, table, sid, lid); err != nil {
		return nil, err
	}
	// Union the MBID: a merge driven by enrichment (two heuristic rows sharing an
	// MBID) should leave the survivor carrying that MBID.
	if et.HasMBID() {
		if err := unionMBID(ctx, tx, table, sid, lid); err != nil {
			return nil, err
		}
	}
	// Adopt the loser's more-specific release-group type (ep/single/compilation/…)
	// when the survivor only carries the default 'album'.
	if et == model.MergeReleaseGroup {
		if err := unionReleaseGroupType(ctx, tx, sid, lid); err != nil {
			return nil, err
		}
	}
	// Union the enrichment marker so a post-enrichment merge doesn't strand the
	// survivor as never-looked-up (or force a redundant re-lookup). Only artist and
	// release_group carry markers, so skip it for album and genre.
	if et == model.MergeArtist || et == model.MergeReleaseGroup {
		if err := unionEnrichmentMarker(ctx, tx, table, sid, lid); err != nil {
			return nil, err
		}
	}

	// Delete the loser. Remaining child rows still pointing at it (aliases,
	// relations, contributor credits, its rollup) cascade away here; everything
	// we care to keep was already re-pointed above.
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id = ?", lid); err != nil {
		return nil, err
	}

	// Recompute the survivor's (and any cross-affected) rollups from base tables
	// now that the loser and its rows are gone.
	if err := maintainRollupsTx(ctx, tx, aff, nowNS()); err != nil {
		return nil, err
	}

	// Per-item deltas for every re-pointed item, then the entity update + loser delete.
	for _, pid := range affectedItems {
		if err := appendChange(ctx, tx, "item", pid, model.OpUpdate); err != nil {
			return nil, err
		}
	}
	if err := appendChange(ctx, tx, table, survivorPID, model.OpUpdate); err != nil {
		return nil, err
	}
	if err := appendChange(ctx, tx, table, loserPID, model.OpDelete); err != nil {
		return nil, err
	}
	return rep, nil
}

// affectedItemPIDs returns the public ids of the playable items whose association
// to the merged entity changes when the loser collapses onto the survivor.
func affectedItemPIDs(ctx context.Context, tx *sql.Tx, et model.MergeEntity, lid int64) ([]model.PID, error) {
	var q string
	var args []any
	switch et {
	case model.MergeArtist:
		q = `SELECT DISTINCT pi.pid FROM playable_item pi WHERE pi.id IN (
			SELECT item_id FROM track WHERE artist_id = ? OR album_artist_id = ?
			UNION SELECT item_id FROM book WHERE author_id = ?
			UNION SELECT item_id FROM item_contributor WHERE artist_id = ?)`
		args = []any{lid, lid, lid, lid}
	case model.MergeReleaseGroup:
		q = `SELECT pi.pid FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN playable_item pi ON pi.id = t.item_id
			WHERE al.release_group_id = ?`
		args = []any{lid}
	case model.MergeAlbum:
		q = `SELECT pi.pid FROM track t JOIN playable_item pi ON pi.id = t.item_id WHERE t.album_id = ?`
		args = []any{lid}
	case model.MergeGenre:
		q = `SELECT pi.pid FROM item_genre ig JOIN playable_item pi ON pi.id = ig.item_id WHERE ig.genre_id = ?`
		args = []any{lid}
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PID
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		out = append(out, model.PID(pid))
	}
	return out, rows.Err()
}

// entityIDByPID resolves an entity's internal id from its public id, returning
// CodeNotFound when no such row exists in the table.
func entityIDByPID(ctx context.Context, tx *sql.Tx, table string, pid model.PID) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, "store.MergeEntity", table+" not found: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "store.MergeEntity", err)
	}
	return id, nil
}

// repointArtist re-points every reference to the loser artist onto the survivor.
// The survivor's rollup is refreshed (its track/release-group counts change); the
// loser's rollup, aliases, relations, and any collided contributor/alias rows
// cascade away on the loser DELETE. It returns the count of distinct items re-parented.
func repointArtist(ctx context.Context, tx *sql.Tx, sid, lid int64, aff *affectedRollups) (int, error) {
	aff.artists[sid] = true

	// Preserve the loser's display name as an alias so the old spelling still
	// resolves. Done before the FK re-points; the alias's own artist_id is the
	// survivor from the start.
	var loserName string
	if err := tx.QueryRowContext(ctx, "SELECT name FROM artist WHERE id = ?", lid).Scan(&loserName); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR IGNORE INTO artist_alias(artist_id, name, sort_key, is_primary) VALUES (?,?,?,0)",
		sid, loserName, model.SortKey(loserName)); err != nil {
		return 0, err
	}

	// Count the distinct first-class items the loser backs (as artist, album-artist,
	// author, or credited contributor) before re-pointing, so a track linked as both
	// artist and album-artist counts once.
	var children int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT item_id FROM track WHERE artist_id = ? OR album_artist_id = ?
		UNION
		SELECT item_id FROM book WHERE author_id = ?
		UNION
		SELECT item_id FROM item_contributor WHERE artist_id = ?)`,
		lid, lid, lid, lid).Scan(&children); err != nil {
		return 0, err
	}

	// Plain re-points (no unique constraint to collide with).
	for _, q := range []string{
		"UPDATE track SET artist_id = ? WHERE artist_id = ?",
		"UPDATE track SET album_artist_id = ? WHERE album_artist_id = ?",
		"UPDATE book SET author_id = ? WHERE author_id = ?",
		"UPDATE release_group SET primary_artist_id = ? WHERE primary_artist_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, sid, lid); err != nil {
			return 0, err
		}
	}

	// Dedup-sensitive re-points: OR IGNORE keeps the survivor's existing row when a
	// (item,role,artist)/(artist,name)/(src,dst,kind) collision occurs; the loser's
	// leftover collided rows cascade away on the loser DELETE.
	for _, q := range []string{
		"UPDATE OR IGNORE item_contributor SET artist_id = ? WHERE artist_id = ?",
		"UPDATE OR IGNORE artist_alias SET artist_id = ? WHERE artist_id = ?",
		"UPDATE OR IGNORE artist_relation SET src_id = ? WHERE src_id = ?",
		"UPDATE OR IGNORE artist_relation SET dst_id = ? WHERE dst_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, q, sid, lid); err != nil {
			return 0, err
		}
	}
	// A relation that pointed loser->survivor (or survivor->loser) becomes a
	// (survivor,survivor) self-loop after re-pointing; drop only those, scoped to the
	// survivor so an unrelated artist's pre-existing self-loop is never destroyed.
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM artist_relation WHERE src_id = ? AND dst_id = ?", sid, sid); err != nil {
		return 0, err
	}
	return children, nil
}

// repointReleaseGroup re-points the loser's albums onto the survivor. The
// release-group rollup of the survivor and the artist rollups of both groups'
// primary artists are refreshed (a group's disappearance changes its primary
// artist's release_group_count).
func repointReleaseGroup(ctx context.Context, tx *sql.Tx, sid, lid int64, aff *affectedRollups) (int, error) {
	aff.rgs[sid] = true
	for _, id := range []int64{sid, lid} {
		var artistID sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			"SELECT primary_artist_id FROM release_group WHERE id = ?", id).Scan(&artistID); err != nil {
			return 0, err
		}
		if artistID.Valid {
			aff.artists[artistID.Int64] = true
		}
	}
	r, err := tx.ExecContext(ctx,
		"UPDATE album SET release_group_id = ? WHERE release_group_id = ?", sid, lid)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// repointAlbum re-points the loser album's tracks onto the survivor. Albums have
// no rollup, but moving tracks between two albums under different release groups
// changes those groups' rollups, so both are refreshed.
func repointAlbum(ctx context.Context, tx *sql.Tx, sid, lid int64, aff *affectedRollups) (int, error) {
	for _, id := range []int64{sid, lid} {
		var rgID sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			"SELECT release_group_id FROM album WHERE id = ?", id).Scan(&rgID); err != nil {
			return 0, err
		}
		if rgID.Valid {
			aff.rgs[rgID.Int64] = true
		}
	}
	r, err := tx.ExecContext(ctx, "UPDATE track SET album_id = ? WHERE album_id = ?", sid, lid)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// repointGenre re-points the loser's item_genre links onto the survivor. Links
// that would collide with an existing survivor link (an item tagged with both)
// are dropped by OR IGNORE and cascade away on the loser DELETE.
func repointGenre(ctx context.Context, tx *sql.Tx, sid, lid int64, aff *affectedRollups) (int, error) {
	aff.genres[sid] = true
	r, err := tx.ExecContext(ctx,
		"UPDATE OR IGNORE item_genre SET genre_id = ? WHERE genre_id = ?", sid, lid)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return int(n), nil
}

// repointArtMap resolves the loser's art_map rows (polymorphic, no FK). The
// survivor keeps its own art when it has any: the art_map primary key is
// (entity_type, entity_id, source_hash), so a blind re-point would move the
// loser's DIFFERENT-hash covers onto the survivor too, leaving it with several
// front covers (ResolveArt then picks by rowid, so the survivor could display the
// loser's cover) plus GC-immune extras. So only inherit the loser's art when the
// survivor has none for this entity; otherwise drop the loser's maps and let GCArt
// reclaim the now-unreferenced sources.
func repointArtMap(ctx context.Context, tx *sql.Tx, entityType string, sid, lid int64) error {
	var survivorHasArt int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM art_map WHERE entity_type = ? AND entity_id = ?",
		entityType, sid).Scan(&survivorHasArt); err != nil {
		return err
	}
	if survivorHasArt == 0 {
		if _, err := tx.ExecContext(ctx,
			"UPDATE OR IGNORE art_map SET entity_id = ? WHERE entity_type = ? AND entity_id = ?",
			sid, entityType, lid); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = ? AND entity_id = ?", entityType, lid)
	return err
}

// unionMBID copies the loser's MBID onto the survivor when the survivor has none,
// so a merge that unifies a heuristic row with an MBID-carrying one keeps the id.
func unionMBID(ctx context.Context, tx *sql.Tx, table string, sid, lid int64) error {
	var loserMBID sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT mbid FROM "+table+" WHERE id = ?", lid).Scan(&loserMBID); err != nil {
		return err
	}
	if !loserMBID.Valid || loserMBID.String == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE "+table+" SET mbid = ? WHERE id = ? AND (mbid IS NULL OR mbid = '')",
		loserMBID.String, sid)
	return err
}

// unionReleaseGroupType adopts the loser's release-group type onto the survivor
// when the loser carries a more specific one (ep/single/compilation/audiobook)
// than the survivor's default 'album'. New groups are created typed 'album', which
// enrichment later refines, so a merged group should keep the refined type.
func unionReleaseGroupType(ctx context.Context, tx *sql.Tx, sid, lid int64) error {
	var loserType sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT type FROM release_group WHERE id = ?", lid).Scan(&loserType); err != nil {
		return err
	}
	lt := strings.TrimSpace(loserType.String)
	if lt == "" || lt == "album" {
		return nil // not more specific than the survivor's default
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE release_group SET type = ? WHERE id = ? AND (type IS NULL OR type = '' OR type = 'album')",
		lt, sid)
	return err
}

// unionEnrichmentMarker inherits the loser's enrichment marker onto the survivor
// when the survivor has none (OR IGNORE keeps the survivor's), then removes the
// loser's. Called only for artist/release_group (the marker's entity types).
func unionEnrichmentMarker(ctx context.Context, tx *sql.Tx, table string, sid, lid int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO entity_enrichment(entity_type, entity_id, provider, matched, mbid, enriched_at)
		 SELECT entity_type, ?, provider, matched, mbid, enriched_at
		 FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?`,
		sid, table, lid); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		"DELETE FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?", table, lid)
	return err
}
