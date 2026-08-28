package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file holds the per-member detach. A track's MusicBrainz linkage is not stored on
// its own row: it is derived from the mbid: prefix on the album and release group it sits
// under, which adoptEntityKeyMBIDs carries into every re-resolve so an edit can never
// fork a member off an identified chain. That carryover leaves no way to take one member
// out, and clearing the entity's id (the whole-entity escape hatch in entityedit.go)
// takes every member with it. Detach is the item-level gesture: it re-resolves one member
// with both ids forced empty, so the member lands on the chain a scan of its tags and
// folder alone would compute.
//
// The reverse direction ghosts by design. When a retag re-adopts a detached member, the
// heuristic album it leaves drains, and the scan re-key reconciliation deliberately does
// not fire on that either, since the destination key is an mbid one. What is left is a
// bare album row with no members, which the standing orphan GC clears.

// DetachItemFromMBIDAlbum moves one track off an album chain that carries a MusicBrainz
// id and onto the heuristic album chain its own tags and folder imply. Either shape
// qualifies: an album keyed on its own release id, and an album whose heuristic key
// embeds an mbid-keyed group (a file tagged with a release-group id and no release id).
// The album it leaves keeps its identity, curation, and remaining members; the member
// joins a heuristic twin when one already exists on the computed key, and otherwise gets
// a fresh, bare album row. A heuristic release group above the album is reused, since
// only the release id is being disowned; an mbid-keyed one is left behind the same way
// the album is.
//
// The linkage is derived rather than curated, so nothing here records a lock: the file's
// tags still name the release, and the durable fix is to strip them, which the facade's
// write-back does. Without that, any scan that re-resolves this item re-adopts it (the
// caller's warning spells out which scans those are).
//
// The refusals are CodeInvalid, apart from an item with no track row at all, which is
// CodeNotFound. A non-track item, or a track on no album, has no album chain to leave. A
// member whose album chain carries no MusicBrainz id already resolves from its own tags.
// And the last member is refused because detaching it would leave the identified album
// standing with its pid, art, and locks and no members at all, which is the ghost the
// scan reconciliation exists to avoid; that case belongs to the whole-entity escape
// hatch, and the message names the one that holds the id.
//
// The scan re-key reconciliation cannot fire on this write. An album keyed on its own
// release id fails its heuristic-pair guard outright; a group-keyed one passes that but
// fails the shared-release-group guard, since the freed member lands under a heuristic
// group and the album it left stays under the identified one.
func (s *Store) DetachItemFromMBIDAlbum(ctx context.Context, itemPID model.PID) (*model.DetachReport, error) {
	const op = "store.Detach"
	rep := &model.DetachReport{ItemPID: itemPID}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if kind != string(model.KindTrack) {
			return waxerr.New(waxerr.CodeInvalid, op, "detach applies to a track, not a "+kind+" item")
		}

		var albumID sql.NullInt64
		var albumPID, albumKey, rgPID, albumMBID sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT al.id, al.pid, al.match_key, rg.pid, al.mbid FROM track t
			LEFT JOIN album al ON al.id = t.album_id
			LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE t.item_id = ?`, itemID).
			Scan(&albumID, &albumPID, &albumKey, &rgPID, &albumMBID)
		if errors.Is(err, sql.ErrNoRows) {
			return waxerr.New(waxerr.CodeNotFound, op, "item has no track row")
		}
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !albumID.Valid {
			return waxerr.New(waxerr.CodeInvalid, op, "this item sits on no album, so there is nothing to detach it from")
		}
		// Three shapes carry a MusicBrainz identifier down to the member: the album's own
		// release id, a heuristic album key whose group segment is the group's id (the
		// shape a file tagged with a release-group id alone produces), and a heuristically
		// keyed album that holds a release id in its column, which is what a member
		// adopted at resolve time is pinned to. All three are re-pinned on every
		// re-resolve, so all three are detachable; without the third the documented escape
		// hatch would be missing for exactly the members adoption creates.
		albumKeyed := strings.HasPrefix(albumKey.String, mbidKeyPrefix)
		groupKeyed := strings.HasPrefix(albumKey.String, heuristicAlbumKeyPrefix+mbidKeyPrefix)
		columnKeyed := albumMBID.String != ""
		if !albumKeyed && !groupKeyed && !columnKeyed {
			return waxerr.New(waxerr.CodeInvalid, op,
				"this item's album chain carries no MusicBrainz id, so its members already resolve from their own tags")
		}
		var members int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM track WHERE album_id = ?", albumID.Int64).Scan(&members); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if members < 2 {
			// The hatch is whichever entity holds the id this member is pinned to.
			what, hatch := "album's release id", "album "+albumPID.String
			if !albumKeyed {
				what, hatch = "release group's id", "release_group "+rgPID.String
			}
			return waxerr.New(waxerr.CodeInvalid, op,
				"this is the album's only member, and detaching it would leave the album empty: "+
					"clear the whole "+what+" instead, with `waxbin entity edit "+hatch+" --set mbid=`")
		}

		tr, _, filePath, err := loadTrackForEditTx(ctx, tx, itemID)
		if err != nil {
			return err
		}
		// loadTrackForEditTx filled these from the keys of the rows the member sits on.
		// Emptying both is the whole detach: everything below is the ordinary re-resolve
		// an item edit runs.
		tr.MBReleaseID, tr.MBReleaseGroupID = "", ""

		affected := newAffectedRollups()
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := resolveAndLinkEntities(ctx, tx, s.log, itemID, tr, filePath, "", 0, affected); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		var newAlbumPID, newRGPID sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT al.pid, rg.pid FROM track t
			LEFT JOIN album al ON al.id = t.album_id
			LEFT JOIN release_group rg ON rg.id = al.release_group_id
			WHERE t.item_id = ?`, itemID).Scan(&newAlbumPID, &newRGPID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		rep.OldAlbumPID = model.PID(albumPID.String)
		rep.NewAlbumPID = model.PID(newAlbumPID.String)
		rep.NewReleaseGroupPID = model.PID(newRGPID.String)

		// The member's own columns are unchanged, but which album it belongs to is not, so
		// a delta consumer has to refetch it. A fresh album row emitted its own create.
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}
