package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
	"github.com/colespringer/waxlabel/tag"
	wlerr "github.com/colespringer/waxlabel/waxerr"
)

// Adapter reads metadata through WaxLabel. One open file feeds tag parsing,
// stream properties, and the tag-independent audio-essence digest.
type Adapter struct{}

// NewReader returns the WaxLabel-backed metadata reader.
func NewReader() *Adapter { return &Adapter{} }

var _ Reader = (*Adapter)(nil)

// fileSource adapts an *os.File to waxlabel.ReaderAtSized (positioned reads plus
// a fixed size), so one open file feeds both the parse and the essence hash.
type fileSource struct {
	f    *os.File
	size int64
}

func (s *fileSource) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }
func (s *fileSource) Size() int64                             { return s.size }

// Read parses path with WaxLabel and returns its tags, properties, and essence
// hash. It never decodes PCM. A parse error is returned; a file that parses but
// carries no hashable essence yields a populated FileMeta with an empty
// EssenceHash (the scanner falls back to the content hash).
func (a *Adapter) Read(ctx context.Context, path string) (*FileMeta, error) {
	const op = "meta.Read"
	f, err := os.Open(path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	src := &fileSource{f: f, size: st.Size()}

	doc, err := waxlabel.Parse(ctx, src)
	if err != nil {
		// Unsupported formats are still cataloged with a filename-derived title and
		// content-hash essence. Other parse failures propagate so corrupted files
		// are reported instead of hidden.
		if errors.Is(err, wlerr.ErrUnsupportedFormat) {
			// Still record a display codec/container from the extension so `show`/
			// `doctor` aren't blank for the file (it has no decoder regardless).
			container, codec := extFormat(path)
			return &FileMeta{
				Tags: model.Tags{Title: titleFromPath(path), Container: container, Codec: codec},
				// Previously swallowed silently: the file cataloged with a filename title
				// and no tags, and nothing said why.
				Diagnostics: []model.FileDiagnostic{{
					Code:     model.DiagUnsupportedFormat,
					Severity: model.SeverityInfo,
					Detail:   capDetail("no parser for this container; cataloged with a filename-derived title"),
				}},
			}, nil
		}
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s", path)
	}

	// fields is projected once here and threaded to every consumer. Document.Fields is
	// a full tag.Project walk with no cache, and Document.Tags deep-clones the whole
	// set, so each accessor call is a cost the scan path would pay per file.
	fields := doc.Fields()

	fm := &FileMeta{Tags: tagsFromDoc(doc, fields), Lyrics: lyricsFromDoc(doc, fields), CoverArt: coverFromDoc(doc)}
	fm.ItemPIDHint = firstTag(doc, tag.Key(model.TagWaxbinItemPID))
	// Before the filename fallback, so a legacy title beats a filename-derived one,
	// and before applyBookFields, which reads Album/Title/Comment/Composer.
	if filled := applyLegacyFallback(&fm.Tags, doc); len(filled) > 0 {
		fm.Diagnostics = append(fm.Diagnostics, model.FileDiagnostic{
			Code:     model.DiagLegacyOnlyTags,
			Severity: model.SeverityInfo,
			Detail:   capDetail("filled from a legacy tag container: " + joinKeys(filled)),
		})
	}
	if fm.Tags.Title == "" {
		fm.Tags.Title = titleFromPath(path)
	}
	fm.Tags.Chapters = chaptersFromDoc(doc)
	applyBookFields(&fm.Tags, fields, path)
	fm.Diagnostics = append(fm.Diagnostics, audioDiagnostics(doc)...)

	// HashAudioEssence covers encoded packets plus decoder-critical config, making
	// it stable across retags. Files with no audio frames fall back to the content
	// hash through an empty EssenceHash.
	if dig, herr := doc.HashAudioEssence(ctx, waxlabel.WithHashSource(src)); herr == nil {
		fm.EssenceHash = dig.String()
	} else if !errors.Is(herr, wlerr.ErrInvalidData) {
		return nil, waxerr.Wrapf(waxerr.CodeIO, op, herr, "hashing essence of %s", path)
	}

	return fm, nil
}

// tagsFromDoc projects a WaxLabel Document into WaxBin's Tags, normalizing codec
// names, joining the genre list for the denormalized column, and resolving a
// single display year from the available date fields. fields is the caller's
// already-projected tag view; doc supplies only the stream properties.
func tagsFromDoc(doc *waxlabel.Document, fields tag.Tags) model.Tags {
	props := doc.Properties()
	at := props.First()

	t := model.Tags{
		Title:           strings.TrimSpace(fields.Title),
		Artist:          strings.TrimSpace(first(fields.Artists)),
		Artists:         trimAll(fields.Artists),
		AlbumArtist:     strings.TrimSpace(fields.AlbumArtist),
		Album:           strings.TrimSpace(fields.Album),
		Composer:        strings.TrimSpace(first(fields.Composers)),
		Comment:         strings.TrimSpace(first(fields.Comment)),
		TrackNo:         fields.TrackNumber,
		TrackTotal:      fields.TrackTotal,
		DiscNo:          fields.DiscNumber,
		DiscTotal:       fields.DiscTotal,
		Year:            firstYear(fields.RecordingDate, fields.ReleaseDate, fields.OriginalDate),
		Genres:          trimAll(fields.Genres),
		Compilation:     fields.Compilation,
		ISRC:            strings.TrimSpace(fields.ISRC),
		Barcode:         strings.TrimSpace(fields.Barcode),
		Label:           strings.TrimSpace(fields.Label),
		CatalogNumber:   strings.TrimSpace(fields.CatalogNumber),
		ArtistSort:      strings.TrimSpace(fields.ArtistSort),
		AlbumSort:       strings.TrimSpace(fields.AlbumSort),
		AlbumArtistSort: strings.TrimSpace(fields.AlbumArtistSort),
		ComposerSort:    strings.TrimSpace(fields.ComposerSort),

		MBID:             strings.TrimSpace(fields.MusicBrainz.RecordingID),
		MBReleaseID:      strings.TrimSpace(fields.MusicBrainz.ReleaseID),
		MBReleaseGroupID: strings.TrimSpace(fields.MusicBrainz.ReleaseGroupID),
		MBArtistID:       strings.TrimSpace(first(fields.MusicBrainz.ArtistID)),
		MBAlbumArtistID:  strings.TrimSpace(first(fields.MusicBrainz.AlbumArtistID)),

		Container:  strings.ToLower(strings.TrimSpace(props.Container)),
		Codec:      normalizeCodec(at.Codec),
		DurationMS: props.Duration().Milliseconds(),
		SampleRate: at.SampleRate,
		Channels:   at.Channels,
		BitDepth:   at.BitsPerSample,

		Acquisition: model.TagAcquisition{
			SourceURL:  strings.TrimSpace(fields.SourceURL),
			SourceID:   strings.TrimSpace(fields.SourceID),
			AcquiredAt: parseAcquiredAt(fields.AcquisitionDate),
		},
	}
	t.Genre = strings.Join(t.Genres, "; ")
	if at.Bitrate > 0 {
		t.Bitrate = at.Bitrate / 1000 // bits/sec -> kbps for display
	}
	t.Custom = customTagsFromDoc(doc)
	return t
}

// customTagsFromDoc collects the file's tag frames that WaxBin's typed model does not
// map (and does not own through another surface), so they are preserved on scan rather
// than dropped. It walks the authoritative canonical tag set and keeps every key that
// is not reserved (see model.IsReservedTagKey) and carries at least one non-empty
// value. WaxLabel keys are already canonical uppercase, so they need no normalization.
func customTagsFromDoc(doc *waxlabel.Document) map[string][]string {
	var out map[string][]string
	for key, vals := range doc.Tags().All() {
		// WaxLabel keys are already canonical uppercase, but normalize defensively so the
		// reserved-key check and the stored key can never diverge from the store's own
		// canonicalization (a non-canonical or invalid key is skipped).
		k, ok := model.CanonicalTagKey(string(key))
		if !ok || model.IsReservedTagKey(k) {
			continue
		}
		kept := make([]string, 0, len(vals))
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				kept = append(kept, v)
			}
		}
		if len(kept) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[k] = kept
	}
	return out
}

