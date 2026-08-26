package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// staticCurationFieldKinds maps a non-scalar curation lock field to the item kinds it
// applies to. Lyrics belong to tracks, chapters to books, art to either.
var staticCurationFieldKinds = map[string]map[model.Kind]bool{
	"lyrics":   {model.KindTrack: true},
	"chapters": {model.KindBook: true},
	"art":      {model.KindTrack: true, model.KindBook: true},
}

// curatableFieldForKind reports whether a provenance/lock field applies to the given
// item kind: a scalar field from the kind's edit whitelist, a credit.<role> whose role
// is valid for the kind, an art.<role> auxiliary-art lock, or a static curation field
// (lyrics/chapters/art).
func curatableFieldForKind(kind, field string) bool {
	if allowed := editableFieldsForKind(kind); allowed != nil && allowed[field] {
		return true
	}
	if role, ok := model.CutCreditPrefix(field); ok {
		return model.RoleValidForKind(model.ContributorRole(role), model.Kind(kind))
	}
	if role, ok := model.CutArtRolePrefix(field); ok {
		// An auxiliary slot goes wherever the front cover does, so it follows "art".
		// There is no art.front, for the reason CutArtRolePrefix gives.
		r := model.ArtRole(role)
		return r.Valid() && r != model.ArtRoleFront && staticCurationFieldKinds["art"][model.Kind(kind)]
	}
	if _, ok := model.CutTagPrefix(field); ok {
		// Custom tags live on the items that carry file tags (tracks and books).
		return model.Kind(kind) == model.KindTrack || model.Kind(kind) == model.KindBook
	}
	if kinds, ok := staticCurationFieldKinds[field]; ok {
		return kinds[model.Kind(kind)]
	}
	return false
}

