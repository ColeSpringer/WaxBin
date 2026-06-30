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

// itemViewFromTags builds the synthetic item view the organization template needs
// to render a destination for a not-yet-cataloged staged file. Only the layout
// fields are populated; DisplayPath carries the source so the extension resolves.
func itemViewFromTags(tags model.Tags, src string) *model.ItemView {
	return &model.ItemView{
		Kind:        model.KindTrack,
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
