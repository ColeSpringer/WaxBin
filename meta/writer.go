package meta

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
	wlerr "github.com/colespringer/waxlabel/waxerr"
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
	"title":         "TITLE",
	"title_sort":    "TITLESORT",
	"artist":        "ARTIST",
	"album":         "ALBUM",
	"album_sort":    "ALBUMSORT",
	"album_artist":  "ALBUMARTIST",
	"composer":      "COMPOSER",
	"composer_sort": "COMPOSERSORT",
	"comment":       "COMMENT",
	"genre":         "GENRE",
	"year":          "DATE",
	"track_no":      "TRACKNUMBER",
	"track_total":   "TRACKTOTAL",
	"disc_no":       "DISCNUMBER",
	"disc_total":    "DISCTOTAL",
	"bpm":           "BPM",
	"isrc":          "ISRC",
	"mbid":          "MUSICBRAINZ_TRACKID", // recording MBID (track write-back only)
	"compilation":   "COMPILATION",
}

// TagKeyForField returns the canonical WaxLabel tag key an on-disk write uses for a
// catalog field name, and whether the field has one.
func TagKeyForField(field string) (string, bool) {
	k, ok := fieldTagKeys[field]
	return k, ok
}

// RetargetedTagKey returns the field a tag write under key lands on, on every format, when
// WaxLabel reads key as another spelling of one (YEAR is RECORDINGDATE), and whether it
// does. A custom tag under such a key cannot be written back as itself.
func RetargetedTagKey(key string) (string, bool) {
	k, err := tag.ParseKey(key)
	if err != nil {
		return "", false
	}
	if to := waxlabel.ResolveAlias(k); to != k {
		return string(to), true
	}
	return "", false
}

// AcquisitionTagEdits builds the tag edits for an item's origin provenance: the two
// source keys plus the acquisition date. An empty or zero value clears its key rather
// than writing a blank one, so a correction that emptied a field empties it on disk too
// and a clear passes three zero values.
//
// It lives here, beside the reader that parses these keys back, for the reason
// fieldTagKeys gives: one place for the correspondence so the two cannot drift. The
// date carries that most directly. ACQUISITION_DATE is a partial-date key with no clock
// and no zone, so a file holds day precision in UTC and nothing finer, and the layout
// written is the one parseAcquiredAt reads.
func AcquisitionTagEdits(sourceURL, sourceID string, acquiredAtNS int64) []TagEdit {
	edits := []TagEdit{
		{Key: string(tag.SourceURL)},
		{Key: string(tag.SourceID)},
		{Key: string(tag.AcquisitionDate)},
	}
	if sourceURL != "" {
		edits[0].Values = []string{sourceURL}
	}
	if sourceID != "" {
		edits[1].Values = []string{sourceID}
	}
	if acquiredAtNS != 0 {
		edits[2].Values = []string{time.Unix(0, acquiredAtNS).UTC().Format(acquisitionDateLayouts[0])}
	}
	return edits
}

// DerivedSortPair names a display field whose edit drops the sort spelling beside it:
// the catalog sort field that can carry an explicit value or a lock (empty for the artist
// and the album artist, whose spellings have no edit surface of their own) and the
// on-disk tag a scan reads the spelling from. The write-back uses it to clear the stale
// sort tag beside a display-name edit; a tag left behind would feed the next scan and
// bring back the spelling of the name the edit replaced. See Library.writeBackFields.
type DerivedSortPair struct {
	Field     string
	SortField string
	TagKey    string
}

// The pairs per kind, each ordered so the produced tag edits are deterministic. A book's
// title is its album tag, so its spelling is ALBUMSORT, and its author is its album
// artist, whose sort is read from ARTISTSORT where ALBUMARTISTSORT says none.
var (
	trackSortPairs = []DerivedSortPair{
		{Field: "title", SortField: "title_sort", TagKey: string(tag.TitleSort)},
		{Field: "artist", SortField: "", TagKey: string(tag.ArtistSort)},
		{Field: "album", SortField: "album_sort", TagKey: string(tag.AlbumSort)},
		{Field: "album_artist", SortField: "", TagKey: string(tag.AlbumArtistSort)},
		{Field: "composer", SortField: "composer_sort", TagKey: string(tag.ComposerSort)},
	}
	bookSortPairs = []DerivedSortPair{
		{Field: "title", SortField: "title_sort", TagKey: string(tag.AlbumSort)},
		{Field: "author", SortField: "author_sort", TagKey: string(tag.AlbumArtistSort)},
		{Field: "author", SortField: "author_sort", TagKey: string(tag.ArtistSort)},
	}
)

