package sqlite

import (
	"context"
	"database/sql"
	"errors"

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
	cover, err := s.examineArt(ctx, in.CoverArt)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		now := nowNS()

		// Each track's key is resolved once, for the relink (a relinked rip may be one
		// backing these tracks, or none), the copy check, the overlay and the item write.
		accept := map[int64]bool{}
		keys := make(map[string]*resolvedItem, len(in.Tracks))
		for _, vt := range in.Tracks {
			r, err := resolveItemTx(ctx, tx, s.log, model.KindTrack, vt.Item.IdentityKey, bookAdoptKey{})
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			keys[vt.Item.IdentityKey] = r
			if r.id != 0 {
				accept[r.id] = true
			}
		}
		fileID, filePID, err := s.resolveScannedFile(ctx, tx, in.LibraryID, in.File, accept, now, res)
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

		// Remove virtual tracks the cue no longer declares. A copy of the rip whose own
		// sheet still declares one takes it over (handVirtualTrackOverTx); one only this
		// file backs is deleted outright.
		lostEdges, _, err := itemLostEdgesTx(ctx, tx, fileID, in.File.EssenceHash)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// The playlists and queues the dropped tracks leave are settled once after the loop.
		var batch entryHolders
		for key, ex := range existing {
			if desired[key] {
				continue
			}
			if err := affected.collect(ctx, tx, ex.itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			p, kept, err := handVirtualTrackOverTx(ctx, tx, ex.itemID, fileID, lostEdges[ex.itemID])
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if kept {
				if p != nil {
					res.Promoted = append(res.Promoted, *p)
				}
				setChanged = true
				continue
			}
			opid, err := deleteItemCascade(ctx, tx, ex.itemID, &batch)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := appendChange(ctx, tx, "item", opid, model.OpDelete); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			setChanged = true
		}
		if err := batch.settleTx(ctx, tx); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// Detach any NON-virtual item still backing this file (a plain track or a book
		// part catalogued before the .cue existed): the file is now a virtual-track
		// container, so those whole-file edges must go. This is the forward conversion
		// plain-track -> virtual-tracks; it is a no-op on every later scan.
		detached, promoted, err := detachWholeFileItems(ctx, tx, fileID, in.File.EssenceHash, affected)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		res.Promoted = append(res.Promoted, promoted...)
		if detached {
			setChanged = true
		}
		// A rip's file takes no write-back, so what an edit owed the whole-file track it
		// was before can never be paid.
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_diagnostic WHERE file_id = ? AND origin = ? AND code = ?`,
			fileID, string(model.OriginEdit), string(model.DiagTagWriteOwed)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// A copy of a rip backs its tracks through alternate edges; one the sheet no longer
		// declares goes.
		dropped, err := dropUndeclaredVirtualAlternatesTx(ctx, tx, fileID, desired)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		setChanged = setChanged || dropped

		var copyOf *standingPrimary
		copies, joined := 0, false
		for _, vt := range in.Tracks {
			ex, had := existing[vt.Item.IdentityKey]
			// A track whose primary is another rip file is a copy of that rip: the file backs
			// it as an alternate over the same window and rewrites nothing, and a missing
			// track is present again. A track reconciliation marked missing whose rip is gone
			// is taken over instead, the way a plain track's copy takes one over.
			if !had {
				primary, err := virtualCopyOfTx(ctx, tx, keys[vt.Item.IdentityKey], fileID)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if primary != nil && !pathExists(primary.path) {
					missing, err := itemMissingTx(ctx, tx, primary.itemID)
					if err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					if missing {
						primary = nil
					}
				}
				if primary != nil {
					changed, err := linkVirtualAlternateFile(ctx, tx, primary.itemID, fileID, vt.StartFrames, vt.EndFrames)
					if err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					revived, err := reviveItemTx(ctx, tx, primary.itemID, now)
					if err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					joined = joined || changed
					if changed || revived {
						if err := appendChange(ctx, tx, "item", primary.itemPID, model.OpUpdate); err != nil {
							return waxerr.Wrap(waxerr.CodeIO, op, err)
						}
					}
					copyOf = &primary.standingPrimary
					copies++
					continue
				}
			}

			// Overlay locked fields and the fills the sheet says nothing about onto the
			// scanned virtual track before the change comparison, so a forced rescan
			// neither reverts them nor counts them as a reason to rewrite the track. vt is
			// a loop-local copy, so mutating it is safe.
			prior, err := overlayStoredTrackTx(ctx, tx, s.log, 0, &vt.Track, &vt.Item, nil, in.PreserveLocks, keys[vt.Item.IdentityKey])
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Rewriting the track re-derives what the sheet now says, so the provenance of
			// each field it replaces goes the way a plain track's does. A track the file
			// already backs is judged first, over the fields a plain track's put compares,
			// so an unchanged one (a forced rescan of a stable rip) does no entity work and
			// emits no delta.
			var rederived []string
			var prov map[string]rederivable
			if had {
				rederived, prov, err = rederivedTrackTx(ctx, tx, ex.itemID, ex.title, vt.Item.Title, vt.Track, in.PreserveLocks, prior)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			metaChanged := !had || len(rederived) > 0
			offsetChanged := !had || ex.startFrames != vt.StartFrames || ex.endFrames != vt.EndFrames
			if !metaChanged && !offsetChanged {
				// The rip read again at its path brings a missing track back.
				revived, err := reviveItemTx(ctx, tx, ex.itemID, now)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if revived {
					if err := appendItemUpdateTx(ctx, tx, ex.itemID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
					setChanged = true
				}
				continue
			}

			itemID, itemPID, created, _, priorTitle, err := upsertItem(ctx, tx, s.log, vt.Item, vt.Track.Year, bookAdoptKey{}, now, "", keys[vt.Item.IdentityKey])
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if created {
				anyCreated = true
			}
			setChanged = true
			// An item the catalog holds that this file did not back yet (a copy of the rip)
			// has columns this put replaces too.
			if !had && !created {
				rederived, prov, err = rederivedTrackTx(ctx, tx, itemID, priorTitle, vt.Item.Title, vt.Track, in.PreserveLocks, prior)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			}
			if err := upsertTrack(ctx, tx, itemID, vt.Track); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Re-resolve entities and rebuild FTS from the (possibly cue-edited) metadata.
			// The cue carries per-track artist/album/genre, so unlike a plain track this
			// must run whenever the metadata changed, not only when the audio did.
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// The enrichment write-back never opens a rip's shared file, so no drift of its
			// own can hang on what this retires.
			if _, err := settleRederivedTx(ctx, tx, itemID, rederived, prov, in.PreserveLocks, affected); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := resolveAndLinkEntities(ctx, tx, s.log, itemID, vt.Track, in.File.Path, res.RelinkedFrom, fileID, affected); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := affected.collect(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}

			// The rip's cover maps onto every virtual track (idempotent), so each browses
			// with its album art. It respects a locked cover like the whole-file scan
			// paths do (catalog.go, book.go), so re-reading the .cue does not undo a
			// chosen cover on one of its tracks.
			if _, err := attachArtRespectingLockTx(ctx, tx, itemID, cover, in.PreserveLocks); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			// Origin provenance from the file's tags, recorded per track when absent.
			if _, err := insertAcquisitionIfAbsentTx(ctx, tx, itemID, in.Acquisition, in.PreserveLocks); err != nil {
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

		if copyOf != nil {
			if err := upsertFileDiagnosticTx(ctx, tx, fileID, model.OriginScan, "",
				copyDiagnostic(in.File.EssenceHash, copyOf), now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		res.AttachedAsCopy = copies > 0 && copies == len(in.Tracks)
		res.Joined = res.AttachedAsCopy && joined

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

// existingVirtualTrack is a virtual track already backing a file: its id and title plus
// the stored window used to decide whether a rescan moved it.
type existingVirtualTrack struct {
	itemID      int64
	title       string
	startFrames int64
	endFrames   int64
}

// virtualTracksForFile returns the virtual tracks currently backing fileID, keyed by
// identity_key. A virtual track is identified by its primary item_file edge carrying
// a start offset (start_frames IS NOT NULL), which no whole-file track or book part
// edge ever has. It drains and closes its cursor before returning so the caller can
// write to the same transaction.
func virtualTracksForFile(ctx context.Context, tx *sql.Tx, fileID int64) (map[string]existingVirtualTrack, error) {
	rows, err := tx.QueryContext(ctx, `SELECT pi.identity_key, pi.id, pi.title, itf.start_frames, itf.end_frames
		FROM item_file itf
		JOIN playable_item pi ON pi.id = itf.item_id
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
		if err := rows.Scan(&key, &ex.itemID, &ex.title, &startFrames, &endFrames); err != nil {
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

// VirtualTracksForPath returns the virtual tracks the file at path backs, in start
// order and with their stored identity keys, or none when it backs no rip. A copy of a
// rip backs its tracks as an alternate, over windows of its own, and they count too. A
// scan that cannot read the file's sheet re-puts these tracks instead of collapsing the
// rip to one whole-file track.
func (s *Store) VirtualTracksForPath(ctx context.Context, path []byte) ([]model.VirtualTrack, error) {
	const op = "store.VirtualTracksForPath"
	rows, err := s.read.QueryContext(ctx, `SELECT COALESCE(pi.identity_key,''), pi.title, COALESCE(t.artist,''), COALESCE(t.album,''),
			COALESCE(t.album_artist,''), COALESCE(t.genre,''), COALESCE(t.track_no,0), COALESCE(t.year,0),
			itf.start_frames, COALESCE(itf.end_frames,0)
		FROM file f
		JOIN item_file itf ON itf.file_id = f.id AND itf.role IN ('primary', 'alternate') AND itf.start_frames IS NOT NULL
		JOIN playable_item pi ON pi.id = itf.item_id
		LEFT JOIN track t ON t.item_id = pi.id
		WHERE f.path = ?
		ORDER BY itf.start_frames`, path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.VirtualTrack
	seen := map[string]bool{}
	for rows.Next() {
		var vt model.VirtualTrack
		if err := rows.Scan(&vt.Item.IdentityKey, &vt.Item.Title, &vt.Track.Artist, &vt.Track.Album, &vt.Track.AlbumArtist,
			&vt.Track.Genre, &vt.Track.TrackNo, &vt.Track.Year, &vt.StartFrames, &vt.EndFrames); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !seen[vt.Item.IdentityKey] {
			seen[vt.Item.IdentityKey] = true
			out = append(out, vt)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// RipTracks returns the tracks a cue sheet carves out of a file, in start order, as the
// items playing a window of it; a file every item plays whole returns none.
func (s *Store) RipTracks(ctx context.Context, filePID model.PID) ([]model.ItemRef, error) {
	const op = "store.RipTracks"
	rows, err := s.read.QueryContext(ctx, `SELECT pi.pid, pi.title, pi.kind
		FROM file f JOIN item_file itf ON itf.file_id = f.id AND itf.start_frames IS NOT NULL
		JOIN playable_item pi ON pi.id = itf.item_id
		WHERE f.pid = ? ORDER BY itf.start_frames, pi.id`, string(filePID))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.ItemRef
	for rows.Next() {
		var r model.ItemRef
		if err := rows.Scan(&r.PID, &r.Title, &r.Kind); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// handVirtualTrackOverTx gives a track its rip no longer declares to another file that
// backs it, a copy of the rip whose sheet still does: this file's edge goes and an
// alternate takes its place (promoteLostTx, then ensurePrimary). It reports false, with
// nothing changed, when no other file backs the track.
func handVirtualTrackOverTx(ctx context.Context, tx *sql.Tx, itemID, fileID int64, lost lostEdge) (*model.PromotedFile, bool, error) {
	var others int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM item_file WHERE item_id = ? AND file_id <> ?",
		itemID, fileID).Scan(&others); err != nil || others == 0 {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE item_id = ? AND file_id = ?", itemID, fileID); err != nil {
		return nil, false, err
	}
	p, err := promoteLostTx(ctx, tx, itemID, false, lost)
	if err == nil && p == nil {
		p, err = ensurePrimary(ctx, tx, itemID)
	}
	if err != nil {
		return nil, false, err
	}
	marked, err := markUnreachableMissingTx(ctx, tx, itemID, nil)
	if err != nil {
		return nil, false, err
	}
	if !marked {
		if err := appendItemUpdateTx(ctx, tx, itemID); err != nil {
			return nil, false, err
		}
	}
	return p, true, nil
}

// virtualCopy is the standing primary of a virtual track an arriving rip file copies.
type virtualCopy struct {
	standingPrimary
	itemID  int64
	itemPID model.PID
}

// virtualCopyOfTx returns the virtual track a resolved key names when its primary edge is
// on another file, or nil. That file may be missing mid-walk (moved to a folder not
// reached yet), so only the scan's reconciliation, or the track being missing already,
// hands a gone rip's tracks to a copy.
func virtualCopyOfTx(ctx context.Context, tx *sql.Tx, key *resolvedItem, fileID int64) (*virtualCopy, error) {
	if key == nil || key.id == 0 {
		return nil, nil
	}
	id := key.id
	primary, err := primaryFileTx(ctx, tx, id)
	if err != nil || primary == nil || primary.fileID == fileID {
		return nil, err
	}
	vc := &virtualCopy{standingPrimary: *primary, itemID: id}
	if err := tx.QueryRowContext(ctx, "SELECT pid FROM playable_item WHERE id = ?", id).Scan(&vc.itemPID); err != nil {
		return nil, err
	}
	return vc, nil
}

// linkVirtualAlternateFile attaches a copy of a rip to one of its virtual tracks as an
// alternate edge over the track's window, leaving the file's other edges alone. It
// reports whether the edge changed.
func linkVirtualAlternateFile(ctx context.Context, tx *sql.Tx, itemID, fileID, startFrames, endFrames int64) (bool, error) {
	var curStart, curEnd sql.NullInt64
	err := tx.QueryRowContext(ctx, "SELECT start_frames, end_frames FROM item_file WHERE item_id = ? AND file_id = ? AND role = 'alternate'",
		itemID, fileID).Scan(&curStart, &curEnd)
	switch {
	case err == nil:
		if curStart.Valid && curStart.Int64 == startFrames && curEnd.Valid == (endFrames != 0) && curEnd.Int64 == endFrames {
			return false, nil
		}
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE item_id = ? AND file_id = ?", itemID, fileID); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO item_file(item_id, file_id, role, position, start_frames, end_frames)
		SELECT ?, ?, 'alternate', COALESCE(MAX(position), 0) + 1, ?, ? FROM item_file WHERE item_id = ?`,
		itemID, fileID, startFrames, nullInt64(endFrames), itemID)
	return err == nil, err
}

// dropUndeclaredVirtualAlternatesTx drops the alternate edges a rip copy holds on tracks
// its sheet no longer declares, each track emitting an update. It reports whether it
// dropped any.
func dropUndeclaredVirtualAlternatesTx(ctx context.Context, tx *sql.Tx, fileID int64, desired map[string]bool) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT pi.id, pi.pid, COALESCE(pi.identity_key, '')
		FROM item_file itf JOIN playable_item pi ON pi.id = itf.item_id
		WHERE itf.file_id = ? AND itf.role = 'alternate' AND itf.start_frames IS NOT NULL`, fileID)
	if err != nil {
		return false, err
	}
	type held struct {
		id  int64
		pid model.PID
	}
	var drop []held
	for rows.Next() {
		var h held
		var key string
		if err := rows.Scan(&h.id, &h.pid, &key); err != nil {
			rows.Close()
			return false, err
		}
		if !desired[key] {
			drop = append(drop, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, h := range drop {
		if _, err := tx.ExecContext(ctx, "DELETE FROM item_file WHERE item_id = ? AND file_id = ? AND role = 'alternate'", h.id, fileID); err != nil {
			return false, err
		}
		if err := appendChange(ctx, tx, "item", h.pid, model.OpUpdate); err != nil {
			return false, err
		}
	}
	return len(drop) > 0, nil
}

// detachWholeFileItems removes any item that backs fileID through a whole-file edge
// (start_frames IS NULL), such as a plain track or a book part catalogued before this
// file became a virtual-track container. It detaches those edges and cleans up: an item
// left with no files is deleted; a multi-file book that lost a part keeps a primary,
// refreshes its duration, and gets an update delta (symmetric with the attach side).
// The affected entities are collected so their rollups stay current. It reports
// whether it removed anything, so the caller can count the conversion as a change, and
// the files promoted in place of a lost primary.
func detachWholeFileItems(ctx context.Context, tx *sql.Tx, fileID int64, essence string, affected *affectedRollups) (bool, []model.PromotedFile, error) {
	prev, err := queryInt64sTx(ctx, tx,
		"SELECT DISTINCT item_id FROM item_file WHERE file_id = ? AND start_frames IS NULL", fileID)
	if err != nil || len(prev) == 0 {
		return false, nil, err
	}
	dep, err := departingTx(ctx, tx, fileID, essence, 0, false)
	if err != nil {
		return false, nil, err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM item_file WHERE file_id = ? AND start_frames IS NULL", fileID); err != nil {
		return false, nil, err
	}
	promoted, _, err := reconcileOrphansTx(ctx, tx, prev, dep, affected)
	return true, promoted, err
}
