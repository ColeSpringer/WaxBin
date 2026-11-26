package waxbin

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// This file exposes the entity-curation edit API (identifiers and sort-name overrides
// on a shared artist/release-group/album) on the Library facade. The edit is catalog-
// first: a set records user provenance and, by default, locks the entity field so an
// enrichment pass preserves it. With opts.WriteBack the fanned identifiers/sort are also
// mirrored into every member file's on-disk tags, and an mbid clear strips that id from
// them so the next scan of those files cannot put the identity back.

// EntityEditOptions controls an entity field edit, mirroring EditOptions.
type EntityEditOptions struct {
	// WriteBack also fans the edited identifiers and sort across the entity's member
	// files' on-disk tags: an album's MUSICBRAINZ_ALBUMID, BARCODE, LABEL, CATALOGNUMBER,
	// RELEASECOUNTRY and ALBUMSORT, a release group's MUSICBRAINZ_RELEASEGROUPID, and an
	// artist's ARTISTSORT. An id goes on every member file or none (meta.EntityFieldTagKey).
	// A release group's type and an artist's id stay DB-only. Clearing an album's or a
	// release group's mbid strips that one id from the member files, which is what makes
	// the clear survive the next scan that re-resolves them.
	WriteBack bool
	// Lock is the instruction for each edited field's lock, which guards it against an
	// enrichment overwrite. The zero value leaves the stored lock alone; the CLI states
	// LockOn or LockOff explicitly.
	Lock model.LockChange
	// Force overrides a locked entity field.
	Force bool
	// Source is where the values came from; empty records a user edit.
	Source model.ProvenanceSource
	// Provider names the service that supplied an enrichment value, and is required
	// with that source and refused with any other.
	Provider string
}

// Attribution is the store-side value Source and Provider describe. See
// EditOptions.Attribution.
func (o EntityEditOptions) Attribution() model.Attribution {
	return model.Attribution{Source: o.Source, Provider: o.Provider}
}

// EditEntity applies curation edits to one shared entity (an artist, release group, or
// album): sort-name overrides and release identifiers (barcode/label/catalog number and
// the entity MBIDs, plus the release-group type). It records opts.Source (empty means a
// user edit) and applies opts.Lock to each edited field. The catalog write is atomic. A
// field that does not apply to the entity type, or an invalid value, is rejected; a
// locked field returns CodeLocked unless opts.Force is set. Clearing an mbid re-keys the
// entity chain, and when that lands on a heuristic twin the entity merges into it, which
// the returned report names so a caller does not go on talking about a pid that is gone;
// only such a merging clear is refused alongside another field, with CodeConflict naming
// the survivor to edit instead, since the merge deletes the row those other values were
// written to. The report's Merges names every entity the clear merged into another, each
// with the one it went into: the entity itself when it merged into its twin, and every
// album the re-key carried onto a key another album held.
//
// With opts.WriteBack the edited values that round-trip through a rescan are also fanned
// out across the entity's member files' on-disk tags, and an album's or a release group's
// mbid clear strips that id from them. The strip is what makes the clear durable: without
// it the member files still name the id, so the linkage returns on the next scan that
// re-resolves them, which is their next retag, move, or content change rather than every
// scan. A set id is written only when every member file can take it, and is otherwise
// held from all of them and left owed. Write-back runs after the catalog edit committed,
// so a file that cannot be written, or an id held, is reported through a *WriteBackError
// while the entity edit stands.
func (l *Library) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, opts EntityEditOptions) (*model.EntityEditReport, error) {
	rep, err := l.store.EditEntityFields(ctx, entityType, entityPID, edits,
		opts.Attribution(), opts.Lock, opts.Force)
	if err != nil {
		return nil, err
	}
	if !opts.WriteBack {
		return rep, nil
	}
	return rep, l.writeBackEntity(ctx, entityType, entityPID, edits, rep)
}

// entityOwedKeys names the owed keys an entity write-back pays on the member files it
// writes (model.DiagTagWriteOwed): "<entity>.<field>" for each edited field it fans out,
// and the id an album's or a release group's mbid clear strips.
func entityOwedKeys(entityType model.MergeEntity, edits map[string]string, cleared []string) []string {
	var out []string
	for field, v := range edits {
		if field == "mbid" && strings.TrimSpace(v) == "" {
			continue
		}
		if model.EntityFieldWritable(entityType, field) {
			out = append(out, string(entityType)+"."+field)
		}
	}
	if len(entityMBIDStripEdits(entityType, edits, cleared)) > 0 {
		out = append(out, string(entityType)+".mbid")
	}
	return out
}

