package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file is the entity-rung rename: moving a whole album or release group onto a new
// identity key by editing the keying fields of every member at once.
//
// The machinery it drives already exists. renameEntitiesForEditsTx runs as an implicit
// pre-pass inside the item-edit transaction and moves a chain in place when the batch it
// is given happens to cover every member, silently falling back to split-and-ghost when
// it does not. Nothing guaranteed that coverage and nothing reported which branch ran.
// This verb closes both halves: it derives the member list from the entity itself, so
// coverage is true by construction, and it reports the branch. It deliberately does not
// reimplement the rename; it runs the same transaction body EditItemsFields runs, so the
// two cannot drift.

// renameKeyFields is the field vocabulary each rung owns, which is the keying fields of
// that rung and nothing else. year is an album-key segment but not a release-group one
// (a group key is anchor and title), so it is refused one rung up rather than silently
// applied to the members and ignored by the key.
var renameKeyFields = map[model.MergeEntity]map[string]bool{
	model.MergeAlbum:        {"album": true, "album_artist": true, "year": true},
	model.MergeReleaseGroup: {"album": true, "album_artist": true},
	// The artist rung takes one field, because the item-level field a rename writes
	// differs per reference kind: a track credits the artist through artist or
	// album_artist, a book through author. Naming those here would ask the caller which
	// references exist, which is what this verb works out for itself.
	model.MergeArtist: {"name": true},
}