// maxDetailBytes bounds a persisted diagnostic detail.
//
// The bound is not hypothetical. A tag_write_lost detail comes from a WaxLabel
// warning whose own doc says the message can embed a file-derived snippet. Upstream
// runs that through tag.SanitizeLine, which escapes the terminal-hijack and newline
// classes but leaves the length alone; sanitizing is a defense against injection,
// not against size. So the bound belongs here, at the seam where waxlabel's
// vocabulary becomes WaxBin's model.
const maxDetailBytes = 512

// capDetail truncates s to maxDetailBytes on a rune boundary, so a capped detail is
// still valid UTF-8 rather than ending in half a multi-byte rune.
func capDetail(s string) string {
	if len(s) <= maxDetailBytes {
		return s
	}
	b := s[:maxDetailBytes]
	for len(b) > 0 {
		// size > 1 distinguishes a genuine U+FFFD in the text from the RuneError the
		// decoder returns for a byte sequence cut in half.
		if r, size := utf8.DecodeLastRuneInString(b); r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// joinKeys renders canonical tag keys as a comma-separated list for a diagnostic
// detail.
func joinKeys(keys []tag.Key) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, string(k))
	}
	return strings.Join(parts, ", ")
}

// severityRank orders diagnostic severities so the most serious one wins when a
// single row must stand for several observations.
func severityRank(s model.AuditSeverity) int {
	switch s {
	case model.SeverityError:
		return 3
	case model.SeverityWarn:
		return 2
	case model.SeverityInfo:
		return 1
	}
	return 0
}

