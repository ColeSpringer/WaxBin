// Package scan walks library roots and persists catalog rows. It is I/O-bound
// and never decodes PCM: per file it stats, hashes content and audio essence,
// reads tags, and writes through model.Catalog. PCM decoding belongs to the
// separate analysis pass.
package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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
	// Force bypasses the incremental fast-path: every file is re-hashed, re-parsed,
	// and re-upserted even when its size and mtime are unchanged. Use it to repair a
	// catalog or after an essence-algorithm change. It does not affect analysis,
	// which re-runs on its own analysis_version.
	Force bool
	// AdoptStampedPIDs makes the scan pass each file's WAXBIN_ITEM_PID tag to the
	// store as a preferred item PID, so a rebuild restores original identities. The
	// store adopts it only when creating a new item and only when unambiguous. Off for
	// a normal scan (the store owns PID assignment).
	AdoptStampedPIDs bool
	// ForceReconcile bypasses the survival gate's ">=50% of known files must be seen"
	// floor, so a deliberate large deletion is reconciled (deleted items become missing)
	// instead of latching present forever. It still requires the root to be readable
	// (a genuinely unreadable/errored root is never reconciled). It is an explicit
	// operator action; the watcher never sets it, so a transient mount loss during a
	// scheduled/forced watch rescan can never wipe the catalog.
	ForceReconcile bool
}

// Result tallies what a scan did. Every field counts files, not items: the Items*
// names say what cataloging a file did, not how many items came out of it.
//
// The two diverge for a single-file album rip, where one .cue-carved file becomes N
// virtual-track items and still reports ItemsCreated 1. Nothing here is an item
// count; query the catalog for that.
//
// The outcome counters do not sum to AudioFiles, so do not present them as a
// partition. Each audio file takes at most one of ItemsCreated/ItemsUpdated/
// SidecarsUpdated on the full path, or Unchanged on the fast path. But Relinked is
// independent and rides along with whichever of those applied, SidecarsUpdated also
// fires alongside Unchanged when the fast path applies a sidecar edit, and a forced
// rescan that finds nothing changed takes none of them.
type Result struct {
	FilesSeen int
	// AudioFiles is every audio file cataloged, on either path.
	AudioFiles int
	// ItemsCreated is audio files whose scan created at least one item (one rip that
	// created twelve virtual tracks counts once).
	ItemsCreated int
	// ItemsUpdated is audio files whose audio content changed.
	ItemsUpdated int
	// Relinked is audio files matched to an existing item by essence hash (a move or
	// rename). Independent of the other outcomes rather than exclusive with them.
	Relinked  int
	Unchanged int // fast-pathed: size+mtime matched, no hashing/parsing/upsert
	// SidecarsUpdated is audio files whose .lrc/.cue change was applied without the
	// audio changing, on either path: the fast path applies a cheap sidecar edit in
	// place, and the full path lands here when a re-read found only sidecars changed.
	SidecarsUpdated int
	Missing         int // items reconciled to 'missing' (backing files gone from disk)
	Skipped         int // non-audio files
	Errored         int
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
	sc := &scanCtx{cache: newArtCache(), force: req.Force, adopt: req.AdoptStampedPIDs}

	// Preload the scope's file index once, so the walk fast-paths an unchanged file
	// (size+mtime match) in memory and reconciles vanished ones at end-of-walk, with
	// no per-file SELECT. A load failure degrades to a full scan with no
	// reconciliation rather than aborting.
	var scopePrefix []byte
	if req.SubPath != "" {
		scopePrefix = append([]byte(walkRoot), filepath.Separator)
	}
	if idx, err := s.cat.LoadScopedFileIndex(ctx, req.Library.ID, scopePrefix); err != nil {
		s.log.Warn("scan fast-path disabled: could not preload file index", "err", err)
	} else {
		sc.index = idx
	}
	knownCount := len(sc.index)

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
		if err := s.scanAudioFile(ctx, req.Library, root, path, res, sc, ""); err != nil {
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

	// Reconcile deletions: entries still in the index were never walked, so their
	// files are gone from disk. The survival gate refuses to act on a transiently
	// unavailable root, so a momentary mount loss cannot mark the whole library
	// missing.
	s.reconcileMissing(ctx, walkRoot, sc.index, knownCount, req.ForceReconcile, res)

	if hb != nil {
		_ = hb(1, "scanned "+strconv.Itoa(res.FilesSeen)+" files")
	}
	return res, nil
}

// scanCtx carries the per-scan fast-path state through the walk. The walk is
// single-goroutine (WalkDir invokes its callback sequentially), so the index map
// needs no locking.
type scanCtx struct {
	index map[string]model.ScopedFile // path -> known file; entries deleted as visited
	force bool                        // bypass the fast-path (re-hash everything)
	adopt bool                        // pass WAXBIN_ITEM_PID hints to the store (rebuild)
	cache *artCache
}

// reconcileMissing marks the items behind the index's residual (unwalked) files as
// missing, behind a survival gate. The gate distinguishes a genuine removal (root
// absent, or a healthy scan that simply saw fewer files) from a transient failure
// (root exists but is empty/unreadable, or a partial walk saw far fewer files than
// known), and skips reconciliation entirely on the transient cases, logging a
// degraded warning and keeping every row, so a momentary mount loss cannot wipe the
// catalog (which Phase 4's orphan GC could then compound).
func (s *Scanner) reconcileMissing(ctx context.Context, walkRoot string, index map[string]model.ScopedFile, knownCount int, forceReconcile bool, res *Result) {
	if len(index) == 0 {
		return // every known file was seen; nothing vanished
	}
	if ctx.Err() != nil {
		return // a canceled scan is incomplete; do not treat unwalked files as missing
	}

	info, statErr := os.Stat(walkRoot)
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// The root is genuinely gone: a real full removal, reconcile everything.
	case statErr != nil:
		s.log.Warn("watch degraded: scan root unreadable, skipping deletion reconciliation",
			"root", walkRoot, "err", statErr)
		return
	case !info.IsDir():
		s.log.Warn("watch degraded: scan root is not a directory, skipping deletion reconciliation",
			"root", walkRoot)
		return
	case forceReconcile:
		// The operator explicitly asked to reconcile deletions (the root is readable),
		// so bypass the floor. This is the recovery path for a genuine >50% deletion that the
		// survival gate would otherwise never reconcile.
	default:
		// Root exists: require a floor. Seeing zero files, or fewer than half of what
		// we previously knew, reads as a transient empty/unreadable mount rather than a
		// real mass deletion, so keep the rows. A genuine large deletion is not
		// reconciled here (the survival gate protects against a mount blip); the operator
		// runs `scan --reconcile-deletions` to force it.
		if res.AudioFiles == 0 || res.AudioFiles*2 < knownCount {
			s.log.Warn("scan: skipping deletion reconciliation (survival gate): fewer than half of known files were seen; "+
				"rows kept in case the root is only transiently unavailable; run `scan --reconcile-deletions` to force",
				"root", walkRoot, "seen", res.AudioFiles, "known", knownCount, "would_mark_missing", len(index))
			return
		}
	}

	pids := make([]model.PID, 0, len(index))
	for _, e := range index {
		pids = append(pids, e.FilePID)
	}
	n, err := s.cat.MarkFilesMissing(ctx, pids)
	if err != nil {
		s.log.Warn("reconciling missing files", "root", walkRoot, "err", err)
		return
	}
	res.Missing = n
}

