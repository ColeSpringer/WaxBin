package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// orphanKind describes one GC-able entity type: its table, its "is childless" WHERE
// predicate (mirroring the read-side EXISTS filters in stats), and the change_log
// entity type. Types are swept children-first (album before release_group, then the
// leaf entities) so deleting an album's last track can retire its release group on a
// later pass.
type orphanKind struct {
	entityType string
	table      string
	// orphanWhere is a predicate on the aliased table (alias `e`) selecting childless
	// rows. It mirrors the read-side NOT EXISTS filters so GC and the displayed counts
	// agree on what "backs content".
	orphanWhere string
}

// orphanKinds is the children-first sweep order. Album precedes release_group
// because an album keeps its release group non-childless; the leaf entities
// (artist/genre/series) come last, once tracks/books/albums referencing them are gone.
var orphanKinds = []orphanKind{
	{
		entityType: "album", table: "album",
		orphanWhere: "NOT EXISTS (SELECT 1 FROM track t WHERE t.album_id = e.id)",
	},
	{
		entityType: "release_group", table: "release_group",
		orphanWhere: "NOT EXISTS (SELECT 1 FROM album al JOIN track t ON t.album_id = al.id WHERE al.release_group_id = e.id)",
	},
	{
		entityType: "artist", table: "artist",
		// Referenced by a track (artist or album-artist), a book author, any credited
		// contributor (narrator/translator/…), or a release group's primary artist.
		orphanWhere: `NOT EXISTS (SELECT 1 FROM track t WHERE t.artist_id = e.id OR t.album_artist_id = e.id)
			AND NOT EXISTS (SELECT 1 FROM book b WHERE b.author_id = e.id)
			AND NOT EXISTS (SELECT 1 FROM item_contributor ic WHERE ic.artist_id = e.id)
			AND NOT EXISTS (SELECT 1 FROM release_group rg WHERE rg.primary_artist_id = e.id)`,
	},
	{
		entityType: "genre", table: "genre",
		orphanWhere: "NOT EXISTS (SELECT 1 FROM item_genre ig WHERE ig.genre_id = e.id)",
	},
	{
		entityType: "series", table: "series",
		orphanWhere: "NOT EXISTS (SELECT 1 FROM book b WHERE b.series_id = e.id)",
	},
}