// audioDiagnostics projects the parse's audio-integrity warnings into diagnostics.
// They come free from the parse the scan already runs, which yields a corrupt-audio
// signal without the full re-read that --integrity's decode probe costs.
//
// The two warnings get different severities rather than a uniform error. Truncated
// audio means the container declares more audio than the file holds, which is a real
// defect. No-audio-frames is ambiguous by its own definition ("the file may be
// tag-only or truncated"), so an unconditional error would permanently flag a
// legitimately tag-only MP3 as broken.
//
// Coverage is format-partial, and callers have to say so. The underlying signals
// exist for MP3, AAC, AIFF, MP4, and WAV, and not for FLAC, Opus, Vorbis, or
// Matroska. They are true positives when they fire and prove nothing when they do
// not.
func audioDiagnostics(doc *waxlabel.Document) []model.FileDiagnostic {
	// One corrupt_audio row per file at most. The primary key is
	// (file_id, origin, code, tag_key) and neither warning names a key, so two rows
	// would collide and warning order would decide the survivor. Keep the more severe
	// one instead.
	var best *model.FileDiagnostic
	for _, w := range doc.Warnings() {
		var sev model.AuditSeverity
		switch w.Code {
		case waxlabel.WarnTruncatedAudio:
			sev = model.SeverityError
		case waxlabel.WarnNoAudioFrames:
			sev = model.SeverityWarn
		default:
			continue
		}
		if best != nil && severityRank(sev) <= severityRank(best.Severity) {
			continue
		}
		best = &model.FileDiagnostic{
			Code:     model.DiagCorruptAudio,
			Severity: sev,
			Detail:   capDetail(w.String()),
		}
	}
	if best == nil {
		return nil
	}
	return []model.FileDiagnostic{*best}
}