// ScanFile catalogs a single audio file under its library, classifying its kind from
// tags. It is the entry point for re-cataloging one restored or freshly-imported file
// without walking the whole root; it shares the per-file path with the full scan, so
// identity, essence-relink, and change detection behave identically. A non-audio path
// is a no-op.
func (s *Scanner) ScanFile(ctx context.Context, lib *model.Library, path string) (*Result, error) {
	return s.scanFileForced(ctx, lib, path, "")
}

// ScanFileAs catalogs a single audio file, forcing its media kind rather than
// classifying it from tags. Use it when the caller already knows the kind, such as an
// audiobook whose tags do not identify it as one. An empty kind classifies from tags.
func (s *Scanner) ScanFileAs(ctx context.Context, lib *model.Library, path string, kind model.Kind) (*Result, error) {
	return s.scanFileForced(ctx, lib, path, kind)
}

func (s *Scanner) scanFileForced(ctx context.Context, lib *model.Library, path string, kind model.Kind) (*Result, error) {
	if lib == nil {
		return nil, waxerr.New(waxerr.CodeInvalid, "scan.ScanFile", "scan request has no library")
	}
	res := &Result{}
	if !isAudio(path) {
		return res, nil
	}
	res.FilesSeen++
	// A single-file scan has no preloaded index, so it always takes the full path.
	if err := s.scanAudioFile(ctx, lib, string(lib.Root), path, res, &scanCtx{cache: newArtCache()}, kind); err != nil {
		res.Errored++
		return res, err
	}
	return res, nil
}

