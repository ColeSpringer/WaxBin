package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// The cover an item draws from disk is the one its files state. Each file's row keeps
// what the file states (file.front_hash and front_source): the picture in its tags or
// beside it, none, or unknown. An item's files speak in a fixed order, its primary, then
// a book's parts in reading order, then its copies in the order they joined, and the
// first to state a picture gives the item its cover, a placeholder only when none of them
// states a real one. So a copy's cover is the item's while its primary has none,
// whichever of them was read last, and a copy that is trashed and restored gives back the
// cover it took with it.
//
// settleFrontTx holds the rule and runs wherever what the files state can move: a read of
// a file, a write of a file's picture, and a file joining or leaving an item. The
// derived-data check counts an item the rule would change (coverDriftQ), so a path that
// moves a file and forgets to settle shows up there.

// scannedCover is what a file states about its cover, as a read of it or a write of its
// picture found: a picture, none, or neither. beside marks a finding made beside the
// file alone, its tags unread, which leaves standing a picture its tags last stated.
// Neither, from a read of the tags, is a file with no picture in them and folders that
// did not say, whose cover is unknown from then on. sidecar is the cover beside a file
// whose tags hold the picture, examined only when it may take that picture's place
// (examineSidecar), and sidecarSkipped says one stood there unexamined. standIn is the
// picture once the sidecar took its place (passOverStandInTx).
type scannedCover struct {
	pic            examinedArt
	none           bool
	beside         bool
	sidecar        examinedArt
	sidecarSkipped bool
	standIn        examinedArt
}

// scanned reads a put's cover fields as one statement: the picture when there is one,
// else the finding of none when the scan made it.
func scanned(pic, sidecar examinedArt, noCover, tagsUnread bool) scannedCover {
	return scannedCover{pic: pic, none: noCover && !pic.present(), beside: tagsUnread, sidecar: sidecar}
}

// said reports whether the statement names a picture or says there is none.
func (c scannedCover) said() bool { return c.pic.present() || c.none }

// read reports whether the statement came with news of the file: every one does but a
// look beside a file that found no answer there.
func (c scannedCover) read() bool { return c.said() || !c.beside }

// recordFileFrontTx keeps what a file states about its cover. A statement that names
// neither a picture nor none sets the record to unknown when the tags were read, since
// they hold no picture and a record of one would be stale, and leaves it alone when they
// were not. One that says what the record holds writes nothing. The picture's bytes are
// stored with the record, since the file may be the one that gives its item the picture
// later, at a write that reads no file. A placeholder in the tags first gives way to the
// cover beside the file, which c then carries.
func recordFileFrontTx(ctx context.Context, tx *sql.Tx, fileID int64, c *scannedCover) error {
	if !c.said() {
		if c.beside {
			return nil
		}
		_, err := tx.ExecContext(ctx, `UPDATE file SET front_hash = NULL, front_source = NULL, front_stand_in = ''
			WHERE id = ? AND (front_hash IS NOT NULL OR front_stand_in <> '')`, fileID)
		return err
	}
	if err := passOverStandInTx(ctx, tx, fileID, c); err != nil {
		return err
	}
	hash, source, standIn := "", sql.NullString{}, ""
	if c.pic.present() {
		hash, source = c.pic.img.Hash, sql.NullString{String: string(c.pic.img.Source), Valid: true}
	}
	if c.standIn.present() {
		standIn = c.standIn.img.Hash
	}
	q := `UPDATE file SET front_hash = ?1, front_source = ?2, front_stand_in = ?4
		WHERE id = ?3 AND (front_hash IS NOT ?1 OR front_source IS NOT ?2 OR front_stand_in IS NOT ?4)`
	if c.beside {
		q = "UPDATE file SET front_hash = ?1, front_source = ?2 WHERE id = ?3 AND (front_hash IS NOT ?1 OR front_source IS NOT ?2) AND front_source IS NOT 'tag'"
	}
	r, err := tx.ExecContext(ctx, q, hash, source, fileID, standIn)
	if err != nil || !c.pic.present() {
		return err
	}
	if n, err := r.RowsAffected(); err != nil || n == 0 {
		return err
	}
	return insertArtSourceTx(ctx, tx, c.pic)
}