// acquisitionDateLayouts are the ISO-8601 reduced precisions WaxLabel accepts for a
// partial-date key: YYYY-MM-DD, YYYY-MM, YYYY. Most precise first. An exact length
// match enforces the zero-padded canonical form, rejecting "2019-5-3".
var acquisitionDateLayouts = []string{"2006-01-02", "2006-01", "2006"}

// parseAcquiredAt converts an ACQUISITION_DATE tag to unix nanoseconds, returning 0
// when it is absent or unparseable.
//
// A reduced precision resolves to the start of the period in UTC. The tag carries no
// zone, so reading it in the scanning machine's local zone would import the same file
// to a different instant on two machines. Zero is the store's "stamp it for me"
// sentinel, so an unusable value falls back to scan time, which is an approximation
// that says so, rather than to a wrong date stated with confidence.
//
// A malformed date is indistinguishable from a missing one here: both yield 0, the
// store stamps scan time, and because it records origin only when a row is absent it
// never revisits the value. A file naming an unreadable acquisition date therefore
// keeps scan time for good. That is accepted rather than overlooked. The alternative
// is a wider diagnostic vocabulary for a case whose only cost is an approximate date
// on a row that would carry the same approximation if the tag were simply absent.
func parseAcquiredAt(s string) int64 {
	s = strings.TrimSpace(s)
	// Year 0000 parses but is not a meaningful acquisition date. WaxLabel's own
	// partial-date validator rejects it, so agree with it.
	if s == "" || strings.HasPrefix(s, "0000") {
		return 0
	}
	for _, layout := range acquisitionDateLayouts {
		if len(s) != len(layout) {
			continue
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().UnixNano()
		}
	}
	return 0
}

// legacyFallbackKeys is the allowlist of canonical keys a legacy container may fill.
//
// The exclusions are forced rather than stylistic. Every MusicBrainz ID, ISRC,
// ASIN/ISBN, NARRATOR, and MEDIATYPE is withheld because identity.TrackKey returns
// "mbid:"+MBID in preference to "essence:"+hash, and applyBookFields keys a book off
// NARRATOR/MEDIATYPE. Promoting a value out of a legacy container into either would
// re-key an existing item on the next full scan, and upsertItem creates a new item on
// an identity miss, orphaning the old item's PID, play state, ratings, and
// provenance. The invariant this preserves is that the fallback fills display and
// entity fields only, and never changes an item's kind or identity key.
var legacyFallbackKeys = map[tag.Key]bool{
	tag.Title:       true,
	tag.Artist:      true,
	tag.Album:       true,
	tag.AlbumArtist: true,
	tag.Composer:    true,
	tag.Genre:       true,
	tag.Comment:     true,

	tag.RecordingDate: true,
	tag.TrackNumber:   true,
	tag.TrackTotal:    true,
	tag.DiscNumber:    true,
	tag.DiscTotal:     true,
}

// legacyFamilyRank orders legacy containers when more than one supplies the same
// legacy-only key: APEv2 > ID3v2 > Lyrics3 > ID3v1. ID3v1 ranks last because its
// fixed 30-byte slots silently truncate a longer value.
//
// The rank never competes with a canonical value. The fallback only fills, so this
// arbitrates among legacy families alone, and only for a key the authoritative tag
// set lacks entirely. ID3v2 appears despite normally being authoritative because
// Legacy is a per-container-role flag rather than a per-format one: ID3v2 is
// canonical for MP3 but legacy as FLAC's leading block.
//
// It switches on the family's string rather than its constant because WaxLabel
// exports no FamilyLyrics3 alias (the enum value exists and stringifies, but no v1.0
// parser emits one). Ranking by name keeps the documented order correct if upstream
// ever produces one, instead of sorting it below ID3v1 as an unranked zero. An
// unknown family ranks lowest and loses to any known one.
func legacyFamilyRank(f waxlabel.Family) int {
	switch f.String() {
	case "apev2":
		return 4
	case "id3v2":
		return 3
	case "lyrics3":
		return 2
	case "id3v1":
		return 1
	}
	return 0
}