// scanAudioFile hashes, reads tags, and persists one audio file. forceKind overrides
// the tag-based track/book classification when non-empty. When the scan context
// carries a preloaded index and the file's size and mtime match its known entry, it
// takes the fast-path: no content/essence hashing, no tag parse, no upsert, just a
// cheap sidecar re-check, and the file is dropped from the index (marking it seen).
func (s *Scanner) scanAudioFile(ctx context.Context, lib *model.Library, root, path string, res *Result, sc *scanCtx, forceKind model.Kind) error {
	info, err := os.Stat(path)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "scan.file", err)
	}

	// Fast-path: an unchanged file (size+mtime match a preloaded entry) skips all
	// hashing, tag parsing, and the full upsert. Whether or not it matches, a known
	// file is removed from the index so it is never treated as a missing deletion.
	//
	// Trust model / blind spot: size+mtime (git's default heuristic) misses a
	// same-size, mtime-preserving change (rsync --times, cp -p, an in-place external
	// tag edit that keeps the byte length, or bit-rot), and exFAT/FAT round mtime to
	// 2 s, so two same-size edits inside one window can collide. That is the accepted
	// cost of a cheap rescan; the watcher's periodic full-content rescan (Force) and
	// an explicit `scan --force` are the backstops that re-hash everything.
	if sc.index != nil {
		if known, ok := sc.index[path]; ok {
			delete(sc.index, path)
			if !sc.force && known.Size == info.Size() && known.MTimeNS == info.ModTime().UnixNano() {
				// Size+mtime match. Reconcile sidecars: cheap changes (.lrc/.cue) apply in
				// place; a change that needs the audio (a sidecar vanished or became
				// unusable, so revert to embedded; a directory cover changed, so resolveCover
				// precedence) returns needsFull and falls through to the full path.
				if !s.reconcileFastPathSidecars(ctx, path, known, sc.cache, res) {
					res.AudioFiles++
					res.Unchanged++
					return nil
				}
			}
		}
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

	cover := resolveCover(path, fm.CoverArt, sc.cache)
	lyrics, aux, sidecarDiags := scanSidecars(path, fm.Lyrics, sc.cache)

	// The reader's own observations (unsupported container, legacy-tag fallback,
	// corrupt audio) plus the sidecar scan's. The store replaces this scan's whole set
	// for the file, so one that comes back clean clears its own stale rows.
	diags := append(fm.Diagnostics, sidecarDiags...)

	// A sibling .cue is examined when the file carries no embedded chapters. A book
	// applies its tracks as chapters; a non-book single file with a multi-track .cue is
	// an album rip whose tracks become virtual tracks.
	//
	// The observation has to be recorded either way. The fast path stat-compares only
	// the sidecars it holds an observation for, so a track with a .cue that yields no
	// chapters would read as new on every scan, route to the full path, and re-hash
	// the audio each time. It is the same trap the directory-cover stat fallback
	// already avoids.
	var cueSheet *meta.CueSheet
	// carve is the sheet reduced to the tracks that can actually become virtual tracks.
	// Its length decides below whether this file is a rip, since a virtual track is
	// nothing but its start offset and the window that offset opens. Counting the
	// tracks that named no usable window instead would carve a rip out of a sheet that
	// has none to carve, and a sheet whose every track was unusable would commit the
	// file with no items at all.
	var carve []meta.CueTrack
	if len(tags.Chapters) == 0 {
		if sheet, cueObs, cueDiags, ok := scanCueSidecar(path); ok {
			aux = append(aux, cueObs)
			diags = append(diags, cueDiags...)
			cueSheet = sheet
		}
		if cueSheet != nil {
			var dropped []string
			carve, dropped = cueTracksToCarve(cueSheet)
			// Report the drops here rather than on the rip path, so they stay visible
			// whichever path the file then takes: a sheet left with fewer than two carvable
			// tracks never reaches virtualTracksInput at all.
			diags = append(diags, cueTracksDroppedDiag(dropped)...)
		}
	}

	// An audiobook takes the book path: it groups by book identity (so a multi-file
	// book collapses its parts into one item) and carries contributors and chapters.
	// Everything else is a music track. A forced kind from the caller wins over the
	// tag heuristic.
	isBook := tags.IsAudiobook
	switch forceKind {
	case model.KindBook:
		isBook = true
	case model.KindTrack:
		isBook = false
	}
	var out *model.ScanItemResult
	switch {
	case !isBook && len(carve) >= 2:
		// A single file with a multi-track .cue is a single-file album rip: carve each
		// cue TRACK into its own virtual track with an offset window, rather than
		// cataloguing the whole file as one track. Fewer than two usable tracks is not a
		// rip: one window over the whole file is just the file, and it would be strictly
		// worse as a virtual track (its tags become unwritable and it exports no
		// fingerprint), so it falls through to the plain whole-file path below.
		out, err = s.cat.PutScannedVirtualTracks(ctx,
			virtualTracksInput(lib.ID, file, tags, essenceHash, cueSheet, carve, cover, aux, diags))
	case isBook:
		bin := bookInput(lib.ID, file, tags, essenceHash, cover)
		// With no embedded chapters, a sibling .cue fills them (marked source='cue' so
		// embedded chapters still win). Its observation was recorded above whether or not
		// it yielded any; apply the chapters only when it did.
		if cueSheet != nil {
			bin.Chapters, bin.ChapterSource = cueSheet.Chapters(), "cue"
		}
		bin.AuxObservations = aux
		bin.PreferredItemPID = adoptedPID(sc, fm)
		bin.Diagnostics = diags
		out, err = s.cat.PutScannedBook(ctx, bin)
	default:
		out, err = s.cat.PutScannedTrack(ctx, model.PutScannedTrackInput{
			LibraryID: lib.ID,
			File:      file,
			Item: model.PlayableItem{
				Kind:        model.KindTrack,
				State:       model.StatePresent,
				Title:       tags.Title,
				SortKey:     model.SortKey(tags.Title),
				IdentityKey: identity.TrackKey(tags.MBID, essenceHash),
			},
			Track:            trackFromTags(tags),
			Lyrics:           lyrics,
			CoverArt:         cover,
			AuxObservations:  aux,
			PreferredItemPID: adoptedPID(sc, fm),
			Acquisition:      tags.Acquisition,
			Diagnostics:      diags,
		})
	}
	if err != nil {
		return err
	}

	res.AudioFiles++
	switch {
	case out.ItemCreated:
		res.ItemsCreated++
	case out.ContentChanged:
		res.ItemsUpdated++
	case out.SidecarsChanged:
		// A sidecar-only change (an edited .lrc, a new cover) reaches the full path but
		// changes no audio bytes, so ItemCreated and ContentChanged are both false.
		// Without this case every counter stays zero, the scan reports changed=false, and
		// watch mode's downstream schedulers are silently skipped. It also keeps
		// SidecarsUpdated meaning what its doc says instead of quietly becoming
		// fast-path-only.
		res.SidecarsUpdated++
	}
	if out.Relinked {
		res.Relinked++
	}
	return nil
}

// adoptedPID returns the file's WAXBIN_ITEM_PID hint when the scan is in adopt mode
// (rebuild), else empty. The store decides whether to actually adopt it.
func adoptedPID(sc *scanCtx, fm *meta.FileMeta) model.PID {
	if !sc.adopt {
		return ""
	}
	return model.PID(fm.ItemPIDHint)
}

