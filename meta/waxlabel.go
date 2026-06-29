package meta

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	fm := &FileMeta{Tags: tagsFromDoc(doc)}
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
