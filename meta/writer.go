package meta

import (
	"context"
	"os"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
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

// fieldTagKeys maps a catalog metadata field name to its canonical WaxLabel tag key.
// It is the one place that correspondence lives. Both the organize tag-write and the
// catalog field-edit write-back look up their keys through TagKeyForField, so the two
// cannot drift apart.
var fieldTagKeys = map[string]string{
	"title":        "TITLE",
	"artist":       "ARTIST",
	"album":        "ALBUM",
	"album_artist": "ALBUMARTIST",
	"composer":     "COMPOSER",
	"comment":      "COMMENT",
	"genre":        "GENRE",
	"year":         "DATE",
	"track_no":     "TRACKNUMBER",
	"disc_no":      "DISCNUMBER",
	"isrc":         "ISRC",
	"mbid":         "MUSICBRAINZ_TRACKID", // recording MBID (track write-back only)
	"compilation":  "COMPILATION",
}

// TagKeyForField returns the canonical WaxLabel tag key an on-disk write uses for a
// catalog field name, and whether the field has one.
func TagKeyForField(field string) (string, bool) {
	k, ok := fieldTagKeys[field]
	return k, ok
}

// roleTagKeys maps a contributor role to its canonical WaxLabel tag key for on-disk
// write-back. The music roles (v1.2.0 keys) drive track credit write-back; the book
// roles are present for completeness but audiobook credit write-back is not wired
// through the writer yet (a book's tags follow their own conventions).
var roleTagKeys = map[model.ContributorRole]string{
	model.RoleComposer:  string(tag.Composer),
	model.RoleLyricist:  string(tag.Lyricist),
	model.RoleConductor: string(tag.Conductor),
	model.RolePerformer: string(tag.Performer),
	model.RoleRemixer:   string(tag.Remixer),
	model.RoleProducer:  string(tag.Producer),
	model.RoleEngineer:  string(tag.Engineer),
	model.RoleMixer:     string(tag.Mixer),
	model.RoleArranger:  string(tag.Arranger),
	model.RoleWriter:    string(tag.Writer),
	model.RoleDJMixer:   string(tag.DJMixer),
	model.RoleNarrator:  string(tag.Narrator),
}

// RoleTagKey returns the canonical WaxLabel tag key an on-disk write uses for a
// contributor role, and whether the role has one wired for write-back.
func RoleTagKey(role model.ContributorRole) (string, bool) {
	k, ok := roleTagKeys[role]
	return k, ok
}

// WriteResult reports the file's on-disk state after a write. When Changed is false
// the edits were a no-op and the file was not rewritten (Size/MTimeNS/ContentHash
// are left zero, since the caller already holds the current values).
//
// Warnings is independent of Changed: a no-op can still carry a warning, so a
// caller that branches on Changed alone would miss exactly the case where the edit
// had no effect because the format could not store the value.
type WriteResult struct {
	Changed     bool
	Size        int64
	MTimeNS     int64
	ContentHash string
	Warnings    []model.TagWriteWarning
}

// unrepresentedCodes are the WaxLabel warning codes that mean the value did not
// land, so the key does not hold what was asked for. These three are the ones
// WaxLabel's own CLI escalates under --strict. The rest of its vocabulary is
// documented as advisory (a number/total conflict, an MP4 multi-value note) and
// carries no loss.
//
// It is an allowlist rather than a denylist for a reason: an unclassified future code
// falls back to today's silence instead of raising a false alarm about a value that
// was written correctly.
var unrepresentedCodes = map[waxlabel.WarningCode]bool{
	waxlabel.WarnValueDropped: true,
	waxlabel.WarnValueCoerced: true,
	waxlabel.WarnValueReduced: true,
}

// writeWarnings projects a write plan's warnings into the model. It is the one place
// WaxLabel's warning vocabulary is interpreted, which keeps waxlabel types out of
// model and gives the allowlist a single home.
//
// A warning naming several keys fans out to one entry per key, so a consumer can
// match a warning to a field without parsing prose. Warning.Keys is documented as
// empty for a warning that names no specific key. All three allowlisted codes are
// documented as keyed, but this cannot rely on that, so a keyless warning is carried
// with an empty Key rather than indexed into or dropped.
func writeWarnings(ws []waxlabel.Warning) []model.TagWriteWarning {
	if len(ws) == 0 {
		return nil
	}
	out := make([]model.TagWriteWarning, 0, len(ws))
	for _, w := range ws {
		// Warning.String renders "[code] message" through tag.SanitizeLine. The raw
		// Message is not sanitized, and it can embed a file-derived snippet.
		//
		// The cap goes here, at the seam where waxlabel's vocabulary becomes the model,
		// rather than at each consumer. SanitizeLine escapes the terminal-hijack and
		// newline classes but leaves the length alone, and the writers persist this
		// Message verbatim as a tag_write_lost detail. Capping once at the seam bounds
		// every consumer, including any added later.
		mw := model.TagWriteWarning{
			Code:          w.Code.String(),
			Message:       capDetail(w.String()),
			Unrepresented: unrepresentedCodes[w.Code],
		}
		if len(w.Keys) == 0 {
			out = append(out, mw)
			continue
		}
		for _, k := range w.Keys {
			e := mw
			e.Key = string(k)
			out = append(out, e)
		}
	}
	return out
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
	// Read the report before the no-op gate. WaxLabel documents that a no-op can still
	// carry a warning the consumer needs to see: an edit whose only effect was a value
	// the format could not store leaves the bytes unchanged, yet is not what was asked
	// for. Gating on IsNoOp first would report that worst case as the cleanest one.
	warnings := writeWarnings(plan.Report().Warnings)
	if plan.IsNoOp() {
		return &WriteResult{Changed: false, Warnings: warnings}, nil
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
		Warnings:    warnings,
	}, nil
}