// GCOrphans deletes childless entities (artist/release_group/album/genre/series) that
// have stayed orphaned past graceNS, and records the rest as candidates so a later
// run can sweep them once the grace window elapses. It is manual-only (invoked by db
// vacuum / db verify --fix), never the watch loop, so a transient reconciliation blip
// cannot immediately destroy enrichment or operator merges. A graceNS of 0 sweeps a
// childless entity on the first run (used by tests and callers that opt out of the
// grace window).
//
// Deletion cascades an entity's rollups and (for an artist) its aliases, relations,
// and contributor links via foreign keys; the polymorphic art_map and
// entity_enrichment rows carry no FK and are removed explicitly, leaving their art
// sources for the GCArt pass the same maintenance flow runs.
func (s *Store) GCOrphans(ctx context.Context, graceNS int64) (*model.OrphanGCReport, error) {
	const op = "store.GCOrphans"
	rep := &model.OrphanGCReport{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()
		cutoff := now - graceNS
		for _, k := range orphanKinds {
			deleted, pending, err := gcOrphanKind(ctx, tx, k, now, cutoff)
			if err != nil {
				return waxerr.Wrapf(waxerr.CodeIO, op, err, "sweeping %s", k.entityType)
			}
			rep.Pending += pending
			switch k.entityType {
			case "album":
				rep.Albums = deleted
			case "release_group":
				rep.ReleaseGroups = deleted
			case "artist":
				rep.Artists = deleted
			case "genre":
				rep.Genres = deleted
			case "series":
				rep.Series = deleted
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// orphanRow is one childless entity found this pass.
type orphanRow struct {
	id  int64
	pid model.PID
}

// gcOrphanKind sweeps one entity type: it reconciles the candidate set against the
// current orphans, deletes those past the grace window, and returns the deleted and
// still-pending counts.
func gcOrphanKind(ctx context.Context, tx *sql.Tx, k orphanKind, now, cutoff int64) (deleted, pending int, err error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT e.id, e.pid FROM "+k.table+" e WHERE "+k.orphanWhere)
	if err != nil {
		return 0, 0, err
	}
	var orphans []orphanRow
	current := map[int64]bool{}
	for rows.Next() {
		var r orphanRow
		if err := rows.Scan(&r.id, &r.pid); err != nil {
			rows.Close()
			return 0, 0, err
		}
		orphans = append(orphans, r)
		current[r.id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()

	// Load this type's existing candidate first-seen times in one query (no per-orphan
	// SELECT), then reconcile in memory.
	existing, err := loadCandidates(ctx, tx, k.entityType)
	if err != nil {
		return 0, 0, err
	}

	// Drop candidates that are no longer orphaned (came back), so a re-populated
	// entity restarts the grace window if it is orphaned again later. One batch delete.
	var stale []int64
	for id := range existing {
		if !current[id] {
			stale = append(stale, id)
		}
	}
	if err := deleteCandidates(ctx, tx, k.entityType, stale); err != nil {
		return 0, 0, err
	}

	// Sweep confirmed orphans; collect newly-pending ones to record in one batch.
	var newlyPending []int64
	for _, o := range orphans {
		firstSeen, seen := existing[o.id]
		if !seen {
			firstSeen = now // first sighting
		}
		if firstSeen > cutoff {
			pending++ // still within the grace window
			if !seen {
				newlyPending = append(newlyPending, o.id)
			}
			continue
		}
		// Past the grace window (or grace=0): delete. deleteOrphanEntity also drops any
		// candidate row, so a swept orphan needs no bookkeeping here.
		if err := deleteOrphanEntity(ctx, tx, k, o); err != nil {
			return 0, 0, err
		}
		deleted++
	}
	if err := insertCandidates(ctx, tx, k.entityType, newlyPending, now); err != nil {
		return 0, 0, err
	}
	return deleted, pending, nil
}

// loadCandidates returns entity_id -> first_seen for one entity type in one query.
func loadCandidates(ctx context.Context, tx *sql.Tx, entityType string) (map[int64]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT entity_id, first_seen FROM orphan_candidate WHERE entity_type = ?", entityType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, firstSeen int64
		if err := rows.Scan(&id, &firstSeen); err != nil {
			return nil, err
		}
		out[id] = firstSeen
	}
	return out, rows.Err()
}

// deleteCandidates removes candidate rows for a set of ids in chunked batch deletes.
func deleteCandidates(ctx context.Context, tx *sql.Tx, entityType string, entityIDs []int64) error {
	return chunkSlice(entityIDs, idBatchSize, func(batch []int64) error {
		args := make([]any, 0, len(batch)+1)
		args = append(args, entityType)
		for _, id := range batch {
			args = append(args, id)
		}
		_, err := tx.ExecContext(ctx,
			"DELETE FROM orphan_candidate WHERE entity_type = ? AND entity_id IN "+placeholders(len(batch)), args...)
		return err
	})
}

// insertCandidates records first-seen times for newly-orphaned entities in chunked
// multi-row inserts.
func insertCandidates(ctx context.Context, tx *sql.Tx, entityType string, entityIDs []int64, now int64) error {
	return chunkSlice(entityIDs, idBatchSize, func(batch []int64) error {
		vals := make([]string, len(batch))
		args := make([]any, 0, len(batch)*3)
		for j, id := range batch {
			vals[j] = "(?,?,?)"
			args = append(args, entityType, id, now)
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO orphan_candidate(entity_type, entity_id, first_seen) VALUES "+strings.Join(vals, ",")+
				" ON CONFLICT(entity_type, entity_id) DO NOTHING", args...)
		return err
	})
}

// deleteOrphanEntity removes one orphaned entity: its polymorphic art_map and
// entity_enrichment rows (no FK), the entity row itself (cascading rollups/aliases/
// relations/contributor links), its candidate row, and a change_log delta.
func deleteOrphanEntity(ctx context.Context, tx *sql.Tx, k orphanKind, o orphanRow) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM art_map WHERE entity_type = ? AND entity_id = ?", k.entityType, o.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM entity_enrichment WHERE entity_type = ? AND entity_id = ?", k.entityType, o.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+k.table+" WHERE id = ?", o.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM orphan_candidate WHERE entity_type = ? AND entity_id = ?", k.entityType, o.id); err != nil {
		return err
	}
	return appendChange(ctx, tx, k.entityType, o.pid, model.OpDelete)
}
