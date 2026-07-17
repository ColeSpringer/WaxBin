package waxbin

import (
	"context"
	"strings"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// CreditEditOptions configures a credit edit, mirroring EditOptions.
type CreditEditOptions struct {
	// WriteBack also writes the role's names into each backing file's on-disk tag
	// (track items only; audiobook credit tags follow their own conventions).
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
// the credit.<role> field. With opts.WriteBack it also mirrors a track's credit into
// its file's on-disk tag; a book credit write-back is refused (returned as a
// *WriteBackError) while the catalog edit stands. It returns the number of contributor
// names actually stored (after trimming blanks and de-duplicating by artist), so a
// caller does not report a wipe (an unresolvable name that cleared the role) as a set.
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

// writeBackCredit mirrors a committed credit edit into the backing files' on-disk
// tag. It runs after the catalog edit committed, so a refusal or failure is reported
// as a *WriteBackError rather than a hard error. Only track credit write-back is
// supported; a book's contributor tags follow their own conventions (Phase 9).
func (l *Library) writeBackCredit(ctx context.Context, itemPID model.PID, role model.ContributorRole, names []string) error {
	edits := map[string]string{model.CreditField(role): strings.Join(names, "; ")}

	item, err := l.store.ItemByPID(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}
	if item.Kind != model.KindTrack {
		return l.refuseWriteBack(ctx, itemPID, edits,
			"on-disk credit write-back is not yet supported for "+string(item.Kind)+" items; the catalog edit was applied")
	}
	key, ok := meta.RoleTagKey(role)
	if !ok {
		return l.refuseWriteBack(ctx, itemPID, edits,
			"no on-disk tag key for role "+string(role)+"; the catalog edit was applied")
	}
	tagEdit := meta.TagEdit{Key: key}
	if len(names) > 0 {
		tagEdit.Values = names
	}

	files, err := l.store.ItemFiles(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, edits, err)
	}
	wbErr := &WriteBackError{ItemPID: itemPID, Edits: edits}
	if len(files) == 0 {
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: "no backing files present to write"})
		return wbErr
	}
	w := meta.NewWriter()
	for _, ref := range files {
		if err := ctx.Err(); err != nil {
			return waxerr.FromContext("waxbin.SetCredits", err, waxerr.CodeCanceled)
		}
		file, err := l.store.FileByPID(ctx, ref.FilePID)
		if err != nil {
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: string(ref.Path), Reason: err.Error()})
			continue
		}
		path := string(file.Path)
		shared, err := l.store.FileSharedOrVirtual(ctx, ref.FilePID)
		if err != nil {
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: err.Error()})
			continue
		}
		if shared {
			const reason = "on-disk tag write-back is unavailable for a file shared by multiple items"
			l.recordWriteBackDrift(ctx, ref.FilePID, reason)
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: reason})
			continue
		}
		res, err := w.Apply(ctx, path, []meta.TagEdit{tagEdit})
		if err != nil {
			l.recordWriteBackDrift(ctx, ref.FilePID, err.Error())
			wbErr.Failures = append(wbErr.Failures, WriteBackFailure{FilePID: ref.FilePID, Path: path, Reason: err.Error()})
			continue
		}
		if derr := l.store.PutFileDiagnostics(ctx, ref.FilePID, model.OriginEdit, nil); derr != nil {
			l.log.Warn("credit write-back diagnostics clear", "path", path, "err", derr)
		}
		if !res.Changed {
			continue
		}
		if _, err := l.store.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
			FilePID:         ref.FilePID,
			ExpectedSize:    file.Size,
			ExpectedMTimeNS: file.MTimeNS,
			NewSize:         res.Size,
			NewMTimeNS:      res.MTimeNS,
			NewContentHash:  res.ContentHash,
		}); err != nil {
			l.log.Warn("credit write-back file-state update", "path", path, "err", err)
		}
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}