// bookInput composes one audiobook file into a book persistence input. The book
// title and author are the album/album-artist (the file title/artist hold a
// chapter or part name in multi-file books); the book key groups the parts of one
// work, falling back to the essence hash for an untitled book so a rescan still
// dedups to one item. A file with no embedded chapters contributes a single
// whole-file chapter so a multi-file book still navigates by part.
func bookInput(libraryID int64, file model.File, tags model.Tags, essenceHash string, cover *model.ArtImage) model.PutScannedBookInput {
	title := cleanBookTitle(firstNonEmpty(tags.Album, tags.Title))
	author := firstNonEmpty(tags.AlbumArtist, tags.Artist)
	key := identity.BookKey(tags.ASIN, tags.ISBN, author, title, tags.Edition)
	if key == "" {
		key = identity.TrackKey("", essenceHash)
	}

	chapters := tags.Chapters
	chapterSource := "embedded"
	if len(chapters) == 0 {
		// No embedded chapters: one whole-file chapter (open-ended) titled by the
		// file, so a multi-file book navigates part-by-part and a single-file book
		// still has one entry. Marked 'synthetic' so an external .cue outranks it.
		chapters = []model.Chapter{{Position: 0, Title: tags.Title}}
		chapterSource = "synthetic"
	}

	position := tags.TrackNo
	if tags.DiscNo > 0 {
		position = tags.DiscNo*100000 + tags.TrackNo
	}

	authorSort := model.SortKey(firstNonEmpty(tags.AlbumArtistSort, tags.ArtistSort, author))
	return model.PutScannedBookInput{
		LibraryID: libraryID,
		File:      file,
		Item: model.PlayableItem{
			Kind:        model.KindBook,
			State:       model.StatePresent,
			Title:       title,
			SortKey:     model.SortKey(title),
			IdentityKey: key,
		},
		Book: model.Book{
			Subtitle:    tags.Subtitle,
			Author:      author,
			AuthorSort:  authorSort,
			Authors:     meta.SplitCredits(author),
			Narrators:   tags.Narrators,
			Narrator:    strings.Join(tags.Narrators, ", "),
			Series:      tags.Series,
			SeriesSeq:   tags.SeriesSeq,
			Year:        tags.Year,
			Publisher:   tags.Publisher,
			ASIN:        tags.ASIN,
			ISBN:        tags.ISBN,
			Edition:     tags.Edition,
			Abridged:    tags.Abridged,
			Description: tags.Description,
			Genres:      tags.Genres,
			Genre:       tags.Genre,
		},
		Position:      position,
		Chapters:      chapters,
		ChapterSource: chapterSource,
		CoverArt:      cover,
		Acquisition:   tags.Acquisition,
	}
}

// virtualTracksInput composes a single-file album rip and its .cue sheet into a
// virtual-track persistence input. Album-level fields prefer the cue header and fall
// back to the file's own tags. Each cue TRACK becomes a virtual track whose start is
// its INDEX 01 offset and whose end is the next track's start (the final track's end
// is left open), and whose per-track performer falls back to the album artist.
// Windows are CD frames throughout, the unit the sheet is written in. Identity is
// offset-anchored via VirtualTrackKey, so a rescan re-keys the same tracks and a
// per-track title retag does not fork identity.
//
// cts are the carvable tracks in start order, already reduced and reported by
// cueTracksToCarve; the caller gated on there being at least two of them, so every
// one here names a real, non-empty window.
func virtualTracksInput(libraryID int64, file model.File, tags model.Tags, essenceHash string, sheet *meta.CueSheet, cts []meta.CueTrack, cover *model.ArtImage, aux []model.AuxObservation, diags []model.FileDiagnostic) model.PutScannedVirtualTracksInput {
	album := firstNonEmpty(sheet.Title, tags.Album, tags.Title)
	albumArtist := firstNonEmpty(sheet.Performer, tags.AlbumArtist, tags.Artist)
	genre := firstNonEmpty(sheet.Genre, tags.Genre)
	year := sheet.Year
	if year == 0 {
		year = tags.Year
	}

	tracks := make([]model.VirtualTrack, 0, len(cts))
	for i, ct := range cts {
		start := ct.StartFrames
		// The final track's end stays open rather than copying the file's probed
		// duration: that duration is a millisecond, and milliseconds are the lossy
		// direction. An open end already reads back as the same duration through
		// COALESCE, and it keeps a re-analysis that refines the file's duration from
		// emitting a spurious change row.
		var end int64
		if i+1 < len(cts) {
			end = cts[i+1].StartFrames
		}
		artist := firstNonEmpty(ct.Performer, albumArtist)
		title := ct.Title
		if title == "" {
			title = fmt.Sprintf("Track %02d", ct.Number)
		}
		tracks = append(tracks, model.VirtualTrack{
			Item: model.PlayableItem{
				Kind:        model.KindTrack,
				State:       model.StatePresent,
				Title:       title,
				SortKey:     model.SortKey(title),
				IdentityKey: identity.VirtualTrackKey(essenceHash, ct.Number, start),
			},
			Track: model.Track{
				Artist:      artist,
				ArtistSort:  model.SortKey(artist),
				Album:       album,
				AlbumArtist: albumArtist,
				TrackNo:     ct.Number,
				Year:        year,
				Genre:       genre,
				Genres:      identity.SplitGenres(genre),
			},
			StartFrames: start,
			EndFrames:   end,
		})
	}
	return model.PutScannedVirtualTracksInput{
		LibraryID:       libraryID,
		File:            file,
		Tracks:          tracks,
		CoverArt:        cover,
		AuxObservations: aux,
		Acquisition:     tags.Acquisition,
		Diagnostics:     diags,
	}
}

// abridgedMarkerRe matches a trailing BRACKETED "(Unabridged)"/"[Abridged]" marker
// that audiobook taggers append to the title or album. parseAbridged in the meta
// adapter reads the flag; this strips it so the stored book title is clean. It
// requires the brackets (mirroring parseAbridged) so a title that genuinely ends in
// the word is not truncated, which would also shift the identity key.
var abridgedMarkerRe = regexp.MustCompile(`(?i)\s*[\(\[]\s*(?:un)?abridged\s*[\)\]]\s*$`)

// cleanBookTitle removes a trailing bracketed abridged/unabridged marker from a
// book title.
func cleanBookTitle(s string) string {
	cleaned := strings.TrimSpace(abridgedMarkerRe.ReplaceAllString(s, ""))
	if cleaned == "" {
		return strings.TrimSpace(s) // never strip the whole title away
	}
	return cleaned
}

// firstNonEmpty returns the first argument that is non-empty after trimming, with
// surrounding whitespace removed, so it does not leak into stored display columns.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
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

