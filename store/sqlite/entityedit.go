package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file implements entity-level curation: editing a field on a shared entity (an
// artist, release group, or album) rather than on one item. It mirrors the item field
// edit (editfield.go) but writes the entity's own column and records provenance in the
// entity_curation table (the entity-scoped analogue of field_provenance). A lock there
// is what protects the value from an enrichment overwrite; enrich.go consults it before
// the one unconditional entity write (release_group.type). A merge re-points the rows
// (merge.go).

// entityTableFor maps a curatable entity type to its table name. Only the three
// identifier/sort-bearing entities are editable; genre and series are not, even though
// both merge.
func entityTableFor(et model.MergeEntity) (string, bool) {
	switch et {
	case model.MergeArtist:
		return "artist", true
	case model.MergeReleaseGroup:
		return "release_group", true
	case model.MergeAlbum:
		return "album", true
	default:
		return "", false
	}
}

// entityDisplayColumn is the display-name column an entity's sort key derives from:
// the artist name, else the release-group/album title. It is what a cleared sort
// override regenerates the sort key from.
func entityDisplayColumn(et model.MergeEntity) string {
	if et == model.MergeArtist {
		return "name"
	}
	return "title"
}

// entityColumnForField maps an entity edit field to the column it writes. "sort" is
// the one indirection (it drives the generated sort_key); every other field names its
// column directly. The field set is validated against a fixed whitelist first, so the
// column name is never attacker-controlled.
func entityColumnForField(field string) string {
	if field == "sort" {
		return "sort_key"
	}
	return field
}

// EditEntityFields applies curation edits to one shared entity (an artist, release
// group, or album) in a single transaction. It writes the entity's own column
// (a sort-name override drives the generated sort_key; identifiers write their own
// columns) and records an entity_curation row per field with the caller's attribution.
// The lock follows the caller's instruction: LockOn protects the value from an
// enrichment overwrite, LockOff releases it, and LockUnchanged leaves whatever was
// there. One item change delta is emitted for every item the entity backs, plus an
// entity delta, so a delta consumer re-resolves the affected rows.
//
// A field that does not apply to the entity type is CodeInvalid; a genre (or any other
// non-curatable entity type) is CodeUnsupported. A locked field is refused with
// CodeLocked unless force is set. On-disk tag write-back of these values fans out
// across the entity's member files and is sequenced separately; this edit is
// DB-only.
//
// Clearing an album's or a release group's mbid also re-keys the entity chain to the
// heuristic form a scan of its members computes. That is what makes the clear a real
// undo: members resolve on the match key, not on the column, so leaving the key would
// keep every member pinned to the identity just disowned. Setting an mbid deliberately
// stays column-only, since re-keying onto mbid: would fork any member whose own file does
// not carry the id on its next scan; enrichment fills the column the same way.
//
// The undo is only as durable as the tags on disk. The members still carry the release id,
// so the next scan that re-resolves them, meaning a retag, move, or content change rather
// than every scan, computes the mbid key again and forks a fresh identified chain while the
// re-keyed row drains. Stripping those tags is the durable fix and this edit is DB-only:
// the strip belongs to the facade, which fans it across the member files when EditEntity is
// asked for write-back.
//
// A release-group clear also settles each dependent album onto the chain its own members
// compute, which sends a differently-titled edition to a group of its own. The report
// names those albums, since their members are no longer under the edited group and a
// caller fanning over its members would miss them.
//
// When that re-key lands on a key a heuristic twin already owns, the entity merges into
// the twin and this edit's own entity ceases to exist. A clear that merges therefore has
// to be the whole edit: any other field in the same call was written to the row the merge
// deletes, and a merge carries curation rows but no column values, so the combination is
// refused with CodeConflict naming the survivor rather than reported as a success that
// wrote half of what it was asked for.
func (s *Store) EditEntityFields(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, attr model.Attribution, lock model.LockChange, force bool) (*model.EntityEditReport, error) {
	const op = "store.EditEntityFields"
	table, ok := entityTableFor(entityType)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "entity editing is not supported for a "+string(entityType)+" entity")
	}
	if len(edits) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no fields to edit")
	}
	attr, err := checkFieldAttribution(attr, op)
	if err != nil {
		return nil, err
	}
	if err := checkLockChange(lock, op); err != nil {
		return nil, err
	}
	// Validate every field name and value up front so a bad edit is rejected before any
	// write. Iterate in a stable order so the edit is deterministic.
	fields := make([]string, 0, len(edits))
	for f := range edits {
		if !model.IsEntityEditField(entityType, f) {
			// "art" gets its own branch rather than the generic message: it is a real
			// curatable field whose value is bytes, so it has a home (the art command
			// group) to point at. Appending that pointer to the catch-all would advertise
			// `art unlock` on every field typo instead.
			if f == "art" {
				return nil, waxerr.New(waxerr.CodeInvalid, op,
					"art is bytes, not a scalar field: use `waxbin art set --type "+string(entityType)+
						"` to set or clear the cover, and `waxbin art unlock --type "+string(entityType)+
						"` to release its lock")
			}
			return nil, waxerr.New(waxerr.CodeInvalid, op, "field "+f+" is not editable on a "+string(entityType)+" entity")
		}
		fields = append(fields, f)
	}
	sort.Strings(fields)
	norm := make(map[string]string, len(edits))
	for _, f := range fields {
		v := strings.TrimSpace(edits[f])
		switch f {
		case "mbid":
			if err := validateMBIDField(v, op); err != nil {
				return nil, err
			}
		case "type":
			if v != "" && !model.ValidReleaseGroupType(v) {
				return nil, waxerr.New(waxerr.CodeInvalid, op, "invalid release-group type: "+v)
			}
		case "barcode":
			// Normalized before the norm map is built, like the item-edit identifier
			// fields, so the stored column, the curation row, and the fanned tag all
			// carry the canonical digits.
			nv, ok := model.NormalizeBarcode(v)
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, op, "invalid barcode value: "+v)
			}
			v = nv
		case "country":
			// Stricter than the column, which holds whatever a tag said. The message names
			// the accepted shape because `entity info` shows values this refuses. It does
			// not promise the code exists: validating that would need the whole ISO table,
			// and a code no release carries decides nothing anyway (see countryAffirmed).
			nv, ok := model.NormalizeCountry(v)
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, op,
					"invalid country value: "+v+" (want one two-letter code, e.g. GB, an alpha-3 or UK alias, or XW/XE)")
			}
			v = nv
		}
		norm[f] = v
	}

	rep := &model.EntityEditReport{}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		entityID, err := entityIDByPID(ctx, tx, table, entityPID, op)
		if err != nil {
			return err
		}
		if !force {
			for _, f := range fields {
				locked, err := entityFieldLockedTx(ctx, tx, string(entityType), entityID, f)
				if err != nil {
					return err
				}
				if locked {
					return waxerr.New(waxerr.CodeLocked, op, "entity field is locked (use force to override): "+f)
				}
			}
		}
		// Reject an MBID already held by another entity of this type. Enrichment treats an
		// entity MBID as unique (relation resolution reads a single artist by mbid;
		// setReleaseGroupMBIDTx refuses to set a duplicate), so a user edit must not
		// deliberately create the collision that would make those lookups ambiguous.
		if v := norm["mbid"]; v != "" {
			var other int64
			switch err := tx.QueryRowContext(ctx,
				"SELECT id FROM "+table+" WHERE mbid = ? AND id <> ?", v, entityID).Scan(&other); {
			case err == nil:
				return waxerr.New(waxerr.CodeConflict, op,
					"mbid "+v+" is already used by another "+string(entityType)+"; merge them instead")
			case !errors.Is(err, sql.ErrNoRows):
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		now := nowNS()
		var newEvidence bool
		for _, f := range fields {
			if err := applyEntityFieldTx(ctx, tx, entityType, table, entityID, f, norm[f]); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := upsertEntityCurationTx(ctx, tx, string(entityType), entityID, f, attr, norm[f], lock, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if entityType == model.MergeAlbum && norm[f] != "" && slices.Contains(albumMatchEvidence, f) {
				newEvidence = true
			}
		}
		// Re-queues an album a past pass failed to match, as the scan top-up does; a
		// matched marker is left alone. See fillAlbumIdentifiersTx.
		if newEvidence {
			if err := clearUnmatchedAlbumMarkerTx(ctx, tx, entityID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		// Clearing the release id is how a wrong match is undone, which the weak edition
		// tier's own marker exists to make possible. Three things go with an album's id, and
		// the same three with a release group's, which holds art of its own at its own rung.
		// The marker must, matched or not, because the queue skips a marked entity and
		// leaving it would mean the undo silently prevented the entity from ever being
		// re-decided; a group carries the aux-art backfill's separate marker as well. The
		// art goes only when a matched marker was there to have written it, since it came
		// from the identity now being disowned; the member tracks' embedded covers are
		// untouched, so the entity falls back to what it showed before, and a hand-set
		// auxiliary cover on a group stays. And the match key goes back to its heuristic
		// form, so the members follow the row rather than staying pinned to the id.
		var survivor model.PID
		if v, edited := norm["mbid"]; edited && v == "" {
			switch entityType {
			case model.MergeAlbum:
				matched, err := albumMarkerMatchedTx(ctx, tx, entityID)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if err := clearAlbumMarkerTx(ctx, tx, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if matched {
					if err := clearAlbumArtTx(ctx, tx, entityID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
				}
				if survivor, err = rekeyAlbumHeuristicTx(ctx, tx, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
			case model.MergeReleaseGroup:
				matched, err := entityMarkerMatchedTx(ctx, tx, model.EnrichReleaseGroupType, entityID)
				if err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				if matched {
					if err := clearReleaseGroupEnrichmentArtTx(ctx, tx, entityID); err != nil {
						return waxerr.Wrap(waxerr.CodeIO, op, err)
					}
				}
				if err := clearEntityMarkerTx(ctx, tx, model.EnrichReleaseGroupType, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				// The aux-art backfill marker gates the same queue from under its own
				// entity type, so it goes with the rest. Before the re-key, so the
				// in-place and guard-skip paths are covered too; a merge deletes it
				// again for nothing.
				if err := deleteAuxArtMarkerTx(ctx, tx, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				var moved []model.PID
				if survivor, moved, err = rekeyReleaseGroupHeuristicTx(ctx, tx, entityID); err != nil {
					return waxerr.Wrap(waxerr.CodeIO, op, err)
				}
				rep.MovedAlbums = moved
			}
		}
		// The sibling fields of a merging clear were written to a row that no longer
		// exists, so refuse the call rather than commit a partial edit. Returning here
		// rolls the merge back with everything else, which is why the check can run after
		// it: the whole edit is one transaction. The message names the survivor because
		// that is the entity the other fields have to be re-applied to.
		if survivor != "" && len(fields) > 1 {
			return waxerr.New(waxerr.CodeConflict, op,
				"clearing this mbid merges the "+string(entityType)+" into "+string(survivor)+
					", which the other edited fields would not survive: clear the mbid on its own, then edit "+
					string(survivor))
		}
		// A merge already wrote the per-item updates, the survivor's update, and this
		// entity's delete, and the row this edit named is gone. The caller is told which
		// row absorbed it, since the pid it asked about no longer names anything.
		if survivor != "" {
			rep.MergedInto = survivor
			return nil
		}

		// Fan a delta out to every item the entity backs, so a delta consumer re-resolves
		// the changed identifier/sort, then an entity delta. Read here rather than before
		// the clear: a merge already repointed these items and wrote their deltas, so the
		// only path that reaches this read is the one where the row is still standing.
		members, err := affectedItemPIDs(ctx, tx, entityType, entityID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, pid := range members {
			if err := appendChange(ctx, tx, "item", pid, model.OpUpdate); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, table, entityPID, model.OpUpdate)
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// mbidKeyPrefix marks an entity key anchored on a MusicBrainz id rather than derived from
// tags and folder. See identity.AlbumKey.
const mbidKeyPrefix = "mbid:"

// rekeyAlbumHeuristicTx rewrites an album whose key is a release id onto the heuristic key
// a scan of its members computes. It returns the surviving album's pid when the re-key
// merged this album away, and an empty pid when the row is still there.
//
// Every guard failure leaves the key standing and returns nil, since the column is cleared
// either way and a wrong key is worse than an outdated one. A childless album has nothing
// to derive from and is GC fodder; a chain whose only grouping evidence was the id itself
// derives no key at all.
//
// The folder in the derived key is the representative member's, so an album whose members
// span folders lands on that one folder's key and the others re-resolve into rows of their
// own on the next scan. That is the heuristic shape those files would have had all along
// without the release id holding them together.
func rekeyAlbumHeuristicTx(ctx context.Context, tx *sql.Tx, albumID int64) (model.PID, error) {
	var curKey, pid string
	if err := tx.QueryRowContext(ctx,
		"SELECT match_key, pid FROM album WHERE id=?", albumID).Scan(&curKey, &pid); err != nil {
		return "", err
	}
	if !strings.HasPrefix(curKey, mbidKeyPrefix) {
		return "", nil
	}
	tr, filePath, _, err := representativeMemberTx(ctx, tx,
		`SELECT t.item_id FROM track t
			JOIN item_file f ON f.item_id = t.item_id AND f.role = 'primary'
			WHERE t.album_id=? ORDER BY t.item_id LIMIT 1`, albumID)
	if err != nil || tr == nil {
		return "", err
	}
	// Only the release id goes. The group's is carried, so an album under an mbid-keyed
	// release group re-keys to al:mbid:<group>… exactly as a scan of these files would.
	tr.MBReleaseID = ""
	_, _, newKey := albumChainKeys(*tr, filePath)
	if newKey == "" || newKey == curKey || albumKeyFolder(newKey) == "" {
		return "", nil
	}
	return rewriteOrMergeEntityKeyTx(ctx, tx, model.MergeAlbum, albumID, model.PID(pid), newKey)
}

// rekeyReleaseGroupHeuristicTx rewrites a release group whose key is a MusicBrainz id onto
// the heuristic key a scan of its members computes, and settles every dependent album onto
// the chain its own members compute. Both halves are needed: a heuristic album key embeds
// its group's key, so moving the group alone would leave every album of the group on a key
// no scan ever computes. A dependent whose members name a different group than the
// representative's is re-parented onto that one, since the album title is part of a
// heuristic group key and two titles cannot share it. It returns the surviving group's pid
// when the re-key merged this group away, and an empty pid when the row is still there,
// along with the pids of the albums it moved to a group of their own. Those albums'
// members are no longer under this group, so a caller fanning over its members has to be
// told where they went.
func rekeyReleaseGroupHeuristicTx(ctx context.Context, tx *sql.Tx, rgID int64) (model.PID, []model.PID, error) {
	var curKey, pid string
	if err := tx.QueryRowContext(ctx,
		"SELECT match_key, pid FROM release_group WHERE id=?", rgID).Scan(&curKey, &pid); err != nil {
		return "", nil, err
	}
	if !strings.HasPrefix(curKey, mbidKeyPrefix) {
		return "", nil, nil
	}
	tr, filePath, _, err := representativeMemberTx(ctx, tx,
		`SELECT t.item_id FROM track t JOIN album al ON al.id = t.album_id
			JOIN item_file f ON f.item_id = t.item_id AND f.role = 'primary'
			WHERE al.release_group_id=? ORDER BY t.item_id LIMIT 1`, rgID)
	if err != nil || tr == nil {
		return "", nil, err
	}
	tr.MBReleaseGroupID = ""
	_, newKey, _ := albumChainKeys(*tr, filePath)
	if newKey == "" || newKey == curKey {
		return "", nil, nil
	}
	// Collected before the group moves: a merge repoints the albums onto the incumbent,
	// after which they are no longer this group's to find.
	deps, err := dependentAlbumsTx(ctx, tx, rgID, curKey)
	if err != nil {
		return "", nil, err
	}
	survivor, err := rewriteOrMergeEntityKeyTx(ctx, tx, model.MergeReleaseGroup, rgID, model.PID(pid), newKey)
	if err != nil {
		return "", nil, err
	}
	affected := newAffectedRollups()
	var moved []model.PID
	// A dependent album folding into an incumbent is not this group disappearing, so its
	// own merge is never the caller's answer.
	for _, d := range deps {
		away, err := settleDependentAlbumTx(ctx, tx, d, newKey, affected)
		if err != nil {
			return "", nil, err
		}
		if away != "" {
			moved = append(moved, away)
		}
	}
	// Non-empty only when an album changed groups, which moves a track count and a
	// release-group count; a merge maintained its own on the way past.
	if !affected.empty() {
		if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
			return "", nil, err
		}
	}
	return survivor, moved, nil
}

// settleDependentAlbumTx moves one dependent album onto the chain its own members
// compute. An album naming the same group as the representative stays under the re-keyed
// row and only its key moves; one naming a different group (a differently-titled edition
// that shared the mbid) has that group found-or-created and is re-parented, since the
// album title lives inside the group key. A key an album twin already owns merges, the
// same as at every other re-key.
//
// It returns the pid the members of a re-parented album ended up under, which is that
// album or the twin it merged into, and an empty pid for an album that stayed. Those
// members are the ones the caller's own fan-out no longer reaches.
func settleDependentAlbumTx(ctx context.Context, tx *sql.Tx, d dependentAlbum, groupKey string, affected *affectedRollups) (model.PID, error) {
	reparented := d.rgKey != groupKey
	if reparented {
		var oldParent sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			"SELECT release_group_id FROM album WHERE id=?", d.id).Scan(&oldParent); err != nil {
			return "", err
		}
		newRGID, err := resolveReleaseGroup(ctx, tx, d.rgKey, d.rgTitle, d.albumArtistID, "", affected)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE album SET release_group_id=? WHERE id=?", newRGID, d.id); err != nil {
			return "", err
		}
		if oldParent.Valid {
			if err := addRGChainToAffected(ctx, tx, oldParent.Int64, affected); err != nil {
				return "", err
			}
		}
		if err := addRGChainToAffected(ctx, tx, newRGID, affected); err != nil {
			return "", err
		}
		// An item read serves its release group's pid, so these members changed even
		// though no column of theirs did. The caller's own fan-out runs off the entity
		// being edited and no longer reaches them.
		items, err := affectedItemPIDs(ctx, tx, model.MergeAlbum, d.id)
		if err != nil {
			return "", err
		}
		for _, pid := range items {
			if err := appendChange(ctx, tx, "item", pid, model.OpUpdate); err != nil {
				return "", err
			}
		}
	}
	merged, err := rewriteOrMergeEntityKeyTx(ctx, tx, model.MergeAlbum, d.id, model.PID(d.pid), d.newKey)
	if err != nil || merged != "" {
		if err == nil && reparented {
			// A merge wrote its own deltas and rollups, and took the members with it, so
			// the incumbent is the row they hang off now.
			return merged, nil
		}
		return "", err
	}
	// The row is still there on a new key, and possibly under a new group, which entity
	// info serves as the album's group pid, so a delta consumer has to refetch it.
	if err := appendChange(ctx, tx, "album", model.PID(d.pid), model.OpUpdate); err != nil {
		return "", err
	}
	if reparented {
		return model.PID(d.pid), nil
	}
	return "", nil
}

// representativeMemberTx loads the member a chain re-key derives its heuristic keys from,
// which the callers' queries pick as the lowest item id under the entity that still has a
// primary file. The folder anchoring an album key comes from that path, so a member
// without one derives a key no scan of the restored files would compute, and an archived
// member is not the whole entity's answer while its siblings are on disk. A nil track
// means no member qualified, which leaves the key standing. The item id comes back so a
// caller can read further columns off the same row rather than picking a second member.
func representativeMemberTx(ctx context.Context, tx *sql.Tx, q string, args ...any) (*model.Track, []byte, int64, error) {
	var itemID int64
	err := tx.QueryRowContext(ctx, q, args...).Scan(&itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, 0, nil
	}
	if err != nil {
		return nil, nil, 0, err
	}
	tr, _, filePath, err := loadTrackForEditTx(ctx, tx, itemID)
	if err != nil {
		return nil, nil, 0, err
	}
	return &tr, filePath, itemID, nil
}

// dependentAlbum is one album whose key embeds a release group's, with the chain a scan
// of its own members computes once the group id is gone.
type dependentAlbum struct {
	id            int64
	pid           string
	rgKey         string // the release group its own members name
	rgTitle       string // that group's display title, for a row that has to be minted
	albumArtistID int64  // the anchor entity its members already resolved to
	newKey        string
}

// dependentAlbumsTx collects the albums under a release group whose own key embeds the
// group's, each carrying the chain its own members compute with both MusicBrainz ids
// forced empty. An mbid-keyed album carries no group segment and is left alone, which is
// right: its identity is its own release id, not the group's. So is an album with no
// member to derive from, the same skip the group itself takes.
//
// Deriving per album rather than splicing the group's new key into every one of them
// matters because the album title lives inside the group segment: a differently-titled
// edition sharing the mbid group would land on the representative's title, a key nothing
// ever recomputes, and would ghost on the next tag-driven re-resolve.
func dependentAlbumsTx(ctx context.Context, tx *sql.Tx, rgID int64, oldRGKey string) ([]dependentAlbum, error) {
	type candidate struct {
		id  int64
		pid string
	}
	var candidates []candidate
	if err := func() error {
		rows, err := tx.QueryContext(ctx,
			"SELECT id, pid, match_key FROM album WHERE release_group_id=? ORDER BY id", rgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		prefix := heuristicAlbumKeyPrefix + oldRGKey + "\x1f"
		for rows.Next() {
			var c candidate
			var key string
			if err := rows.Scan(&c.id, &c.pid, &key); err != nil {
				return err
			}
			if strings.HasPrefix(key, prefix) {
				candidates = append(candidates, c)
			}
		}
		return rows.Err()
	}(); err != nil {
		return nil, err
	}

	// The per-album derivation queries the same single-connection transaction, so it
	// waits until the cursor above is closed.
	var out []dependentAlbum
	for _, c := range candidates {
		tr, filePath, itemID, err := representativeMemberTx(ctx, tx,
			`SELECT t.item_id FROM track t
				JOIN item_file f ON f.item_id = t.item_id AND f.role = 'primary'
				WHERE t.album_id=? ORDER BY t.item_id LIMIT 1`, c.id)
		if err != nil {
			return nil, err
		}
		if tr == nil {
			continue
		}
		tr.MBReleaseGroupID, tr.MBReleaseID = "", ""
		_, rgKey, albumKey := albumChainKeys(*tr, filePath)
		if rgKey == "" || albumKey == "" || albumKeyFolder(albumKey) == "" {
			continue
		}
		anchorID, err := trackAnchorArtistTx(ctx, tx, itemID)
		if err != nil {
			return nil, err
		}
		out = append(out, dependentAlbum{
			id: c.id, pid: c.pid, rgKey: rgKey, rgTitle: tr.Album,
			albumArtistID: anchorID, newKey: albumKey,
		})
	}
	return out, nil
}

// trackAnchorArtistTx reads the album-artist entity one member already resolved to, which
// is the primary a group minted from that member's tags should carry: the clear disowns an
// identifier, not the credit underneath it. It takes the representative item rather than
// the album, since members of an mbid-keyed album can carry different ALBUMARTIST tags and
// only the representative's is the one inside the derived key.
func trackAnchorArtistTx(ctx context.Context, tx *sql.Tx, itemID int64) (int64, error) {
	var id sql.NullInt64
	err := tx.QueryRowContext(ctx,
		"SELECT album_artist_id FROM track WHERE item_id=?", itemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id.Int64, err
}

// rewriteOrMergeEntityKeyTx moves one entity onto a new match key. A free key is rewritten
// in place, so the row keeps its id, pid, and everything hanging off them; a taken key
// folds the row into the incumbent through mergeEntityTx, since refusing would leave the
// entity on the key the caller is undoing and the clear half-applied. It returns the
// incumbent's pid when the row merged away, and an empty pid when it was rewritten.
//
// The table comes from the entity type rather than the caller, so a mismatched pair
// cannot compile and then rewrite the wrong one.
func rewriteOrMergeEntityKeyTx(ctx context.Context, tx *sql.Tx, et model.MergeEntity,
	id int64, pid model.PID, newKey string) (model.PID, error) {
	table, ok := entityTableFor(et)
	if !ok {
		return "", errors.New("rewriteOrMergeEntityKeyTx: no table for a " + string(et) + " entity")
	}
	var incPID string
	err := tx.QueryRowContext(ctx,
		"SELECT pid FROM "+table+" WHERE match_key=? AND id<>?", newKey, id).Scan(&incPID)
	switch {
	case err == nil:
		if _, err := mergeEntityTx(ctx, tx, et, table, model.PID(incPID), pid); err != nil {
			return "", err
		}
		return model.PID(incPID), nil
	case errors.Is(err, sql.ErrNoRows):
		_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET match_key=? WHERE id=?", newKey, id)
		return "", err
	default:
		return "", err
	}
}

// applyEntityFieldTx writes one entity field to its column. A sort-name override drives
// the generated, BINARY-sortable sort_key: a non-empty value seeds it, and an empty
// value (a clear) regenerates it from the entity's display name. Every other field
// writes its own column, with an empty value stored as NULL.
func applyEntityFieldTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity, table string, entityID int64, field, value string) error {
	if field == "sort" {
		sortKey := model.SortKey(value)
		if value == "" {
			// Regenerate the sort key from the entity's display name so a cleared override
			// leaves the sort key matching what db verify recomputes.
			var name string
			if err := tx.QueryRowContext(ctx,
				"SELECT "+entityDisplayColumn(entityType)+" FROM "+table+" WHERE id = ?", entityID).Scan(&name); err != nil {
				return err
			}
			sortKey = model.SortKey(name)
		}
		_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET sort_key = ? WHERE id = ?", sortKey, entityID)
		return err
	}
	col := entityColumnForField(field)
	_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET "+col+" = ? WHERE id = ?", nullStr(value), entityID)
	return err
}

// refreshEntitySortKeyTx rewrites an entity's sort_key from its current curation and
// display name, the same source sortKeyDrift checks. Any path that moves a sort
// override without going through applyEntityFieldTx has to call it. A non-curatable
// type has no override to inherit and is a no-op.
func refreshEntitySortKeyTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity, table string, entityID int64) error {
	if _, ok := entityTableFor(entityType); !ok {
		return nil
	}
	var text string
	if err := tx.QueryRowContext(ctx,
		"SELECT COALESCE(NULLIF(TRIM(ec.value), ''), t."+entityDisplayColumn(entityType)+")"+
			" FROM "+table+" t LEFT JOIN entity_curation ec"+
			" ON ec.entity_type = ? AND ec.entity_id = t.id AND ec.field = 'sort'"+
			" WHERE t.id = ?", string(entityType), entityID).Scan(&text); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		"UPDATE "+table+" SET sort_key = ? WHERE id = ?", model.SortKey(text), entityID)
	return err
}

// upsertEntityCurationTx writes an entity field's curation row with the caller's
// attribution, the curated value, and the lock the caller asked for. It mirrors
// upsertEditProvenanceTx for the entity scope, LockUnchanged included: the stored lock
// survives a write that formed no lock intent, and a fresh row inserts unlocked.
func upsertEntityCurationTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, field string, attr model.Attribution, value string, lock model.LockChange, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_curation(entity_type, entity_id, field, source, provider, locked, value, updated_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(entity_type, entity_id, field) DO UPDATE SET
			source=excluded.source, provider=excluded.provider,
			locked=CASE WHEN ? THEN excluded.locked ELSE locked END,
			value=excluded.value, updated_at=excluded.updated_at`,
		entityType, entityID, field, string(attr.Source), nullStr(attr.Provider),
		boolInt(lock == model.LockOn), value, now,
		boolInt(lock != model.LockUnchanged))
	return err
}

// setEntityCurationLockTx upserts the lock bit on an entity's curation field, carrying
// the attribution of the write that asked for the lock, or drops a pure-lock row when
// unlocking, keeping the table sparse. LockUnchanged writes nothing at all. It is the
// entity-scoped twin of setCurationLockTx, for the artifact fields that have no scalar
// value of their own to carry ("art").
//
// The row it writes is not inert: ArtRoles synthesizes an entity's front entry from it
// when the cover was cleared and locked, so the attribution recorded here is the only
// one that read has to report.
func setEntityCurationLockTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, field string, attr model.Attribution, lock model.LockChange) error {
	switch lock {
	case model.LockUnchanged:
		return nil
	case model.LockOn:
		_, err := tx.ExecContext(ctx, `INSERT INTO entity_curation(entity_type, entity_id, field, source, provider, locked, updated_at)
			VALUES (?,?,?,?,?,1,?)
			ON CONFLICT(entity_type, entity_id, field) DO UPDATE SET
				source=excluded.source, provider=excluded.provider, locked=1, updated_at=excluded.updated_at`,
			entityType, entityID, field, string(attr.Source), nullStr(attr.Provider), nowNS())
		return err
	}
	_, err := tx.ExecContext(ctx,
		"DELETE FROM entity_curation WHERE entity_type=? AND entity_id=? AND field=? AND (value IS NULL OR value='')",
		entityType, entityID, field)
	return err
}

// entityFieldLockedTx reports whether an entity field is locked in entity_curation. A
// missing row means unlocked. It is the guard enrichment calls before overwriting a
// shared entity field. It takes a queryer so the ArtRoles read can share it with the
// in-transaction writers.
func entityFieldLockedTx(ctx context.Context, q queryer, entityType string, entityID int64, field string) (bool, error) {
	var locked int
	err := q.QueryRowContext(ctx,
		"SELECT locked FROM entity_curation WHERE entity_type=? AND entity_id=? AND field=?",
		entityType, entityID, field).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return locked == 1, nil
}

// EntityCuration returns an entity's curation rows (only the non-default fields are
// present), or CodeNotFound for an unknown entity or an entity type that is not
// curatable.
func (s *Store) EntityCuration(ctx context.Context, entityType model.MergeEntity, entityPID model.PID) ([]model.EntityCuration, error) {
	const op = "store.EntityCuration"
	table, ok := entityTableFor(entityType)
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "entity curation is not supported for a "+string(entityType)+" entity")
	}
	var entityID int64
	err := s.read.QueryRowContext(ctx, "SELECT id FROM "+table+" WHERE pid = ?", string(entityPID)).Scan(&entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, waxerr.New(waxerr.CodeNotFound, op, "no such "+string(entityType)+": "+string(entityPID))
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	rows, err := s.read.QueryContext(ctx, `SELECT field, source, COALESCE(provider,''), locked,
		COALESCE(value,''), updated_at
		FROM entity_curation WHERE entity_type = ? AND entity_id = ? ORDER BY field`,
		string(entityType), entityID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.EntityCuration
	for rows.Next() {
		ec := model.EntityCuration{EntityType: entityType, EntityPID: entityPID}
		var source string
		var locked int
		if err := rows.Scan(&ec.Field, &source, &ec.Provider, &locked, &ec.Value, &ec.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ec.Source = model.ProvenanceSource(source)
		ec.Locked = locked == 1
		out = append(out, ec)
	}
	return out, rows.Err()
}
