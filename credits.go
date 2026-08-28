package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// CreditEditOptions configures a credit edit, mirroring EditOptions.
type CreditEditOptions struct {
	// WriteBack also writes the role's names into each backing file's on-disk tag: a
	// track's music role, or a book's author (ALBUMARTIST) / narrator (NARRATOR+COMPOSER).
	// A book translator/editor credit has no round-trippable tag and is refused.
	WriteBack bool
	// Lock is the instruction for the credit.<role> field's lock, which guards it
	// against enrichment and organize. The zero value leaves the stored lock alone;
	// the CLI states LockOn or LockOff explicitly.
	Lock model.LockChange
	// Force overrides a locked credit role.
	Force bool
	// SkipLocked reports a locked credit instead of failing on it: a batch skips that
	// entry and applies the rest, and the single-item surface reports the skip in place
	// of a CodeLocked error.
	SkipLocked bool
	// Source is where the names came from; empty records a user edit.
	Source model.ProvenanceSource
	// Provider names the service that supplied an enrichment value, and is required
	// with that source and refused with any other.
	Provider string
}

// Attribution is the store-side value Source and Provider describe. See
// EditOptions.Attribution.
func (o CreditEditOptions) Attribution() model.Attribution {
	return model.Attribution{Source: o.Source, Provider: o.Provider}
}

// Credits returns an item's contributors across every role.
func (l *Library) Credits(ctx context.Context, itemPID model.PID) ([]model.Contributor, error) {
	return l.store.ItemCredits(ctx, itemPID)
}

// SetCredits replaces the contributors of one role on an item (music roles on a
// track, book roles on a book), recording opts.Source (empty means a user edit) and
// opts.Lock on the credit.<role> field. With opts.WriteBack it also mirrors the credit into the
// backing file's on-disk tag: a track's music role, or a book's author/narrator across
// its parts. A book translator/editor credit has no round-trippable tag and is
// refused (returned as a *WriteBackError) while the catalog edit stands. It returns the
// number of contributor names actually stored (after trimming blanks and de-duplicating
// by artist), so a caller does not report a wipe (an unresolvable name that cleared the
// role) as a set. It also reports whether opts.SkipLocked skipped a locked credit instead
// of setting it; a skipped edit stores nothing, so nothing is written back either.
func (l *Library) SetCredits(ctx context.Context, itemPID model.PID, role model.ContributorRole, names []string, opts CreditEditOptions) (int, bool, error) {
	stored, skipped, err := l.store.SetItemCredits(ctx, itemPID, role, names, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
	if err != nil {
		return 0, false, err
	}
	if skipped || !opts.WriteBack {
		return len(stored), skipped, nil
	}
	// Write back the stored names (deduped), so the on-disk tag matches the catalog.
	return len(stored), false, l.writeBackCredit(ctx, itemPID, []creditRoleEdit{{role: role, names: stored}})
}

// SetCreditsBatch replaces one role's contributors on each of several items in one
// atomic transaction, where SetCredits does a single item. An entry is an (item, role)
// pair, so one item may take an author entry and a narrator entry in the same batch;
// naming the same pair twice rejects it. The batch commits or rolls back as a whole,
// and with opts.SkipLocked a locked credit role is skipped and reported instead of
// failing it.
//
// Gathering the entries lets the rename pre-pass see every reference before the first
// write, so an artist the whole batch moves is renamed in place (keeping its pid,
// curation, star, and art) instead of being left behind while a fresh row takes its
// credits. An entry naming several people renames onto the first of them and forks the
// rest.
//
// Write-back is per entry and best-effort, mirroring each stored credit into the backing
// files' tags exactly as SetCredits does: a failure lands in the result's
// WriteBackErrors rather than failing the batch. A book translator or editor credit has
// no round-trippable tag, so it is refused there and stays DB-only, which also means an
// in-place rename on those roles is durable through the credit lock alone.
func (l *Library) SetCreditsBatch(ctx context.Context, edits []model.ItemCreditEdit, opts CreditEditOptions) (*CreditBatchResult, error) {
	res, err := l.store.SetItemCreditsBatch(ctx, edits, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
	if err != nil {
		return nil, err
	}
	out := &CreditBatchResult{Edited: res.Edited, Skipped: res.Skipped}
	if !opts.WriteBack {
		return out, nil
	}
	// Write back the stored names, grouped by item: an item edited under two roles has
	// both mirrored in one pass per file, where a per-entry loop would rewrite the same
	// file twice. Items keep the order the entries applied in.
	var order []model.PID
	byItem := map[model.PID][]creditRoleEdit{}
	for _, e := range res.Edited {
		if _, seen := byItem[e.ItemPID]; !seen {
			order = append(order, e.ItemPID)
		}
		byItem[e.ItemPID] = append(byItem[e.ItemPID], creditRoleEdit{role: e.Role, names: e.Names})
	}
	for _, pid := range order {
		if err := recordWriteBack(&out.WriteBackErrors, pid, l.writeBackCredit(ctx, pid, byItem[pid])); err != nil {
			return out, err
		}
	}
	return out, nil
}

// creditRoleEdit is one role's stored names inside an item's grouped credit write-back.
type creditRoleEdit struct {
	role  model.ContributorRole
	names []string
}

// writeBackCredit mirrors an item's committed credit edits into its backing files'
// on-disk tags, every role in one pass per file so an item edited under two roles is not
// rewritten twice. It runs after the catalog edit committed, so a refusal or failure is
// reported as a *WriteBackError rather than a hard error. A track writes each role's
// music tag (RoleTagKey) to its file. A book writes an author credit to ALBUMARTIST and a
// narrator credit to NARRATOR and COMPOSER across its parts. Those are the two roles a
// scan reconstructs from a tag; a book translator or editor credit has no round-trippable
// tag, so that role is refused and stays DB-only while the roles beside it still write.
// The catalog edit stands regardless.
func (l *Library) writeBackCredit(ctx context.Context, itemPID model.PID, roles []creditRoleEdit) error {
	return l.writeBackItemEdits(ctx, "waxbin.SetCredits", itemPID, nil, roles)
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
