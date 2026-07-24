package sqlite

import (
	"context"
	"database/sql"
	"errors"
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
// identifier/sort-bearing entities are editable; genre is not.
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
// columns) and records an entity_curation row per field with the source and, when lock
// is set, a lock that protects the value from an enrichment overwrite. One item change
// delta is emitted for every item the entity backs, plus an entity delta, so a delta
// consumer re-resolves the affected rows.
//
// A field that does not apply to the entity type is CodeInvalid; a genre (or any other
// non-curatable entity type) is CodeUnsupported. A locked field is refused with
// CodeLocked unless force is set. On-disk tag write-back of these values fans out
// across the entity's member files and is sequenced separately; this edit is
// DB-only.
func (s *Store) EditEntityFields(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, source model.ProvenanceSource, lock, force bool) error {
	const op = "store.EditEntityFields"
	table, ok := entityTableFor(entityType)
	if !ok {
		return waxerr.New(waxerr.CodeUnsupported, op, "entity editing is not supported for a "+string(entityType)+" entity")
	}
	if len(edits) == 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "no fields to edit")
	}
	if !source.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "invalid provenance source: "+string(source))
	}
	// Validate every field name and value up front so a bad edit is rejected before any
	// write. Iterate in a stable order so the edit is deterministic.
	fields := make([]string, 0, len(edits))
	for f := range edits {
		if !model.IsEntityEditField(entityType, f) {
			return waxerr.New(waxerr.CodeInvalid, op, "field "+f+" is not editable on a "+string(entityType)+" entity")
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
				return err
			}
		case "type":
			if v != "" && !model.ValidReleaseGroupType(v) {
				return waxerr.New(waxerr.CodeInvalid, op, "invalid release-group type: "+v)
			}
		case "barcode":
			// Normalized before the norm map is built, like the item-edit identifier
			// fields, so the stored column, the curation row, and the fanned tag all
			// carry the canonical digits.
			nv, ok := model.NormalizeBarcode(v)
			if !ok {
				return waxerr.New(waxerr.CodeInvalid, op, "invalid barcode value: "+v)
			}
			v = nv
		}
		norm[f] = v
	}

	return s.writeTx(ctx, func(tx *sql.Tx) error {
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
		for _, f := range fields {
			if err := applyEntityFieldTx(ctx, tx, entityType, table, entityID, f, norm[f]); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if err := upsertEntityCurationTx(ctx, tx, string(entityType), entityID, f, source, norm[f], lock, now); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, op, err)
			}
		}

		// Fan a delta out to every item the entity backs, so a delta consumer re-resolves
		// the changed identifier/sort, then an entity delta.
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

// upsertEntityCurationTx writes an entity field's curation row with the source, the
// curated value, and the lock bit. It mirrors upsertEditProvenanceTx for the entity
// scope.
func upsertEntityCurationTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, field string, source model.ProvenanceSource, value string, lock bool, now int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO entity_curation(entity_type, entity_id, field, source, locked, value, updated_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(entity_type, entity_id, field) DO UPDATE SET
			source=excluded.source, locked=excluded.locked, value=excluded.value, updated_at=excluded.updated_at`,
		entityType, entityID, field, string(source), boolInt(lock), value, now)
	return err
}

// entityFieldLockedTx reports whether an entity field is locked in entity_curation. A
// missing row means unlocked. It is the guard enrichment calls before overwriting a
// shared entity field.
func entityFieldLockedTx(ctx context.Context, tx *sql.Tx, entityType string, entityID int64, field string) (bool, error) {
	var locked int
	err := tx.QueryRowContext(ctx,
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
	rows, err := s.read.QueryContext(ctx, `SELECT field, source, locked, COALESCE(value,''), updated_at
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
		if err := rows.Scan(&ec.Field, &source, &locked, &ec.Value, &ec.UpdatedAt); err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		ec.Source = model.ProvenanceSource(source)
		ec.Locked = locked == 1
		out = append(out, ec)
	}
	return out, rows.Err()
}
