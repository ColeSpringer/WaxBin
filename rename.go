package waxbin

import (
	"context"
	"maps"
	"slices"

	"github.com/colespringer/waxbin/model"
)

// This file exposes the entity-rung rename on the Library facade: moving a whole album
// or release group onto a new name, keeping the row and everything attached to it.
// EditEntity next door edits an entity's curation columns, which never key it; this one
// edits the keying fields of every member at once, which is the only thing that moves an
// entity's identity.

// RenameOptions controls an entity rename. It mirrors EditOptions, minus SkipLocked:
// skipping a locked member would drop it out of the coverage the in-place rename needs
// and silently split the entity in two, which is the failure this verb exists to remove.
type RenameOptions struct {
	// WriteBack also writes the new values into every member file's on-disk tags. The
	// values live in each member's ALBUM/ALBUMARTIST/DATE/ARTIST rather than at the
	// entity level, so this is the per-track tag fan, not the entity one. Without it the
	// catalog change lasts until a scan re-resolves the members from their unchanged
	// files.
	WriteBack bool
	// Lock is the instruction for each edited field's lock on every member, and for each
	// moved credit's own credit.<role> lock. The default locks both, so an artist rename
	// leaves every item whose credit it rewrote needing Force to have that role set again.
	Lock model.LockChange
	// Force overrides a locked keying field, or a locked credit role, on a member.
	Force bool
	// Source is where the values came from; empty records a user edit.
	Source model.ProvenanceSource
	// Provider names the service that supplied an enrichment value, and is required
	// with that source and refused with any other.
	Provider string
}

// Attribution is the store-side value Source and Provider describe. See
// EditOptions.Attribution.
func (o RenameOptions) Attribution() model.Attribution {
	return model.Attribution{Source: o.Source, Provider: o.Provider}
}

// RenameEntity renames a whole album, release group or artist by editing the keying
// fields of every one of its members in one transaction, so the entity's identity key
// moves and the row stays: the pid, art, curation, stars and enrichment marker all
// survive. The report says which branch it took, renamed, merged or refreshed, and names
// the survivor when a taken key folded the entity into an incumbent.
//
// Renamable fields are the ones that key the rung: album, album_artist and year on an
// album, album and album_artist on a release group, and name on an artist. The artist
// rung takes one field because the item-level field it writes differs per reference kind,
// which it works out itself; each referring credit list keeps its other names.
//
// Renaming a name to nothing is refused, as is a member with a locked keying field
// (without opts.Force), an archived member with no primary file, members that would land
// on different keys (their files sit in different folders, which the heuristic album key
// carries), and a release group whose albums are titled apart. Each of those would
// otherwise leave the entity split in two with no report saying so.
//
// An artist's contributor credits move with it. The roles that back no item field of
// their own (producer, composer, narrator, translator, editor) are applied on the credit
// surface inside the same transaction, and the report counts them separately from the
// members. A locked credit refuses like a locked keying field does, and takes the same
// opts.Force.
//
// One side effect is worth stating rather than discovering: on an album whose members
// carry no album_artist of their own, the release group is anchored on the first credited
// artist, so renaming album_artist here moves that anchor and can carry the album under a
// different group. The report's MovedAlbums names it when it happens.
//
// With opts.WriteBack the new values are also written into each member file's tags,
// which is what makes the rename survive the next scan that re-resolves them. Write-back
// runs after the catalog change committed, so a file that cannot be written is reported
// through a *WriteBackError while the rename itself stands.
func (l *Library) RenameEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID,
	fields map[string]string, opts RenameOptions) (*model.EntityRenameReport, error) {
	rep, err := l.store.RenameEntity(ctx, entityType, entityPID, fields,
		opts.Attribution(), opts.Lock, opts.Force)
	if err != nil {
		return nil, err
	}
	if !opts.WriteBack {
		return rep, nil
	}
	return rep, l.writeBackRename(ctx, entityPID, fields, rep)
}

// writeBackRename fans the renamed values across the member files. The rename's values
// are per-track tags (ALBUM, ALBUMARTIST, DATE, ARTIST) plus, at the artist rung, the
// credit tags of the contributor roles that moved with it, so this is a per-member tag
// fan rather than the entity-level one writeBackEntity uses for identifiers.
//
// Both halves are grouped onto one pass per item. An artist who is a track's performing
// credit and its producer produces a field edit and a credit edit on the same item, and
// fanning them out separately would rewrite that file twice.
//
// The member list and the values come from the report, which is what the rename actually
// wrote. Re-deriving them here would have to ask an entity for its members, and after a
// merge the only entity left is the incumbent, whose own files were never part of the
// rename and must not be rewritten. A failure never rolls the rename back, and the
// failures are collected across members into one error so a partial disk sync names every
// file it did not reach. A credit with no round-trippable tag (a book translator or
// editor) lands there as a refusal while the catalog rename stands.
func (l *Library) writeBackRename(ctx context.Context, entityPID model.PID,
	fields map[string]string, rep *model.EntityRenameReport) error {
	if rep == nil || (len(rep.MemberEdits) == 0 && len(rep.CreditEdits) == 0) {
		return nil
	}
	byItem := make(map[model.PID]map[string]string, len(rep.MemberEdits))
	credits := make(map[model.PID][]creditRoleEdit, len(rep.CreditEdits))
	out := BatchEditResult{Edited: make([]model.PID, 0, len(rep.MemberEdits))}
	for _, e := range rep.MemberEdits {
		byItem[e.ItemPID] = e.Fields
		out.Edited = append(out.Edited, e.ItemPID)
	}
	for _, e := range rep.CreditEdits {
		if _, seen := byItem[e.ItemPID]; !seen && len(credits[e.ItemPID]) == 0 {
			out.Edited = append(out.Edited, e.ItemPID)
		}
		credits[e.ItemPID] = append(credits[e.ItemPID], creditRoleEdit{role: e.Role, names: e.Names})
	}
	if err := l.batchWriteBack(ctx, &out, func(pid model.PID) error {
		return l.writeBackItemEdits(ctx, "waxbin.RenameEntity", pid, byItem[pid], credits[pid])
	}); err != nil {
		return err
	}
	return foldWriteBackErrors(entityPID, fields, out.WriteBackErrors)
}

// foldWriteBackErrors collapses a per-member write-back map into the one typed error a
// single-entity verb returns. Every failed file is still named, since that is what the
// caller needs to re-write; which member it hung off is not, because the rename wrote the
// same values to all of them.
func foldWriteBackErrors(pid model.PID, edits map[string]string, byItem map[model.PID]*WriteBackError) error {
	if len(byItem) == 0 {
		return nil
	}
	out := &WriteBackError{ItemPID: pid, Edits: edits}
	for _, member := range slices.Sorted(maps.Keys(byItem)) {
		out.Failures = append(out.Failures, byItem[member].Failures...)
	}
	return out
}