// passOverStandInTx puts the cover beside a file in the place of the picture in its tags
// while the catalog holds that picture a placeholder, as a read passes over a picture
// that does not decode, and keeps the picture as the one passed over. The mark is read
// again here, in the transaction that records the statement. One that landed after the
// put looked left the cover beside the file unexamined, so the file is read again at the
// next scan to put it in place.
func passOverStandInTx(ctx context.Context, tx *sql.Tx, fileID int64, c *scannedCover) error {
	if !c.pic.present() || c.pic.img.Source != model.SourceTag || !c.sidecar.present() && !c.sidecarSkipped {
		return nil
	}
	if c.sidecar.present() && c.sidecar.img.Hash == c.pic.img.Hash {
		return nil
	}
	var marked bool
	err := tx.QueryRowContext(ctx, "SELECT COALESCE(placeholder, 0) = 1 FROM art_source WHERE hash = ?", c.pic.img.Hash).Scan(&marked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || !marked {
		return err
	}
	if !c.sidecar.present() {
		_, err := tx.ExecContext(ctx, "UPDATE file SET mtime_ns = -1 WHERE id = ?", fileID)
		return err
	}
	c.standIn, c.pic = c.pic, c.sidecar
	return nil
}

// examineSidecar examines the cover beside a file only when the catalog may state it in
// place of the picture in the file's tags (pic): that picture is marked a placeholder,
// or the put marks it (flagged) and nobody decided about it. Examining scales a large
// cover, which every read of a file holding its own picture would otherwise pay for
// nothing. skipped says a cover stood there and was left unexamined.
func (s *Store) examineSidecar(ctx context.Context, pic examinedArt, sidecar *model.ArtImage, flagged bool) (e examinedArt, skipped bool, err error) {
	if sidecar == nil || len(sidecar.Data) == 0 || !pic.present() || pic.img.Source != model.SourceTag {
		return examinedArt{}, false, nil
	}
	var mark sql.NullInt64
	err = s.rdb().QueryRowContext(ctx, "SELECT placeholder FROM art_source WHERE hash = ?", pic.img.Hash).Scan(&mark)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return examinedArt{}, false, err
	}
	if !(mark.Valid && mark.Int64 == 1) && !(flagged && !(found && mark.Valid)) {
		return examinedArt{}, true, nil
	}
	e, err = s.examineArt(ctx, sidecar)
	return e, false, err
}

// frontStatement is what one of an item's files states: a picture's hash, the empty
// string for none, or null for unknown, and whether the picture is a placeholder.
type frontStatement struct {
	hash        sql.NullString
	source      string
	placeholder bool
}

// frontStatementsQ lists what an item's files state, in the order they speak for it.
// coverDriftQ orders them the same way.
const frontStatementsQ = `SELECT f.front_hash, COALESCE(f.front_source, ''), ` + placeholderOf + `f.front_hash)
	FROM item_file e JOIN file f ON f.id = e.file_id WHERE e.item_id = ?
	ORDER BY ` + frontFileOrder

// frontFileOrder is the order an item's files speak in, over the edge e and the file f.
const frontFileOrder = `e.role = 'primary' DESC, e.role = 'alternate', e.position, f.id`

// statedFrontTx works out the cover an item's files state. Of the files ahead of the
// first whose cover the catalog does not know, the first real picture one of them states
// gives it, else the first placeholder. It is none (a nil statement) when they state no
// picture, and unknown when that unknown file comes before any picture, since what it
// states would come first.
func statedFrontTx(ctx context.Context, tx *sql.Tx, itemID int64) (stated *frontStatement, files int, unknown bool, err error) {
	rows, err := tx.QueryContext(ctx, frontStatementsQ, itemID)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	var stand *frontStatement
	for rows.Next() {
		var f frontStatement
		if err := rows.Scan(&f.hash, &f.source, &f.placeholder); err != nil {
			return nil, 0, false, err
		}
		files++
		if stated != nil || unknown {
			continue
		}
		switch {
		case !f.hash.Valid:
			unknown = true
		case f.hash.String == "":
		case !f.placeholder:
			stated = &f
		case stand == nil:
			stand = &f
		}
	}
	if stated == nil && stand != nil {
		stated, unknown = stand, false
	}
	return stated, files, unknown, rows.Err()
}

