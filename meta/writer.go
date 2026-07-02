package meta

import (
	"context"
	"os"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
)

// Writer applies tag edits to files on disk through WaxLabel. It builds on
// WaxLabel's file-path parse (ParseFile), which carries the source identity SaveBack
// needs, and enables essence verification so a tag-only write can never silently
// alter the audio: the rewrite re-hashes the copied audio bytes and refuses to
// commit if they changed. WaxBin uses WaxLabel read-only everywhere else; this is
// the one writer seam.
type Writer struct{}

// NewWriter returns a WaxLabel-backed tag writer.
func NewWriter() *Writer { return &Writer{} }

// TagEdit is one set-or-clear of a tag key. Values sets the key (replacing any
// existing values); an empty/nil Values clears it. Key is a canonical tag key name
// (e.g. "ALBUMARTIST", "REPLAYGAIN_TRACK_GAIN", "R128_TRACK_GAIN", a custom
// "WAXBIN_ITEM_PID"); an unknown key round-trips as a native custom field.
type TagEdit struct {
	Key    string
	Values []string
}

// WriteResult reports the file's on-disk state after a write. When Changed is false
// the edits were a no-op and the file was not rewritten (Size/MTimeNS/ContentHash
// are left zero, since the caller already holds the current values).
type WriteResult struct {
	Changed     bool
	Size        int64
	MTimeNS     int64
	ContentHash string
}

// Apply writes edits to the file at path atomically and in place, preserving the
// audio essence (WithVerifyEssence). It returns the new size, mtime, and content
// hash so the caller can update the catalog's file row through the optimistic
// file-state seam. A no-op edit set reports Changed=false without rewriting.
func (w *Writer) Apply(ctx context.Context, path string, edits []TagEdit) (*WriteResult, error) {
	const op = "meta.Writer.Apply"
	if len(edits) == 0 {
		return &WriteResult{Changed: false}, nil
	}

	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s for tag write", path)
	}

	ed := doc.Edit()
	for _, e := range edits {
		if len(e.Values) == 0 {
			ed.Clear(tag.Key(e.Key))
		} else {
			ed.Set(tag.Key(e.Key), e.Values...)
		}
	}

	// Verify essence: the rewrite re-hashes the audio it copies and fails the write
	// if it differs, so a tag edit can never mutate audio.
	plan, err := ed.Prepare(waxlabel.WithVerifyEssence())
	if err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "preparing tag write for %s", path)
	}
	if plan.IsNoOp() {
		return &WriteResult{Changed: false}, nil
	}

	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeIO, op, err, "writing tags to %s", path)
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	ch, err := identity.ContentHash(path)
	if err != nil {
		return nil, err
	}
	return &WriteResult{
		Changed:     true,
		Size:        info.Size(),
		MTimeNS:     info.ModTime().UnixNano(),
		ContentHash: ch,
	}, nil
}