// applyLegacyFallback fills empty fields on t from values that exist only in a legacy
// container (an MP3's ID3v1/APEv2 trailer, a FLAC's leading ID3v2 block). WaxLabel
// builds the canonical tag set from the authoritative container alone, so without
// this an MP3 carrying only an ID3v1 trailer catalogs with no artist, no album, and a
// filename-derived title.
//
// It fills and nothing more. A legacy container is non-authoritative by definition,
// so it can never override a canonical value, and legacyFallbackKeys restricts which
// keys it may fill at all.
//
// It returns the keys it filled, in a stable order. The caller reports a diagnostic
// only when that list is non-empty, meaning a fallback was applied, rather than
// whenever LegacyOnlyKeys is non-empty, which fires on a legacy-only ENCODEDBY that
// no consumer reads.
func applyLegacyFallback(t *model.Tags, doc *waxlabel.Document) []tag.Key {
	// LegacyOnlyKeys walks the family list without copying it, while Families deep-
	// copies every entry and its values. Gating on it spares the common clean file,
	// which has no legacy-only values at all, from paying for the copy.
	only := doc.LegacyOnlyKeys()
	if len(only) == 0 {
		return nil
	}
	want := make(map[tag.Key]bool, len(only))
	for _, k := range only {
		if legacyFallbackKeys[k] {
			want[k] = true
		}
	}
	if len(want) == 0 {
		return nil
	}

	// Keep the highest-ranked legacy family per key. Restricting to LegacyOnlyKeys
	// (Legacy, and the canonical set lacks the key) rather than checking Legacy alone
	// is what makes the fill safe: a key the canonical set holds is never a candidate,
	// so t's field and its projected sibling (Artist/Artists) cannot disagree.
	best := make(map[tag.Key]waxlabel.FamilyValue, len(want))
	for _, fv := range doc.Families() {
		if !fv.Legacy || !want[fv.Key] || len(fv.Values) == 0 {
			continue
		}
		if cur, ok := best[fv.Key]; ok && legacyFamilyRank(fv.Family) <= legacyFamilyRank(cur.Family) {
			continue
		}
		best[fv.Key] = fv
	}
	if len(best) == 0 {
		return nil
	}

	// filled records the keys a fill consumed, so the caller's diagnostic reports what
	// changed rather than what happened to be available. It is deduped because one
	// field can read a single key through both accessors (Artist/Artists).
	var filled []tag.Key
	seen := make(map[tag.Key]bool, len(best))
	note := func(k tag.Key, used bool) {
		if used && !seen[k] {
			seen[k] = true
			filled = append(filled, k)
		}
	}

	// A legacy container is not projected through tag.Project, so trim here for the
	// same reason applyBookFields does.
	firstVal := func(k tag.Key) string {
		fv, ok := best[k]
		if !ok {
			return ""
		}
		v := strings.TrimSpace(first(fv.Values))
		note(k, v != "")
		return v
	}
	allVals := func(k tag.Key) []string {
		fv, ok := best[k]
		if !ok {
			return nil
		}
		v := trimAll(fv.Values)
		note(k, len(v) > 0)
		return v
	}

	if t.Title == "" {
		t.Title = firstVal(tag.Title)
	}
	if t.Artist == "" {
		t.Artist = firstVal(tag.Artist)
		t.Artists = allVals(tag.Artist)
	}
	if t.AlbumArtist == "" {
		t.AlbumArtist = firstVal(tag.AlbumArtist)
	}
	if t.Album == "" {
		t.Album = firstVal(tag.Album)
	}
	if t.Composer == "" {
		t.Composer = firstVal(tag.Composer)
	}
	if t.Comment == "" {
		t.Comment = firstVal(tag.Comment)
	}
	if len(t.Genres) == 0 {
		t.Genres = allVals(tag.Genre)
		t.Genre = strings.Join(t.Genres, "; ")
	}
	if t.Year == 0 {
		t.Year = firstYear(firstVal(tag.RecordingDate))
	}
	// ParseNumPair matches tag.Project's convention: it trims, and every error
	// (including overflow) yields 0 rather than a partial value.
	if t.TrackNo == 0 && t.TrackTotal == 0 {
		t.TrackNo, t.TrackTotal = tag.ParseNumPair(firstVal(tag.TrackNumber), firstVal(tag.TrackTotal))
	}
	if t.DiscNo == 0 && t.DiscTotal == 0 {
		t.DiscNo, t.DiscTotal = tag.ParseNumPair(firstVal(tag.DiscNumber), firstVal(tag.DiscTotal))
	}
	return filled
}