// DerivedSortPairs returns a kind's display-field to sort-spelling correspondences, in a
// fixed order; none for a kind with no on-disk write-back.
func DerivedSortPairs(kind model.Kind) []DerivedSortPair {
	switch kind {
	case model.KindTrack:
		return slices.Clone(trackSortPairs)
	case model.KindBook:
		return slices.Clone(bookSortPairs)
	}
	return nil
}

// roleTagKeys maps a contributor role to its canonical WaxLabel tag key for on-disk
// write-back. Only a track's credit write-back reads it (writeBackItemEdits, the track
// branch). A book's credits never come here: they write through the book field path
// (bookRoleField in credits.go), where author rides ALBUMARTIST, narrator NARRATOR and
// COMPOSER, and translator or editor are refused. The narrator entry stays so every role
// with a canonical key is listed, and nothing reaches it.
var roleTagKeys = map[model.ContributorRole]string{
	model.RoleArtist:    string(tag.Artist),
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
// contributor role, and whether the role has one wired for write-back. Only a track's
// credits are written through it; a book's go through the book field path instead.
func RoleTagKey(role model.ContributorRole) (string, bool) {
	k, ok := roleTagKeys[role]
	return k, ok
}

// CreditTagValues shapes a track credit's names for its on-disk key. A multivalued
// key (ARTIST, PERFORMER, PRODUCER) takes one value per holder, which is what the key
// means and what every reader expects. A single-valued key (CONDUCTOR, REMIXER,
// NARRATOR) takes the names joined with "; ", the separator identity.SplitCredits
// splits back apart: the tag library stores several values on such a key faithfully,
// but its typed projection, WaxBin's own reader included, sees only the first, so two
// conductors written as two values read back as one. The book path joins every key
// the same way for its own reason (its scanner reads a single string), see the
// KindBook arm of writeBackItemEdits.
func CreditTagValues(key string, names []string) []string {
	if len(names) <= 1 || tag.Key(key).Multivalued() {
		return names
	}
	return []string{strings.Join(names, "; ")}
}

// bookFieldTagKeys maps a book metadata field to the on-disk tag key(s) the audiobook
// scanner reads back for it, so a book edit round-trips through a rescan. It is a
// separate map from fieldTagKeys because a book reconstructs the same catalog fields
// from different tags than a track does: a book's title is the ALBUM tag (the file
// TITLE holds a part or chapter name) and its author is ALBUMARTIST, so its author
// sort is ALBUMARTISTSORT (the first key the scanner's author_sort derive reads).
// narrator maps to two keys, NARRATOR and the COMPOSER fallback the scanner also reads
// (the Audiobookshelf convention), so both stay in step. series is deliberately not in
// this map. It packs a name and a sequence into one GROUPING value, so the caller builds
// that through BookSeriesTagKey and PackSeriesGrouping.
//
// Every key here is one PromoteBookFields reads back, and that is the rule for adding one:
// a key only the writer knows is cleared by the next content-changed rescan, which is the
// loss the write-back exists to prevent. publisher writes the LABEL key because that is
// the frame it is read from (TPUB, or PUBLISHER on Vorbis and Matroska), which a book
// does not otherwise use. subtitle and edition have no key in the tag library, so they
// ride custom keys of their own names (a TXXX user frame on ID3, a freeform atom on MP4)
// that the reader promotes for a book alone; description is the typed DESCRIPTION key.
//
// asin, isbn, and edition are identity inputs (identity.BookKey), unlike every other
// entry here except title and author. A caller that writes them must re-anchor the
// book's stored identity key from the file's post-write tags, or the next scan resolves
// a different item; see reanchorBookIdentity and bookIdentityEdited. mbid is not an
// identity input, so it needs no re-anchor.
//
// author_sort round-trips as written: the scanner keeps the ALBUMARTISTSORT spelling, else
// the ARTISTSORT one, and folds the key it orders by beside it, so a clear empties both
// (BookFieldClearKeys).
var bookFieldTagKeys = map[string][]string{
	"title":       {string(tag.Album)},
	"title_sort":  {string(tag.AlbumSort)},
	"author":      {string(tag.AlbumArtist)},
	"author_sort": {string(tag.AlbumArtistSort)},
	"narrator":    {string(tag.Narrator), string(tag.Composer)},
	"genre":       {string(tag.Genre)},
	"year":        {"DATE"}, // same key a track's year uses; no tag constant, matching fieldTagKeys
	"asin":        {"ASIN"}, // no tag constant: read back by name in PromoteBookFields
	"isbn":        {"ISBN"},
	"publisher":   {string(tag.Label)},
	"mbid":        {"MUSICBRAINZ_ALBUMID"}, // a book's release id, read back by bookInput
	"subtitle":    {"SUBTITLE"},
	"edition":     {"EDITION"},
	"description": {string(tag.Description)},
}

// BookFieldTagKeys returns the on-disk tag keys the audiobook scanner reads back for a
// book metadata field, and whether the field has any. Only series has none; it is
// handled through BookSeriesTagKey.
func BookFieldTagKeys(field string) ([]string, bool) {
	k, ok := bookFieldTagKeys[field]
	return k, ok
}

// BookFieldClearKeys returns the keys a clear of a book field must empty besides the ones
// it writes. The reader folds DESCRIPTION and LONGDESCRIPTION into one description, reads
// the author sort from ARTISTSORT where ALBUMARTISTSORT is empty, and reads the title
// spelling of a book with no album from TITLESORT where ALBUMSORT is empty, so clearing
// the first key alone lets the other read back as the old value; a set leaves the other
// alone, since the key it writes is the one read first.
func BookFieldClearKeys(field string) []string {
	switch field {
	case "description":
		return []string{string(tag.LongDescription)}
	case "author_sort":
		return []string{string(tag.ArtistSort)}
	case "title_sort":
		return []string{string(tag.TitleSort)}
	}
	return nil
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
// curation and locks. Clearing one is the exception that does re-key, deliberately, and
// its write-back strips the id from the member files rather than fanning a value; that
// strip keys on the clear itself (entityMBIDStripEdits) and not on this table, which maps
// fields to the values a rescan reads back. Barcode/label/catalog#/sort are not identity
// inputs, so they fan safely. A release-group field and a release-group type also stay
// DB-only.
//
// An album's media stays DB-only, for a reason that is about the tag rather than
// identity: MEDIA is a per-medium tag while the fan-out writes one value to every member
// file, so an album-level edit would stamp "CD" over a DVD medium's tracks. Country has no
// such problem, since every disc of a release shares one, and it fans. It only became
// fannable with WaxLabel v1.3.0, which made RELEASECOUNTRY canonical and writes it as the
// Picard-interoperable frame on each format; before that there was no key worth writing.
// The two-letter code model.NormalizeCountry produces is also what WaxLabel's
// malformed-country lint accepts, so a fanned value is clean on disk.
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
		case "country":
			return string(tag.ReleaseCountry), true
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

// unrepresentedCodes are the WaxLabel warning codes that mean the write was lossy:
// the key does not hold what was asked for, or the rewrite could not carry content
// the file held. They are the codes WaxLabel's own CLI refuses under --strict,
// narrowed to the edits this writer makes (tag values and a front cover; chapters and
// synced lyrics are never written here). The rest of the vocabulary is advisory (a
// number/total conflict, an MP4 multi-atom note) and carries no loss.
//
// It is an allowlist rather than a denylist for a reason: an unclassified future code
// falls back to today's silence instead of raising a false alarm about a value that
// was written correctly. WarnPictureSelectorMiss stays out on purpose: the CLI
// escalates it for a picture removal that matched nothing, but ApplyPicture removes
// the front cover before every embed, so on a coverless file the miss is the normal
// path rather than a loss.
var unrepresentedCodes = map[waxlabel.WarningCode]bool{
	// The value did not land as given: dropped, normalized, stored with less
	// precision, or a numeric GENRE read back as a name on an ID3-based format.
	// Several values on a key whose canonical reading is one count too: the file
	// keeps them all and every typed reader sees the first alone. WaxBin's own credit
	// write-back joins such names (CreditTagValues), so this guards the other routes.
	waxlabel.WarnValueDropped:      true,
	waxlabel.WarnValueCoerced:      true,
	waxlabel.WarnValueReduced:      true,
	waxlabel.WarnNumericGenre:      true,
	waxlabel.WarnSingleValuedMulti: true,
	// Structure around the value the destination could not keep, or a picture it
	// cannot store at all.
	waxlabel.WarnTagStructureDropped:       true,
	waxlabel.WarnCommentDescriptionDropped: true,
	waxlabel.WarnPictureMetadataDropped:    true,
	waxlabel.WarnPictureUnsupported:        true,
	// Content the rewrite destroyed that the edit never named. Keyless apart from a
	// legacy strip naming the keys it took, so the diagnostic carries the loss in prose.
	waxlabel.WarnDuplicateTagBlockDropped: true,
	waxlabel.WarnMalformedTagEntryDropped: true,
	waxlabel.WarnLegacyStripDropped:       true,
}

// PostWriteWarningCode marks a write whose bytes landed but whose post-commit step
// (the directory sync, a restored file attribute) failed. WaxLabel v1.4 reports
// that outcome as committed-with-error; the writer folds it into Warnings so the
// caller records the file's new state instead of counting a landed write as
// failed. It is never Unrepresented, since no value was lost; consumers that want
// the event visible log entries carrying this code.
const PostWriteWarningCode = "post-write"

// writeCode classifies a WaxLabel failure. Cancellation is CodeCanceled wherever
// it surfaces. A write refusal, meaning the file or format cannot take the edit at
// all (a fragmented MP4, a chained or unaligned Ogg stream, an unwritable or
// unparseable container, an unsupported tag, an oversized picture), is
// CodeUnsupported, matching ApplyPicture's capability gate. A filesystem error
// reaching the file is CodeIO. Anything else takes fallback: a bad edit at parse
// or prepare, an I/O failure at execute.
func writeCode(err error, fallback waxerr.Code) waxerr.Code {
	var pe *fs.PathError
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return waxerr.CodeCanceled
	case errors.Is(err, wlerr.ErrFragmented) || errors.Is(err, wlerr.ErrUnsupportedFormat) ||
		errors.Is(err, wlerr.ErrUnsupportedTag) || errors.Is(err, wlerr.ErrPictureTooLarge) ||
		errors.Is(err, wlerr.ErrChainedStream) || errors.Is(err, wlerr.ErrUnalignedStream):
		return waxerr.CodeUnsupported
	case errors.As(err, &pe):
		return waxerr.CodeIO
	}
	return fallback
}

// commitPlan executes the plan in place and reports the file's post-write state.
// A landed write is success even when a post-commit step failed (the plan is
// spent and the bytes are on disk); that failure rides in Warnings under
// PostWriteWarningCode. One narrow exception keeps the reported state truthful: a
// landed write whose content hash cannot be read reports the error anyway, since
// the caller cannot update the file row without the hash, and the next scan
// re-hashes and heals that row. Size and mtime come from WaxLabel's own
// post-commit stat (Dest), so a racing external write cannot skew them.
func commitPlan(ctx context.Context, plan *waxlabel.Plan, op, path, what string, warnings []model.TagWriteWarning) (*WriteResult, error) {
	_, sres, err := plan.Execute(ctx, waxlabel.SaveBack())
	if err != nil && !sres.Committed {
		return nil, waxerr.Classifyf(writeCode(err, waxerr.CodeIO), op, err, "writing %s to %s", what, path)
	}
	if err != nil {
		// Sanitized at the seam like every other warning; the error text embeds the
		// file path, which the terminal must not have to trust.
		warnings = append(warnings, model.TagWriteWarning{
			Code: PostWriteWarningCode, Message: model.CapDetail(tag.SanitizeLine(err.Error())),
		})
	}
	size, mtime := sres.Dest.Size, sres.Dest.ModTimeUnixNano
	if mtime == 0 {
		// Dest's zero mtime is WaxLabel's "unknown" marker; fall back to a stat.
		info, serr := os.Stat(path)
		if serr != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, op, serr)
		}
		size, mtime = info.Size(), info.ModTime().UnixNano()
	}
	ch, err := identity.ContentHash(path)
	if err != nil {
		return nil, err
	}
	return &WriteResult{Changed: true, Size: size, MTimeNS: mtime, ContentHash: ch, Warnings: warnings}, nil
}

