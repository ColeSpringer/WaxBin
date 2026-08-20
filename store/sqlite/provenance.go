package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// LockField marks an item's field as protected from enrichment and organize
// writes. It upserts a sparse field_provenance row, preserving any existing
// source and value. Returns CodeNotFound for an unknown item and CodeInvalid for
// a non-metadata field.
func (s *Store) LockField(ctx context.Context, itemPID model.PID, field string) error {
	return s.setLock(ctx, itemPID, field, true)
}

// UnlockField clears a field's lock. If the row was only present to carry the
// lock (tag-sourced, no curated value), it is removed so the table stays sparse.
func (s *Store) UnlockField(ctx context.Context, itemPID model.PID, field string) error {
	return s.setLock(ctx, itemPID, field, false)
}

func (s *Store) setLock(ctx context.Context, itemPID model.PID, field string, locked bool) error {
	const op = "store.LockField"
	if !model.IsCuratableField(field) {
		return waxerr.New(waxerr.CodeInvalid, op, "not a lockable metadata field: "+field)
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		// Which fields are lockable depends on the kind. MetadataFields is the union of
		// the track and book vocabularies, and credit.<role> is kind-specific too, so
		// guard against locking a field that does not apply to this item's kind (author
		// on a track, credit.narrator on a track). That would leave an inert junk
		// provenance row, the very thing the whitelist exists to prevent.
		if !curatableFieldForKind(kind, field) {
			return waxerr.New(waxerr.CodeInvalid, op, "field "+field+" is not valid for a "+kind+" item")
		}
		// Idempotent: if the field is already in the desired lock state, do nothing
		// and emit no delta. Without this, unlocking a field with no row would
		// upsert and delete a tag row while still publishing a spurious item change.
		if cur, err := fieldLockedTx(ctx, tx, itemID, field); err != nil {
			return err
		} else if cur == locked {
			return nil
		}
		now := nowNS()
		// Upsert: a new row defaults to source='tag' (the value is still the tag's);
		// an existing row keeps its source/value and only flips the lock bit.
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, updated_at)
			VALUES (?,?,?,?,?)
			ON CONFLICT(item_id, field) DO UPDATE SET locked=excluded.locked, updated_at=excluded.updated_at`,
			itemID, field, string(model.SourceTag), boolInt(locked), now); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		// Keep the table sparse: drop a row that now carries neither a lock nor a
		// non-tag provenance nor a curated value. The art row goes whatever source it
		// carries, because art keeps no value here (the bytes and their real attribution
		// live in art_map) and this command has no artifact to attribute, so it guesses
		// "tag" where a curation set records what it was told. Without the exception an
		// `art lock` followed by `unlock <pid> art` would strand an inert row claiming a
		// cover that does not exist.
		if !locked {
			const sparse = `DELETE FROM field_provenance
				WHERE item_id=? AND field=? AND locked=0 AND (value IS NULL OR value='')`
			q, args := sparse+" AND source=?", []any{itemID, field, string(model.SourceTag)}
			if field == "art" {
				q, args = sparse, []any{itemID, field}
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// checkFieldAttribution is the single gate every scalar curation write runs its
// caller's attribution through. It applies the one rule governing the source (a write
// that names no origin is a user edit, one that names an origin is honoured, one that
// names an impossible origin is refused) and returns the attribution to store.
//
// A non-empty SourceURL is refused rather than dropped, the way setEntityArtRoleTx
// refuses an unstamped image. Neither field_provenance nor entity_curation has a column
// for one, and a URL attributes a fetched artifact rather than a value copied into a
// tag, so there is nowhere honest to put it.
func checkFieldAttribution(attr model.Attribution, op string) (model.Attribution, error) {
	attr = attr.OrUser()
	if err := checkAttribution(attr, attr.ValidForField, op); err != nil {
		return attr, err
	}
	if attr.SourceURL != "" {
		return attr, waxerr.New(waxerr.CodeInvalid, op,
			"a scalar field records no fetch URL: "+attr.SourceURL)
	}
	return attr, nil
}

// checkAttribution refuses an attribution the surface ok guards cannot hold, naming the
// field that is actually wrong. The pairing failures are reported as pairing failures:
// a caller whose provider is missing is not told its source is bad, which is the one
// thing about the write that was right.
func checkAttribution(attr model.Attribution, ok func() bool, op string) error {
	if ok() {
		return nil
	}
	switch {
	case attr.Source == model.SourceEnrichment && attr.Provider == "":
		return waxerr.New(waxerr.CodeInvalid, op,
			"an enrichment source must name the provider that supplied it")
	case attr.Provider != "" && attr.Source.Valid():
		return waxerr.New(waxerr.CodeInvalid, op,
			"only an enrichment source names a provider, not "+string(attr.Source))
	default:
		return waxerr.New(waxerr.CodeInvalid, op, "invalid provenance source: "+string(attr.Source))
	}
}

// SetFieldProvenance records that a field was set by a non-tag source, storing the
// curated value and preserving any lock. Enrichment and edit paths should use this
// method. It does not clobber a locked field unless force is set, such as a user edit
// overriding that user's own lock.
//
// The row is re-attributed whole: the caller's source and provider replace whatever was
// there, so a provider left over from a previous source does not survive a write that
// names a different one. Only the lock is preserved.
func (s *Store) SetFieldProvenance(ctx context.Context, itemPID model.PID, field string, attr model.Attribution, value string, force bool) error {
	const op = "store.SetFieldProvenance"
	if !model.IsMetadataField(field) {
		return waxerr.New(waxerr.CodeInvalid, op, "not a metadata field: "+field)
	}
	attr, err := checkFieldAttribution(attr, op)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, err := itemIDByPID(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		if !force {
			locked, err := fieldLockedTx(ctx, tx, itemID, field)
			if err != nil {
				return err
			}
			if locked {
				return waxerr.New(waxerr.CodeConflict, op, "field is locked: "+field)
			}
		}
		// The shared upsert with no lock instruction, which is what preserves the stored
		// lock, and what carries the provider a bespoke statement here used to drop.
		if err := upsertEditProvenanceTx(ctx, tx, itemID, field, attr, value, model.LockUnchanged, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// FieldProvenance returns an item's provenance rows (only the non-default fields
// are present), with the item's own front cover reported as an "art" row. An item with
// no curated/locked fields and no cover returns an empty slice; a nonexistent item
// returns CodeNotFound (distinguished from "all tag-sourced", so a bad pid is reported
// rather than rendered as a clean tag-only item).
//
// The "art" and "lyrics" rows are overlays, never replacements. A field_provenance row
// for either can exist with no artifact attached at all, which is what locking one to
// stop the scanner filling it looks like, so that row survives on its own; when the
// item does carry the artifact, its own source, provider, URL and timestamp are laid
// over the row, and when there is no row at all one is inserted in sort position. The
// overlay is what makes them truthful when the artifact is present, because a lock row
// only ever carries what its own write knew: a curation set records the attribution it
// was given, while `waxbin lock <pid> art` goes through LockField and can only guess
// "tag". Enrichment writes no provenance row for lyrics at all.
//
// Only the item's own cover appears; art inherited from an album or artist is not the
// item's field, matching has_art's semantics. Value stays empty on both overlaid rows,
// since the value is bytes.
func (s *Store) FieldProvenance(ctx context.Context, itemPID model.PID) ([]model.FieldProvenance, error) {
	const op = "store.FieldProvenance"
	itemID, err := itemIDByPIDRead(ctx, s.read, itemPID, op)
	if err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx, `SELECT fp.field, fp.source, fp.locked,
		COALESCE(fp.value,''), COALESCE(fp.provider,''), fp.updated_at
		FROM field_provenance fp WHERE fp.item_id = ? ORDER BY fp.field`, itemID)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer rows.Close()
	var out []model.FieldProvenance
	for rows.Next() {
		var fp model.FieldProvenance
		var source string
		var locked int
		if err := rows.Scan(&fp.Field, &source, &locked, &fp.Value, &fp.Provider, &fp.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		fp.ItemPID = itemPID
		fp.Source = model.ProvenanceSource(source)
		fp.Locked = locked == 1
		out = append(out, fp)
	}
	if err := rows.Err(); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The item's own cover sits under the kind-switched art slot the query fields use:
	// a track's and a book's under 'track', an episode's under 'episode'.
	out, err = s.overlayArtifact(ctx, itemPID, itemID, out, "art",
		`SELECT am.source, am.provider, am.source_url, am.updated_at
		 FROM art_map am JOIN playable_item pi ON pi.id = am.entity_id
		 WHERE pi.id = ? AND am.entity_type = `+itemArtSlotExpr+` AND am.role = 'front'`)
	if err != nil {
		return nil, err
	}
	return s.overlayArtifact(ctx, itemPID, itemID, out, "lyrics",
		"SELECT source, provider, '', updated_at FROM lyrics WHERE item_id = ?")
}

// overlayArtifact folds one structured artifact's own attribution into an item's
// provenance rows: it rewrites that field's row or inserts one in sort position, and
// leaves the rows alone when the item carries no such artifact. q selects the
// artifact's source, provider, url, and timestamp for one item id. rows is ordered by
// field, which the insert preserves.
func (s *Store) overlayArtifact(ctx context.Context, itemPID model.PID, itemID int64, rows []model.FieldProvenance, field, q string) ([]model.FieldProvenance, error) {
	const op = "store.FieldProvenance"
	var source string
	fp := model.FieldProvenance{ItemPID: itemPID, Field: field}
	err := s.read.QueryRowContext(ctx, q, itemID).Scan(&source, &fp.Provider, &fp.SourceURL, &fp.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rows, nil
	}
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	fp.Source = model.ProvenanceSource(source)
	at, found := slices.BinarySearchFunc(rows, field,
		func(r model.FieldProvenance, f string) int { return strings.Compare(r.Field, f) })
	if found {
		fp.Locked = rows[at].Locked
		rows[at] = fp
		return rows, nil
	}
	return slices.Insert(rows, at, fp), nil
}

// LockedFields returns the set of an item's locked fields in one query, so a writer
// checking several fields (organize tag write-back) does not issue one SELECT per
// field. An item with no locks returns an empty (non-nil) map.
func (s *Store) LockedFields(ctx context.Context, itemPID model.PID) (map[string]bool, error) {
	rows, err := s.read.QueryContext(ctx, `SELECT fp.field
		FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.locked = 1`, string(itemPID))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "store.LockedFields", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "store.LockedFields", err)
		}
		out[field] = true
	}
	return out, rows.Err()
}

// IsFieldLocked reports whether an item field is locked. It is the guard a writer
// (organize tag write-back, enrichment) calls before overwriting a field, so
// curated data survives. A missing row means unlocked.
func (s *Store) IsFieldLocked(ctx context.Context, itemPID model.PID, field string) (bool, error) {
	var locked int
	err := s.read.QueryRowContext(ctx, `SELECT fp.locked
		FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? AND fp.field = ?`, string(itemPID), field).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.IsFieldLocked", err)
	}
	return locked == 1, nil
}

// fieldLockedTx reports whether an item field is locked in field_provenance. It takes
// a queryer for the same reason entityFieldLockedTx does.
func fieldLockedTx(ctx context.Context, q queryer, itemID int64, field string) (bool, error) {
	var locked int
	err := q.QueryRowContext(ctx,
		"SELECT locked FROM field_provenance WHERE item_id=? AND field=?", itemID, field).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, waxerr.Wrap(waxerr.CodeIO, "store.fieldLocked", err)
	}
	return locked == 1, nil
}

// itemIDByPID resolves a public item id to its rowid, or CodeNotFound.
func itemIDByPID(ctx context.Context, tx *sql.Tx, pid model.PID, op string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM playable_item WHERE pid = ?", string(pid)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, waxerr.New(waxerr.CodeNotFound, op, "no such item: "+string(pid))
	}
	if err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return id, nil
}
