package waxbin

import (
	"context"
	"strings"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// CreditEditOptions configures a credit edit, mirroring EditOptions.
type CreditEditOptions struct {
	// WriteBack also writes the role's names into each backing file's on-disk tag: a
	// track's music role, or a book's author (ALBUMARTIST) / narrator (NARRATOR+COMPOSER).
	// A book translator/editor credit has no round-trippable tag and is refused.
	WriteBack bool
	// Lock locks the credit.<role> field against enrichment/organize; on by default.
	Lock bool
	// Force overrides a locked credit role.
	Force bool
}

// Credits returns an item's contributors across every role.
func (l *Library) Credits(ctx context.Context, itemPID model.PID) ([]model.Contributor, error) {
	return l.store.ItemCredits(ctx, itemPID)
}

// SetCredits replaces the contributors of one role on an item (music roles on a
// track, book roles on a book), recording user provenance and, by default, locking
// the credit.<role> field. With opts.WriteBack it also mirrors the credit into the
// backing file's on-disk tag: a track's music role, or a book's author/narrator across
// its parts. A book translator/editor credit has no round-trippable tag and is
// refused (returned as a *WriteBackError) while the catalog edit stands. It returns the
// number of contributor names actually stored (after trimming blanks and de-duplicating
// by artist), so a caller does not report a wipe (an unresolvable name that cleared the
// role) as a set.
func (l *Library) SetCredits(ctx context.Context, itemPID model.PID, role model.ContributorRole, names []string, opts CreditEditOptions) (int, error) {
	stored, err := l.store.SetItemCredits(ctx, itemPID, role, names, model.SourceUser, opts.Lock, opts.Force)
	if err != nil {
		return 0, err
	}
	if !opts.WriteBack {
		return len(stored), nil
	}
	// Write back the STORED names (deduped), so the on-disk tag matches the catalog.
	return len(stored), l.writeBackCredit(ctx, itemPID, role, stored)
}

// writeBackCredit mirrors a committed credit edit into the backing files' on-disk tag.
// It runs after the catalog edit committed, so a refusal or failure is reported as a
// *WriteBackError rather than a hard error. A track writes the role's music tag
// (RoleTagKey) to its file. A book writes an author credit to ALBUMARTIST and a narrator
// credit to NARRATOR and COMPOSER across its parts. Those are the two roles a scan
// reconstructs from a tag; a book translator or editor credit has no round-trippable tag,
// so it is refused and stays DB-only. The catalog edit stands regardless.
func (l *Library) writeBackCredit(ctx context.Context, itemPID model.PID, role model.ContributorRole, names []string) error {
	edits := map[string]string{model.CreditField(role): strings.Join(names, "; ")}

	item, err := l.store.ItemByPID(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}

	var tagEdits []meta.TagEdit
	var files []model.ItemFileRef
	switch item.Kind {
	case model.KindTrack:
		key, ok := meta.RoleTagKey(role)
		if !ok {
			return l.refuseWriteBack(ctx, itemPID, edits,
				"no on-disk tag key for role "+string(role)+"; the catalog edit was applied")
		}
		te := meta.TagEdit{Key: key}
		if len(names) > 0 {
			te.Values = names
		}
		tagEdits = []meta.TagEdit{te}
		files, err = l.store.ItemFiles(ctx, itemPID)
		if err != nil {
			return writeBackSetupFailure(itemPID, edits, err)
		}
	case model.KindBook:
		field, ok := bookRoleField(role)
		if !ok {
			return l.refuseWriteBack(ctx, itemPID, edits,
				"on-disk credit write-back for the "+string(role)+" role is not supported for books; the catalog edit was applied")
		}
		keys, _ := meta.BookFieldTagKeys(field)
		// Join with a separator the scanner splits back apart ("; ", not the ", " the
		// display column uses), so a multi-name book credit round-trips through a rescan.
		joined := strings.Join(names, "; ")
		for _, k := range keys {
			te := meta.TagEdit{Key: k}
			if len(names) > 0 {
				te.Values = []string{joined}
			}
			tagEdits = append(tagEdits, te)
		}
		// Write every part: an author credit is ALBUMARTIST, the book's identity anchor, so
		// writing it to one part alone would split a multi-file book on the next rescan (a
		// narrator credit is inert on the non-primary parts but harmless there).
		files, err = l.store.ItemFiles(ctx, itemPID)
		if err != nil {
			return writeBackSetupFailure(itemPID, edits, err)
		}
	default:
		return l.refuseWriteBack(ctx, itemPID, edits,
			"on-disk credit write-back is not supported for "+string(item.Kind)+" items; the catalog edit was applied")
	}

	wbErr := &WriteBackError{ItemPID: itemPID, Edits: edits}
	if len(files) == 0 {
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: "no backing files present to write"})
		return wbErr
	}
	if err := l.writeBackFiles(ctx, "waxbin.SetCredits", files, wbErr,
		func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.Apply(ctx, path, tagEdits)
		}); err != nil {
		return err
	}
	// A book author credit writes ALBUMARTIST, a book identity field, so re-anchor the
	// catalog's identity key to the file's post-write value (the same protection the
	// EditFields path gives an author edit). reanchorBookIdentity reads the file's actual
	// state, so it is a no-op if the write did not land. A narrator credit does not touch
	// identity, so it needs none.
	if item.Kind == model.KindBook && role == model.RoleAuthor && len(files) > 0 {
		l.reanchorBookIdentity(ctx, itemPID, files[0].FilePID)
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}

// bookRoleField maps a book contributor role to the book metadata field whose on-disk
// tag a scan reads it back from, and whether the role round-trips at all. Only author
// (ALBUMARTIST) and narrator (NARRATOR+COMPOSER) are reconstructed from a tag; a
// translator or editor credit has no scanner tag and stays DB-only.
func bookRoleField(role model.ContributorRole) (string, bool) {
	switch role {
	case model.RoleAuthor:
		return "author", true
	case model.RoleNarrator:
		return "narrator", true
	default:
		return "", false
	}
}
