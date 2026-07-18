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

// bookFieldTagKeys maps a book metadata field to the on-disk tag key(s) the audiobook
// scanner reads back for it, so a book edit round-trips through a rescan. It is a
// separate map from fieldTagKeys because a book reconstructs the same catalog fields
// from different tags than a track does: a book's title is the ALBUM tag (the file
// TITLE holds a part or chapter name) and its author is ALBUMARTIST. narrator maps to
// two keys, NARRATOR and the COMPOSER fallback the scanner also reads (the Audiobookshelf
// convention), so both stay in step. A book field absent here (subtitle, asin, isbn,
// publisher, edition, description, mbid) is one the scanner does not reconstruct from a
// tag: it stays DB-only, since writing it to disk could not survive a rescan. series is
// deliberately not in this map. It packs a name and a sequence into one GROUPING value,
// so the caller builds that through BookSeriesTagKey and PackSeriesGrouping.
var bookFieldTagKeys = map[string][]string{
	"title":    {string(tag.Album)},
	"author":   {string(tag.AlbumArtist)},
	"narrator": {string(tag.Narrator), string(tag.Composer)},
	"genre":    {string(tag.Genre)},
	"year":     {"DATE"}, // same key a track's year uses; no tag constant, matching fieldTagKeys
}

// BookFieldTagKeys returns the on-disk tag keys the audiobook scanner reads back for a
// book metadata field, and whether the field round-trips through a tag at all. A field
// with no keys is DB-only by design; see bookFieldTagKeys. series is handled through
// BookSeriesTagKey, not here.
func BookFieldTagKeys(field string) ([]string, bool) {
	k, ok := bookFieldTagKeys[field]
	return k, ok
}

// BookSeriesTagKey is the single tag that carries a book's series and sequence. The
// scanner splits it back into a series name and sequence (parseSeries); PackSeriesGrouping
// is the inverse that builds the value an edit writes.
const BookSeriesTagKey = string(tag.Grouping)

// EntityFieldTagKey returns the on-disk tag key an entity-curation field fans out to
// across the entity's member files, and whether the field has one. Only the fields that
// round-trip through a rescan WITHOUT disturbing the entity's identity are wired: an
// album's non-identity release identifiers and sort (BARCODE, LABEL, CATALOGNUMBER,
// ALBUMSORT) and an artist's sort (ARTISTSORT).
//
// An entity MBID is deliberately NOT fanned (neither album nor artist): a member track's
// MusicBrainz ID is the entity's identity key on the next scan (AlbumKey/artist match are
// mbid-first), but the entity edit updates only the mbid column, not the row's match_key,
// so writing the MBID to the files would re-key the entity to a fresh row and orphan its
// curation and locks. Barcode/label/catalog#/sort are not identity inputs, so they fan
// safely. A release-group field and a release-group type also stay DB-only.
func EntityFieldTagKey(entityType model.MergeEntity, field string) (string, bool) {
	switch entityType {
	case model.MergeAlbum:
		switch field {
		case "sort":
			return string(tag.AlbumSort), true
		case "barcode":
			return string(tag.Barcode), true
		case "label":
			return string(tag.Label), true
		case "catalog_number":
			return string(tag.CatalogNumber), true
		}
	case model.MergeArtist:
		if field == "sort" {
			return string(tag.ArtistSort), true
		}
	}
	return "", false
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

// PictureEdit is a change to a file's embedded front cover. Clear removes the front
// cover; otherwise Data (raw image bytes) is embedded as the front cover, replacing any
// existing one. Either way only the front cover is touched. Other embedded pictures such
// as a back cover or booklet scan are left intact, matching the catalog's "art" field,
// which models the front cover alone.
type PictureEdit struct {
	Clear bool
	Data  []byte
}

// ApplyPicture embeds (or clears) the front-cover picture on the file at path,
// preserving the audio essence exactly as Apply does (WithVerifyEssence re-hashes the
// copied audio and refuses to commit if it changed). A format that cannot store a
// picture, or a read-only format, is refused with CodeUnsupported so the caller records
// a write-back failure rather than silently dropping the cover. A no-op reports
// Changed=false without rewriting: this covers clearing a file that has no pictures and
// re-embedding the bytes it already carries.
func (w *Writer) ApplyPicture(ctx context.Context, path string, edit PictureEdit) (*WriteResult, error) {
	const op = "meta.Writer.ApplyPicture"

	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s for picture write", path)
	}
	// The format gate is a capability check, not a byte check: a read-only container or
	// one with no picture slot cannot carry the cover, so refuse rather than write a
	// no-op the caller would read as a clean sync.
	caps := doc.Capabilities()
	if caps.ReadOnly || caps.Pictures.Write == waxlabel.AccessNone {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "this file's format cannot store an embedded cover")
	}

	ed := doc.Edit()
	opts := []waxlabel.WriteOption{waxlabel.WithVerifyEssence()}
	// Drop any existing front cover first. On a set this stops repeated edits from
	// accumulating duplicate covers, since AddPicture appends. On a clear it removes only
	// the front cover and leaves a back cover or booklet scan intact. Never call
	// ClearPictures(): that would delete pictures the catalog's front-cover "art" field
	// does not model.
	ed.RemovePictures(func(p waxlabel.Picture) bool { return p.Type == waxlabel.PicFrontCover })
	if !edit.Clear {
		ed.AddPicture(waxlabel.Picture{Type: waxlabel.PicFrontCover, Data: edit.Data})
		if !waxlabel.IsRecognizedImage(edit.Data) {
			// An exotic but valid cover (AVIF/HEIC, already probed and accepted by the
			// store) is not decodable by the picture validator; allow it explicitly so the
			// embed is not rejected at Prepare.
			opts = append(opts, waxlabel.WithUnrecognizedPictures())
		}
	}

	plan, err := ed.Prepare(opts...)
	if err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "preparing picture write for %s", path)
	}
	warnings := writeWarnings(plan.Report().Warnings)
	if plan.IsNoOp() {
		return &WriteResult{Changed: false, Warnings: warnings}, nil
	}
	if _, _, err := plan.Execute(ctx, waxlabel.SaveBack()); err != nil {
		return nil, waxerr.Wrapf(waxerr.CodeIO, op, err, "writing cover to %s", path)
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
