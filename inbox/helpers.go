package inbox

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/identity"
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
// audiobook template names (author/authorsort/series/seq/narrator/subtitle/asin), mirroring
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
		v.Title = scan.BookTitle(tags)
		v.Artist, v.AlbumArtist = author, author
		v.AuthorSort = firstNonEmpty(tags.AlbumArtistSort, tags.ArtistSort)
		v.Narrator = strings.Join(tags.Narrators, ", ")
		v.Series = tags.Series
		v.SeriesSeq = tags.SeriesSeq
		v.Subtitle = tags.Subtitle
		v.ASIN = tags.ASIN
	}
	return v
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

// stagingRoot is the folder an import reads from, the edge of what the folders above a
// file can name: the request's source folder, or the file's own folder when the file was
// handed over alone, and the folder above either when it is a disc folder, so the disc
// folder still names its disc.
func stagingRoot(source, path string) string {
	root := filepath.Dir(path)
	if source != "" && pathx.UnderRoot(source, path) && !pathx.SamePath(source, path) {
		root = source
	}
	if d, ok := identity.DiscFolder(filepath.Base(root)); ok && d > 0 {
		return filepath.Dir(root)
	}
	return root
}

// placeFile moves (or copies) a staged file to its destination through the shared
// long-path-safe mover, without clobbering an existing file there. The folders the
// destination names take its spelling (fsx.Speller).
func placeFile(sp *fsx.Speller, src, dst string, asCopy bool) error {
	const op = "inbox.place"
	if err := sp.MoveOrCopy(src, dst, asCopy); err != nil {
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

func nowNS() int64 { return time.Now().UnixNano() }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// heldAudio splits the cataloged files of a staged file's audio into the first that holds
// it and the first gone one backing an item of kind in library, the one the file lands in,
// which the import restores. A file on disk holds the audio, and so does one under a root
// that cannot be reached (it is out of sight, not gone). A gone file the import cannot
// bring back, another kind's item's (a track restores no book) or another library's (a
// scan takes a gone row back only in its own library), holds it only when no gone file the
// import restores does.
func heldAudio(occ []model.FileOccupant, kind model.Kind, library int64) (held, gone *model.FileOccupant) {
	var other *model.FileOccupant
	for i := range occ {
		o := &occ[i]
		switch {
		case o.OnDisk || !o.Reachable:
			if held == nil {
				held = o
			}
		case o.ItemPID == "":
		case o.Kind == kind && o.LibraryID == library:
			if gone == nil {
				gone = o
			}
		case other == nil:
			other = o
		}
	}
	if held == nil && gone == nil {
		held = other
	}
	return held, gone
}

// HeldAudio reports the cataloged file, of those holding an audio essence, that holds it
// for a staged file of kind landing in library (heldAudio), or nil: the rule an applied
// import rechecks a DupSkip file by.
func HeldAudio(occ []model.FileOccupant, kind model.Kind, library int64) *model.FileOccupant {
	held, _ := heldAudio(occ, kind, library)
	return held
}

// heldBy names what a cataloged file at a destination backs, as a reason says it.
func heldBy(o model.FileOccupant) string {
	if o.ItemPID == "" {
		return "a file that backs no item"
	}
	return "item " + string(o.ItemPID)
}

// occupantOf names a cataloged file as what holds an action back.
func occupantOf(o model.FileOccupant) *Occupant {
	return &Occupant{Path: o.DisplayPath, FilePID: o.FilePID, ItemPID: o.ItemPID}
}

// occupantAt names the file at path, with the catalog's identity for it when the catalog
// holds a file there.
func occupantAt(path string, occ []model.FileOccupant) *Occupant {
	for _, o := range occ {
		if o.DisplayPath == path {
			return occupantOf(o)
		}
	}
	return &Occupant{Path: path}
}

// checkRequest refuses a request no plan can serve and defaults its duplicate policy.
func checkRequest(op string, req *Request) error {
	if req.Library == nil || req.Library.Mode != model.ModeManaged {
		return waxerr.New(waxerr.CodeInvalid, op, "import target must be a managed library")
	}
	if req.DupPolicy == "" {
		req.DupPolicy = model.DupSkip
	}
	if !req.DupPolicy.Valid() {
		return waxerr.New(waxerr.CodeInvalid, op, "invalid duplicate policy: "+string(req.DupPolicy))
	}
	return nil
}

// sharedFolder is the deepest folder holding every staged file.
func sharedFolder(files []*staged) string {
	if len(files) == 0 {
		return ""
	}
	dir := filepath.Dir(files[0].Src)
	for _, f := range files[1:] {
		for !pathx.UnderRoot(dir, f.Src) {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return dir
}