// writeWarnings projects a write plan's warnings into the model. It is the one place
// WaxLabel's warning vocabulary is interpreted, which keeps waxlabel types out of
// model and gives the allowlist a single home.
//
// A warning naming several keys fans out to one entry per key, so a consumer can
// match a warning to a field without parsing prose. Warning.Keys is documented as
// empty for a warning that names no specific key: the value-level codes are keyed
// and the rewrite-loss codes mostly are not, so a keyless warning is carried with an
// empty Key rather than indexed into or dropped.
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
			Message:       model.CapDetail(w.String()),
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

// ApplyOption tunes one Apply call.
type ApplyOption func(*applyOptions)

type applyOptions struct {
	outputGain *int
}

// WithOutputGain sets the file's header output gain, the gain a player applies
// before it reads any tag, as an absolute Q7.8 integer. Only Ogg Opus stores one:
// anywhere else WaxLabel refuses the whole write, tags included, which writeCode
// reports as CodeUnsupported. A value the file already carries is a no-op.
func WithOutputGain(q78 int) ApplyOption {
	return func(o *applyOptions) { o.outputGain = &q78 }
}

// Apply writes edits to the file at path atomically and in place, preserving the
// audio essence (WithVerifyEssence). It returns the new size, mtime, and content
// hash so the caller can update the catalog's file row through the optimistic
// file-state seam. A no-op edit set reports Changed=false without rewriting.
//
// An error means the file was not rewritten, apart from the one narrow exception
// commitPlan documents (a landed write whose hash cannot be read; the next scan
// heals it). A write whose bytes landed but whose post-commit step failed reports
// success with a post-write warning instead.
func (w *Writer) Apply(ctx context.Context, path string, edits []TagEdit, opts ...ApplyOption) (*WriteResult, error) {
	var o applyOptions
	for _, opt := range opts {
		opt(&o)
	}
	if len(edits) == 0 && o.outputGain == nil {
		return &WriteResult{Changed: false}, nil
	}
	return writeTags(ctx, "meta.Writer.Apply", path, func(_ *waxlabel.Document, ed *waxlabel.Editor) {
		for _, e := range edits {
			if len(e.Values) == 0 {
				ed.Clear(tag.Key(e.Key))
			} else {
				ed.Set(tag.Key(e.Key), e.Values...)
			}
		}
		if o.outputGain != nil {
			ed.SetOutputGain(*o.outputGain)
		}
	}, nil)
}

