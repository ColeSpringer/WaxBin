package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// acquiredItemView builds the synthetic item view the organization template needs to
// render a destination for a not-yet-cataloged staged file, for the given media kind.
// Only the layout fields are populated; DisplayPath carries the source so the
// extension resolves. A book kind additionally maps the spoken-word tag fields the
// audiobook template names (authorsort/series/seq/narrator/subtitle/asin), mirroring
// the scanner's book projection, so a book renders under the audiobook layout rather
// than the music one.
func acquiredItemView(tags model.Tags, src string, kind model.Kind) *model.ItemView {
	v := &model.ItemView{
		Kind:        kind,
		Title:       tags.Title,
		Artist:      tags.Artist,
		AlbumArtist: tags.AlbumArtist,
		Album:       tags.Album,
		TrackNo:     tags.TrackNo,
		DiscNo:      tags.DiscNo,
		Year:        tags.Year,
		Genre:       tags.Genre,
		Compilation: tags.Compilation,
		Codec:       tags.Codec,
		DisplayPath: src,
	}
	if kind == model.KindBook {
		author := firstNonEmpty(tags.AlbumArtist, tags.Artist)
		v.Title = firstNonEmpty(tags.Album, tags.Title)
		v.Artist, v.AlbumArtist = author, author
		v.AuthorSort = model.SortKey(firstNonEmpty(tags.AlbumArtistSort, tags.ArtistSort, author))
		v.Narrator = strings.Join(tags.Narrators, ", ")
		v.Series = tags.Series
		v.SeriesSeq = tags.SeriesSeq
		v.Subtitle = tags.Subtitle
		v.ASIN = tags.ASIN
	}
	return v
}

// classifyKind decides a staged file's media kind from its tags: an audiobook (per
// the WaxLabel adapter's stik/NARRATOR/extension heuristic) is a book, everything
// else a track. Episodes are never classified here; they are ingested explicitly.
func classifyKind(tags model.Tags) model.Kind {
	if tags.IsAudiobook {
		return model.KindBook
	}
	return model.KindTrack
}

// firstNonEmpty returns the first argument that is non-empty after trimming.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func isAudio(path string) bool { return scan.IsAudio(path) }

// placeFile moves (or copies) a staged file to its destination through the shared
// long-path-safe mover, without clobbering an existing file there.
func placeFile(src, dst string, asCopy bool) error {
	const op = "inbox.place"
	if err := fsx.MoveOrCopy(src, dst, asCopy); err != nil {
		if errors.Is(err, fsx.ErrExist) {
			return waxerr.New(waxerr.CodeConflict, op, "destination already exists: "+dst)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

func onDiskSize(path string) int64 {
	if info, err := os.Stat(pathx.Long(path)); err == nil {
		return info.Size()
	}
	return 0
}

func pathExists(path string) bool {
	_, err := os.Stat(pathx.Long(path))
	return err == nil
}

func caseFold(s string) string { return strings.ToLower(filepath.Clean(s)) }

func nowNS() int64 { return time.Now().UnixNano() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
