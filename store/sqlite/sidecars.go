package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// UpdateItemSidecars refreshes an item's sidecar-sourced data (lyrics, cover art,
// and external cue/chapter-file chapters) outside the audio-change gate, and records
// the new sidecar observations in file_aux_state, all in one transaction. It is the
// fast-path's seam for an edited .lrc / cover.jpg / .cue over unchanged audio: it
// never re-runs resolveFile / upsertTrack / entity resolution, and it emits an item
// change_log delta only when something actually changed, so a no-op stays silent.
//
// Nil Lyrics and CoverArt leave those untouched (a scan never clears art on a failed
// read); a non-nil empty Lyrics clears the lyrics row. Observations are persisted
// regardless of a content change so a mtime-only touch is not re-parsed next scan.
func (s *Store) UpdateItemSidecars(ctx context.Context, in model.SidecarUpdate) (bool, error) {
	const op = "store.UpdateItemSidecars"
	var changed bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, err := itemIDByPID(ctx, tx, in.ItemPID, op)
		if err != nil {
			return err
		}

		var fileID int64
		haveFile := false
		if in.FilePID != "" {
			fileID, err = fileIDByPID(ctx, tx, in.FilePID, op)
			if err != nil {
				return err
			}
			haveFile = true
		}

		// A nil Lyrics leaves the stored lyrics alone; this seam never clears them.
		//
		// The scan fast path no longer supplies Lyrics: a changed .lrc routes to the full
		// path so its diagnostics are re-derived there, so today only .cue chapters reach
		// this seam. The lyrics handling stays because SidecarUpdate.Lyrics is part of the
		// Catalog port's contract and is covered by its own store test (as CoverArt has
		// been all along), but no production caller exercises it right now.
		if in.Lyrics != nil {
			ly := in.Lyrics
			// A caller supplying only synced lines has not read the audio, so preserve the
			// stored unsynchronized block, which came from the file's embedded USLT on the
			// last full scan, rather than clobbering it to NULL. The full scan path already
			// merges embedded unsynced with .lrc synced before it writes.
			if len(ly.Synced) > 0 && ly.Unsynced == "" {
				if stored, err := currentUnsynced(ctx, tx, itemID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				} else if stored != "" {
					merged := *ly
					merged.Unsynced = stored
					ly = &merged
				}
			}
			c, err := putLyricsTx(ctx, tx, itemID, ly)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			changed = changed || c
		}
		if in.CoverArt != nil {
			c, err := attachArtTxChanged(ctx, tx, itemID, in.CoverArt)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			changed = changed || c
		}
		if in.ReplaceChapters && haveFile && itemIsBook(ctx, tx, itemID) {
			src := in.ChapterSource
			if src == "" {
				src = "cue"
			}
			c, err := syncChaptersForFileSource(ctx, tx, itemID, fileID, src, in.Chapters)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if c {
				changed = true
				// The chapter span may extend the book's effective duration.
				if err := refreshBookDuration(ctx, tx, itemID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
		}

		if haveFile && len(in.Observations) > 0 {
			if err := putFileAuxTx(ctx, tx, fileID, in.Observations); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		if changed {
			if err := appendChange(ctx, tx, "item", in.ItemPID, model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// itemIsBook reports whether an item is a book, so the sidecar seam applies
// cue/chapter-file chapters only to books (a plain track has no chapter navigation).
func itemIsBook(ctx context.Context, tx *sql.Tx, itemID int64) bool {
	var kind string
	if err := tx.QueryRowContext(ctx, "SELECT kind FROM playable_item WHERE id = ?", itemID).Scan(&kind); err != nil {
		return false
	}
	return kind == string(model.KindBook)
}

// currentUnsynced returns an item's stored unsynchronized lyrics block, or "".
func currentUnsynced(ctx context.Context, tx *sql.Tx, itemID int64) (string, error) {
	var unsynced sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT unsynced FROM lyrics WHERE item_id = ?", itemID).Scan(&unsynced)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return unsynced.String, nil
}

// replaceFileAuxTx makes a file's file_aux_state rows exactly obs: it deletes the
// existing rows and inserts the current set. The full scan path uses it (obs = the
// sidecars that still exist) so a sidecar deleted since the last scan has its
// observation pruned; otherwise the vanished path would be stat'd, and force a full
// re-hash of the file, on every future scan forever.
func replaceFileAuxTx(ctx context.Context, tx *sql.Tx, fileID int64, obs []model.AuxObservation) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_aux_state WHERE file_id = ?", fileID); err != nil {
		return err
	}
	return putFileAuxTx(ctx, tx, fileID, obs)
}

// putFileAuxTx upserts a file's sidecar observations (one row per (file_id, kind)),
// so the scanner can stat-compare a sidecar on the next scan and skip re-parsing an
// unchanged one.
func putFileAuxTx(ctx context.Context, tx *sql.Tx, fileID int64, obs []model.AuxObservation) error {
	for _, o := range obs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO file_aux_state(file_id, kind, path, size, mtime_ns, hash, missing)
			 VALUES (?,?,?,?,?,?,?)
			 ON CONFLICT(file_id, kind) DO UPDATE SET
			   path=excluded.path, size=excluded.size, mtime_ns=excluded.mtime_ns,
			   hash=excluded.hash, missing=excluded.missing`,
			fileID, o.Kind, o.Path, o.Size, o.MTimeNS, o.Hash, boolInt(o.Missing)); err != nil {
			return err
		}
	}
	return nil
}