// ErrNotCustomTag is the cause of an ApplyCustomTags refusal that no later write-back
// changes: the file's format writes the key as one of its own fields.
var ErrNotCustomTag = errors.New("not a custom tag in this file's format")

// CustomTagsResult is what ApplyCustomTags did to one file.
type CustomTagsResult struct {
	// WriteResult is the file as the rewrite left it (Changed false when there was none),
	// with its warnings. There is at most one rewrite.
	WriteResult
	// Refused holds each key that did not go in, with why: wrapping ErrNotCustomTag when
	// the file's format writes the key as a field of its own, a CodeUnsupported refusal
	// when writing it would change another field, or the tag writer refusing its value.
	// The keys not named here went in.
	Refused map[string]error
}

// ApplyCustomTags writes custom tags (distinct keys, no values clearing one) to the file at
// path in one rewrite, each only where it lands as itself. When the file's format writes a
// key onto another field, as an ID3 frame id like TPE2 or an MP4 name folded onto a
// MusicBrainz id would be, or the rewrite would change another field besides, or the tag
// writer refuses a value, each key is judged by a plan prepared and not written, beside
// the keys taken so far, and the keys taken go in together, the others named in Refused
// and left as they were. Values the file would not hold as given are reported as an
// unrepresented warning. An error means nothing was written: the file could not be read
// or written, or the writer refused every key's value.
func (w *Writer) ApplyCustomTags(ctx context.Context, path string, edits []TagEdit) (*CustomTagsResult, error) {
	const op = "meta.Writer.ApplyCustomTags"
	out := &CustomTagsResult{}
	res, refused, err := applyCustomTags(ctx, op, path, edits, true)
	switch {
	case err != nil:
		return nil, err
	case refused == nil:
		out.WriteResult = *res
		return out, nil
	case len(edits) == 1:
		if bad, ok := refused.(*badValue); ok {
			return nil, bad.err
		}
		out.refuse(edits[0].Key, refused)
		return out, nil
	}
	var taken []TagEdit
	var bad error // the writer's refusal of a value, kept while every key meets one
	allBad := true
	for _, e := range edits {
		_, refused, err := applyCustomTags(ctx, op, path, append(slices.Clone(taken), e), false)
		if err != nil {
			return nil, err
		}
		if refused == nil {
			taken = append(taken, e)
			allBad = false
			continue
		}
		if len(taken) > 0 {
			// Refused beside the others: its own verdict says why, unless alone it goes in.
			_, alone, err := applyCustomTags(ctx, op, path, []TagEdit{e}, false)
			if err != nil {
				return nil, err
			}
			if alone != nil {
				refused = alone
			}
		}
		if v, ok := refused.(*badValue); ok {
			refused = v.err
			bad = cmp.Or(bad, v.err)
		} else {
			allBad = false
		}
		out.refuse(e.Key, refused)
	}
	if len(taken) == 0 {
		if allBad {
			return nil, bad
		}
		return out, nil
	}
	res, refused, err = applyCustomTags(ctx, op, path, taken, true)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		// The file changed since the keys were judged, and none went in.
		if v, ok := refused.(*badValue); ok {
			refused = v.err
		}
		for _, e := range taken {
			out.refuse(e.Key, refused)
		}
		return out, nil
	}
	out.WriteResult = *res
	return out, nil
}