// scanSidecars resolves an audio file's structured lyrics and records the on-disk
// observations of its sidecars (the sibling .lrc and the directory cover), so a
// later scan can stat-compare them and re-parse only a changed one.
//
// A sibling .lrc sidecar (read directly and parsed) is authoritative when present
// and carries timed lines; it supersedes the file's embedded lyrics but keeps the
// embedded unsynchronized block when the sidecar has only synced lines. With no
// usable sidecar, the embedded lyrics stand.
//
// It also returns any diagnostics the sidecars warrant (a partly-timed .lrc).
func scanSidecars(audioPath string, embedded *model.Lyrics, cache *artCache) (*model.Lyrics, []model.AuxObservation, []model.FileDiagnostic) {
	lyrics := embedded
	var aux []model.AuxObservation
	var diags []model.FileDiagnostic

	// Stat before reading. The stat bounds the read (maxSidecarBytes) and supplies the
	// observation, so an oversized or vanished .lrc is never pulled into memory.
	lrcPath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".lrc"
	if info, serr := os.Stat(lrcPath); serr == nil {
		switch {
		case info.Size() > maxSidecarBytes:
			// Skipped, but not in silence: record the skip so it is visible, and a
			// stat-only observation so the fast path does not route here every scan. The
			// embedded lyrics stand, which is the truthful result, since the sidecar that
			// would have superseded them was never read.
			diags = append(diags, model.FileDiagnostic{
				Code: model.DiagSidecarSkipped, Severity: model.SeverityWarn,
				Detail: sidecarSkippedDetail(model.AuxLyrics, info.Size()),
			})
			aux = append(aux, statOnlyObs(model.AuxLyrics, lrcPath, info))
		default:
			if data, err := os.ReadFile(lrcPath); err == nil {
				synced, dropped := meta.ParseLRC(string(data))
				if len(synced) > 0 {
					ly := &model.Lyrics{Source: "lrc", Synced: synced}
					if embedded != nil {
						ly.Unsynced = embedded.Unsynced
					}
					lyrics = ly
				}
				if meta.LRCPartial(synced, dropped) {
					diags = append(diags, model.FileDiagnostic{
						Code: model.DiagLyricsPartial, Severity: model.SeverityWarn,
						Detail: lrcPartialDetail(synced, dropped),
					})
				}
				aux = append(aux, model.AuxObservation{
					Kind: model.AuxLyrics, Path: []byte(lrcPath),
					Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), Hash: art.Hash(data),
				})
			}
		}
	}
	// Record the directory cover observation so the fast-path can stat-compare it next
	// time. Prefer the resolved (decodable) cover's hashed observation; otherwise fall
	// back to a stat-only observation of a present-but-undecodable cover file. Without
	// that fallback the fast-path (which detects covers by existence, not decodability)
	// would treat the undecodable file as newly-appeared on every scan and force a full
	// reprocess forever.
	dir := filepath.Dir(audioPath)
	if obs := cache.dirCoverObs(dir); obs != nil {
		aux = append(aux, *obs)
	} else if obs := cache.dirCoverStat(dir); obs != nil {
		aux = append(aux, *obs)
	}
	return lyrics, aux, diags
}

// maxSidecarBytes bounds a sidecar read. A .lrc/.cue is a few KiB of text, so a file
// orders of magnitude larger is corrupt or hostile and should not be pulled whole
// into memory during a scan. It guards memory, and is not a limit on content.
//
// The bound belongs on the read rather than the parse. WaxBin pulls the whole sidecar
// in with os.ReadFile before any parser sees it, so a parser-side line cap never
// protected anything.
const maxSidecarBytes = 8 << 20

// statOnlyObs builds a sidecar observation from a stat alone, with no content hash,
// for a file that is on disk but was not read.
//
// Recording one is what keeps a skip from repeating. The fast path stat-compares
// against the stored observation, so without it an oversized sidecar reads as newly
// appeared on every scan, routes to the full path, and re-hashes the audio each time.
// With it, the size and mtime match and the scan short-circuits, while a user who
// shrinks the file changes its size and has it picked up normally. It mirrors the
// stat-only fallback the directory cover uses for an image it cannot decode, for the
// same reason.
func statOnlyObs(kind, path string, info os.FileInfo) model.AuxObservation {
	return model.AuxObservation{
		Kind: kind, Path: []byte(path),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
	}
}

// sidecarSkippedDetail explains a skipped sidecar in terms the user can act on: its
// size against the bound that rejected it.
func sidecarSkippedDetail(kind string, size int64) string {
	return fmt.Sprintf("%s sidecar is %d bytes, past the %d-byte read limit; not applied",
		kind, size, int64(maxSidecarBytes))
}

// lrcPartialDetail summarizes a partly-timed .lrc as a count plus the first offending
// line. It leaves the dropped list unserialized: the message needs to stay bounded
// and actionable, and a mostly-untimed file would otherwise carry thousands of
// numbers.
//
// It reports the dropped count and the timed count separately rather than as "N of M
// lines", because the two do not share a denominator. Dropped counts input lines,
// while one input line carrying several leading timestamps yields a timed entry per
// tag, so adding them would state a line total the file does not have.
func lrcPartialDetail(lines []model.SyncedLine, dropped []int) string {
	first := 0
	if len(dropped) > 0 {
		first = dropped[0]
	}
	return fmt.Sprintf("%d line(s) had no usable timestamp (first at line %d); %d timed lyric(s) parsed",
		len(dropped), first, len(lines))
}