// EntityCuration returns an entity's curation rows (only non-default fields have rows,
// so an un-curated entity returns an empty slice).
func (l *Library) EntityCuration(ctx context.Context, entityType model.MergeEntity, entityPID model.PID) ([]model.EntityCuration, error) {
	return l.store.EntityCuration(ctx, entityType, entityPID)
}

// writeBackEntity fans a committed entity edit out across the entity's member files'
// on-disk tags: the values that round-trip through a rescan, and the id strip an album's
// or a release group's mbid clear leaves behind. Both go into one pass over the files, so
// a clear carrying sibling edits writes each file once. It runs after the catalog edit
// committed, so a refusal or failure is reported as a *WriteBackError rather than a hard
// error. An edit that touched no round-trippable field and cleared no mbid (a type edit,
// an artist's id) writes nothing to disk and returns nil, since those values are DB-only
// by design.
//
// A set id is held from every file when one of them cannot take it (entityIDHeld): the
// other fields still fan, the id stays owed on every file, and each file is reported with
// the reason under the id's key.
//
// The files are those of the members the edit found under the entity (rep.MemberPIDs), so
// the fan follows them wherever a clear took them, merged into a twin or settled under a
// group of their own, and never reaches the twin's or that group's other members, which
// were no part of the edit.
func (l *Library) writeBackEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, rep *model.EntityEditReport) error {
	tagEdits := entityTagEditsForFields(entityType, edits)
	strips := entityMBIDStripEdits(entityType, edits, rep.ClearedIDs)
	if len(tagEdits)+len(strips) == 0 {
		return nil
	}
	files, err := l.store.MemberFiles(ctx, rep.MemberPIDs)
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
	owed := entityOwedKeys(entityType, edits, rep.ClearedIDs)
	if idKey, ok := meta.EntityFieldTagKey(entityType, "mbid"); ok && strings.TrimSpace(edits["mbid"]) != "" {
		if reason := l.entityIDHeld(ctx, files, append(slices.Clone(tagEdits), strips...), idKey); reason != "" {
			reason = "the " + strings.ReplaceAll(string(entityType), "_", " ") + "'s id goes on every member file or none, and " + reason
			tagEdits = slices.DeleteFunc(tagEdits, func(e meta.TagEdit) bool { return e.Key == idKey })
			owed = slices.DeleteFunc(owed, func(k string) bool { return k == string(entityType)+".mbid" })
			for _, ref := range files {
				wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: string(ref.Path), Key: idKey, Reason: reason})
			}
		}
	}
	tagEdits = append(tagEdits, strips...)
	if len(tagEdits) == 0 {
		return wbErr.result()
	}
	if err := l.writeBackFiles(ctx, "waxbin.EditEntity", model.OriginEdit, files, wbErr, nil, owed,
		wholeFile(func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.Apply(ctx, path, tagEdits)
		})); err != nil {
		return err
	}
	// A track whose file took the artist's ARTISTSORT or the album's ALBUMSORT spells its
	// artist or album that way now, as the next scan of the file would read it.
	if v, ok := edits["sort"]; ok && (entityType == model.MergeArtist || entityType == model.MergeAlbum) {
		var landed []model.PID
		for _, ref := range files {
			if wbErr.landed[ref.FilePID] {
				landed = append(landed, ref.FilePID)
			}
		}
		follow := l.store.FollowArtistSortWrite
		if entityType == model.MergeAlbum {
			follow = l.store.FollowAlbumSortWrite
		}
		// A sort edit never rides a clear that merges (the store refuses the pair), so the
		// entity is still the one the caller named.
		if err := follow(ctx, entityPID, landed, strings.TrimSpace(v)); err != nil {
			l.log.Warn("sort spelling after write-back", "entity", string(entityType), "pid", entityPID, "err", err)
		}
	}
	return wbErr.result()
}