// SetItemCredits replaces the contributors of one role on an item, resolving each
// name to an artist entity. It rewrites ONLY the target role (not every role, unlike
// a scan's contributor resolution), so setting the producers leaves the composers and
// the book's author untouched. It keeps the denormalized column in step for the roles
// that have one (composer on a track; author/narrator on a book), refreshes the
// touched artists' rollups, records a credit.<role> provenance row carrying the
// caller's attribution and lock instruction, rebuilds the item's search row when the
// role feeds it, and emits one item delta. A role that does not apply to the item's
// kind is CodeInvalid; a locked credit role is CodeLocked unless force is set. It
// returns the names actually stored (trimmed, resolvable, de-duplicated by artist),
// which is what the denorm column, provenance value, and any tag write-back reflect.
//
// For a track's performing credit (artist) or a book's author, it also runs the
// entity rename pre-pass as a single-entry batch, so a single-reference rename lands
// in place instead of always splitting off a fresh artist; an artist referenced
// elsewhere in the catalog still splits, since a one-item edit surface cannot prove
// every reference moved. Only the artist stage runs. The release-group and album
// stages deliberately do not, because nothing re-resolves the chain behind this call:
// they would move the chain keys whatever the artist stage decided, leaving them
// keyed on a credit the columns underneath no longer spell.
func (s *Store) SetItemCredits(ctx context.Context, itemPID model.PID, role model.ContributorRole, names []string, attr model.Attribution, lock model.LockChange, force bool) ([]string, error) {
	const op = "store.SetItemCredits"
	if !role.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "unknown contributor role: "+string(role))
	}
	attr, err := checkFieldAttribution(attr, op)
	if err != nil {
		return nil, err
	}
	if err := checkLockChange(lock, op); err != nil {
		return nil, err
	}
	// Trim and drop empties, preserving order, so a blank entry never resolves to a
	// junk artist. An empty result clears the role.
	clean := make([]string, 0, len(names))
	for _, n := range names {
		if t := strings.TrimSpace(n); t != "" {
			clean = append(clean, t)
		}
	}
	field := model.CreditField(role)

	var stored []string
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !model.RoleValidForKind(role, model.Kind(kind)) {
			return waxerr.New(waxerr.CodeInvalid, op, "role "+string(role)+" does not apply to a "+kind+" item")
		}
		if !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, field)
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeLocked, op, "credit role is locked (use force to override): "+string(role))
			}
		}

		affected := newAffectedRollups()
		// Run the rename pre-pass as a batch of one, ahead of the contributor rewrite
		// below: for the two roles that back an entity-key field (artist on a track,
		// author on a book) a covered single-reference rename lands in place instead of
		// splitting. Every other role has no pre-pass field and is left alone, and so is
		// a set naming several people: that is one entity becoming many, and nothing here
		// can say which of them should inherit the old row's pid, curation, and star.
		if renameField, ok := creditRenameField(role); ok && len(clean) == 1 {
			entry := editEntry{
				pid:     itemPID,
				itemID:  itemID,
				kind:    kind,
				fields:  []string{renameField},
				norm:    map[string]string{renameField: strings.Join(clean, ", ")},
				credits: clean,
			}
			if err := renameArtistsForCreditEditTx(ctx, tx, entry, affected, op); err != nil {
				return err
			}
		}
		// The artist role rewrites track.artist_id, and the outgoing artist need not be
		// a prior contributor: a catalog scanned before credits existed has no
		// RoleArtist rows, so its rollup would drift unrecomputed.
		if err := affected.collect(ctx, tx, itemID); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Collect the role's prior artists so a dropped contributor's rollup is refreshed.
		prior, err := contributorArtistIDsForRole(ctx, tx, itemID, role)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		for _, aid := range prior {
			affected.artists[aid] = true
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM item_contributor WHERE item_id=? AND role=?", itemID, string(role)); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		var firstID int64
		seen := make(map[int64]bool, len(clean))
		resolved := make([]string, 0, len(clean))
		for _, name := range clean {
			aid, err := resolveArtist(ctx, tx, name, "")
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if aid == 0 {
				continue
			}
			affected.artists[aid] = true
			// De-duplicate by resolved artist: two spellings of one artist (or an exact
			// repeat) collapse to one item_contributor row (PK includes artist_id), so the
			// stored name list, denorm column, and provenance value must not double-count.
			if seen[aid] {
				continue
			}
			seen[aid] = true
			if firstID == 0 {
				firstID = aid
			}
			// position is the credited order (0, 1, 2, ...) with no gaps for dropped names.
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO item_contributor(item_id, artist_id, role, position) VALUES (?,?,?,?)",
				itemID, aid, string(role), len(resolved)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			resolved = append(resolved, name)
		}

		// Keep the denormalized column in step for the roles that carry one, and rebuild
		// the search row for a book (whose FTS carries author + narrator).
		ftsDirty, err := syncCreditDenormTx(ctx, tx, itemID, kind, role, resolved, firstID)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}

		if !affected.empty() {
			if err := maintainRollupsTx(ctx, tx, affected, nowNS()); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		if ftsDirty {
			// The rebuild is per kind: search_fts.artist for a track is artist +
			// album artist, where a book's is author + narrator, so the book rebuild
			// run against a track would write the wrong row.
			rebuild := rebuildBookSearchFTSTx
			if kind == string(model.KindTrack) {
				rebuild = rebuildTrackSearchFTSTx
			}
			if err := rebuild(ctx, tx, itemID); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Record the credit's provenance (value = the display list) under the caller's
		// attribution and lock instruction.
		if err := upsertEditProvenanceTx(ctx, tx, itemID, field, attr, strings.Join(resolved, "; "), lock, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		stored = resolved
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}

// creditRenameField reports the rename pre-pass's key field for a credit role, if it
// has one: artist for a track's performing credit, author for a book's author. These
// are the only two roles that also back an editKeyFields/bookKeyFields entity-key
// column, so they're the only ones the pre-pass can rename in place.
func creditRenameField(role model.ContributorRole) (string, bool) {
	switch role {
	case model.RoleArtist:
		return "artist", true
	case model.RoleAuthor:
		return "author", true
	default:
		return "", false
	}
}

// syncCreditDenormTx updates the denormalized column a role feeds, returning whether
// the item's search row must be rebuilt (true for a book author/narrator change).
// The roles that carry a derived sort column (composer_sort, author_sort) regenerate
// it from the new display, unless that sort is locked: a locked sort is curated
// state the credit edit did not name, so it survives, exactly as it does on the
// scalar edit path (the editTrackFieldsTx/editBookFieldsTx probes).
func syncCreditDenormTx(ctx context.Context, tx *sql.Tx, itemID int64, kind string, role model.ContributorRole, names []string, firstArtistID int64) (bool, error) {
	switch {
	case kind == string(model.KindTrack) && role == model.RoleArtist:
		// Here the display IS rebuilt from the edited names, unlike the scan path,
		// which keeps the file's raw credit string. The user typed these, so there is
		// no file text to stay faithful to. artist_sort follows because it has no edit
		// surface of its own, so unlike composer_sort there is no lock to probe.
		display := strings.Join(names, ", ")
		if _, err := tx.ExecContext(ctx, "UPDATE track SET artist=?, artist_sort=?, artist_id=? WHERE item_id=?",
			display, model.SortKey(display), nullInt64(firstArtistID), itemID); err != nil {
			return false, err
		}
		return true, nil
	case kind == string(model.KindTrack) && role == model.RoleComposer:
		// The composer denormalization uses "; " (matching the scanner's multi-composer join).
		display := strings.Join(names, "; ")
		sortLocked, err := fieldLockedTx(ctx, tx, itemID, "composer_sort")
		if err != nil {
			return false, err
		}
		if sortLocked {
			_, err = tx.ExecContext(ctx, "UPDATE track SET composer=? WHERE item_id=?", display, itemID)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE track SET composer=?, composer_sort=? WHERE item_id=?",
				display, model.SortKey(display), itemID)
		}
		if err != nil {
			return false, err
		}
		return false, nil
	case kind == string(model.KindBook) && role == model.RoleAuthor:
		display := strings.Join(names, ", ")
		sortLocked, err := fieldLockedTx(ctx, tx, itemID, "author_sort")
		if err != nil {
			return false, err
		}
		if sortLocked {
			_, err = tx.ExecContext(ctx, "UPDATE book SET author=?, author_id=? WHERE item_id=?",
				display, nullInt64(firstArtistID), itemID)
		} else {
			_, err = tx.ExecContext(ctx, "UPDATE book SET author=?, author_sort=?, author_id=? WHERE item_id=?",
				display, model.SortKey(display), nullInt64(firstArtistID), itemID)
		}
		if err != nil {
			return false, err
		}
		return true, nil
	case kind == string(model.KindBook) && role == model.RoleNarrator:
		if _, err := tx.ExecContext(ctx, "UPDATE book SET narrator=? WHERE item_id=?",
			strings.Join(names, ", "), itemID); err != nil {
			return false, err
		}
		return true, nil
	default:
		// Other roles have no denormalized column; a track's non-composer credit does
		// not feed its search row either.
		return false, nil
	}
}

// rebuildTrackSearchFTSTx reloads a track's current state and rewrites its search
// row, so an artist credit change is reflected in search. The scan and scalar-edit
// paths reach syncSearchFTS through resolveAndLinkEntities; the credit path is the
// only one that rewrites track.artist without passing through it.
func rebuildTrackSearchFTSTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	tr, _, _, err := loadTrackForEditTx(ctx, tx, itemID)
	if err != nil {
		return err
	}
	return syncSearchFTS(ctx, tx, itemID, tr)
}

