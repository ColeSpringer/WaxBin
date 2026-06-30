package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	waxlabel "github.com/colespringer/waxlabel"
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
			return &FileMeta{Tags: model.Tags{Title: titleFromPath(path), Container: container, Codec: codec}}, nil
		}
		return nil, waxerr.Wrapf(waxerr.CodeInvalid, op, err, "parsing %s", path)
	}

	fm := &FileMeta{Tags: tagsFromDoc(doc), Lyrics: lyricsFromDoc(doc), CoverArt: coverFromDoc(doc)}
	if fm.Tags.Title == "" {
		fm.Tags.Title = titleFromPath(path)
	}

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
// single display year from the available date fields.
func tagsFromDoc(doc *waxlabel.Document) model.Tags {
	fields := doc.Fields()
	props := doc.Properties()
	at := props.First()

	t := model.Tags{
		Title:           strings.TrimSpace(fields.Title),
		Artist:          strings.TrimSpace(first(fields.Artists)),
		Artists:         trimAll(fields.Artists),
		AlbumArtist:     strings.TrimSpace(fields.AlbumArtist),
		Album:           strings.TrimSpace(fields.Album),
		Composer:        strings.TrimSpace(first(fields.Composers)),
		Comment:         strings.TrimSpace(fields.Comment),
		TrackNo:         fields.TrackNumber,
		TrackTotal:      fields.TrackTotal,
		DiscNo:          fields.DiscNumber,
		DiscTotal:       fields.DiscTotal,
		Year:            firstYear(fields.RecordingDate, fields.ReleaseDate, fields.OriginalDate),
		Genres:          trimAll(fields.Genres),
		Compilation:     fields.Compilation,
		ISRC:            strings.TrimSpace(fields.ISRC),
		ArtistSort:      strings.TrimSpace(fields.ArtistSort),
		AlbumSort:       strings.TrimSpace(fields.AlbumSort),
		AlbumArtistSort: strings.TrimSpace(fields.AlbumArtistSort),

		MBID:             strings.TrimSpace(fields.MusicBrainz.RecordingID),
		MBReleaseID:      strings.TrimSpace(fields.MusicBrainz.ReleaseID),
		MBReleaseGroupID: strings.TrimSpace(fields.MusicBrainz.ReleaseGroupID),
		MBArtistID:       strings.TrimSpace(first(fields.MusicBrainz.ArtistID)),
		MBAlbumArtistID:  strings.TrimSpace(first(fields.MusicBrainz.AlbumArtistID)),

		Container:  strings.ToLower(strings.TrimSpace(props.Container)),
		Codec:      normalizeCodec(at.Codec, props.Container),
		DurationMS: props.Duration().Milliseconds(),
		SampleRate: at.SampleRate,
		Channels:   at.Channels,
		BitDepth:   at.BitsPerSample,
	}
	t.Genre = strings.Join(t.Genres, "; ")
	if at.Bitrate > 0 {
		t.Bitrate = at.Bitrate / 1000 // bits/sec -> kbps for display
	}
	return t
}

// lyricsFromDoc projects a Document's embedded lyrics into WaxBin's model: the
// unsynchronized USLT block plus the first non-empty synced (SYLT) set, each timed
// line reduced to a millisecond offset. It returns nil when the file carries no
// lyric content. A sibling .lrc sidecar, read by the scanner, supersedes this.
func lyricsFromDoc(doc *waxlabel.Document) *model.Lyrics {
	unsynced := strings.TrimSpace(doc.Fields().Lyrics)
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
	}
	if i := strings.LastIndex(mime, "/"); i >= 0 {
		return strings.ToLower(strings.TrimSpace(mime[i+1:]))
	}
	return ""
}

// ParseLRC parses .lrc sidecar text into WaxBin synced lines (millisecond
// offsets, time-ordered). WaxBin reads .lrc sidecars directly; this wraps
// WaxLabel's canonical LRC parser and projects it into the model type. It returns
// nil for text with no timed lines.
func ParseLRC(text string) []model.SyncedLine {
	lines := waxlabel.ParseLRC(text)
	if len(lines) == 0 {
		return nil
	}
	out := make([]model.SyncedLine, 0, len(lines))
	for _, ln := range lines {
		out = append(out, model.SyncedLine{TimeMS: ln.Time.Milliseconds(), Text: ln.Text})
	}
	return out
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