// cueTracksToCarve reduces a .cue sheet to the tracks that can actually become
// virtual tracks, in start order, and describes the ones it dropped.
//
// Two kinds cannot be carved, and neither may be stored. One the sheet gave no usable
// INDEX 01: its start would fall back to 0 and claim the head of the file while
// truncating the track before it. And an interior one the next track starts on top
// of: its window is empty, and an end of 0 is the sentinel for "runs to the end of
// the file", so it would read back as the whole album under that track's name.
// Dropping the empty one costs its neighbours nothing, since it spans no frames.
//
// It runs BEFORE the rip dispatch, because the surviving count is what decides
// whether the file is a rip at all: a sheet naming no usable window is not one.
func cueTracksToCarve(sheet *meta.CueSheet) (tracks []meta.CueTrack, dropped []string) {
	for _, ct := range sheet.Tracks {
		if !ct.StartValid {
			dropped = append(dropped, cueTrackDesc(ct, "has no usable INDEX 01"))
		}
	}
	// Sort by start so each track's end can be read off the next track's start; a
	// well-formed cue is already ordered, but a malformed one must not yield a negative
	// window.
	cts := sheet.UsableTracks()
	sort.SliceStable(cts, func(i, j int) bool { return cts[i].StartFrames < cts[j].StartFrames })
	tracks = make([]meta.CueTrack, 0, len(cts))
	for i, ct := range cts {
		if i+1 < len(cts) && cts[i+1].StartFrames <= ct.StartFrames {
			dropped = append(dropped, cueTrackDesc(ct, "is empty (the next TRACK's INDEX 01 names the same frame)"))
			continue
		}
		tracks = append(tracks, ct)
	}
	return tracks, dropped
}

// cueTrackDesc names one dropped TRACK by the sheet's own track number and title,
// which is what the user has to go look at in the .cue. The parser does not keep the
// offending INDEX text, and the number locates the track without it.
func cueTrackDesc(ct meta.CueTrack, reason string) string {
	if ct.Title != "" {
		return fmt.Sprintf("TRACK %02d (%q) %s", ct.Number, ct.Title, reason)
	}
	return fmt.Sprintf("TRACK %02d %s", ct.Number, reason)
}

// maxCueDropsShown bounds the dropped-track list in a diagnostic, so a wholly
// malformed sheet yields a readable line rather than a hundred clauses.
const maxCueDropsShown = 3

// cueTracksDroppedDiag summarizes a sheet's unusable tracks as ONE diagnostic.
//
// One row per dropped track would be wrong twice over: file_diagnostic is keyed by
// (file_id, origin, code, tag_key), so rows sharing this code collide and all but one
// silently vanish; and an unbounded list is what lrcPartialDetail already declines to
// build for the same reason. It reports the count plus the first few, like that one.
func cueTracksDroppedDiag(dropped []string) []model.FileDiagnostic {
	if len(dropped) == 0 {
		return nil
	}
	shown := dropped
	suffix := ""
	if len(shown) > maxCueDropsShown {
		shown = shown[:maxCueDropsShown]
		suffix = fmt.Sprintf(" (and %d more)", len(dropped)-maxCueDropsShown)
	}
	return []model.FileDiagnostic{{
		Code: model.DiagCueTrackDropped, Severity: model.SeverityWarn,
		Detail: fmt.Sprintf("%d cue TRACK(s) dropped from the sheet: %s%s",
			len(dropped), strings.Join(shown, "; "), suffix),
	}}
}

// scanCueSidecar reads a sibling .cue for an audio file, parsing it into a cue sheet
// and returning its on-disk observation. The bool reports whether the .cue was
// READABLE, meaning the observation is valid; it does not report whether the .cue
// yielded any tracks. A readable .cue that parses to zero tracks still returns true
// with a nil sheet so the caller can record its observation. Recording it either way
// is what keeps the fast-path, which only stat-compares recorded sidecars, from
// treating a trackless .cue as new on every scan and forcing a full reprocess.
// WaxBin keeps the .cue on disk as an uncatalogued sidecar (it is already in
// organize's set); only the parsed tracks/chapters land in the catalog.
// An oversized .cue yields no sheet but still reports readable, with a stat-only
// observation and a skip diagnostic: the caller records the observation (so the
// fast path stops re-routing here) and applies nothing, while the diagnostic keeps
// the skip from being invisible.
func scanCueSidecar(audioPath string) (*meta.CueSheet, model.AuxObservation, []model.FileDiagnostic, bool) {
	cuePath := strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ".cue"
	// Stat before reading, so the same memory guard the .lrc read and the fast path
	// apply also covers the .cue.
	info, serr := os.Stat(cuePath)
	if serr != nil {
		return nil, model.AuxObservation{}, nil, false
	}
	if info.Size() > maxSidecarBytes {
		diags := []model.FileDiagnostic{{
			Code: model.DiagSidecarSkipped, Severity: model.SeverityWarn,
			Detail: sidecarSkippedDetail(model.AuxCue, info.Size()),
		}}
		return nil, statOnlyObs(model.AuxCue, cuePath, info), diags, true
	}
	data, err := os.ReadFile(cuePath)
	if err != nil {
		return nil, model.AuxObservation{}, nil, false
	}
	obs := model.AuxObservation{
		Kind: model.AuxCue, Path: []byte(cuePath), Hash: art.Hash(data),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
	}
	return meta.ParseCueSheet(string(data)), obs, nil, true
}

