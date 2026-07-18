package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// This file exposes the entity-curation edit API (identifiers and sort-name overrides
// on a shared artist/release-group/album) on the Library facade. Like the item edit it
// is catalog-only in this phase: a set records user provenance and, by default, locks
// the entity field so an enrichment pass preserves it. Fanning the values out into the
// entity's member files' on-disk tags is a later, opt-in write-back concern.

// EntityEditOptions controls an entity field edit, mirroring EditOptions.
type EntityEditOptions struct {
	// Lock locks each edited entity field against enrichment overwrites; on by default.
	Lock bool
	// Force overrides a locked entity field.
	Force bool
}

// EditEntity applies curation edits to one shared entity (an artist, release group, or
// album): sort-name overrides and release identifiers (barcode/label/catalog number and
// the entity MBIDs, plus the release-group type). It records user provenance and, by
// default, locks each edited field so enrichment leaves it alone. The catalog write is
// atomic. A field that does not apply to the entity type, or an invalid value, is
// rejected; a locked field returns CodeLocked unless opts.Force is set.
func (l *Library) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, opts EntityEditOptions) error {
	return l.store.EditEntityFields(ctx, entityType, entityPID, edits, model.SourceUser, opts.Lock, opts.Force)
}

// EntityCuration returns an entity's curation rows (only non-default fields have rows,
// so an un-curated entity returns an empty slice).
func (l *Library) EntityCuration(ctx context.Context, entityType model.MergeEntity, entityPID model.PID) ([]model.EntityCuration, error) {
	return l.store.EntityCuration(ctx, entityType, entityPID)
}