// rebuildBookSearchFTSTx reloads a book's current state and rewrites its search row,
// so an author/narrator credit change is reflected in search.
func rebuildBookSearchFTSTx(ctx context.Context, tx *sql.Tx, itemID int64) error {
	b, _, err := loadBookForEditTx(ctx, tx, itemID)
	if err != nil {
		return err
	}
	return syncBookSearchFTS(ctx, tx, itemID, b, bookAuthorDisplay(b))
}

// contributorNamesForRoleTx returns the names currently credited in one role, in
// credited order, draining its cursor before returning so the caller can write to the
// same transaction.
func contributorNamesForRoleTx(ctx context.Context, tx *sql.Tx, itemID int64, role model.ContributorRole) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.name FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		WHERE ic.item_id = ? AND ic.role = ? ORDER BY ic.position`, itemID, string(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// contributorArtistIDsForRole returns the artist ids currently credited in one role,
// draining its cursor before returning so the caller can write to the same tx.
func contributorArtistIDsForRole(ctx context.Context, tx *sql.Tx, itemID int64, role model.ContributorRole) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT artist_id FROM item_contributor WHERE item_id=? AND role=?", itemID, string(role))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var aid int64
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		out = append(out, aid)
	}
	return out, rows.Err()
}

// ItemCredits returns an item's contributors across every role, in role then credited
// order. It reads through the artist entity so each credit carries the artist's pid.
func (s *Store) ItemCredits(ctx context.Context, itemPID model.PID) ([]model.Contributor, error) {
	const op = "store.ItemCredits"
	rows, err := s.read.QueryContext(ctx, `SELECT a.pid, a.name, ic.role, ic.position
		FROM item_contributor ic
		JOIN artist a ON a.id = ic.artist_id
		JOIN playable_item pi ON pi.id = ic.item_id
		WHERE pi.pid = ? ORDER BY ic.role, ic.position`, string(itemPID))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.Contributor
	for rows.Next() {
		var c model.Contributor
		var pid, role string
		if err := rows.Scan(&pid, &c.Name, &role, &c.Position); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		c.ArtistPID = model.PID(pid)
		c.Role = model.ContributorRole(role)
		out = append(out, c)
	}
	return out, rows.Err()
}