// reconcileFastPathSidecars re-checks an unchanged audio file's sidecars in one pass.
// It applies changes that do NOT need the audio (a .lrc yielding synced lyrics, a
// .cue yielding chapters) cheaply through the standalone UpdateItemSidecars seam, and
// returns needsFull=true when a change requires re-reading the audio: a sidecar
// vanished or became unusable (revert lyrics/chapters to embedded), or the directory
// cover changed/appeared/vanished (resolveCover must re-decide embedded-vs-directory
// precedence). Each sidecar is stat'd once; only a changed one is read.
func (s *Scanner) reconcileFastPathSidecars(ctx context.Context, path string, known model.ScopedFile, cache *artCache, res *Result) (needsFull bool) {
	stored := make(map[string]model.AuxObservation, len(known.Aux))
	for _, o := range known.Aux {
		stored[o.Kind] = o
	}

	// The directory cover is stat-gated (no per-scan read+hash of an unchanged cover);
	// any change routes through the full path so resolveCover keeps embedded precedence.
	if coverChangedFast(path, stored, cache) {
		return true
	}

	upd := model.SidecarUpdate{ItemPID: known.ItemPID, FilePID: known.FilePID}
	dirty := false

	// Any .lrc change routes to the full path, whatever the change is.
	//
	// That matters most in the repair direction. Re-deriving the lyrics_partial
	// diagnostic is full-path work, since the store replaces the scan's whole
	// diagnostic set there. Routing only a break would cover half the story: a .lrc
	// edited from partial back to clean would take the fast path, UpdateItemSidecars
	// would run, and the now-false lyrics_partial row would survive indefinitely,
	// which is the staleness the diagnostics design exists to prevent.
	//
	// The cost is bounded. The .lrc just changed, so one re-parse is cheap, and the
	// stat comparison short-circuits every later scan before touching the file again.
	// An oversized .lrc routes here too, once: the full path records its skip and a
	// stat-only observation, and that observation makes the next scan's size and mtime
	// comparison match.
	//
	// The check stats without reading. This branch needs no content, and reading here
	// would mean reading and hashing the file twice, since the full path reads it too.
	lrcPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".lrc"
	switch statSidecar(lrcPath, model.AuxLyrics, stored) {
	case sidecarVanished, sidecarChanged, sidecarOversized:
		return true
	}

	// .cue. A book applies its chapters cheaply in place. A track (or a virtual-track
	// container) routes ANY .cue change to the full path instead: the full-path
	// discriminator owns creating, reconciling, and tearing down the virtual-track set,
	// and the fast path has no seam for it. A book with a changed/vanished cue keeps its
	// cheap chapter update; a non-book only needs to detect the change (statSidecar), not
	// read the file, since it re-reads on the full path anyway.
	cuePath := strings.TrimSuffix(path, filepath.Ext(path)) + ".cue"
	if known.ItemKind == model.KindBook {
		switch state, data, obs := checkSidecarFile(cuePath, model.AuxCue, stored); state {
		case sidecarVanished, sidecarOversized:
			return true
		case sidecarChanged:
			chapters := meta.ParseCue(string(data))
			if len(chapters) == 0 {
				return true
			}
			upd.ReplaceChapters, upd.Chapters, upd.ChapterSource = true, chapters, "cue"
			upd.Observations = append(upd.Observations, obs)
			dirty = true
		}
	} else {
		switch statSidecar(cuePath, model.AuxCue, stored) {
		case sidecarChanged, sidecarVanished, sidecarOversized:
			return true
		}
	}

	if !dirty {
		return false
	}
	changed, err := s.cat.UpdateItemSidecars(ctx, upd)
	if err != nil {
		s.log.Warn("updating sidecars on fast-path", "path", path, "err", err)
	} else if changed {
		res.SidecarsUpdated++
	}
	return false
}

// sidecarState is the result of stat-comparing one sidecar to its last observation.
type sidecarState int

const (
	sidecarUnchanged sidecarState = iota // present and size+mtime match the observation
	sidecarChanged                       // present but changed/new; data + obs are returned
	sidecarVanished                      // was observed before but is now gone from disk
	sidecarAbsent                        // never observed and not present now
	// sidecarOversized: present, new or changed, and past maxSidecarBytes, so it went
	// unread. It is kept apart from sidecarUnchanged so the caller routes it to the
	// full path once, where the skip picks up its stat-only observation and its
	// diagnostic. An already-observed oversized file that has not moved never reaches
	// this state, because the size and mtime match wins first, and that is what keeps
	// the file from routing to the full path on every later scan.
	sidecarOversized
)

// checkSidecarFile stat-compares a sidecar against its stored observation, reading it
// only when it changed. It is the shared core of the per-extension fast-path checks
// (one stat per sidecar; no read when unchanged).
func checkSidecarFile(sidecarPath, kind string, stored map[string]model.AuxObservation) (sidecarState, []byte, model.AuxObservation) {
	state := statSidecar(sidecarPath, kind, stored)
	if state != sidecarChanged {
		// Only sidecarChanged has data to return. The oversized case builds no
		// observation here; the full path it routes to records that one, so building a
		// second for the caller to discard would be waste.
		return state, nil, model.AuxObservation{}
	}
	info, err := os.Stat(sidecarPath)
	if err != nil {
		return sidecarVanished, nil, model.AuxObservation{}
	}
	data, rerr := os.ReadFile(sidecarPath)
	if rerr != nil {
		// Unreadable right now: leave it as-is rather than churn on a transient error.
		return sidecarUnchanged, nil, model.AuxObservation{}
	}
	obs := model.AuxObservation{
		Kind: kind, Path: []byte(sidecarPath),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), Hash: art.Hash(data),
	}
	return sidecarChanged, data, obs
}

// statSidecar stat-compares a sidecar against its stored observation without reading
// it. It is the shared core of the fast-path checks, and the whole answer for a
// caller that needs only to know whether the sidecar changed. Reading and hashing a
// file whose contents the caller discards is wasted work, and the .lrc caller would
// do exactly that: any change routes it to the full path, which reads the file again.
func statSidecar(sidecarPath, kind string, stored map[string]model.AuxObservation) sidecarState {
	prev, had := stored[kind]
	info, err := os.Stat(sidecarPath)
	if err != nil {
		if had {
			return sidecarVanished
		}
		return sidecarAbsent
	}
	if had && prev.Size == info.Size() && prev.MTimeNS == info.ModTime().UnixNano() {
		return sidecarUnchanged
	}
	// The stat is already in hand, so bounding the read is free here. Reported as its
	// own state rather than folded into unchanged: the full path is where the skip is
	// recorded and surfaced, and a silently-ignored sidecar is the failure this whole
	// diagnostic vocabulary exists to prevent.
	if info.Size() > maxSidecarBytes {
		return sidecarOversized
	}
	return sidecarChanged
}

