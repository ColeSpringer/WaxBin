package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// placeholderOf is true when the picture whose hash column follows it, with a closing
// parenthesis, is marked a placeholder. It probes the partial index
// art_source_placeholder and reads no art_source row; the planner would take the primary
// key's index instead and read the row, so the index is named.
const placeholderOf = `EXISTS (SELECT 1 FROM art_source phs INDEXED BY art_source_placeholder
	WHERE phs.placeholder = 1 AND phs.hash = `

// placeholderPicTx reports whether pic is a stored picture marked a placeholder.
func placeholderPicTx(ctx context.Context, tx *sql.Tx, pic examinedArt) (bool, error) {
	if !pic.present() {
		return false, nil
	}
	var marked bool
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(placeholder, 0) = 1 FROM art_source WHERE hash = ?", pic.img.Hash).Scan(&marked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return marked, err
}

// statingItemsQ lists the items, an episode aside, a file states the picture for as its
// cover.
const statingItemsQ = `SELECT DISTINCT e.item_id FROM file f
	JOIN item_file e ON e.file_id = f.id JOIN playable_item pi ON pi.id = e.item_id
	WHERE f.front_hash = ? AND f.front_hash <> '' AND pi.kind <> 'episode' ORDER BY e.item_id`

// SetArtPlaceholder marks a stored picture as a placeholder, or unmarks it. hash is the
// stored source's, or the hash a picture arrived under where it was stored scaled down.
// It reports whether the mark moved; a hash the catalog holds no picture under is
// CodeNotFound.
func (s *Store) SetArtPlaceholder(ctx context.Context, hash string, on bool) (bool, error) {
	const op = "store.SetArtPlaceholder"
	changed := false
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		stored, err := storedArtHashTx(ctx, tx, hash)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "no stored picture with hash "+hash)
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		changed, err = setArtPlaceholderTx(ctx, tx, stored, on)
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
	return changed, err
}

// storedArtHashTx answers the hash a picture is stored under: hash itself, or the stored
// source an oversized arrival with that hash was scaled to.
func storedArtHashTx(ctx context.Context, tx *sql.Tx, hash string) (string, error) {
	var stored string
	err := tx.QueryRowContext(ctx, `SELECT hash FROM art_source WHERE hash = ?1
		UNION ALL SELECT hash FROM art_resized WHERE from_hash = ?1 LIMIT 1`, hash).Scan(&stored)
	return stored, err
}

