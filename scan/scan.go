// Package scan walks library roots and persists catalog rows. It is I/O-bound
// and never decodes PCM: per file it stats, hashes content and audio essence,
// reads tags, and writes through model.Catalog. PCM decoding belongs to the
// separate analysis pass.
package scan

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Scanner indexes files into the catalog.
type Scanner struct {
	cat    model.Catalog
	reader meta.Reader
	log    *slog.Logger
}

// New builds a scanner over a catalog and metadata reader.
func New(cat model.Catalog, reader meta.Reader, log *slog.Logger) *Scanner {
	if reader == nil {
		reader = meta.NewReader()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{cat: cat, reader: reader, log: log}
}

// Request describes one scan.
type Request struct {
	Library *model.Library // target library (provides root + id)
	SubPath string         // optional sub-path under the root; empty scans the whole root
}

// Result tallies what a scan did.
type Result struct {
	FilesSeen    int
	AudioFiles   int
	ItemsCreated int
	ItemsUpdated int
	Relinked     int
	Skipped      int // non-audio files
	Errored      int
}

// Heartbeat is the progress callback invoked periodically during a scan.
type Heartbeat func(progress float64, msg string) error

const heartbeatEvery = 50

// Scan walks the request's root (or sub-path) and persists every audio file.
// Symlinks are not followed (no-follow + no loops). hb may be nil.
func (s *Scanner) Scan(ctx context.Context, req Request, hb Heartbeat) (*Result, error) {
	const op = "scan.Scan"
	if req.Library == nil {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "scan request has no library")
	}
	root := string(req.Library.Root)
	walkRoot := root
	if req.SubPath != "" {
		// A relative sub-path is interpreted under the library root; an absolute
		// one is used as-is. Either way it must resolve to within the root.
		walkRoot = req.SubPath
		if !filepath.IsAbs(walkRoot) {
			walkRoot = filepath.Join(root, walkRoot)
		}
		walkRoot = filepath.Clean(walkRoot)
		if !pathx.UnderRoot(root, walkRoot) {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "sub-path is outside the library root")
		}
	}

	res := &Result{}
	cache := newArtCache()
	walkErr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.log.Warn("walk entry", "path", path, "err", err)
			res.Errored++
			return nil // keep going past unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			// Never descend into the library's trash: those files were deleted, and
			// re-cataloging them would resurrect the items they backed.
			if d.Name() == model.TrashDirName {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil // no-follow symlink policy
		}
		if !d.Type().IsRegular() {
			return nil
		}

		res.FilesSeen++
		if !isAudio(path) {
			res.Skipped++
			return nil
		}
		if err := s.scanAudioFile(ctx, req.Library, root, path, res, cache); err != nil {
			s.log.Warn("scanning file", "path", path, "err", err)
			res.Errored++
		}
		if hb != nil && res.FilesSeen%heartbeatEvery == 0 {
			if err := hb(0, "scanned "+strconv.Itoa(res.FilesSeen)+" files"); err != nil {
				return err
			}
		}
		return nil
	})
	if walkErr != nil {
		return res, waxerr.FromContext(op, walkErr, waxerr.CodeIO)
	}
	if hb != nil {
		_ = hb(1, "scanned "+strconv.Itoa(res.FilesSeen)+" files")
	}
	return res, nil
}

// ScanFile catalogs a single audio file under its library. It is the entry point
// for re-cataloging one restored or freshly-imported file without walking the
// whole root; it shares the per-file path with the full scan, so identity,
// essence-relink, and change detection behave identically. A non-audio path is a
// no-op.
func (s *Scanner) ScanFile(ctx context.Context, lib *model.Library, path string) (*Result, error) {
	if lib == nil {
		return nil, waxerr.New(waxerr.CodeInvalid, "scan.ScanFile", "scan request has no library")
	}
	res := &Result{}
	if !isAudio(path) {
		return res, nil
	}
	res.FilesSeen++
	if err := s.scanAudioFile(ctx, lib, string(lib.Root), path, res, newArtCache()); err != nil {
		res.Errored++
		return res, err
	}
	return res, nil
}