func (r *CustomTagsResult) refuse(key string, why error) {
	if r.Refused == nil {
		r.Refused = map[string]error{}
	}
	r.Refused[key] = why
}

// badValue is the tag writer refusing to prepare an edit, a value it cannot hold, as
// against a file it cannot read or write.
type badValue struct{ err error }

func (b *badValue) Error() string { return b.err.Error() }

// applyCustomTags makes one plan of edits, judged by judgeCustomTag for one key and
// judgeCustomTags for several, and writes it when commit is set. A judge's refusal, or the
// writer's refusal of the edit as a *badValue, comes back as refused, apart from err, which
// is the file failing to parse or write.
func applyCustomTags(ctx context.Context, op, path string, edits []TagEdit, commit bool) (res *WriteResult, refused, err error) {
	held := make(map[string][]string, len(edits))
	var parsed, judged bool
	edit := func(doc *waxlabel.Document, ed *waxlabel.Editor) {
		parsed = true
		for _, e := range edits {
			k := tag.Key(e.Key)
			held[e.Key], _ = doc.Tags().Get(k)
			if len(e.Values) == 0 {
				ed.Clear(k)
			} else {
				ed.Set(k, e.Values...)
			}
		}
	}
	check := func(plan *waxlabel.Plan, warnings []model.TagWriteWarning) ([]model.TagWriteWarning, error) {
		judged = true
		if len(edits) == 1 {
			warnings, refused = judgeCustomTag(op, edits[0].Key, edits[0].Values, held[edits[0].Key], plan.Changes(), warnings)
		} else {
			warnings, refused = judgeCustomTags(op, edits, held, plan.Changes(), warnings)
		}
		return warnings, refused
	}
	if commit {
		res, err = writeTags(ctx, op, path, edit, check)
	} else {
		_, _, err = prepareTags(ctx, op, path, edit, check)
	}
	switch {
	case refused != nil:
		return nil, refused, nil
	case err != nil && parsed && !judged:
		return nil, &badValue{err}, nil
	}
	return res, nil, err
}