// setArtPlaceholderTx flips a stored picture's mark and reports whether it moved. The
// cover an item's files state depends on the mark, so every item a file states the
// picture for is settled again. Each item whose own cover the picture is, was or becomes
// is named, and so is each album whose cover moved. A mark re-opens a moved album's
// front to the backfill, since the member cover that answered it may be gone, and an
// unmark gives the album's cover back to the member over a fetched one, as a real cover
// arriving from disk does. An unmark of a picture nobody decided about moves nothing and
// records the decision.
//
// A placeholder in a file's tags gives way to the cover beside the file. Only a read
// sees that cover, so a mark sends each file whose tags hold the picture, with a cover
// beside it, to be read again at the next scan; an unmark states the picture again on
// every file that passed it over.
func setArtPlaceholderTx(ctx context.Context, tx *sql.Tx, hash string, on bool) (bool, error) {
	var cur sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT placeholder FROM art_source WHERE hash = ?", hash).Scan(&cur); err != nil {
		return false, err
	}
	marked := cur.Valid && cur.Int64 == 1
	if on == marked {
		if !on && !cur.Valid {
			_, err := tx.ExecContext(ctx, "UPDATE art_source SET placeholder = 0 WHERE hash = ?", hash)
			return false, err
		}
		return false, nil
	}
	if !on {
		if _, err := tx.ExecContext(ctx, `UPDATE file SET front_hash = front_stand_in, front_source = 'tag', front_stand_in = ''
			WHERE front_stand_in = ?1 AND front_stand_in <> ''`, hash); err != nil {
			return false, err
		}
	}
	const ownersQ = "SELECT entity_id FROM art_map WHERE entity_type = 'track' AND role = 'front' AND source_hash = ? ORDER BY entity_id"
	owners, err := queryInt64sTx(ctx, tx, ownersQ, hash)
	if err != nil {
		return false, err
	}
	stating, err := queryInt64sTx(ctx, tx, statingItemsQ, hash)
	if err != nil {
		return false, err
	}
	aff := newAffectedRollups()
	for _, id := range append(slices.Clone(owners), stating...) {
		if err := aff.watchAlbumOf(ctx, tx, id); err != nil {
			return false, err
		}
	}
	mark := 0
	if on {
		mark = 1
	}
	if _, err := tx.ExecContext(ctx, "UPDATE art_source SET placeholder = ? WHERE hash = ?", mark, hash); err != nil {
		return false, err
	}
	named := map[int64]bool{}
	for _, id := range owners {
		named[id] = true
	}
	for _, id := range stating {
		changed, err := settleFrontTx(ctx, tx, aff, id, examinedArt{}, false, true)
		if err != nil {
			return false, err
		}
		if changed {
			named[id] = true
		}
	}
	after, err := queryInt64sTx(ctx, tx, ownersQ, hash)
	if err != nil {
		return false, err
	}
	for _, id := range after {
		named[id] = true
	}
	if !on {
		// A cover from the disk is the file's own claim on its album; one set by hand
		// leaves the album's fetched cover, as SetItemArt does.
		fromDisk, err := queryInt64sTx(ctx, tx, `SELECT entity_id FROM art_map WHERE entity_type = 'track' AND role = 'front'
			AND source_hash = ? AND source IN ('tag', 'sidecar') ORDER BY entity_id`, hash)
		if err != nil {
			return false, err
		}
		for _, id := range fromDisk {
			if err := yieldAlbumEnrichmentFrontTx(ctx, tx, id, hash); err != nil {
				return false, err
			}
		}
	}
	if on {
		if _, err := tx.ExecContext(ctx, `UPDATE file SET mtime_ns = -1
			WHERE front_hash = ?1 AND front_hash <> '' AND front_source = 'tag' AND EXISTS (SELECT 1 FROM file_aux_state a
				WHERE a.file_id = file.id AND a.kind = 'cover' AND a.missing = 0 AND a.hash NOT IN ('', ?1))`, hash); err != nil {
			return false, err
		}
		moved, err := aff.movedFrontsTx(ctx, tx)
		if err != nil {
			return false, err
		}
		for _, albumID := range moved {
			if err := clearEntityMarkerTx(ctx, tx, enrichEntityAlbumFront, albumID); err != nil {
				return false, err
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(named)) {
		if err := appendItemUpdateTx(ctx, tx, id); err != nil {
			return false, err
		}
	}
	return true, aff.announceFrontsTx(ctx, tx)
}

// markStatedPlaceholderTx marks the picture a put found in the file's tags, when the
// put says it stands in for a cover, ahead of the file's statement and the cover
// settling. A picture new to the catalog is marked as it is stored, since nothing else
// draws it yet, and one held already is marked only while nobody has decided about it,
// so a picture the user unmarked stays a cover.
func markStatedPlaceholderTx(ctx context.Context, tx *sql.Tx, c scannedCover, placeholder bool) error {
	if !placeholder || !c.pic.present() || c.pic.img.Source != model.SourceTag {
		return nil
	}
	hash := c.pic.img.Hash
	var decided sql.NullInt64
	err := tx.QueryRowContext(ctx, "SELECT placeholder FROM art_source WHERE hash = ?", hash).Scan(&decided)
	if errors.Is(err, sql.ErrNoRows) {
		if err := insertArtSourceTx(ctx, tx, c.pic); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE art_source SET placeholder = 1 WHERE hash = ?", hash)
		return err
	}
	if err != nil || decided.Valid {
		return err
	}
	_, err = setArtPlaceholderTx(ctx, tx, hash, true)
	return err
}
