package waxbin

import (
	"context"
	"strings"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// This file exposes the entity-curation edit API (identifiers and sort-name overrides
// on a shared artist/release-group/album) on the Library facade. The edit is catalog-
// first: a set records user provenance and, by default, locks the entity field so an
// enrichment pass preserves it. With opts.WriteBack the fanned identifiers/sort are also
// mirrored into every member file's on-disk tags.

// EntityEditOptions controls an entity field edit, mirroring EditOptions.
type EntityEditOptions struct {
	// WriteBack also fans the edited identifiers and sort across the entity's member
	// files' on-disk tags: an album's BARCODE, LABEL, CATALOGNUMBER, and ALBUMSORT, and an
	// artist's ARTISTSORT. A release-group field, a release-group type, and an entity MBID
	// have no fanned tag and stay DB-only.
	WriteBack bool
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
//
// With opts.WriteBack the edited values that round-trip through a rescan are also fanned
// out across the entity's member files' on-disk tags. Write-back runs after the catalog
// edit committed, so a file that cannot be written is reported through a *WriteBackError
// naming the failed files while the entity edit stands.
func (l *Library) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, opts EntityEditOptions) error {
	if err := l.store.EditEntityFields(ctx, entityType, entityPID, edits, model.SourceUser, opts.Lock, opts.Force); err != nil {
		return err
	}
	if !opts.WriteBack {
		return nil
	}
	return l.writeBackEntity(ctx, entityType, entityPID, edits)
}

// EntityCuration returns an entity's curation rows (only non-default fields have rows,
// so an un-curated entity returns an empty slice).
func (l *Library) EntityCuration(ctx context.Context, entityType model.MergeEntity, entityPID model.PID) ([]model.EntityCuration, error) {
	return l.store.EntityCuration(ctx, entityType, entityPID)
}

// writeBackEntity fans a committed entity edit out across the entity's member files'
// on-disk tags. It runs after the catalog edit committed, so a refusal or failure is
// reported as a *WriteBackError rather than a hard error. An edit that touched no
// round-trippable field (a release-group edit, a type edit, or any entity MBID edit)
// writes nothing to disk and returns nil, since those values are DB-only by design.
func (l *Library) writeBackEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string) error {
	tagEdits := entityTagEditsForFields(entityType, edits)
	if len(tagEdits) == 0 {
		return nil
	}
	files, err := l.store.EntityMemberFiles(ctx, entityType, entityPID)
	if err != nil {
		return writeBackSetupFailure(entityPID, edits, err)
	}
	// An entity with no present member files has nothing to fan out to; the catalog edit
	// stands, so this is a clean no-op rather than a failure (unlike a single item's
	// write-back, which reports its own missing file).
	if len(files) == 0 {
		return nil
	}
	wbErr := &WriteBackError{ItemPID: entityPID, Edits: edits}
	if err := l.writeBackFiles(ctx, "waxbin.EditEntity", files, wbErr,
		func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.Apply(ctx, path, tagEdits)
		}); err != nil {
		return err
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}

// entityTagEditsForFields maps a committed entity edit to the on-disk tags that fan out
// across the member files. A field with no fanned tag (a release-group field, a type or
// artist MBID) is skipped: those values stay DB-only. Values are trimmed and
// identifier-normalized the way the store normalized them before commit (barcode is
// the one identifier here), so the fanned tag always carries the stored form. A value
// empty after trimming clears its tag.
func entityTagEditsForFields(entityType model.MergeEntity, edits map[string]string) []meta.TagEdit {
	out := make([]meta.TagEdit, 0, len(edits))
	for field, value := range edits {
		key, ok := meta.EntityFieldTagKey(entityType, field)
		if !ok {
			continue
		}
		e := meta.TagEdit{Key: key}
		if v, _ := model.NormalizeIdentifierField(field, strings.TrimSpace(value)); v != "" {
			e.Values = []string{v}
		}
		out = append(out, e)
	}
	return out
}
