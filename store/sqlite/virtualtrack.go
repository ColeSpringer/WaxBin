package sqlite

import (
	"context"
	"database/sql"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// PutScannedVirtualTracks persists the virtual tracks a .cue sheet carves out of one
// single-file album rip. Every track is an ordinary playable_item(kind='track') that
// shares the one backing file through its own primary item_file edge, and that edge
// carries the track's [start_frames, end_frames) offset window. The tracks are
// reconciled as a SET keyed by identity_key: a rescan creates new tracks, updates
// changed ones, and deletes stale ones, all against the same file.
//
// This departs deliberately from the single-owner file model the rest of the scan
// path enforces (linkPrimaryFile detaches a file from every other item and deletes
// an item left with no files). Here one file legitimately backs N items, so it uses
// linkVirtualTrackFile, which attaches the shared file without detaching the
// siblings, and it never treats a sibling as an orphan. It is only ever invoked when
// something actually changed: the scan fast-path skips an unchanged rip entirely and
// routes a changed .cue (or changed audio) to the full path, which lands here. A
// per-track comparison then decides which tracks actually changed and emits a delta
// only for those, so a forced rescan of an unchanged rip stays silent.
func (s *Store) PutScannedVirtualTracks(ctx context.Context, in model.PutScannedVirtualTracksInput) (*model.ScanItemResult, error) {
	const op = "store.PutScannedVirtualTracks"
	res := &model.ScanItemResult{}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		fileID, filePID, err := s.resolveScannedFile(ctx, tx, in.LibraryID, in.File, now, res)
		if err != nil {
			return err
		}
		res.FilePID = filePID

		if err := replaceFileAuxTx(ctx, tx, fileID, in.AuxObservations); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := replaceFileDiagnosticsTx(ctx, tx, fileID, model.OriginScan, in.Diagnostics); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := stampDiagVersionTx(ctx, tx, fileID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		affected := newAffectedRollups()
		anyCreated := false
		// setChanged tracks whether this scan changed the file's virtual-track set at all:
		// a track deleted, a whole-file item detached, or a track created or updated. It
		// has to cover the delete and detach paths too, or a cue edit that only removes a
		// track without shifting any survivor's window (dropping the LEADING track) would
		// report no change and silently skip watch-mode's downstream schedulers.
		setChanged := false

		// The virtual tracks currently backing this file, keyed by identity_key, plus
		// their stored metadata and window for the per-track change comparison.
		existing, err := virtualTracksForFile(ctx, tx, fileID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		desired := make(map[string]bool, len(in.Tracks))
		for _, vt := range in.Tracks {
			desired[vt.Item.IdentityKey] = true
		}

		// Remove virtual tracks the cue no longer declares. Their only file is this one,
		// so they are deleted outright (no promote/ensure-primary as a multi-file book
		// would need).
		for key, ex := range existing {
			if desired[key] {
				continue
			}
			if err := affected.collect(ctx, tx, ex.itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			opid, err := deleteItemCascade(ctx, tx, ex.itemID)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			setChanged = true
		}

		// Detach any NON-virtual item still backing this file (a plain track or a book
		// part catalogued before the .cue existed): the file is now a virtual-track
		// container, so those whole-file edges must go. This is the forward conversion
		// plain-track -> virtual-tracks; it is a no-op on every later scan.
		detached, err := detachWholeFileItems(ctx, tx, fileID, affected)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if detached {
			setChanged = true
		}

		for _, vt := range in.Tracks {
			// Overlay any user-locked fields onto the scanned virtual track before the
			// change comparison, so a forced rescan neither reverts a curated edit nor
			// counts a locked-field .cue change as a reason to rewrite the track. vt is a
			// loop-local copy, so mutating it is safe.
			if in.PreserveLocks {
				if err := preserveLockedTrackFieldsTx(ctx, tx, &vt.Track, &vt.Item); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			ex, had := existing[vt.Item.IdentityKey]
			metaChanged := !had || virtualTrackMetaDiffers(ex, vt)
			offsetChanged := !had || ex.startFrames != vt.StartFrames || ex.endFrames != vt.EndFrames
			if !metaChanged && !offsetChanged {
				continue // an unchanged virtual track (a forced rescan of a stable rip)
			}

			itemID, itemPID, created, _, err := upsertItem(ctx, tx, vt.Item, now, "")
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if created {
				anyCreated = true
			}
			setChanged = true

			if err := upsertTrack(ctx, tx, itemID, vt.Track); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Re-resolve entities and rebuild FTS from the (possibly cue-edited) metadata.
			// The cue carries per-track artist/album/genre, so unlike a plain track this
			// must run whenever the metadata changed, not only when the audio did.
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := resolveAndLinkEntities(ctx, tx, itemID, vt.Track, in.File.Path); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}

			// The rip's cover maps onto every virtual track (idempotent), so each browses
			// with its album art.
			if _, err := attachArtTxChanged(ctx, tx, itemID, in.CoverArt); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Origin provenance from the file's tags, recorded per track when absent.
			if _, err := insertAcquisitionIfAbsentTx(ctx, tx, itemID, in.Acquisition); err != nil {
				return err
			}

			if _, err := linkVirtualTrackFile(ctx, tx, itemID, fileID, vt.StartFrames, vt.EndFrames); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}

			if err := appendChange(ctx, tx, "item", itemPID, opFor(created)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		if res.FileCreated || res.ContentChanged || res.Relinked {
			if err := appendChange(ctx, tx, "file", filePID, opFor(res.FileCreated)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		res.ItemCreated = anyCreated
		// A set change with no create and no content change is the sidecar-only outcome
		// (a cue-edit that reshaped the tracks over unchanged audio), so the scanner's
		// counters and watch schedulers still see the work.
		res.SidecarsChanged = setChanged && !anyCreated && !res.ContentChanged
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// existingVirtualTrack is a virtual track already backing a file: its ids plus the
// stored metadata and window used to decide whether a rescan changed it.
type existingVirtualTrack struct {
	itemID      int64
	title       string
	artist      string
	album       string
	albumArtist string
	genre       string
	trackNo     int
	year        int
	startFrames int64
	endFrames   int64
}

// virtualTracksForFile returns the virtual tracks currently backing fileID, keyed by
// identity_key. A virtual track is identified by its primary item_file edge carrying
// a start offset (start_frames IS NOT NULL), which no whole-file track or book part
// edge ever has. It drains and closes its cursor before returning so the caller can
// write to the same transaction.
func virtualTracksForFile(ctx context.Context, tx *sql.Tx, fileID int64) (map[string]existingVirtualTrack, error) {
	rows, err := tx.QueryContext(ctx, `SELECT pi.identity_key, pi.id, pi.title,
			COALESCE(t.artist,''), COALESCE(t.album,''), COALESCE(t.album_artist,''),
			COALESCE(t.genre,''), COALESCE(t.track_no,0), COALESCE(t.year,0),
			itf.start_frames, itf.end_frames
		FROM item_file itf
		JOIN playable_item pi ON pi.id = itf.item_id
		LEFT JOIN track t ON t.item_id = pi.id
		WHERE itf.file_id = ? AND itf.role = 'primary' AND itf.start_frames IS NOT NULL`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]existingVirtualTrack)
	for rows.Next() {
		var key sql.NullString
		var ex existingVirtualTrack
		var startFrames, endFrames sql.NullInt64
		if err := rows.Scan(&key, &ex.itemID, &ex.title, &ex.artist, &ex.album,
			&ex.albumArtist, &ex.genre, &ex.trackNo, &ex.year, &startFrames, &endFrames); err != nil {
			return nil, err
		}
		ex.startFrames = startFrames.Int64
		ex.endFrames = endFrames.Int64
		if key.Valid {
			out[key.String] = ex
		}
	}
	return out, rows.Err()
}

// virtualTrackMetaDiffers reports whether a desired virtual track's display metadata
// differs from what is already stored, so an unchanged track (common on a forced
// rescan) does no entity work and emits no delta. Offsets are compared separately by
// the caller.
func virtualTrackMetaDiffers(ex existingVirtualTrack, vt model.VirtualTrack) bool {
	return ex.title != vt.Item.Title ||
		ex.artist != vt.Track.Artist ||
		ex.album != vt.Track.Album ||
		ex.albumArtist != vt.Track.AlbumArtist ||
		ex.genre != vt.Track.Genre ||
		ex.trackNo != vt.Track.TrackNo ||
		ex.year != vt.Track.Year
}

// detachWholeFileItems removes any item that backs fileID through a whole-file edge
// (start_frames IS NULL), such as a plain track or a book part catalogued before this
// file became a virtual-track container. It detaches those edges and cleans up: an item
// left with no files is deleted; a multi-file book that lost a part keeps a primary,
// refreshes its duration, and gets an update delta (symmetric with the attach side).
// The affected entities are collected so their rollups stay current. It reports
// whether it removed anything, so the caller can count the conversion as a change.
func detachWholeFileItems(ctx context.Context, tx *sql.Tx, fileID int64, affected *affectedRollups) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND start_frames IS NULL", fileID)
	if err != nil {
		return false, err
	}
	var prev []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		prev = append(prev, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if len(prev) == 0 {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE file_id = ? AND start_frames IS NULL", fileID); err != nil {
		return false, err
	}
	for _, oid := range prev {
		has, err := itemHasAnyFile(ctx, tx, oid)
		if err != nil {
			return false, err
		}
		if err := affected.collect(ctx, tx, oid); err != nil {
			return false, err
		}
		if has {
			if err := ensurePrimary(ctx, tx, oid); err != nil {
				return false, err
			}
			if err := refreshBookDuration(ctx, tx, oid); err != nil {
				return false, err
			}
			var pid string
			if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", oid).Scan(&pid); err != nil {
				return false, err
			}
			if err := appendChange(ctx, tx, "item", model.PID(pid), model.OpUpdate); err != nil {
				return false, err
			}
			continue
		}
		opid, err := deleteItemCascade(ctx, tx, oid)
		if err != nil {
			return false, err
		}
		if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
			return false, err
		}
	}
	return true, nil
}