// RenameEntity moves an album or release group onto the identity key its members would
// compute after the given field edits, keeping the row (and so its pid, art, curation,
// stars and enrichment marker) rather than leaving it behind as a ghost.
//
// It enumerates every member itself instead of taking a caller's list. That is the whole
// point: renameAlbumChainTx moves a chain in place only when the batch covers every
// track the album has, and an under-covered batch falls back to splitting the entity in
// two without saying so. Deriving the list from the album or the group makes that count
// unconditionally true.
//
// skipLocked is not a parameter and is forced off. collectEditEntriesTx drops a locked
// target from the entry list when it is set, which would silently break the coverage
// count and produce exactly the split this verb exists to prevent. A locked member is a
// refusal (or a force), never a skip.
//
// Four conditions that today cause a silent split are refused up front instead:
// an archived member with no primary file, a locked keying field on any member without
// force, a rename to an empty name, and, at the release-group rung, a group whose albums
// do not share a title. See the checks below for what each one would otherwise do.
//
// The anchor moves with an album_artist rename, and that is not a surprise to hide: a
// blank album_artist anchors the release group on the first credited artist, which is
// why artist is a keying field at all. So renaming album_artist on an album whose members
// carry no album_artist of their own moves the group anchor underneath it, and the report
// says so through MovedAlbums.
func (s *Store) RenameEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID,
	fields map[string]string, attr model.Attribution, lock model.LockChange, force bool) (*model.EntityRenameReport, error) {
	const op = "store.RenameEntity"
	vocab, ok := renameKeyFields[entityType]
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupported, op,
			"renaming is not supported for a "+string(entityType)+" entity")
	}
	table, _ := entityTableFor(entityType)
	if len(fields) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no fields to rename")
	}
	for f, v := range fields {
		if !vocab[f] {
			return nil, waxerr.New(waxerr.CodeInvalid, op,
				"field "+f+" does not key a "+string(entityType)+" entity; renamable fields are "+
					strings.Join(sortedKeys(vocab), ", "))
		}
		// A name-bearing key segment cannot go empty. The resulting key would be empty
		// too, which un-groups every member and ghosts the entity, and that is a clear
		// rather than a rename: `waxbin entity edit` owns clearing. year may go empty,
		// which only drops a segment from an album key.
		if f != "year" && strings.TrimSpace(v) == "" {
			return nil, waxerr.New(waxerr.CodeInvalid, op,
				"cannot rename "+f+" to nothing: that un-groups every member rather than moving the entity")
		}
	}
	// A name of nothing but punctuation ("???", "---") survives the check above and then
	// folds to an empty match key, which is not a name this catalog can hold. The artist
	// stage skips such a target, so the row stays put while the applies below still run:
	// resolveArtist drops every name it cannot key, so the credits are deleted and not
	// rebuilt. The member-key check catches this for an artist with field references, and
	// reaches nothing for one held by credits alone.
	if entityType == model.MergeArtist && identity.MatchKey(fields["name"]) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op,
			"cannot rename to "+strconv.Quote(fields["name"])+
				": it carries no letters or digits, so it folds to an empty key that groups nothing")
	}

	attr, err := checkFieldAttribution(attr, op)
	if err != nil {
		return nil, err
	}
	if err := checkLockChange(lock, op); err != nil {
		return nil, err
	}

	rep := &model.EntityRenameReport{EntityPID: entityPID}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		entityID, err := idByPIDTx(ctx, tx, table, entityPID, op)
		if err != nil {
			return err
		}
		var targets []editEntry
		var creditTargets []creditEntry
		if entityType == model.MergeArtist {
			targets, creditTargets, err = artistRenameTargetsTx(ctx, tx, entityID, fields["name"], force, op)
		} else {
			var members []model.PID
			members, err = renameMemberPIDsTx(ctx, tx, entityType, entityID, op)
			if err == nil {
				targets, err = renameTargetsFor(members, fields, op)
			}
		}
		if err != nil {
			return err
		}
		if len(targets) == 0 && len(creditTargets) == 0 {
			return waxerr.New(waxerr.CodeInvalid, op,
				"entity has no members to carry the rename; its identity comes from its members' tags")
		}
		rep.Members = len(targets)
		members := make([]model.PID, 0, len(targets))
		for _, t := range targets {
			members = append(members, t.pid)
		}
		if err := checkRenameMembersTx(ctx, tx, entityType, entityID, entityPID, members, op); err != nil {
			return err
		}

		before, err := albumGroupsTx(ctx, tx, members)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		curKey, err := entityMatchKeyTx(ctx, tx, table, entityID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		// The same body EditItemsFields runs, over the derived member list. No second
		// implementation of the rename, so the pre-pass cannot drift from a copy of
		// itself, and every provenance row, lock and delta an ordinary edit writes is
		// written here too.
		entries, err := collectEditEntriesTx(ctx, tx, targets, force, false, op, nil)
		if err != nil {
			return err
		}
		keys, err := checkRenameKeysUniformTx(ctx, tx, entityType, entries, op)
		if err != nil {
			return err
		}
		affected := newAffectedRollups()
		if err := renameEntitiesForEditsTx(ctx, tx, s.log, entries, creditTargets, affected, op); err != nil {
			return err
		}
		rep.MemberEdits = make([]model.ItemFieldEdit, 0, len(entries))
		for _, e := range entries {
			if err := applyItemEditTx(ctx, tx, s.log, e.pid, e.itemID, e.kind, e.fields, e.norm,
				attr, lock, op, affected); err != nil {
				return err
			}
			rep.MemberEdits = append(rep.MemberEdits, model.ItemFieldEdit{ItemPID: e.pid, Fields: e.norm})
		}
		// The credit half rides the same affected set, so the one maintainRollupsTx below
		// still covers both. applyItemCreditsTx deliberately does not call it itself.
		rep.CreditEdits = make([]model.ItemCreditEdit, 0, len(creditTargets))
		for _, e := range creditTargets {
			stored, err := applyItemCreditsTx(ctx, tx, e, attr, lock, affected, op)
			if err != nil {
				return err
			}
			rep.CreditEdits = append(rep.CreditEdits,
				model.ItemCreditEdit{ItemPID: e.pid, Role: e.role, Names: stored})
		}
		rep.Credits = len(rep.CreditEdits)
		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if err := checkRenameLandedTx(ctx, tx, entityType, entityID, table, fields, keys, op); err != nil {
			return err
		}
		return finishRenameReportTx(ctx, tx, entityType, entityID, table, curKey, fields, before, members, rep, op)
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// renameMemberPIDsTx lists every item the entity is made of, in id order. For an album
// that is its tracks; for a release group, the tracks of all its albums.
func renameMemberPIDsTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity, entityID int64, op string) ([]model.PID, error) {
	q := `SELECT pi.pid FROM track t JOIN playable_item pi ON pi.id = t.item_id
		WHERE t.album_id = ? ORDER BY t.item_id`
	if entityType == model.MergeReleaseGroup {
		q = `SELECT pi.pid FROM track t
			JOIN album al ON al.id = t.album_id
			JOIN playable_item pi ON pi.id = t.item_id
			WHERE al.release_group_id = ? ORDER BY t.item_id`
	}
	rows, err := tx.QueryContext(ctx, q, entityID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.PID
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		out = append(out, model.PID(pid))
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return out, nil
}

// artistRenameTargetsTx derives the per-item edits an artist rename needs, one entry per
// referring item carrying every field that item credits the artist through, plus one
// credit entry per contributor role that backs no field of its own.
//
// An artist is referenced five ways and artistRenameCoveredTx checks all five before the
// pre-pass will move the row: track.artist_id, track.album_artist_id, book.author_id, the
// item_contributor rows, and release_group.primary_artist_id. The first four are covered
// by enumerating them here. The fifth needs nothing of its own: a group is anchored on
// its members' album-artist credit, and those members are in the album_artist set.
//
// Each referring row's new value is its own credit list with the old name substituted,
// not the new name alone. A track credited to "Alpha; Beta" renaming Alpha keeps Beta,
// and the substitution is what makes the fold-back guard meaningful: a list that still
// spells the old name somewhere else keeps the artist referenced while its curation
// moves, and the pre-pass refuses to move the row at all in that case.
//
// A track whose album_artist column is empty is deliberately left out of the album_artist
// set even when it points at this artist: the anchor falls back to the track artist when
// the column is blank, so editing artist already moves it, and writing an album_artist
// that was never there would change the album's key shape rather than its name.
func artistRenameTargetsTx(ctx context.Context, tx *sql.Tx, artistID int64, newName string,
	force bool, op string) ([]editEntry, []creditEntry, error) {
	refs, err := contributorRefsTx(ctx, tx, artistID)
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	curKey, err := entityMatchKeyTx(ctx, tx, "artist", artistID)
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The two roles that back an item field ride that field. Every other role has no
	// field to ride, so it becomes an entry on the credit surface, applied in this same
	// transaction beside the field edits.
	byItem := map[int64]map[string]bool{}
	var credits []creditEntry
	for _, c := range refs {
		switch model.ContributorRole(c.role) {
		case model.RoleArtist:
			addRenameField(byItem, c.itemID, "artist")
		case model.RoleAuthor:
			addRenameField(byItem, c.itemID, "author")
		default:
			e, err := creditRenameEntryTx(ctx, tx, c, artistID, curKey, newName, force, op)
			if err != nil {
				return nil, nil, err
			}
			credits = append(credits, e)
		}
	}
	for _, ref := range []struct {
		q     string
		field string
	}{
		{"SELECT item_id FROM track WHERE artist_id=?", "artist"},
		{"SELECT item_id FROM track WHERE album_artist_id=? AND album_artist <> ''", "album_artist"},
		{"SELECT item_id FROM book WHERE author_id=?", "author"},
	} {
		ids, err := queryInt64sTx(ctx, tx, ref.q, artistID)
		if err != nil {
			return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, id := range ids {
			addRenameField(byItem, id, ref.field)
		}
	}

	out := make([]editEntry, 0, len(byItem))
	for _, itemID := range slices.Sorted(maps.Keys(byItem)) {
		pid, err := pidByIDTx(ctx, tx, "playable_item", itemID)
		if err != nil {
			return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		fields := make([]string, 0, len(byItem[itemID]))
		norm := make(map[string]string, len(byItem[itemID]))
		for _, f := range slices.Sorted(maps.Keys(byItem[itemID])) {
			names, err := renameCreditListTx(ctx, tx, itemID, f)
			if err != nil {
				return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			v, ok := substituteCredit(names, curKey, newName)
			if !ok {
				// The reference says this item credits the artist through this field, but
				// the field's own credit list does not name it. Writing the list back
				// unchanged would leave the reference behind while the entity moved, so
				// the disagreement is a refusal rather than a silent no-op.
				if f == "album_artist" {
					continue
				}
				return nil, nil, waxerr.New(waxerr.CodeConflict, op,
					"item "+string(pid)+" credits this artist through "+f+
						", but its "+f+" list does not name it, so the rename cannot be written there")
			}
			fields = append(fields, f)
			norm[f] = v
		}
		if len(fields) == 0 {
			continue
		}
		out = append(out, editEntry{pid: pid, fields: fields, norm: norm})
	}
	return out, credits, nil
}

// creditRenameEntryTx turns one contributor row into the credit-surface entry that moves
// it, its names being the role's current credit list with the old spelling substituted so
// the role's other credits survive. It is the credit twin of the field entries above.
//
// The lock is checked here rather than at apply because collectEditEntriesTx already
// checks the field half up front, and a rename that refused halfway would have moved some
// of an artist's references and not others. skipLocked has no counterpart: a skipped
// credit leaves a reference behind, which is the split this verb exists to prevent.
func creditRenameEntryTx(ctx context.Context, tx *sql.Tx, c contributorRef, artistID int64,
	curKey, newName string, force bool, op string) (creditEntry, error) {
	pid, err := pidByIDTx(ctx, tx, "playable_item", c.itemID)
	if err != nil {
		return creditEntry{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	role := model.ContributorRole(c.role)
	if !force {
		locked, err := fieldLockedTx(ctx, tx, c.itemID, model.CreditField(role))
		if err != nil {
			return creditEntry{}, err
		}
		if locked {
			return creditEntry{}, waxerr.New(waxerr.CodeLocked, op,
				"item "+string(pid)+" has a locked "+c.role+" credit (use force to override)")
		}
	}
	var kind string
	if err := tx.QueryRowContext(ctx,
		"SELECT kind FROM playable_item WHERE id=?", c.itemID).Scan(&kind); err != nil {
		return creditEntry{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	names, err := contributorNamesForRoleTx(ctx, tx, c.itemID, role)
	if err != nil {
		return creditEntry{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	clean, ok := substituteCreditNames(names, curKey, newName)
	if !ok {
		// The row says this item credits the artist in this role, but the role's own list
		// does not name it. Same disagreement the field half refuses on.
		return creditEntry{}, waxerr.New(waxerr.CodeConflict, op,
			"item "+string(pid)+" credits this artist as "+c.role+
				", but its "+c.role+" list does not name it, so the rename cannot be written there")
	}
	return creditEntry{
		pid: pid, itemID: c.itemID, kind: kind, role: role, clean: clean,
		renamePriorID: artistID, renameTarget: newName,
	}, nil
}

// pidByIDTx is the inverse of idByPIDTx, for the reference enumerations that come back
// as rowids and have to name items to a caller. table is a caller constant.
func pidByIDTx(ctx context.Context, tx *sql.Tx, table string, id int64) (model.PID, error) {
	var pid string
	err := tx.QueryRowContext(ctx, "SELECT pid FROM "+table+" WHERE id = ?", id).Scan(&pid)
	return model.PID(pid), err
}

func addRenameField(byItem map[int64]map[string]bool, itemID int64, field string) {
	if byItem[itemID] == nil {
		byItem[itemID] = map[string]bool{}
	}
	byItem[itemID][field] = true
}

// renameCreditListTx reads the names an item currently credits in the role a rename
// field writes, in credited order. field is a caller constant, never caller text.
//
// It reads the contributor rows rather than re-splitting the display column, and the
// difference is where a whole credit used to be lost. A file that states its own artist
// list resolves each name to an entity while the column keeps the joined display, and
// SplitPerformerCredit does not break on a comma or an ampersand, so re-splitting
// "Alpha, Beta" or "Alpha & Beta" yields one name matching neither artist. The
// substitution below then found nothing, returned the string untouched, and the pre-pass
// renamed the artist row onto that whole string while the co-credit was dropped.
//
// album_artist has no contributor rows: it is a single entity resolved from the first
// name of the anchor credit, so its own split is the faithful reading.
func renameCreditListTx(ctx context.Context, tx *sql.Tx, itemID int64, field string) ([]string, error) {
	switch field {
	case "artist":
		return contributorNamesForRoleTx(ctx, tx, itemID, model.RoleArtist)
	case "author":
		return contributorNamesForRoleTx(ctx, tx, itemID, model.RoleAuthor)
	}
	var v string
	err := tx.QueryRowContext(ctx, "SELECT album_artist FROM track WHERE item_id=?", itemID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return identity.SplitPerformerCredit(v), nil
}

// substituteCredit replaces the name matching curKey in a credit list and rejoins it,
// reporting false when the list names no such artist.
//
// The rejoin separator is "; " because that is what both splitters read back: an item
// edit stores the joined display and drops any stated list (applyTrackEdit), so
// resolution re-splits the column, and only a separator the splitter understands keeps
// the other credits as entities of their own. A list that arrived comma-joined therefore
// comes back semicolon-joined; that is a visible change to the credit's punctuation, and
// the only spelling of it this surface can store without losing a name.
func substituteCredit(names []string, curKey, newName string) (string, bool) {
	out, ok := substituteCreditNames(names, curKey, newName)
	if !ok {
		return "", false
	}
	return strings.Join(out, "; "), true
}

// substituteCreditNames is the same substitution kept as a list, for the credit surface,
// which stores the names rather than a joined display and does its own resolution.
func substituteCreditNames(names []string, curKey, newName string) ([]string, bool) {
	out := make([]string, len(names))
	replaced := false
	for i, n := range names {
		if identity.MatchKey(n) == curKey {
			out[i] = newName
			replaced = true
			continue
		}
		out[i] = n
	}
	if !replaced {
		return nil, false
	}
	return out, true
}

// checkRenameMembersTx refuses the conditions that would otherwise make the rename fall
// back to a split without saying so. It runs over the derived member list, so the artist
// rung gets the same answers as the chain rungs: an artist rename edits its members'
// keying fields too, so an archived member vetoes the album stage there exactly as it
// does for an album rename.
func checkRenameMembersTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity,
	entityID int64, entityPID model.PID, members []model.PID, op string) error {
	// An archived member has no primary file, so the folder segment of its computed key
	// is empty and no scan of the restored files would compute the key the rename lands
	// on. renameAlbumChainTx vetoes the whole in-place move on one such member and says
	// nothing, so the caller sees a split instead of a refusal.
	for _, pid := range members {
		var present int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_file f
			JOIN playable_item pi ON pi.id = f.item_id
			WHERE pi.pid = ? AND f.role = 'primary'`, string(pid)).Scan(&present)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if present == 0 {
			return waxerr.New(waxerr.CodeConflict, op,
				"member "+string(pid)+" has no primary file (archived), so the rename cannot derive a key its "+
					"restored files would compute; restore it or detach it first")
		}
	}
	if entityType != model.MergeReleaseGroup {
		return nil
	}
	// At the group rung the field vocabulary reaches the members' own ALBUM tags, so a
	// title rename is per-member album edits. A group whose editions are deliberately
	// titled apart would be flattened into one album by it, which is a merge, not a
	// rename. dependentAlbumsTx contemplates exactly this shape.
	titles, err := distinctAlbumTitleKeysTx(ctx, tx, entityID)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if len(titles) > 1 {
		return waxerr.New(waxerr.CodeConflict, op,
			"release group "+string(entityPID)+" holds editions titled differently ("+
				strings.Join(titles, ", ")+"), and renaming the group retitles every member, "+
				"which would flatten them into one album; rename each album instead")
	}
	return nil
}

// renameTargetKeys are the identity keys a rename's members compute once their edits are
// overlaid: one album key per current album, and the group key they share.
type renameTargetKeys struct {
	albumKeys map[int64]string
	rgKey     string
}

// checkRenameKeysUniformTx refuses a rename whose members would not land on one key, and
// returns the keys they do land on so the caller can check afterwards that the entity got
// there.
//
// This is the last silent fallback. renameAlbumChainTx moves a chain in place only when
// every member computes the same target album key, and returns without a word when they
// do not, leaving the per-item loop to resolve each member separately and split the
// entity. Coverage alone does not guarantee uniformity: the heuristic album key carries
// the member's folder, so an album whose members live in different folders (one that a
// shared MusicBrainz id united, say) computes a different key per folder no matter how
// complete the batch is.
//
// It derives the keys through buildRenameMember, the same function the pre-pass uses, so
// the condition checked here is the condition tested there rather than a restatement of
// it. That is a second load per member; the reads are the price of refusing before any
// write rather than reporting a split after one. An entity with no track members passes
// with no keys: the artist rung reaches this with book members, whose identity key the
// pre-pass moves by another route.
func checkRenameKeysUniformTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity,
	entries []editEntry, op string) (renameTargetKeys, error) {
	out := renameTargetKeys{albumKeys: map[int64]string{}}
	byAlbum := map[int64][]*renameMember{}
	for _, e := range entries {
		if e.kind != string(model.KindTrack) {
			continue
		}
		if !slices.ContainsFunc(e.fields, func(f string) bool { return editKeyFields[f] }) {
			continue
		}
		m, err := buildRenameMember(ctx, tx, e, op)
		if err != nil {
			return out, err
		}
		if m.curAlbumID != 0 {
			byAlbum[m.curAlbumID] = append(byAlbum[m.curAlbumID], m)
		}
	}
	// Ascending album id, so a catalog with more than one offending album names the same
	// pair every run instead of whichever the map happened to yield first.
	for _, albumID := range slices.Sorted(maps.Keys(byAlbum)) {
		group := byAlbum[albumID]
		// Only an album the batch covers entirely is one renameAlbumChainTx will try to
		// move, and only a move can split. An artist rename reaches albums it covers in
		// part (a compilation where the artist has two of twelve tracks); the chain stage
		// leaves those alone, so their members' keys are none of this check's business.
		var total int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM track WHERE album_id=?", albumID).Scan(&total); err != nil {
			return out, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		if total != len(group) {
			continue
		}
		want := group[0]
		if want.newAlbumKey == "" {
			return out, waxerr.New(waxerr.CodeInvalid, op,
				"the renamed values leave member "+string(want.entry.pid)+
					" with nothing to group on, which would un-group the whole entity")
		}
		for _, m := range group[1:] {
			if m.newAlbumKey != want.newAlbumKey {
				return out, waxerr.New(waxerr.CodeConflict, op,
					"members "+string(want.entry.pid)+" and "+string(m.entry.pid)+
						" would land on different album keys after the rename, which splits the "+
						string(entityType)+" instead of moving it; their files sit in different "+
						"folders, so no single key covers them")
			}
		}
		out.albumKeys[albumID] = want.newAlbumKey
		// One release group across the whole batch is a chain-rung expectation only. An
		// artist spans as many groups as it has albums, and that is not a split.
		if entityType == model.MergeArtist {
			continue
		}
		if out.rgKey == "" {
			out.rgKey = want.newRGKey
		} else if out.rgKey != want.newRGKey {
			return out, waxerr.New(waxerr.CodeConflict, op,
				"member "+string(want.entry.pid)+" would land under a different release group "+
					"than the rest, which splits the "+string(entityType)+" instead of moving it")
		}
	}
	return out, nil
}

// checkRenameLandedTx confirms the entity actually reached the key the rename implies,
// and refuses (rolling the whole transaction back) when it did not.
//
// The pre-pass declines rather than errors on every condition it cannot satisfy, and its
// artist stage has coverage rules of its own that no caller-side check can restate
// without becoming a second implementation of them: a member credit still spelling the
// old name keeps the artist referenced, and artistRenameCoveredTx vetoes the move. Left
// alone that reads as a successful "refreshed" while the entity sat still. Asking where
// the entity ended up catches every such decline at once, whatever caused it, and needs
// no copy of the rules.
//
// A merged entity is not checked here: its row is gone, which is a move by any reading,
// and the survivor is what the report names.
func checkRenameLandedTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity,
	entityID int64, table string, fields map[string]string, keys renameTargetKeys, op string) error {
	want := keys.rgKey
	switch entityType {
	case model.MergeArtist:
		want = identity.MatchKey(fields["name"])
	case model.MergeAlbum:
		want = keys.albumKeys[entityID]
	}
	if want == "" {
		return nil
	}
	got, err := entityMatchKeyTx(ctx, tx, table, entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if got == want {
		return nil
	}
	return waxerr.New(waxerr.CodeConflict, op,
		"the rename did not move "+string(entityType)+" onto its new identity: some reference to it "+
			"is not covered by this batch, most often a member credit that still spells the old name "+
			"or a credit on an item outside the entity. Nothing was written")
}

// distinctAlbumTitleKeysTx returns the normalized titles the group's albums carry, in
// title order, deduped the way identity does so a case difference is not a conflict.
func distinctAlbumTitleKeysTx(ctx context.Context, tx *sql.Tx, rgID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT title FROM album WHERE release_group_id = ? ORDER BY title", rgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, err
		}
		mk := identity.MatchKey(title)
		if mk == "" || seen[mk] {
			continue
		}
		seen[mk] = true
		out = append(out, title)
	}
	return out, rows.Err()
}

// renameTargetsFor turns one field set into the per-member edit entries the shared body
// takes. Every member gets the same edits, which is what a rename means: one entity, one
// new name, applied to each item that spells it.
func renameTargetsFor(members []model.PID, fields map[string]string, op string) ([]editEntry, error) {
	names, norm, err := normalizeEdits(fields, op)
	if err != nil {
		return nil, err
	}
	out := make([]editEntry, 0, len(members))
	for _, pid := range members {
		out = append(out, editEntry{pid: pid, fields: names, norm: norm})
	}
	return out, nil
}

// albumGroupsTx maps each album the given members sit on to the release group it is under,
// in one query. It reads from the members rather than from the entity so it serves all
// three rungs, including the artist one, whose albums are not reachable from the artist
// row. The caller runs it before and after the rename and compares.
func albumGroupsTx(ctx context.Context, tx *sql.Tx, members []model.PID) (map[string]int64, error) {
	out := map[string]int64{}
	if len(members) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(members))
	for _, pid := range members {
		args = append(args, string(pid))
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT al.pid, COALESCE(al.release_group_id,0)
		FROM track t JOIN playable_item pi ON pi.id = t.item_id
		JOIN album al ON al.id = t.album_id
		WHERE pi.pid IN `+placeholders(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var rgID int64
		if err := rows.Scan(&pid, &rgID); err != nil {
			return nil, err
		}
		out[pid] = rgID
	}
	return out, rows.Err()
}

// entityMatchKeyTx reads one entity's current identity key.
func entityMatchKeyTx(ctx context.Context, tx *sql.Tx, table string, entityID int64) (string, error) {
	var key string
	err := tx.QueryRowContext(ctx, "SELECT match_key FROM "+table+" WHERE id = ?", entityID).Scan(&key)
	return key, err
}

// finishRenameReportTx reads back which branch the shared body took. It observes the
// result rather than being told: the pre-pass is the same code an ordinary item edit
// runs, and threading an outcome out of it would be a second contract to keep in step
// with the first.
//
// A row that is gone was merged away, and the survivor is wherever the members landed,
// which is also the only answer that stays right if a merge ever chains.
func finishRenameReportTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity, entityID int64,
	table, curKey string, fields map[string]string, before map[string]int64, members []model.PID,
	rep *model.EntityRenameReport, op string) error {
	newKey, err := entityMatchKeyTx(ctx, tx, table, entityID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		rep.Outcome = model.EntityRenameMerged
		survivor, err := renameSurvivorPIDTx(ctx, tx, entityType, fields, members)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		rep.MergedInto = survivor
	case err != nil:
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	case newKey == curKey:
		rep.Outcome = model.EntityRenameRefreshed
	default:
		rep.Outcome = model.EntityRenamed
	}

	moved, err := movedAlbumsTx(ctx, tx, before, members)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	rep.MovedAlbums = moved
	return nil
}

// renameSurvivorPIDTx finds the entity the members ended up on after a merge folded the
// renamed row away.
func renameSurvivorPIDTx(ctx context.Context, tx *sql.Tx, entityType model.MergeEntity,
	fields map[string]string, members []model.PID) (model.PID, error) {
	// The artist rung is the one that knows its destination key outright, since the key
	// is the name it was handed. The chain rungs derive theirs from tags and folder, so
	// there the survivor is read from where the members ended up.
	if entityType == model.MergeArtist {
		var pid string
		err := tx.QueryRowContext(ctx, "SELECT pid FROM artist WHERE match_key = ?",
			identity.MatchKey(fields["name"])).Scan(&pid)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return model.PID(pid), err
	}
	col, table := "t.album_id", "album"
	if entityType == model.MergeReleaseGroup {
		col, table = "al.release_group_id", "release_group"
	}
	q := `SELECT e.pid FROM track t
		JOIN playable_item pi ON pi.id = t.item_id
		LEFT JOIN album al ON al.id = t.album_id
		JOIN ` + table + ` e ON e.id = ` + col + `
		WHERE pi.pid = ? LIMIT 1`
	for _, pid := range members {
		var survivor string
		err := tx.QueryRowContext(ctx, q, string(pid)).Scan(&survivor)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		return model.PID(survivor), nil
	}
	return "", nil
}

// movedAlbumsTx names the albums whose release group changed across the rename, comparing
// the map albumGroupsTx built beforehand against the same read taken after. An album the
// members left entirely is not "moved": the before map is keyed on album pid, so it simply
// does not appear in the after map.
func movedAlbumsTx(ctx context.Context, tx *sql.Tx, before map[string]int64, members []model.PID) ([]model.PID, error) {
	after, err := albumGroupsTx(ctx, tx, members)
	if err != nil {
		return nil, err
	}
	var moved []model.PID
	for _, albumPID := range slices.Sorted(maps.Keys(after)) {
		if prior, known := before[albumPID]; known && prior != after[albumPID] {
			moved = append(moved, model.PID(albumPID))
		}
	}
	return moved, nil
}

// sortedKeys returns a map's keys in order, for a deterministic error message.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