// scanAudioFile hashes, reads tags, and persists one audio file.
func (s *Scanner) scanAudioFile(ctx context.Context, lib *model.Library, root, path string, res *Result, cache *artCache) error {
	info, err := os.Stat(path)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "scan.file", err)
	}

	contentHash, err := identity.ContentHash(path)
	if err != nil {
		return err
	}
	fm, err := s.reader.Read(ctx, path)
	if err != nil {
		return err
	}
	tags := fm.Tags
	// essence_hash anchors file identity independently of tags. Files with no
	// hashable essence fall back to the content hash, so they are still cataloged
	// but re-key on any byte change.
	essenceHash := fm.EssenceHash
	if essenceHash == "" {
		essenceHash = contentHash
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}

	file := model.File{
		Path:        []byte(path),
		DisplayPath: path,
		RelPath:     []byte(rel),
		Kind:        model.FileAudio,
		Size:        info.Size(),
		MTimeNS:     info.ModTime().UnixNano(),
		ContentHash: contentHash,
		EssenceHash: essenceHash,
		Container:   tags.Container,
		Codec:       tags.Codec,
		DurationMS:  tags.DurationMS,
		Bitrate:     tags.Bitrate,
		SampleRate:  tags.SampleRate,
		Channels:    tags.Channels,
		BitDepth:    tags.BitDepth,
		ScanState:   model.ScanIndexed,
	}

	in := model.PutScannedTrackInput{
		LibraryID: lib.ID,
		File:      file,
		Item: model.PlayableItem{
			Kind:        model.KindTrack,
			State:       model.StatePresent,
			Title:       tags.Title,
			SortKey:     model.SortKey(tags.Title),
			IdentityKey: identity.TrackKey(tags.MBID, essenceHash),
		},
		Track:    trackFromTags(tags),
		Lyrics:   sidecarLyrics(path, fm.Lyrics),
		CoverArt: resolveCover(path, fm.CoverArt, cache),
	}

	out, err := s.cat.PutScannedTrack(ctx, in)
	if err != nil {
		return err
	}

	res.AudioFiles++
	switch {
	case out.ItemCreated:
		res.ItemsCreated++
	case out.ContentChanged:
		res.ItemsUpdated++
	}
	if out.Relinked {
		res.Relinked++
	}
	return nil
}

// trackFromTags builds the track subtype from the parsed tags. ArtistSort
// prefers the tagged sort name and falls back to a generated key over the
// primary (or album) artist, so collation is correct even when a file carries no
// ARTISTSORT tag.
func trackFromTags(tags model.Tags) model.Track {
	artistForSort := tags.Artist
	if artistForSort == "" {
		artistForSort = tags.AlbumArtist
	}
	// Generate every stored sort key through model.SortKey. A tagged ARTISTSORT is
	// honored as input, but storing it raw would bypass normalization and sort
	// inconsistently against generated keys.
	sortInput := tags.ArtistSort
	if sortInput == "" {
		sortInput = artistForSort
	}
	artistSort := model.SortKey(sortInput)
	return model.Track{
		Artist:           tags.Artist,
		ArtistSort:       artistSort,
		Album:            tags.Album,
		AlbumArtist:      tags.AlbumArtist,
		Composer:         tags.Composer,
		Comment:          tags.Comment,
		TrackNo:          tags.TrackNo,
		TrackTotal:       tags.TrackTotal,
		DiscNo:           tags.DiscNo,
		DiscTotal:        tags.DiscTotal,
		Year:             tags.Year,
		Genre:            tags.Genre,
		Genres:           tags.Genres,
		Compilation:      tags.Compilation,
		ISRC:             tags.ISRC,
		MBID:             tags.MBID,
		MBReleaseID:      tags.MBReleaseID,
		MBReleaseGroupID: tags.MBReleaseGroupID,
		MBArtistID:       tags.MBArtistID,
		MBAlbumArtistID:  tags.MBAlbumArtistID,
	}
}