// lyricsFromDoc projects a Document's embedded lyrics into WaxBin's model: the
// unsynchronized USLT block plus the first non-empty synced (SYLT) set, each timed
// line reduced to a millisecond offset. It returns nil when the file carries no
// lyric content. A sibling .lrc sidecar, read by the scanner, supersedes this.
// fields is the caller's already-projected tag view; doc supplies the SYLT sets.
func lyricsFromDoc(doc *waxlabel.Document, fields tag.Tags) *model.Lyrics {
	unsynced := strings.TrimSpace(fields.Lyrics)
	var synced []model.SyncedLine
	for _, set := range doc.SyncedLyrics() {
		if len(set.Lines) == 0 {
			continue
		}
		synced = make([]model.SyncedLine, 0, len(set.Lines))
		for _, ln := range set.Lines {
			synced = append(synced, model.SyncedLine{TimeMS: ln.Time.Milliseconds(), Text: ln.Text})
		}
		break // first non-empty set; alternate-language sets are ignored
	}
	// model.Lyrics.Synced promises time order; a SYLT frame may arrive out of order,
	// so sort defensively (the .lrc path is already sorted by ParseLRC). Stable, so
	// equal-timestamp lines keep their authored order.
	sort.SliceStable(synced, func(i, j int) bool { return synced[i].TimeMS < synced[j].TimeMS })
	if len(synced) == 0 && unsynced == "" {
		return nil
	}
	return &model.Lyrics{Source: "embedded", Synced: synced, Unsynced: unsynced}
}

// coverFromDoc extracts the embedded front-cover image from a Document: it prefers
// an explicit front-cover picture and otherwise takes the first picture. It returns
// the raw bytes plus a format derived from the picture MIME; the scanner finalizes
// the content hash and dimensions. It returns nil when the file embeds no usable
// picture.
func coverFromDoc(doc *waxlabel.Document) *model.ArtImage {
	pics := doc.Pictures()
	// Prefer a non-empty front cover; otherwise the first picture with bytes. A
	// zero-length entry (e.g. an empty front-cover frame) must not shadow a real one.
	var best *waxlabel.Picture
	for i := range pics {
		if pics[i].Type == waxlabel.PicFrontCover && len(pics[i].Data) > 0 {
			best = &pics[i]
			break
		}
	}
	if best == nil {
		for i := range pics {
			if len(pics[i].Data) > 0 {
				best = &pics[i]
				break
			}
		}
	}
	if best == nil {
		return nil
	}
	return &model.ArtImage{Data: best.Data, Format: formatFromMIME(best.MIME)}
}

// formatFromMIME maps an image MIME type to WaxBin's short format token, falling
// back to the MIME subtype for anything unrecognized.
func formatFromMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/avif":
		return "avif"
	case "image/heic", "image/heif":
		return "heic"
	}
	if i := strings.LastIndex(mime, "/"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(mime[i+1:]))
	}
	return ""
}