// settleFrontTx brings the front an item draws from disk, the one in the track slot, to
// the cover its files state, after what they state moved. pic is the picture in hand when
// a file's statement just arrived with one. read says the move was a read of one of the
// item's files (or a file joining with what its row holds); a write of the catalog's own
// picture into a file and a file leaving are not.
//
// While the files' cover is unknown nothing is touched. A picture they state is attached
// over an empty front, over one the disk gave (a tag's or a sidecar's), and at a read
// over an unlocked front of any other origin, as a read of the item's file always has
// done. A write-back or a departure never replaces a cover a user or a provider set, and
// a placeholder never does, read or not. A front that holds the stated picture already
// and came from elsewhere is left as it is: a cover a user chose stays theirs once a
// write-back has put it in the file, and the files it has not reached go on owing it.
// When the files state none, a front the disk gave goes and any other stays. An item
// with no file left keeps its cover for the file that may come back. A lock gates every
// write when preserveLock is set.
//
// It reports whether the front changed. A track's front is what its album borrows, so the
// album is watched before a write (affectedRollups.watchAlbumOf) and the caller names it
// once the write is done, if what the album draws moved.
func settleFrontTx(ctx context.Context, tx *sql.Tx, aff *affectedRollups, itemID int64, pic examinedArt, read, preserveLock bool) (bool, error) {
	stated, files, unknown, err := statedFrontTx(ctx, tx, itemID)
	if err != nil || files == 0 || unknown {
		return false, err
	}
	var hash, source string
	var standIn bool
	err = tx.QueryRowContext(ctx, `SELECT am.source_hash, am.source, `+placeholderOf+`am.source_hash)
		FROM art_map am WHERE am.entity_type = 'track' AND am.entity_id = ? AND am.role = 'front'`, itemID).Scan(&hash, &source, &standIn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	fromDisk := source == string(model.SourceTag) || source == string(model.SourceSidecar)
	switch {
	case stated == nil && !fromDisk:
		return false, nil
	case stated != nil && hash == stated.hash.String && (!fromDisk || source == stated.source):
		return false, nil
	case stated != nil && hash != "" && hash != stated.hash.String && !fromDisk && (!read || stated.placeholder):
		return false, nil
	}
	if preserveLock {
		locked, err := artFillBlockedTx(ctx, tx, model.ArtTrack, itemID, model.ArtRoleFront)
		if err != nil || locked {
			return false, err
		}
	}
	attr := model.Attribution{}
	if stated != nil {
		attr.Source = model.ProvenanceSource(stated.source)
		if hash == stated.hash.String {
			// The same picture from another place: beside the file before, in its tags now.
			return false, refreshArtProvenanceTx(ctx, tx, string(model.ArtTrack), itemID, string(model.ArtRoleFront), &model.ArtImage{Attribution: attr})
		}
	}
	if err := aff.watchAlbumOf(ctx, tx, itemID); err != nil {
		return false, err
	}
	if stated == nil {
		if _, err := setEntityArtRoleTx(ctx, tx, string(model.ArtTrack), itemID, string(model.ArtRoleFront), examinedArt{}); err != nil {
			return false, err
		}
		return true, memberFrontClearedTx(ctx, tx, itemID)
	}
	if pic.present() && pic.img.Hash == stated.hash.String {
		if _, err := setEntityArtRoleTx(ctx, tx, string(model.ArtTrack), itemID, string(model.ArtRoleFront), pic); err != nil {
			return false, err
		}
	} else if held, err := mapStoredArtTx(ctx, tx, string(model.ArtTrack), itemID, string(model.ArtRoleFront), stated.hash.String, attr); err != nil || !held {
		// A picture the store no longer holds is one no write here leaves behind: the
		// file's next read restates it.
		return false, err
	}
	if !stated.placeholder {
		return true, yieldAlbumEnrichmentFrontTx(ctx, tx, itemID, stated.hash.String)
	}
	// A stand-in leaves the album's fetched cover where it is, and one taking a real
	// cover's place opens the album's front to be asked about again.
	if hash != "" && !standIn {
		return true, memberFrontClearedTx(ctx, tx, itemID)
	}
	return true, nil
}

// settleFrontAfterLeaveTx is settleFrontTx for an item a file just left.
func settleFrontAfterLeaveTx(ctx context.Context, tx *sql.Tx, aff *affectedRollups, itemID int64, preserveLock bool) error {
	_, err := settleFrontTx(ctx, tx, aff, itemID, examinedArt{}, false, preserveLock)
	return err
}

// settleFileFrontsTx settles every item a file backs after the file's statement arrived
// outside a put, and names in the change log each item whose front moved and each album
// that borrows one. It reports whether any front moved.
func settleFileFrontsTx(ctx context.Context, tx *sql.Tx, fileID int64, pic examinedArt, read, preserveLock bool) (bool, error) {
	items, err := queryInt64sTx(ctx, tx, `SELECT DISTINCT e.item_id FROM item_file e
		JOIN playable_item pi ON pi.id = e.item_id WHERE e.file_id = ? AND pi.kind <> 'episode' ORDER BY e.item_id`, fileID)
	if err != nil {
		return false, err
	}
	aff := newAffectedRollups()
	moved := false
	for _, id := range items {
		changed, err := settleFrontTx(ctx, tx, aff, id, pic, read, preserveLock)
		if err != nil {
			return false, err
		}
		if changed {
			moved = true
			if err := appendItemUpdateTx(ctx, tx, id); err != nil {
				return false, err
			}
		}
	}
	return moved, aff.announceFrontsTx(ctx, tx)
}

// coverDriftQ selects the items whose cover from disk is not the one their files state:
// settleFrontTx's rule read off the tables, for the derived-data check and its repair. An
// item is left out while its files' cover is unknown or its cover is locked, as the rule
// leaves it alone. picture_at is the place of the picture the files state, the first real
// one ahead of an unknown file, else the first placeholder there.
const coverDriftQ = `WITH stated AS (
		SELECT e.item_id, f.front_hash AS hash, ` + placeholderOf + `f.front_hash) AS stand_in,
			ROW_NUMBER() OVER (PARTITION BY e.item_id ORDER BY ` + frontFileOrder + `) AS place
		FROM item_file e JOIN file f ON f.id = e.file_id
		WHERE e.item_id IN (SELECT entity_id FROM art_map
			WHERE entity_type = 'track' AND role = 'front' AND source IN ('tag', 'sidecar'))
	), places AS (
		SELECT item_id, MIN(place) FILTER (WHERE hash IS NULL) AS unknown_at,
			MIN(place) FILTER (WHERE hash <> '' AND NOT stand_in) AS real_at,
			MIN(place) FILTER (WHERE hash <> '' AND stand_in) AS stand_in_at
		FROM stated GROUP BY item_id
	), firsts AS (
		SELECT item_id, unknown_at, CASE
			WHEN real_at < COALESCE(unknown_at, real_at + 1) THEN real_at
			WHEN stand_in_at < COALESCE(unknown_at, stand_in_at + 1) THEN stand_in_at END AS picture_at
		FROM places
	)
	SELECT am.entity_id FROM art_map am
	JOIN firsts s ON s.item_id = am.entity_id
	LEFT JOIN stated p ON p.item_id = s.item_id AND p.place = s.picture_at
	WHERE am.entity_type = 'track' AND am.role = 'front' AND am.source IN ('tag', 'sidecar')
	  AND (s.unknown_at IS NULL OR s.picture_at IS NOT NULL)
	  AND COALESCE(p.hash, '') <> am.source_hash
	  AND NOT EXISTS (SELECT 1 FROM field_provenance fp
		WHERE fp.item_id = am.entity_id AND fp.field = 'art' AND fp.locked = 1)
	ORDER BY am.entity_id`

// refreshAllFrontsTx settles the cover of every item coverDriftQ selects, naming each
// item and album whose cover moved, and returns how many items' covers it moved.
func refreshAllFrontsTx(ctx context.Context, tx *sql.Tx) (int, error) {
	items, err := queryInt64sTx(ctx, tx, coverDriftQ)
	if err != nil {
		return 0, err
	}
	aff := newAffectedRollups()
	moved := 0
	for _, id := range items {
		changed, err := settleFrontTx(ctx, tx, aff, id, examinedArt{}, false, true)
		if err != nil {
			return 0, err
		}
		if changed {
			moved++
			if err := appendItemUpdateTx(ctx, tx, id); err != nil {
				return 0, err
			}
		}
	}
	return moved, aff.announceFrontsTx(ctx, tx)
}

// PutFolderCover records what stands beside a file as the cover it states, found without
// reading its audio, and settles the items it backs. It applies only while the catalog
// does not know what the file states: such a file has no picture in its tags, so what is
// beside it is all it states. An import whose covers land after its audio leaves its
// files that way, as does a write that stripped a file's picture and a read that could
// not list a cover folder. It reports whether an item's cover changed; a file the catalog
// does not hold, or one whose cover it knows, is left alone, which is checked before the
// picture is examined or the write lock taken.
func (s *Store) PutFolderCover(ctx context.Context, in model.FolderCoverInput) (bool, error) {
	const op = "store.PutFolderCover"
	if in.Cover.Art == nil && !in.Cover.None {
		return false, nil
	}
	const unknownQ = "SELECT id FROM file WHERE pid = ? AND front_hash IS NULL"
	var fileID int64
	err := s.rdb().QueryRowContext(ctx, unknownQ, string(in.FilePID)).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	pic, err := s.examineArt(ctx, in.Cover.Art)
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	c := scanned(pic, examinedArt{}, in.Cover.None, true)
	changed := false
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, unknownQ, string(in.FilePID)).Scan(&fileID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil && in.Observed != nil {
			err = putFileAuxTx(ctx, tx, fileID, []model.AuxObservation{*in.Observed})
		}
		if err == nil {
			err = recordFileFrontTx(ctx, tx, fileID, &c)
		}
		if err == nil {
			changed, err = settleFileFrontsTx(ctx, tx, fileID, c.pic, true, in.PreserveLocks)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	})
	return changed, err
}
