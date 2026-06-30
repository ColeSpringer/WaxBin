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
		if err := s.scanAudioFile(ctx, req.Library, root, path, res); err != nil {
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
	if err := s.scanAudioFile(ctx, lib, string(lib.Root), path, res); err != nil {
		res.Errored++
		return res, err
	}
	return res, nil
}

// scanAudioFile hashes, reads tags, and persists one audio file.
func (s *Scanner) scanAudioFile(ctx context.Context, lib *model.Library, root, path string, res *Result) error {
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
		Track: trackFromTags(tags),
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

var audioExts = map[string]bool{
	".mp3": true, ".flac": true, ".wav": true, ".ogg": true, ".oga": true,
	".opus": true, ".m4a": true, ".m4b": true, ".aac": true, ".mp4": true,
	".wma": true, ".aiff": true, ".aif": true, ".ape": true, ".wv": true,
}

func isAudio(path string) bool { return audioExts[strings.ToLower(filepath.Ext(path))] }

// IsAudio reports whether a path has a recognized audio extension. It is the one
// source of truth for the audio-file set, shared with the importer.
func IsAudio(path string) bool { return isAudio(path) }