// ParseLRC parses .lrc sidecar text into WaxBin synced lines (millisecond offsets,
// time-ordered), plus the 1-based numbers of the lines the parser dropped: non-blank
// lines that yielded no timed lyric and are not recognized LRC structure, meaning a
// malformed timestamp or plain untimed text. Blank lines, [ar:]/[ti:] metadata,
// [offset:], and bare [section] headers count as structure and are not reported.
//
// It returns nil lines for text with no timed lines.
//
// The dropped list is why this reads through the reporting parser at all: it is the
// only way to tell a partly-broken sidecar from a plain-text one. It uses the
// uncapped variant because WaxBin reads the whole file into memory before parsing,
// so the parser's own line cap never protected anything. The read is bounded instead
// (see maxSidecarBytes).
func ParseLRC(text string) (lines []model.SyncedLine, dropped []int) {
	parsed, dropped := waxlabel.ParseLRCReportFull(text)
	if len(parsed) == 0 {
		return nil, dropped
	}
	out := make([]model.SyncedLine, 0, len(parsed))
	for _, ln := range parsed {
		out = append(out, model.SyncedLine{TimeMS: ln.Time.Milliseconds(), Text: ln.Text})
	}
	return out, dropped
}

// LRCPartial reports whether a .lrc parse is worth acting on, meaning some lines
// were timed and some were dropped.
//
// The guard on len(lines) > 0 carries the weight. A fully untimed .lrc reports every
// line as dropped, and that is the ordinary case of a plain-text file next to the
// audio rather than a broken sidecar. Reporting it would flag a great many normal
// files.
func LRCPartial(lines []model.SyncedLine, dropped []int) bool {
	return len(lines) > 0 && len(dropped) > 0
}

// chaptersFromDoc projects a Document's embedded navigation chapters (M4B Nero/
// QuickTime, Matroska, MP3 CHAP, read by WaxLabel) into WaxBin's model in file
// order, as file-relative offsets. The scanner persists them only for books; a
// music file with stray chapters keeps them unused. It returns nil for none.
func chaptersFromDoc(doc *waxlabel.Document) []model.Chapter {
	chs := doc.Chapters()
	if len(chs) == 0 {
		return nil
	}
	out := make([]model.Chapter, 0, len(chs))
	for i, c := range chs {
		out = append(out, model.Chapter{
			Position:    i,
			Title:       strings.TrimSpace(c.Title),
			FileStartMS: c.Start.Milliseconds(),
			FileEndMS:   c.End.Milliseconds(),
		})
	}
	return out
}

// applyBookFields classifies a file as an audiobook and, when it is, fills the
// spoken-word fields on t from the caller's already-projected tag view.
// A file is a book when its container is .m4b, its iTunes media kind is audiobook
// (stik=2), or it carries a narrator credit. Series/sequence, abridged/edition come
// from conventional tag patterns; ASIN/ISBN/subtitle/publisher are
// enrichment-populated (the schema and layout carry them, tags rarely do).
func applyBookFields(t *model.Tags, fields tag.Tags, path string) {
	// tag.Project does not trim, so trim the two values whose meaning depends on it:
	// mediaType is compared exactly, and narrator gates on being non-empty. The rest
	// reach a helper that trims by contract (firstNonEmpty, parseSeries).
	narrator := strings.TrimSpace(fields.Narrator)
	mediaType := strings.TrimSpace(fields.MediaType)
	isBook := strings.EqualFold(filepath.Ext(path), ".m4b") || mediaType == "2" || narrator != ""
	if !isBook {
		return
	}
	t.IsAudiobook = true
	// A common m4b/Audiobookshelf convention stores the narrator in COMPOSER when
	// there is no dedicated NARRATOR tag.
	if narrator == "" {
		narrator = t.Composer
	}
	t.Narrators = SplitCredits(narrator)
	t.Description = firstNonEmpty(fields.Description, fields.LongDescription)
	t.Series, t.SeriesSeq = parseSeries(fields.Grouping)
	t.Abridged, t.Edition = parseAbridged(t.Album, t.Title, t.Comment)
}