// entityMBIDStripEdits returns the tag strips an mbid clear fans across the entity's
// member files, and nothing for any other edit. It strips exactly the ids the clear took
// back (cleared, EntityEditReport.ClearedIDs), from the files that name one: an album
// clear takes them from MUSICBRAINZ_ALBUMID, a release-group clear from
// MUSICBRAINZ_RELEASEGROUPID, and neither touches the other's. A file naming another id,
// such as one an album merge brought in, keeps it. An artist clear re-keys nothing, so it
// strips nothing.
//
// Leaving the other id alone is the point of the rule. A release-group clear re-keys only
// the albums whose own key embeds the group segment and leaves an album keyed on its own
// release id standing (dependentAlbumsTx), so a member that keeps its release id
// re-resolves onto that same intact row. Stripping it as well would mint a heuristic twin
// and drain the identified album of its art and curation. Detach strips both ids because
// it pulls one member out of the identified chain entirely, which is a different gesture
// with a different answer.
func entityMBIDStripEdits(entityType model.MergeEntity, edits map[string]string, cleared []string) []meta.TagEdit {
	v, ok := edits["mbid"]
	if !ok || strings.TrimSpace(v) != "" || len(cleared) == 0 {
		return nil
	}
	switch entityType {
	case model.MergeAlbum:
		return []meta.TagEdit{{Key: "MUSICBRAINZ_ALBUMID", Drop: cleared}}
	case model.MergeReleaseGroup:
		return []meta.TagEdit{{Key: "MUSICBRAINZ_RELEASEGROUPID", Drop: cleared}}
	}
	return nil
}

// entityTagEditsForFields maps a committed entity edit to the on-disk tags that fan out
// across the member files. A field with no fanned tag (a type, an artist's id) is
// skipped: those values stay DB-only. A cleared id is skipped too, since what a clear
// takes off the files is entityMBIDStripEdits' business, not a value fan. Values are
// trimmed and identifier-normalized the way the store normalized them before commit
// (barcode and country both dispatch through NormalizeIdentifierField), so the fanned tag
// always carries the stored form. A value empty after trimming clears its tag.
func entityTagEditsForFields(entityType model.MergeEntity, edits map[string]string) []meta.TagEdit {
	out := make([]meta.TagEdit, 0, len(edits))
	for field, value := range edits {
		key, ok := meta.EntityFieldTagKey(entityType, field)
		if !ok || field == "mbid" && strings.TrimSpace(value) == "" {
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

// entityIDHeld is why an album's or a release group's id cannot go on its member files,
// or "" when every one of them can take it: on disk, in a writable library, backing its
// item alone as a whole file, and taking the write's edits with the id's key (idKey)
// stored as given, as the tag writer judges a write prepared and not made (meta.Writer
// Check, which also asks whether the file can be replaced now). The id goes on all of
// them or none, since a catalog rebuilt from a half-written album would key the tagged
// members apart from the rest.
func (l *Library) entityIDHeld(ctx context.Context, files []model.ItemFileRef, edits []meta.TagEdit, idKey string) string {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return "the libraries could not be read: " + err.Error()
	}
	readOnly := readOnlyIDs(libs)
	w := meta.NewWriter()
	for _, ref := range files {
		file, err := l.store.FileByPID(ctx, ref.FilePID)
		if err != nil {
			return ref.DisplayPath + " could not be read: " + err.Error()
		}
		if readOnly[file.LibraryID] {
			return ref.DisplayPath + " is in a read-only library"
		}
		shared, err := l.store.FileSharedOrVirtual(ctx, ref.FilePID)
		if err != nil {
			return ref.DisplayPath + " could not be read: " + err.Error()
		}
		if shared {
			return ref.DisplayPath + " backs several items or a window of a cue rip"
		}
		if _, err := os.Stat(pathx.Long(string(file.Path))); err != nil {
			return ref.DisplayPath + " is not on disk"
		}
		lost, err := w.Check(ctx, string(file.Path), edits)
		if heldForGood(err, lost) {
			l.noteTagWriteRefused(ctx, ref.FilePID, string(file.Path))
		}
		switch {
		case waxerr.Is(err, waxerr.CodeUnsupported):
			return ref.DisplayPath + " cannot take tag writes in its current form"
		case err != nil:
			return ref.DisplayPath + " cannot be written now: " + err.Error()
		case slices.ContainsFunc(lost, func(k string) bool { return strings.EqualFold(k, idKey) }):
			return ref.DisplayPath + " cannot store it"
		}
	}
	return ""
}