// judgeCustomTag decides a custom tag write from the plan's changes, given the values the
// file held under key before. Another tag key changing means the format put the values
// elsewhere when key itself did not end up holding them, and a side effect of the rewrite
// otherwise; either refuses the write. The lowercase pseudo-keys for pictures, chapters and
// the like are not tag keys, and the warnings already report what they lose.
func judgeCustomTag(op, key string, values, held []string, changes []tag.Change, warnings []model.TagWriteWarning) ([]model.TagWriteWarning, error) {
	landed := held
	var others []string
	for _, c := range changes {
		switch {
		case string(c.Key) == key:
			landed = c.New
		case c.Key.Valid():
			others = append(others, tag.SanitizeLine(string(c.Key)))
		}
	}
	asked := slices.Equal(landed, values)
	if len(others) > 0 {
		if !asked {
			return nil, waxerr.Wrapf(waxerr.CodeUnsupported, op, ErrNotCustomTag,
				"%s is written as %s", key, strings.Join(others, ", "))
		}
		return nil, waxerr.New(waxerr.CodeUnsupported, op,
			"writing "+key+" to this file would also change "+strings.Join(others, ", "))
	}
	if !asked {
		warnings = withLostWarning(warnings, key)
	}
	return warnings, nil
}

// judgeCustomTags is judgeCustomTag for several keys written together. Any other tag key
// changing refuses the combined write without saying which key caused it, since writing
// the keys one at a time tells.
func judgeCustomTags(op string, edits []TagEdit, held map[string][]string, changes []tag.Change, warnings []model.TagWriteWarning) ([]model.TagWriteWarning, error) {
	landed := make(map[string][]string, len(edits))
	for _, e := range edits {
		landed[e.Key] = held[e.Key]
	}
	var others []string
	for _, c := range changes {
		if _, ours := landed[string(c.Key)]; ours {
			landed[string(c.Key)] = c.New
		} else if c.Key.Valid() {
			others = append(others, tag.SanitizeLine(string(c.Key)))
		}
	}
	if len(others) > 0 {
		return nil, waxerr.New(waxerr.CodeUnsupported, op,
			"writing these tags together would also change "+strings.Join(others, ", "))
	}
	for _, e := range edits {
		if !slices.Equal(landed[e.Key], e.Values) {
			warnings = withLostWarning(warnings, e.Key)
		}
	}
	return warnings, nil
}

