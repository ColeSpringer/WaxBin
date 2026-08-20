package waxbin

import (
	"context"
	"strconv"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
)

// This file exposes the structured-curation edit APIs (lyrics, chapters, artwork) on
// the Library facade. The edit is catalog-first: a set records user provenance and, by
// default, locks the artifact so a scan/enrichment pass preserves it. Artwork also
// supports opt-in on-disk write-back, embedding the cover into the backing file(s).

// SetLyrics replaces an item's lyrics with a curated set. Passing nil clears them. The
// lyrics carry their own provenance on the *model.Lyrics: an unstamped set is recorded
// as a user edit. lock is the instruction for the "lyrics" field's lock, and its zero
// value leaves whatever is stored alone.
func (l *Library) SetLyrics(ctx context.Context, itemPID model.PID, ly *model.Lyrics, lock model.LockChange, force bool) error {
	return l.store.SetItemLyrics(ctx, itemPID, ly, lock, force)
}

// SetChapters replaces a book's user-curated chapters (which win on read over the
// scanned ones). An empty list clears them. The input is a flat book-timeline list
// (StartMS/EndMS, what Chapters returns); for a multi-file book the store splits it
// across the parts. lock is the instruction for the "chapters" field's lock, and its
// zero value leaves whatever is stored alone.
func (l *Library) SetChapters(ctx context.Context, itemPID model.PID, chapters []model.Chapter, lock model.LockChange, force bool) error {
	return l.store.SetItemChapters(ctx, itemPID, chapters, lock, force)
}

// ArtEditOptions configures an artwork edit, mirroring EditOptions.
type ArtEditOptions struct {
	// WriteBack also embeds a front cover into the backing file(s) on disk. It is off
	// by default, so a cover edit is catalog-only unless the caller opts in.
	WriteBack bool
	// Lock is the instruction for the entity's "art" lock, which guards the front
	// cover alone. The zero value leaves the stored lock exactly as it stands, so a
	// caller that only meant to change the picture cannot clobber a pin it never read;
	// the CLI states LockOn or LockOff explicitly.
	Lock model.LockChange
	// Force overrides a locked front cover for this one write. It skips the lock check
	// and nothing else: the lock itself still follows Lock.
	Force bool
	// Source is where the image came from (tag|sidecar|user|enrichment|feed). Empty
	// records a user set, which is what a person at a keyboard is.
	Source model.ProvenanceSource
	// Provider names the service that supplied an enrichment cover. An enrichment
	// source without one is refused; every other source must leave it empty.
	Provider string
	// SourceURL is where a fetched cover was fetched from. Empty otherwise.
	SourceURL string
}

// Attribution is the store-side value the three provenance fields describe. See
// EditOptions.Attribution.
func (o ArtEditOptions) Attribution() model.Attribution {
	return model.Attribution{Source: o.Source, Provider: o.Provider, SourceURL: o.SourceURL}
}

// SetItemArt sets (or, with empty bytes, clears) one artwork role on a track/book
// item from raw image bytes. An empty role means the front cover, the only role the
// "art" lock guards (the lock guards the scan's cover re-derive; the other roles have
// no scan producer, so Lock and Force are ignored for them). A clear deletes only the
// named role. The cover is recorded under opts.Source, so a caller that fetched the
// picture itself is not reported as having chosen it by hand. With opts.WriteBack the
// cover is also embedded into the item's backing file, or into every part of a
// multi-file book so an external player sees the same cover on each part; only the
// front cover has an embedded representation, so write-back with any other role is
// refused with CodeInvalid before anything is written. A file that cannot be written is
// reported through a *WriteBackError while the catalog edit stands.
func (l *Library) SetItemArt(ctx context.Context, itemPID model.PID, role model.ArtRole, raw []byte, opts ArtEditOptions) error {
	if role == "" {
		role = model.ArtRoleFront
	}
	// Validate the role before the write-back refusal below, so an unknown role
	// gets the real diagnosis instead of advice to drop the write-back flag.
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, "waxbin.SetItemArt", "unknown art role: "+string(role))
	}
	// The refusal happens before any write, because a half-applied case (catalog
	// row committed, embed failed on every file) would otherwise be the normal
	// outcome of this flag combination.
	if opts.WriteBack && role != model.ArtRoleFront {
		return waxerr.New(waxerr.CodeInvalid, "waxbin.SetItemArt",
			"write-back embeds only the front cover; set role "+string(role)+" without --write-back")
	}
	if err := l.store.SetItemArt(ctx, itemPID, role, raw, opts.Attribution(), opts.Lock, opts.Force); err != nil {
		return err
	}
	if !opts.WriteBack {
		return nil
	}
	return l.writeBackItemArt(ctx, itemPID, raw)
}