// firstTag returns the first value of a canonical key, trimmed, or "". It reads
// through Document.Get, which indexes the same map as TagSet.First with no
// normalization on either side, so it reads one key without cloning the whole set.
func firstTag(doc *waxlabel.Document, key tag.Key) string {
	v, ok := doc.Get(key)
	if !ok || len(v) == 0 {
		return ""
	}
	return strings.TrimSpace(v[0])
}

// SplitCredits splits a combined credit string (authors or narrators) into trimmed
// individual names. It delegates to identity.SplitCredits, the canonical shared
// splitter, and stays here for the adapter/scanner callers that reference
// meta.SplitCredits.
func SplitCredits(s string) []string { return identity.SplitCredits(s) }

// seriesSeqRe pulls a trailing sequence off a series/grouping value, but only when
// a clear marker precedes the number ('#' or book/part/vol[ume]), so a series name
// that simply ends in a number ("Area 51") is not split. The number may be decimal
// ("#1.5") for in-between entries.
var seriesSeqRe = regexp.MustCompile(`(?i)^(.+?)\s*(?:#|,?\s*(?:book|bk|part|vol\.?|volume)\s+)\s*([0-9]+(?:\.[0-9]+)?)\s*$`)

// parseSeries splits a grouping tag into a series name and an optional sequence.
// With no recognizable sequence marker the whole value is the series name.
func parseSeries(grouping string) (name, seq string) {
	grouping = strings.TrimSpace(grouping)
	if grouping == "" {
		return "", ""
	}
	if m := seriesSeqRe.FindStringSubmatch(grouping); m != nil {
		return strings.TrimSpace(m[1]), m[2]
	}
	return grouping, ""
}

// PackSeriesGrouping builds the GROUPING tag value that carries a book's series name
// and sequence, the inverse of parseSeries: it uses the "#" marker (the shortest form
// parseSeries recognizes) so a written value re-reads to the same name and sequence on
// a rescan. An empty name yields an empty value (clearing the tag); an empty sequence
// yields the bare name.
func PackSeriesGrouping(name, seq string) string {
	name = strings.TrimSpace(name)
	seq = strings.TrimSpace(seq)
	switch {
	case name == "":
		return ""
	case seq == "":
		return name
	default:
		return name + " #" + seq
	}
}

// abridgedRe matches the conventional bracketed marker "(Unabridged)"/"[Abridged]".
// Requiring the brackets (not a bare word anywhere) keeps a real title that merely
// contains the word, like "An Abridged History of Time", from getting a spurious
// Abridged flag and Edition, which would also pollute the identity key. The optional
// "un" capture decides the flag without depending on match order.
var abridgedRe = regexp.MustCompile(`(?i)[\(\[]\s*(un)?abridged\s*[\)\]]`)

// parseAbridged reads the conventional "(Unabridged)"/"(Abridged)" marker that
// audiobook taggers put in the album or title. It returns the abridged flag (nil
// when unmarked) and a matching edition label so the layout's {edition} and the
// abridged/unabridged distinction are real from tags alone.
func parseAbridged(texts ...string) (*bool, string) {
	m := abridgedRe.FindStringSubmatch(strings.Join(texts, " "))
	if m == nil {
		return nil, ""
	}
	if m[1] != "" { // the "un" prefix is present
		no := false
		return &no, "Unabridged"
	}
	yes := true
	return &yes, "Abridged"
}

// trimAll trims each element and drops the empties, preserving order.
func trimAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// extFormat derives a fallback display container/codec from a file extension.
// It is used only when WaxLabel cannot parse the format, keeping `show` and
// `doctor` from displaying a blank codec.
func extFormat(path string) (container, codec string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".wma":
		return "asf", "wma"
	case ".ape":
		return "ape", "ape"
	case ".wv":
		return "wavpack", "wavpack"
	default:
		return "", strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	}
}

// titleFromPath derives a display title from the filename (extension stripped),
// the last-resort fallback for a fully untagged file.
func titleFromPath(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base = strings.TrimSpace(base); base != "" {
		return base
	}
	return "Untitled"
}