// sidecarLyrics resolves an audio file's structured lyrics. A sibling .lrc
// sidecar (read directly and parsed) is authoritative when present and carries
// timed lines; it supersedes the file's embedded lyrics but keeps the embedded
// unsynchronized block when the sidecar has only synced lines. With no usable
// sidecar, the embedded lyrics stand.
func sidecarLyrics(audioPath string, embedded *model.Lyrics) *model.Lyrics {
	lrcPath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".lrc"
	data, err := os.ReadFile(lrcPath)
	if err != nil {
		return embedded // no sidecar (or unreadable): fall back to embedded
	}
	synced := meta.ParseLRC(string(data))
	if len(synced) == 0 {
		return embedded
	}
	ly := &model.Lyrics{Source: "lrc", Synced: synced}
	if embedded != nil {
		ly.Unsynced = embedded.Unsynced
	}
	return ly
}

// artCache memoizes per-directory cover-image lookups for one scan run, so an
// album's directory cover is read and hashed once rather than for every track.
type artCache struct {
	dirs map[string]*model.ArtImage // resolved cover per directory (nil = none)
}

func newArtCache() *artCache { return &artCache{dirs: map[string]*model.ArtImage{}} }

// dirCover returns the directory's cover image, probing the directory once and
// caching the (possibly nil) result.
func (c *artCache) dirCover(dir string) *model.ArtImage {
	if img, ok := c.dirs[dir]; ok {
		return img
	}
	img := findDirCover(dir)
	c.dirs[dir] = img
	return img
}

// coverCandidates are the directory cover-image filenames WaxBin recognizes, in
// priority order. The match is case-insensitive against the directory listing.
var coverCandidates = []string{
	"cover.jpg", "cover.jpeg", "cover.png", "cover.webp",
	"folder.jpg", "folder.jpeg", "folder.png",
	"front.jpg", "front.jpeg", "front.png",
	"album.jpg", "albumart.jpg",
}

// resolveCover chooses a track's cover, preferring a decodable embedded image,
// then the directory cover image, and only then a non-decodable embedded image as
// a last resort. That keeps an embedded format without a registered decoder
// available to serve, while a corrupt or placeholder embedded picture does not
// shadow a valid cover.jpg next to the file.
func resolveCover(path string, embedded *model.ArtImage, cache *artCache) *model.ArtImage {
	if embedded != nil && finalizeArt(embedded) {
		return embedded // decodable embedded cover
	}
	if dir := cache.dirCover(filepath.Dir(path)); dir != nil {
		return dir // a valid (decodable) directory cover
	}
	// No usable directory cover: keep the embedded bytes even if they did not decode,
	// so a format without a local decoder is not dropped (finalizeArt set its hash).
	if embedded != nil && len(embedded.Data) > 0 && embedded.Hash != "" {
		return embedded
	}
	return nil
}

// findDirCover returns the first recognized cover image in dir (case-insensitive,
// in coverCandidates priority order), finalized, or nil when there is none.
func findDirCover(dir string) *model.ArtImage {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	byLower := make(map[string]string, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			byLower[strings.ToLower(e.Name())] = e.Name()
		}
	}
	for _, cand := range coverCandidates {
		name, ok := byLower[cand]
		if !ok {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		img := &model.ArtImage{Data: data}
		if finalizeArt(img) {
			return img
		}
	}
	return nil
}

// finalizeArt fills an image's content hash and, when the bytes decode, its
// format and pixel dimensions. It reports whether decoding succeeded. The hash is
// always set, so undecodable bytes can still be stored as a last resort, but a
// decodable cover is preferred over one that is not. Empty bytes return false.
func finalizeArt(img *model.ArtImage) bool {
	if img == nil || len(img.Data) == 0 {
		return false
	}
	img.Hash = art.Hash(img.Data)
	format, w, h, err := art.Probe(img.Data)
	if err != nil {
		return false
	}
	img.Format, img.Width, img.Height = format, w, h
	return true
}

var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".wav": true, ".ogg": true, ".oga": true,
	".opus": true, ".m4a": true, ".m4b": true, ".aac": true, ".mp4": true,
	".wma": true, ".aiff": true, ".aif": true, ".ape": true, ".wv": true,
}

func isAudio(path string) bool { return audioExts[strings.ToLower(filepath.Ext(path))] }

// IsAudio reports whether a path has a recognized audio extension. It is the one
// source of truth for the audio-file set, shared with the importer.
func IsAudio(path string) bool { return isAudio(path) }