// SetEntityArt sets a durable image on a non-item entity (album, artist, release
// group, genre, podcast, or playlist) under one role (empty = front; the closed
// model.ArtRole vocabulary, validated by the store). This makes album art durable:
// ResolveArt prefers it over the read-derived track cover. A front cover honours
// opts.Lock, which is what keeps a chosen cover through an enrich --force and a podcast
// feed's next image-URL change; a locked cover is refused without opts.Force. The cover
// is recorded under opts.Source, so a generated playlist mosaic no hand ever touched
// does not report itself as a user cover. With opts.WriteBack an album front cover is
// also embedded into every member track's file; other entity covers stay catalog-only
// on disk (they have no single natural file target), and a non-front role is refused
// with write-back for the same reason an item's is.
//
// One consequence of the album write-back is worth knowing. Embedding the chosen cover
// into every member track's file means the next scan reads it back as that track's own
// tag cover, so the track rung reports source "tag" while the album rung still reports
// "user". Both are accurate, since the track's cover genuinely is a tag by then.
func (l *Library) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role model.ArtRole, raw []byte, opts ArtEditOptions) error {
	if role == "" {
		role = model.ArtRoleFront
	}
	// Same ordering as SetItemArt: an unknown role reports itself, not the
	// write-back restriction.
	if !role.Valid() {
		return waxerr.New(waxerr.CodeInvalid, "waxbin.SetEntityArt", "unknown art role: "+string(role))
	}
	if opts.WriteBack && role != model.ArtRoleFront {
		return waxerr.New(waxerr.CodeInvalid, "waxbin.SetEntityArt",
			"write-back embeds only the front cover; set role "+string(role)+" without --write-back")
	}
	if err := l.store.SetEntityArt(ctx, entityType, entityPID, role, raw, opts.Attribution(), opts.Lock, opts.Force); err != nil {
		return err
	}
	if !opts.WriteBack {
		return nil
	}
	return l.writeBackEntityArt(ctx, entityType, entityPID, raw)
}

// ArtLocked reports whether an entity's front cover is locked. It is what turns a
// bare "no front art" or a refused `art set` into an explanation, since a cleared and
// locked cover leaves nothing else to see.
func (l *Library) ArtLocked(ctx context.Context, entityType model.ArtEntity, entityPID model.PID) (bool, error) {
	return l.store.ArtLocked(ctx, entityType, entityPID)
}

// SetArtLock sets or clears an entity's front-cover lock without touching the cover.
// SetEntityArt cannot express that, since it always writes the cover slot as well, so
// this is the way out of a cover that was cleared and locked. For a track entity it
// writes the same row `Lock(pid, "art")` does.
func (l *Library) SetArtLock(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, lock bool) error {
	return l.store.SetArtLock(ctx, entityType, entityPID, lock)
}

// writeBackItemArt embeds (or clears) a committed item cover into the item's backing
// file. It runs after the catalog edit committed, so a refusal or failure is reported as
// a *WriteBackError rather than a hard error.
func (l *Library) writeBackItemArt(ctx context.Context, itemPID model.PID, raw []byte) error {
	edits := artEditDesc(raw)
	// Embed into every backing file, not just the primary part: a multi-file audiobook
	// keeps the same cover in each part by convention, so writing only the primary would
	// leave the other parts showing a stale cover to an external player. A track or a
	// single-file book has one file, so this is unchanged for them.
	files, err := l.store.ItemFiles(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}
	if len(files) == 0 {
		// An archived item has no file to embed into; report the skipped write-back so a
		// caller does not read a silent success.
		wbErr := &WriteBackError{ItemPID: itemPID, Edits: edits}
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: "no backing files present to write"})
		return wbErr
	}
	return l.writeBackPicture(ctx, "waxbin.SetItemArt", itemPID, edits, files,
		meta.PictureEdit{Clear: len(raw) == 0, Data: raw})
}