// coverChangedFast reports whether the directory cover changed relative to the stored
// observation, using a cheap stat (no read+hash of an unchanged cover). It stats the
// previously-observed cover file when there was one; otherwise it checks whether a
// cover newly appeared. A newly-added HIGHER-priority candidate beside an existing
// cover is missed until a full rescan (accepted; the periodic full rescan backstops).
func coverChangedFast(path string, stored map[string]model.AuxObservation, cache *artCache) bool {
	prev, had := stored[model.AuxCover]
	if had {
		info, err := os.Stat(string(prev.Path))
		if err != nil {
			return true // the observed cover vanished
		}
		return info.Size() != prev.Size || info.ModTime().UnixNano() != prev.MTimeNS
	}
	// No prior cover: a cover newly appearing is a change. dirCoverStat lists the
	// directory once (cached), without reading/hashing the image.
	return cache.dirCoverStat(filepath.Dir(path)) != nil
}

// artCache memoizes per-directory cover-image lookups for one scan run, so an
// album's directory cover is read and hashed once rather than for every track. It
// caches both the resolved image and the cover file's on-disk observation (path,
// size, mtime, hash) for the sidecar fast-path.
type dirCoverEntry struct {
	img *model.ArtImage       // resolved cover (nil = none)
	obs *model.AuxObservation // the cover FILE observation (nil = none)
}

type artCache struct {
	dirs     map[string]dirCoverEntry         // full resolve (image + hashed obs)
	statObs  map[string]*model.AuxObservation // stat-only cover obs (no read/hash)
	statDone map[string]bool                  // whether statObs[dir] has been computed
}

func newArtCache() *artCache {
	return &artCache{
		dirs:     map[string]dirCoverEntry{},
		statObs:  map[string]*model.AuxObservation{},
		statDone: map[string]bool{},
	}
}

// dirCoverStat returns the directory cover file's observation from a cheap stat (no
// read or hash), for the fast-path change check. It lists the directory once (cached)
// and stats the highest-priority existing candidate. nil means no cover file exists.
func (c *artCache) dirCoverStat(dir string) *model.AuxObservation {
	if c.statDone[dir] {
		return c.statObs[dir]
	}
	obs := coverStat(dir)
	c.statObs[dir] = obs
	c.statDone[dir] = true
	return obs
}

// resolve probes a directory's cover once, caching the image and its observation.
func (c *artCache) resolve(dir string) dirCoverEntry {
	if e, ok := c.dirs[dir]; ok {
		return e
	}
	img, coverPath := findDirCover(dir)
	var obs *model.AuxObservation
	if img != nil && coverPath != "" {
		if info, err := os.Stat(coverPath); err == nil {
			obs = &model.AuxObservation{
				Kind: model.AuxCover, Path: []byte(coverPath),
				Size: info.Size(), MTimeNS: info.ModTime().UnixNano(), Hash: img.Hash,
			}
		}
	}
	e := dirCoverEntry{img: img, obs: obs}
	c.dirs[dir] = e
	return e
}

// dirCover returns the directory's cover image (nil when there is none).
func (c *artCache) dirCover(dir string) *model.ArtImage { return c.resolve(dir).img }

// dirCoverObs returns the directory cover file's on-disk observation (nil none).
func (c *artCache) dirCoverObs(dir string) *model.AuxObservation { return c.resolve(dir).obs }

// resolveCover chooses a track's cover, preferring a decodable embedded image,
// then the directory cover image, and only then a non-decodable embedded image as
// a last resort. That keeps an embedded format without a registered decoder
// available to serve, while a corrupt or placeholder embedded picture does not
// shadow a valid cover.jpg next to the file.
func resolveCover(path string, embedded *model.ArtImage, cache *artCache) *model.ArtImage {
	// finalizeArt also returns true for an exotic (AVIF/HEIC) embedded image that was
	// only recognized by its magic bytes, not decoded, so its dimensions stay 0 until an
	// external decoder runs. Require real decoded dimensions (Width > 0) here so such an
	// image does not shadow a decodable cover.jpg beside the file. It is still kept by
	// the last-resort branch below.
	if embedded != nil && finalizeArt(embedded) && embedded.Width > 0 {
		return embedded // decodable embedded cover with known dimensions
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

// coverFilesByLower lists dir once and returns a lowercase-name -> actual-name map of
// its regular files, so cover-candidate matching is case-insensitive (a "Cover.JPG"
// matches "cover.jpg"). It returns nil when the directory cannot be read.
func coverFilesByLower(dir string) map[string]string {
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
	return byLower
}

// findDirCover returns the first recognized cover image in dir (case-insensitive,
// in CoverArtNames priority order), finalized, plus its full path, or (nil, "")
// when there is none.
func findDirCover(dir string) (*model.ArtImage, string) {
	byLower := coverFilesByLower(dir)
	for _, cand := range model.CoverArtNames {
		name, ok := byLower[cand]
		if !ok {
			continue
		}
		full := filepath.Join(dir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		img := &model.ArtImage{Data: data}
		if finalizeArt(img) {
			return img, full
		}
	}
	return nil, ""
}

// coverStat finds the highest-priority existing cover-candidate file in dir and
// stats it (no read/hash), returning its observation, or nil when none exists. It is
// the cheap fast-path check for a newly-appeared cover.
func coverStat(dir string) *model.AuxObservation {
	byLower := coverFilesByLower(dir)
	for _, cand := range model.CoverArtNames {
		name, ok := byLower[cand]
		if !ok {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		return &model.AuxObservation{
			Kind: model.AuxCover, Path: []byte(full),
			Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
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
		// An AVIF/HEIC cover has no pure-Go decoder: recognize it by magic so it is
		// still found and stored (dimensions unknown until an external helper decodes
		// it). This keeps a cover.avif from being skipped as unreadable.
		if f, ok := art.SniffExotic(img.Data); ok {
			img.Format = f
			return true
		}
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