// withLostWarning adds an unrepresented warning for key unless the plan already reported one.
func withLostWarning(warnings []model.TagWriteWarning, key string) []model.TagWriteWarning {
	if slices.ContainsFunc(warnings, func(wn model.TagWriteWarning) bool { return wn.Unrepresented && wn.Key == key }) {
		return warnings
	}
	return append(warnings, model.TagWriteWarning{
		Code: waxlabel.WarnValueDropped.String(), Key: key, Unrepresented: true,
		Message: model.CapDetail("this file does not hold " + key + " as written"),
	})
}

// prepareTags parses the file at path, records edit on it, and prepares the
// essence-verified rewrite, letting check (when set) judge the plan and its warnings.
// Nothing is written: the audio is read and verified only when the plan is executed.
func prepareTags(ctx context.Context, op, path string, edit func(*waxlabel.Document, *waxlabel.Editor),
	check func(*waxlabel.Plan, []model.TagWriteWarning) ([]model.TagWriteWarning, error)) (*waxlabel.Plan, []model.TagWriteWarning, error) {
	doc, err := waxlabel.ParseFile(ctx, path)
	if err != nil {
		return nil, nil, waxerr.Classifyf(writeCode(err, waxerr.CodeInvalid), op, err, "parsing %s for tag write", path)
	}
	ed := doc.Edit()
	edit(doc, ed)

	// Verify essence: the rewrite re-hashes the audio it copies and fails the write
	// if it differs, so a tag edit can never mutate audio.
	plan, err := ed.Prepare(waxlabel.WithVerifyEssence())
	if err != nil {
		return nil, nil, waxerr.Classifyf(writeCode(err, waxerr.CodeInvalid), op, err, "preparing tag write for %s", path)
	}
	// Read the report before the no-op gate. WaxLabel documents that a no-op can still
	// carry a warning the consumer needs to see: an edit whose only effect was a value
	// the format could not store leaves the bytes unchanged, yet is not what was asked
	// for. Gating on IsNoOp first would report that worst case as the cleanest one.
	warnings := writeWarnings(plan.Report().Warnings)
	if check != nil {
		if warnings, err = check(plan, warnings); err != nil {
			return nil, nil, err
		}
	}
	return plan, warnings, nil
}