// writeBackEntityArt fans a committed album cover across every member track's file. Only
// album covers fan out to disk (each album track embeds the cover); an artist, release
// group, genre, podcast, or playlist cover stays durable in the catalog with no on-disk
// target, so write-back for those is a no-op.
func (l *Library) writeBackEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, raw []byte) error {
	if entityType != model.ArtAlbum {
		return nil
	}
	edits := artEditDesc(raw)
	files, err := l.store.EntityMemberFiles(ctx, model.MergeAlbum, entityPID)
	if err != nil {
		return writeBackSetupFailure(entityPID, edits, err)
	}
	return l.writeBackPicture(ctx, "waxbin.SetEntityArt", entityPID, edits, files,
		meta.PictureEdit{Clear: len(raw) == 0, Data: raw})
}

// writeBackPicture applies a cover embed/clear across files through the shared
// per-file write-back engine, returning a *WriteBackError on any refusal or failure. An
// empty file set is a clean no-op (the album had no member files); a caller that needs
// to report a missing file for a single item does so before calling this.
func (l *Library) writeBackPicture(ctx context.Context, op string, refPID model.PID, edits map[string]string, files []model.ItemFileRef, pedit meta.PictureEdit) error {
	if len(files) == 0 {
		return nil
	}
	wbErr := &WriteBackError{ItemPID: refPID, Edits: edits}
	if err := l.writeBackFiles(ctx, op, files, wbErr,
		func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.ApplyPicture(ctx, path, pedit)
		}); err != nil {
		return err
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}

// artEditDesc is the WriteBackError.Edits record for a cover write-back (art has no
// scalar value; this names the operation for the diagnostic and the error message).
func artEditDesc(raw []byte) map[string]string {
	if len(raw) == 0 {
		return map[string]string{"art": "cleared"}
	}
	return map[string]string{"art": "set (" + strconv.Itoa(len(raw)) + " bytes)"}
}

// TagEditOptions configures a custom-tag edit, mirroring EditOptions.
type TagEditOptions struct {
	// Lock is the instruction for the "tag.<KEY>" field's lock, which guards the tag
	// against a scan re-deriving it from the file. The zero value leaves the stored
	// lock alone; the CLI states LockOn or LockOff explicitly.
	Lock model.LockChange
	// Force overrides a locked custom tag.
	Force bool
	// Source is where the values came from; empty records a user edit.
	Source model.ProvenanceSource
	// Provider names the service that supplied an enrichment value, and is required
	// with that source and refused with any other.
	Provider string
}

// Attribution is the store-side value Source and Provider describe. See
// EditOptions.Attribution.
func (o TagEditOptions) Attribution() model.Attribution {
	return model.Attribution{Source: o.Source, Provider: o.Provider}
}

// SetItemTag replaces a custom tag's ordered values on an item, recording opts.Source
// (empty means a user edit) and opts.Lock on "tag.<KEY>" so a scan does not re-derive it
// from the file. Empty (or whitespace-only) values clear the tag. The key is normalized
// to canonical uppercase; a reserved key (one WaxBin owns through the scalar, credit, or
// identifier APIs) is rejected. It returns the canonical key stored and the number of
// values actually stored after trimming (0 means the tag was cleared).
func (l *Library) SetItemTag(ctx context.Context, itemPID model.PID, key string, values []string, opts TagEditOptions) (string, int, error) {
	return l.store.SetItemTag(ctx, itemPID, key, values, opts.Attribution(), opts.Lock, opts.Force)
}

// ItemTags returns an item's custom tags (the non-standard frames WaxBin's model does
// not map, plus user-set tags), grouped by key.
func (l *Library) ItemTags(ctx context.Context, itemPID model.PID) ([]model.ItemTag, error) {
	return l.store.ItemTags(ctx, itemPID)
}

// TagKeys returns every custom-tag key in the catalog with the number of distinct items
// carrying it, most-used first. It is the discovery primitive for custom-tag browse
// dimensions: list the keys, then facet or filter on tag.<KEY>.
func (l *Library) TagKeys(ctx context.Context) ([]read.TagKeyCount, error) {
	return l.store.TagKeys(ctx)
}
