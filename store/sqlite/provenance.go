package sqlite

import (
	"context"
	"database/sql"
	"errors"

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
	if !model.IsMetadataField(field) {
		return waxerr.New(waxerr.CodeInvalid, op, "not a lockable metadata field: "+field)
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		itemID, kind, err := itemIDKindByPIDTx(ctx, tx, itemPID, op)
		if err != nil {
			return err
		}
		// Which fields are lockable depends on the kind. MetadataFields is the union of
		// the track and book vocabularies, so guard against locking a field that does not
		// apply to this item's kind, such as author on a track. That would leave an inert
		// junk provenance row, the very thing the whitelist exists to prevent.
		if allowed := editableFieldsForKind(kind); allowed == nil || !allowed[field] {
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
		// non-tag provenance nor a curated value.
		if !locked {
			if _, err := tx.ExecContext(ctx, `DELETE FROM field_provenance
				WHERE item_id=? AND field=? AND locked=0 AND source=? AND (value IS NULL OR value='')`,
				itemID, field, string(model.SourceTag)); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// SetFieldProvenance records that a field was set by a non-tag source, storing
// the curated value and preserving any lock. Enrichment and edit paths should use
// this method. It does not clobber a locked field unless force is set, such as a
// user edit overriding that user's own lock.
func (s *Store) SetFieldProvenance(ctx context.Context, itemPID model.PID, field string, source model.ProvenanceSource, value string, force bool) error {
	const op = "store.SetFieldProvenance"
	if !model.IsMetadataField(field) {
		return waxerr.New(waxerr.CodeInvalid, op, "not a metadata field: "+field)
	}
	if !source.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "invalid provenance source: "+string(source))
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
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_provenance(item_id, field, source, locked, value, updated_at)
			VALUES (?,?,?,0,?,?)
			ON CONFLICT(item_id, field) DO UPDATE SET source=excluded.source, value=excluded.value, updated_at=excluded.updated_at`,
			itemID, field, string(source), value, nowNS()); err != nil {
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return appendChange(ctx, tx, "item", itemPID, model.OpUpdate)
	})
}

// FieldProvenance returns an item's provenance rows (only the non-default fields
// are present). An item with no curated/locked fields returns an empty slice; a
// nonexistent item returns CodeNotFound (distinguished from "all tag-sourced", so
// a bad pid is reported rather than rendered as a clean tag-only item).
func (s *Store) FieldProvenance(ctx context.Context, itemPID model.PID) ([]model.FieldProvenance, error) {
	const op = "store.FieldProvenance"
	if _, err := itemIDByPIDRead(ctx, s.read, itemPID, op); err != nil {
		return nil, err
	}
	rows, err := s.read.QueryContext(ctx, `SELECT fp.field, fp.source, fp.locked,
		COALESCE(fp.value,''), COALESCE(fp.provider,''), fp.updated_at
		FROM field_provenance fp JOIN playable_item pi ON pi.id = fp.item_id
		WHERE pi.pid = ? ORDER BY fp.field`, string(itemPID))
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
	return out, rows.Err()
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

func fieldLockedTx(ctx context.Context, tx *sql.Tx, itemID int64, field string) (bool, error) {
	var locked int
	err := tx.QueryRowContext(ctx,
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