// writeTags is the write both Apply and ApplyCustomTags make: prepareTags, then the commit
// unless the plan is a no-op.
func writeTags(ctx context.Context, op, path string, edit func(*waxlabel.Document, *waxlabel.Editor),
	check func(*waxlabel.Plan, []model.TagWriteWarning) ([]model.TagWriteWarning, error)) (*WriteResult, error) {
	plan, warnings, err := prepareTags(ctx, op, path, edit, check)
	if err != nil {
		return nil, err
	}
	if plan.IsNoOp() {
		return &WriteResult{Changed: false, Warnings: warnings}, nil
	}

	// A committed-with-error write is a landed write. WaxLabel documents that
	// quadrant as a warning: the plan is spent and a retry is refused, so reporting
	// failure would leave every caller skipping the file-state update for a file
	// whose bytes did change (and the enrichment pass abandoning a book whose
	// primary part was in fact written). commitPlan carries that contract.
	return commitPlan(ctx, plan, op, path, "tags", warnings)
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
		return nil, waxerr.Classifyf(writeCode(err, waxerr.CodeInvalid), op, err, "parsing %s for picture write", path)
	}
	// The gate is a capability check, not a byte check: a read-only file (WaxLabel
	// v1.4 reports ReadOnly per file, so this covers a fragmented MP4) or a format
	// with no picture slot cannot carry the cover, so refuse rather than write a
	// no-op the caller would read as a clean sync.
	caps := doc.Capabilities()
	if caps.ReadOnly || caps.Pictures.Write == waxlabel.AccessNone {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "this file cannot store an embedded cover")
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
			// The sniffer has known HEIF, AVIF and JPEG XL since 1.7, so this now covers
			// a cover the store probed and accepted in a format it still does not read.
			// Allow it explicitly so the embed is not rejected at Prepare.
			opts = append(opts, waxlabel.WithUnrecognizedPictures())
		}
	}

	plan, err := ed.Prepare(opts...)
	if err != nil {
		return nil, waxerr.Classifyf(writeCode(err, waxerr.CodeInvalid), op, err, "preparing picture write for %s", path)
	}
	warnings := writeWarnings(plan.Report().Warnings)
	if plan.IsNoOp() {
		return &WriteResult{Changed: false, Warnings: warnings}, nil
	}
	return commitPlan(ctx, plan, op, path, "cover", warnings)
}

// MergeKeylessDiagnostics folds diagnostics that share a code and name no tag key into
// one row per code, joining their details and keeping the worst severity. The
// file_diagnostic key is (file, origin, code, tag_key), so two keyless losses on one
// file would otherwise collapse to whichever was written last. Keyed rows and the
// order of first appearance are left alone.
func MergeKeylessDiagnostics(ds []model.FileDiagnostic) []model.FileDiagnostic {
	out := make([]model.FileDiagnostic, 0, len(ds))
	at := map[model.DiagnosticCode]int{}
	for _, d := range ds {
		if d.TagKey != "" {
			out = append(out, d)
			continue
		}
		i, seen := at[d.Code]
		if !seen {
			at[d.Code] = len(out)
			out = append(out, d)
			continue
		}
		switch {
		case d.Detail == "":
		case out[i].Detail == "":
			out[i].Detail = d.Detail
		default:
			out[i].Detail = model.CapDetail(out[i].Detail + "; " + d.Detail)
		}
		if severityRank(d.Severity) > severityRank(out[i].Severity) {
			out[i].Severity = d.Severity
		}
	}
	return out
}
