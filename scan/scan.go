// Package scan walks library roots and persists catalog rows. It is I/O-bound
// and never decodes PCM: per file it stats, hashes content and audio essence,
// reads tags, and writes through model.Catalog. PCM decoding belongs to the
// separate analysis pass.
//
// The one call into decode is decode.Probe, for a file whose container no tag
// parser covers. It reads that container's header so the row carries a real
// duration and sample rate instead of zeroes, and it decodes nothing.
package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/decode"
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
	now    func() time.Time // the heartbeat floor's clock; tests swap it
	// The walk and the per-file stat, which tests swap to make a folder or a file
	// unreadable without changing permissions.
	walk func(root string, fn fs.WalkDirFunc) error
	stat func(path string) (fs.FileInfo, error)
}

// New builds a scanner over a catalog and metadata reader.
func New(cat model.Catalog, reader meta.Reader, log *slog.Logger) *Scanner {
	if reader == nil {
		reader = meta.NewReader()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Scanner{cat: cat, reader: reader, log: log, now: time.Now, walk: filepath.WalkDir, stat: os.Stat}
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
	// IgnoreLocks re-derives every field from the file's tags even when the user has
	// locked it, so a `scan --force --ignore-locks` deliberately discards curated
	// edits. Off by default: a normal or forced scan preserves locked fields.
	IgnoreLocks bool
}

// Result tallies what a scan did. Every field but Missing counts files, not items: the
// Items* names say what cataloging a file did, not how many items came out of it.
//
// The two diverge for a single-file album rip, where one .cue-carved file becomes N
// virtual-track items and still reports ItemsCreated 1. Nothing here is an item
// count; query the catalog for that.
//
// The outcomes partition the files: each audio file takes exactly one of ItemsCreated,
// ItemsUpdated, SidecarsUpdated, Copies, Unchanged and Errored, so
//
//	AudioFiles == ItemsCreated + ItemsUpdated + SidecarsUpdated + Copies + Unchanged + Errored
//	FilesSeen  == AudioFiles + Skipped
//
// Relinked and Reread ride alongside whichever outcome a file took. Missing counts
// items, Promoted and Dropped count files reconciliation and the scan's writes settled
// outside the walk, and WalkErrors counts entries the walk could not read at all, which
// are in none of the others.
type Result struct {
	FilesSeen int
	// AudioFiles is every audio file the scan visited, an errored one included.
	AudioFiles int
	// ItemsCreated is audio files whose scan created at least one item (one rip that
	// created twelve virtual tracks counts once).
	ItemsCreated int
	// ItemsUpdated is audio files whose content changed, whose rescan re-derived a
	// stored field over unchanged bytes (a forced rescan of a catalog-only edit), or
	// that joined an existing item as a new file of it (a book's next part, a better
	// encoding taking the item over).
	ItemsUpdated int
	// SidecarsUpdated is audio files whose .lrc/.cue or cover change was applied
	// without the audio changing.
	SidecarsUpdated int
	// Copies is audio files that joined an existing item as alternates: the same audio
	// as its primary, or another encoding of its recording, newly found or given up by
	// another item or a part's place. A copy read again is Unchanged, or ItemsUpdated when
	// its bytes changed, and one moved with its folder is Unchanged and Relinked.
	Copies int
	// Unchanged is audio files the scan left as they were: fast-pathed on a size and
	// mtime match, or read in full and found the same.
	Unchanged int
	Errored   int
	// Relinked is audio files matched to an existing file row by essence hash (a move
	// or rename).
	Relinked int
	// Reread is cataloged audio files the scan read in full rather than fast-pathing:
	// every one under Force, else those whose size or mtime moved or whose sidecar
	// changed.
	Reread  int
	Missing int // items reconciled to 'missing' (backing files gone from disk)
	// Promoted is alternate files given the place of a primary or part their item lost
	// (a vanished file, or a primary re-keyed to another item), each re-read as the scan
	// ends.
	Promoted int
	// Dropped is the rows of gone files reconciliation removed: a gone alternate, or a
	// row no item held.
	Dropped    int
	Skipped    int // non-audio files
	WalkErrors int // entries the walk could not read
	// SubPathGone says the sub-path asked for was not there, so the scan walked nothing
	// and reconciled what the catalog held under it.
	SubPathGone bool

	// LibraryPID and LibraryName name the library a scan walked, its pid and display
	// root; a total over several libraries leaves them empty.
	LibraryPID  model.PID
	LibraryName string
}

// Heartbeat is the progress callback invoked periodically during a scan. progress is the
// fraction of the audio files to visit that the scan has visited, below 1 until the
// closing call.
type Heartbeat func(progress float64, msg string) error

// A heartbeat is due every heartbeatEvery files seen, and sent only when heartbeatFloor
// has passed since the last, since a server writes the job row on each one.
const (
	heartbeatEvery = 50
	heartbeatFloor = 250 * time.Millisecond
)

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
		// Re-anchor on the stored root's spelling. UnderRoot folds case, so on Windows
		// a sub-path may pass the guard while carrying a different casing of the root
		// than the catalog holds. Left alone, scopePrefix would miss the path byte-range
		// in LoadScopedFileIndex and every walked path would miss fileByPathTx, so the
		// whole subtree would re-insert as new file rows.
		//
		// This fixes the root prefix only. Rel returns the remainder as the caller typed
		// it, so a re-cased component below the root still walks and stores that casing;
		// see the note beside fileByPathDB for why that is left alone.
		if rel, err := filepath.Rel(root, walkRoot); err == nil {
			walkRoot = filepath.Join(root, rel)
			// A walk starting below the trash folder would never meet the skip below. The
			// sub-path is judged as typed and as the disk spells it, since Windows reaches
			// the folder through a short or trailing-dot spelling of its name too.
			if model.InTrash(rel) || model.InTrash(spelledRel(root, walkRoot)) {
				return nil, waxerr.New(waxerr.CodeInvalid, op, "sub-path is inside the library trash")
			}
		}
	}

	res := &Result{LibraryPID: req.Library.PID, LibraryName: req.Library.DisplayRoot}
	sc := &scanCtx{cache: artCacheAt(root), force: req.Force, adopt: req.AdoptStampedPIDs, preserveLocks: !req.IgnoreLocks,
		folders: map[string]*folderState{}}

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

	// The progress denominator: the files the index expects, or on a first scan a count
	// taken before cataloging starts.
	expected := knownCount
	if expected == 0 && hb != nil {
		expected = countAudio(ctx, walkRoot)
	}
	lastBeat := s.now()
	beat := func() error {
		if t := s.now(); t.Sub(lastBeat) >= heartbeatFloor {
			lastBeat = t
			done := res.AudioFiles
			return hb(float64(done)/float64(max(expected, done+1)), "scanned "+strconv.Itoa(res.FilesSeen)+" files")
		}
		return nil
	}

	// A sub-path removed since it was named (a folder a delete or a move emptied, or one
	// removed by hand) is nothing to walk and no walk error: the reconcile below marks what
	// the catalog held under it missing.
	walk := s.walk
	if walkRoot != root && subPathGone(root, walkRoot) {
		s.log.Debug("scan sub-path is gone", "path", walkRoot)
		res.SubPathGone = true
		walk = func(string, fs.WalkDirFunc) error { return nil }
	}
	walkErr := walk(walkRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			s.log.Warn("walk entry", "path", path, "err", err)
			res.WalkErrors++
			sc.unreadable = append(sc.unreadable, path)
			return nil // keep going past unreadable entries
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			// Never descend into the library's trash: those files were deleted, and
			// re-cataloging them would resurrect the items they backed.
			if model.IsTrashName(d.Name()) {
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
		s.leaveFolders(ctx, req.Library, root, walkRoot, path, sc, res)
		if isAudio(path) {
			res.AudioFiles++
			if err := s.scanAudioFile(ctx, req.Library, root, path, res, sc, ""); err != nil {
				s.log.Warn("scanning file", "path", path, "err", err)
				res.Errored++
			}
		} else {
			res.Skipped++
		}
		if hb != nil && res.FilesSeen%heartbeatEvery == 0 {
			return beat()
		}
		return nil
	})
	if walkErr != nil {
		return res, waxerr.FromContext(op, walkErr, waxerr.CodeIO)
	}
	s.leaveFolders(ctx, req.Library, root, walkRoot, "", sc, res)

	// Reconcile deletions: entries still in the index were never walked, so their
	// files are gone from disk. The survival gate refuses to act on a transiently
	// unavailable root, so a momentary mount loss cannot mark the whole library
	// missing.
	s.reconcileMissing(ctx, walkRoot, sc, knownCount, req.ForceReconcile, res)
	s.rereadPromoted(ctx, sc, res)

	if hb != nil {
		_ = hb(1, "scanned "+strconv.Itoa(res.FilesSeen)+" files")
	}
	return res, nil
}

// subPathGone reports whether sub is absent while the library root holding it is there.
func subPathGone(root, sub string) bool {
	if _, err := os.Lstat(sub); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	info, err := os.Stat(root)
	return err == nil && info.IsDir()
}

// scanCtx carries the per-scan fast-path state through the walk. The walk is
// single-goroutine (WalkDir invokes its callback sequentially), so the index map
// needs no locking.
type scanCtx struct {
	index         map[string]model.ScopedFile // path -> known file; entries deleted as visited
	force         bool                        // bypass the fast-path (re-hash everything)
	adopt         bool                        // pass WAXBIN_ITEM_PID hints to the store (rebuild)
	preserveLocks bool                        // keep user-locked fields from being re-derived from tags
	cache         *artCache
	promoted      []model.PromotedFile  // alternates a write promoted, re-read as the scan ends
	last          *model.ScanItemResult // the store's outcome for the file put last
	unreadable    []string              // entries the walk could not read, whose files are not reconciled
	// The folder rule's state (folder.go): a walk's album folders, the ones it is inside
	// (innermost last), and whether a read is the rule's second look at a file, written
	// only when the file joins a book (adoptOnly) or read with the rule left out
	// (unadopt). folders is nil outside a walk. albums caches the ALBUM of a book's
	// primary file the rule read.
	folders            map[string]*folderState
	open               []string
	adoptOnly, unadopt bool
	albums             map[string]string
	// album and position are FileOptions', for a single-file scan.
	album    string
	position *int
}

// reconcileMissing marks the items behind the index's residual (unwalked) files as
// missing, behind a survival gate. The gate distinguishes a genuine removal (root
// absent, or a healthy scan that simply saw fewer files) from a transient failure
// (root exists but is empty/unreadable, or a partial walk saw far fewer files than
// known), and skips reconciliation entirely on the transient cases, logging a
// degraded warning and keeping every row, so a momentary mount loss cannot wipe the
// catalog.
func (s *Scanner) reconcileMissing(ctx context.Context, walkRoot string, sc *scanCtx, knownCount int, forceReconcile bool, res *Result) {
	index := sc.index
	if len(index) == 0 {
		return // every known file was seen; nothing vanished
	}
	if ctx.Err() != nil {
		return // a canceled scan is incomplete; do not treat unwalked files as missing
	}

	info, statErr := os.Stat(walkRoot)
	unread := sc.unreadable
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		// The root is genuinely gone: a real full removal, reconcile everything. The walk
		// reported the root itself unreadable, which here is the removal.
		unread = nil
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
		// Root exists: require a floor. Reading no file, or fewer than half of what we
		// previously knew, reads as a transient empty/unreadable mount rather than a real
		// mass deletion, so keep the rows. A file the scan visited but could not read
		// counts as unread. A genuine large deletion is not reconciled here (the survival
		// gate protects against a mount blip); the operator runs `scan
		// --reconcile-deletions` to force it.
		if read := res.AudioFiles - res.Errored; read == 0 || read*2 < knownCount {
			s.log.Warn("scan: skipping deletion reconciliation (survival gate): fewer than half of known files were read; "+
				"rows kept in case the root is only transiently unavailable; run `scan --reconcile-deletions` to force",
				"root", walkRoot, "read", read, "known", knownCount, "would_mark_missing", len(index))
			return
		}
	}

	pids := make([]model.PID, 0, len(index))
	for path, e := range index {
		// A file under a folder the walk could not read was never looked for.
		if slices.ContainsFunc(unread, func(dir string) bool { return pathx.UnderRoot(dir, path) }) {
			continue
		}
		pids = append(pids, e.FilePID)
	}
	if len(pids) == 0 {
		return
	}
	mr, err := s.cat.MarkFilesMissing(ctx, pids)
	if err != nil {
		s.log.Warn("reconciling missing files", "root", walkRoot, "err", err)
		return
	}
	res.Missing, res.Dropped = mr.Marked, mr.Dropped
	sc.promoted = append(sc.promoted, mr.Promoted...)
}

// rereadPromoted re-reads the files the scan's writes promoted, counting them in the
// result: each was counted on its own visit or belongs to another library, so they take
// no outcome of their own.
func (s *Scanner) rereadPromoted(ctx context.Context, sc *scanCtx, res *Result) {
	res.Promoted += s.reread(ctx, sc.promoted, sc.preserveLocks)
	sc.promoted = nil
}

// RereadPromoted re-reads files a write promoted in place of one their item lost, so each
// item follows the tags of the file that now owns it, and returns how many distinct files
// it was given. Each is read under its own library's root. A file no longer on disk is
// skipped (a later action took it too), and a failure is logged; the promotion cleared
// each file's stamp, so the next scan reads it in full either way.
func (s *Scanner) RereadPromoted(ctx context.Context, promoted []model.PromotedFile) int {
	return s.reread(ctx, promoted, true)
}

func (s *Scanner) reread(ctx context.Context, promoted []model.PromotedFile, preserveLocks bool) int {
	seen := map[string]bool{}
	var files []model.PromotedFile
	for _, p := range promoted {
		if !seen[string(p.Path)] {
			seen[string(p.Path)] = true
			files = append(files, p)
		}
	}
	if len(files) == 0 || ctx.Err() != nil {
		return len(files)
	}
	libs, err := s.cat.Libraries(ctx)
	if err != nil {
		s.log.Warn("re-reading promoted files", "err", err)
		return len(files)
	}
	for _, p := range files {
		path := string(p.Path)
		i := slices.IndexFunc(libs, func(l *model.Library) bool { return l.ID == p.LibraryID })
		if i < 0 {
			continue
		}
		if _, err := s.stat(path); err != nil {
			continue
		}
		lib := libs[i]
		reread := &scanCtx{cache: artCacheAt(string(lib.Root)), preserveLocks: preserveLocks}
		if err := s.scanAudioFile(ctx, lib, string(lib.Root), path, &Result{}, reread, ""); err != nil {
			s.log.Warn("re-reading a promoted file", "path", path, "err", err)
		}
	}
	return len(files)
}

// ScanFile catalogs a single audio file under its library, classifying its kind by
// EffectiveKind. It is the entry point for re-cataloging one restored or freshly-imported file
// without walking the whole root; it shares the per-file path with the full scan, so
// identity, essence-relink, and change detection behave identically. A non-audio path
// is a no-op.
func (s *Scanner) ScanFile(ctx context.Context, lib *model.Library, path string) (*Result, error) {
	res, _, err := s.ScanFileWith(ctx, lib, path, FileOptions{})
	return res, err
}

// ScanFileAs catalogs a single audio file, forcing its media kind rather than classifying
// it. Use it when the caller already knows the kind, such as an audiobook whose tags do not
// identify it as one; a forced kind the rule would not give the file (its library, its
// tags or its folder) is pinned with a kind lock, so later scans keep it. An empty kind
// classifies by the rule.
// It also returns the store's outcome for the file, nil for a path that is not audio.
func (s *Scanner) ScanFileAs(ctx context.Context, lib *model.Library, path string, kind model.Kind) (*Result, *model.ScanItemResult, error) {
	return s.ScanFileWith(ctx, lib, path, FileOptions{Kind: kind})
}

// FileOptions steers a single-file scan. Kind is ScanFileAs's. Album names the book a
// file whose tags name none belongs to, as an import's folder rule found it in the
// folder the file was staged in: the file joins a book of that title in its folder the
// way a file whose ALBUM names it does, and a book file takes it as its title. Position
// is a book part's position as the staged file gave it (PartPosition), for the disc or
// place the name it was placed under no longer states.
type FileOptions struct {
	Kind     model.Kind
	Album    string
	Position *int
}

// ScanFileWith is ScanFileAs with the options an import passes.
func (s *Scanner) ScanFileWith(ctx context.Context, lib *model.Library, path string, opts FileOptions) (*Result, *model.ScanItemResult, error) {
	if lib == nil {
		return nil, nil, waxerr.New(waxerr.CodeInvalid, "scan.ScanFile", "scan request has no library")
	}
	res := &Result{}
	if !isAudio(path) {
		return res, nil, nil
	}
	res.FilesSeen++
	res.AudioFiles++
	// A single-file scan has no preloaded index, so it always takes the full path.
	// Preserve user-locked fields by default, like a full scan.
	sc := &scanCtx{cache: artCacheAt(string(lib.Root)), preserveLocks: true, album: strings.TrimSpace(opts.Album), position: opts.Position}
	if err := s.scanAudioFile(ctx, lib, string(lib.Root), path, res, sc, opts.Kind); err != nil {
		res.Errored++
		return res, nil, err
	}
	s.rereadPromoted(ctx, sc, res)
	return res, sc.last, nil
}

// scanAudioFile hashes, reads tags, and persists one audio file. forceKind overrides
// the tag-based track/book classification when non-empty. When the scan context
// carries a preloaded index and the file's size and mtime match its known entry, it
// takes the fast-path: no content/essence hashing, no tag parse, no upsert, just a
// cheap sidecar re-check, and the file is dropped from the index (marking it seen).
func (s *Scanner) scanAudioFile(ctx context.Context, lib *model.Library, root, path string, res *Result, sc *scanCtx, forceKind model.Kind) error {
	info, err := s.stat(path)
	if err != nil {
		// A file the walk listed but cannot stat for any reason but its absence is still
		// there, so it is not reconciled as gone.
		if sc.index != nil && !errors.Is(err, fs.ErrNotExist) {
			delete(sc.index, path)
		}
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
			if !sc.force && known.Size == info.Size() && known.MTimeNS == info.ModTime().UnixNano() && !kindOwed(lib, known) {
				// Size+mtime match. A sidecar change (a .lrc or .cue edited or gone, a
				// directory cover changed) still needs the full path, which
				// reconcileFastPathSidecars reports.
				if !s.reconcileFastPathSidecars(path, known, sc.cache) {
					res.Unchanged++
					if f := sc.folderState(bookFolder(root, path), true); f != nil {
						f.unread = append(f.unread, unreadFile{path: path, known: known})
					}
					return nil
				}
			}
			res.Reread++
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
	if unsupportedFormat(fm.Diagnostics) {
		if err := s.probeProperties(ctx, path, &tags); err != nil {
			return err
		}
	}
	// essence_hash anchors file identity independently of tags. Files with no
	// hashable essence fall back to the content hash, so they are still cataloged
	// but re-key on any byte change.
	essenceHash := fm.EssenceHash
	if essenceHash == "" {
		essenceHash = contentHash
	}

	// An audiobook takes the book path: it groups by book identity (so a multi-file book
	// collapses its parts into one item) and carries contributors and chapters. Everything
	// else is a music track. The kind is a forced one, a lock's, the library's or the tags'
	// (EffectiveKind), and the folder rule (folder.go) then gives a file whose tags name no
	// book the one it belongs to. A file a folder settle reads again is written only when it
	// joins a book.
	standing, err := s.cat.FileStanding(ctx, lib.ID, []byte(path), essenceHash)
	if err != nil {
		return err
	}
	var lock model.Kind
	if forceKind == "" && standing != nil && standing.KindLocked && sc.preserveLocks {
		lock = standing.Kind
	}
	kind := EffectiveKind(&tags, lib, forceKind, lock)
	// A sibling .cue is read once, here: a file with no embedded chapters and a sheet
	// beside it is a rip the folder rule leaves alone, and the sheet is applied below.
	var cueRead struct {
		sheet      *meta.CueSheet
		obs        model.AuxObservation
		diags      []model.FileDiagnostic
		refusal    string
		unread, ok bool
	}
	if len(tags.Chapters) == 0 {
		c := &cueRead
		c.sheet, c.obs, c.diags, c.refusal, c.unread, c.ok = scanCueSidecar(path)
	}
	album := strings.TrimSpace(tags.Album)
	folder := bookFolder(root, path)
	loose, looseTrack := joins(kind, forceKind == "" && lock == "", album, lib, cueRead.ok)
	part := album == "" && PartShaped(&tags, path)
	// An import names the book a file it staged with no ALBUM belongs to, which the file's
	// new name no longer shows; the rule then reads it as the file's album.
	albumName, hinted := album, album == "" && sc.album != ""
	if hinted {
		albumName = sc.album
	}
	var adopt *model.FolderBook
	walked, by := false, joinedBy(0)
	if loose && !sc.unadopt {
		if adopt, walked, by, err = s.adoption(ctx, lib, root, folder, sc, looseTrack, part, albumName, standing); err != nil {
			return err
		}
	}
	if sc.adoptOnly && adopt == nil {
		return nil
	}
	// A kind forced against the rule, the folder rule included, is pinned, so a later scan
	// keeps it.
	lockKind := false
	if forceKind != "" {
		rule := EffectiveKind(&tags, lib, "", "")
		if ok, track := joins(rule, true, album, lib, cueRead.ok); ok && rule == model.KindTrack {
			b, _, _, err := s.adoption(ctx, lib, root, folder, sc, track, part, albumName, standing)
			if err != nil {
				return err
			}
			if b != nil {
				rule = model.KindBook
			}
		}
		lockKind = forceKind != rule
	}
	if adopt != nil {
		kind = model.KindBook
	}
	isBook := kind == model.KindBook
	folderTitled := false
	if isBook {
		// A numbered part with no album is a part of the book its folder names, so the
		// parts of an untagged book key one book.
		// WaxBin gave a managed file its name and folder, so where they would give a book's
		// primary file its title they state none: the book keeps the one the catalog holds.
		kept := lib.Mode == model.ModeManaged && album == "" && (part && folder != "" || fm.TitleFromName) &&
			standing != nil && standing.Book != nil && standing.Book.Title != "" && bytes.Equal(standing.Book.Primary, []byte(path))
		switch {
		case hinted:
			tags.Album, folderTitled = sc.album, true
		case kept:
			tags.Album, folderTitled = standing.Book.Title, true
		case part && folder != "":
			tags.Album, folderTitled = filepath.Base(folder), true
		}
		meta.PromoteBookFields(&tags)
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

	// A track's empty display fields fall back to its sort tags, file name and (when the
	// library opts in) folders. derived names what the file's tags do not state, which for
	// a book is its title when no album tag names it.
	var derived []string
	switch {
	case !isBook:
		var d *model.FileDiagnostic
		derived, d = meta.DisplayFallbacks(&tags, root, path, fm.TitleFromName)
		if d != nil {
			diags = append(diags, *d)
		}
		if lib.FolderFallback && lib.Mode == model.ModeInPlace {
			derived = append(derived, folderNames(&tags, rel)...)
		}
	case album == "" && (fm.TitleFromName || adopt != nil || folderTitled):
		derived = []string{"title"}
	}
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
	// carve is the sheet's windows, one per track that can actually become a virtual
	// track. Its length decides below whether this file is a rip, since a virtual track
	// is nothing but its window. Counting the tracks that named no usable window instead
	// would carve a rip out of a sheet that has none to carve, and a sheet whose every
	// track was unusable would commit the file with no items at all.
	var carve []meta.CueWindow
	var cueDropped []string
	cueUnread, cueRefusal := false, ""
	if len(tags.Chapters) == 0 {
		if cueRead.ok {
			aux = append(aux, cueRead.obs)
			diags = append(diags, cueRead.diags...)
			cueSheet, cueUnread, cueRefusal = cueRead.sheet, cueRead.unread, cueRead.refusal
		}
		switch {
		case cueSheet == nil:
		case isBook:
			cueDropped = cueSheet.ChapterDrops()
		default:
			// Only a lossless file's length is declared rather than estimated, and one
			// that runs long would end the last track past its audio.
			var fileMS int64
			if model.LosslessCodec(tags.Codec) {
				fileMS = tags.DurationMS
			}
			var err error
			if carve, cueDropped, err = cueSheet.Carve(fileMS); err != nil {
				cueRefusal = errMsg(err)
			}
		}
	}

	// A rip is carved only from a sheet that reads clean: the parser skips a line it
	// cannot read, and a skipped TRACK line merges two songs under one title. Such a
	// sheet, like an unread one, says nothing reliable about the tracks, so a rip keeps
	// the windows it was last carved into; a whole-file fallback would delete every
	// virtual track with its plays and stars. A book takes the lines that did read.
	var cueWarnings []meta.CueWarning
	if cueSheet != nil {
		cueWarnings = cueSheet.Warnings
	}
	ripUnread := cueUnread || cueRefusal != "" || len(cueWarnings) > 0
	var cueChapters []model.Chapter
	// rip is the virtual tracks a rip is written as. Fewer than two carvable tracks is
	// not a rip: one window over the whole file is just the file, and it would be
	// strictly worse as a virtual track (its tags become unwritable and it exports no
	// fingerprint), so the file takes the plain whole-file path below.
	var rip []model.VirtualTrack
	disposition := ""
	switch {
	case isBook:
		if cueSheet != nil {
			cueChapters = cueSheet.Chapters()
		}
		switch {
		case len(cueWarnings) > 0 && len(cueChapters) > 0:
			disposition = "the readable lines were applied"
		case len(cueWarnings) > 0 || cueRefusal != "":
			disposition = "the sheet was not applied"
		}
	case ripUnread:
		cueDropped = nil
		kept, err := s.keptRip(ctx, path, tags)
		if err != nil {
			return err
		}
		disposition = "the sheet was not applied"
		if len(kept) >= 2 {
			rip, disposition = kept, "the existing tracks were kept"
		}
	case len(carve) >= 2:
		rip = carvedTracks(essenceHash, tags, cueSheet, carve)
	}
	// Reported here rather than on the rip path, so it stays visible whichever path
	// the file then takes: a sheet left with fewer than two carvable tracks never
	// reaches virtualTracksInput at all.
	diags = append(diags, cueSheetDiag(cueSheet, cueDropped, cueRefusal, disposition)...)
	var out *model.ScanItemResult
	var bookKey string
	switch {
	case len(rip) > 0:
		// A single file with a multi-track .cue is a single-file album rip: each cue
		// TRACK is its own virtual track with an offset window, rather than the whole
		// file cataloged as one track.
		out, err = s.cat.PutScannedVirtualTracks(ctx, model.PutScannedVirtualTracksInput{
			LibraryID: lib.ID, File: file, Tracks: rip, CoverArt: cover, AuxObservations: aux,
			Acquisition: tags.Acquisition, Diagnostics: diags, PreserveLocks: sc.preserveLocks,
		})
	case isBook:
		bin := bookInput(lib.ID, file, tags, essenceHash, cover, partPlace(&tags, root, path, lib.Mode == model.ModeManaged, sc.position))
		if adopt != nil {
			bin.OwnKey = bin.Item.IdentityKey
			bin.Item.IdentityKey, bin.Adopted = adopt.Key, true
		}
		bookKey = bin.Item.IdentityKey
		// With no embedded chapters, a sibling .cue fills them (marked source='cue' so
		// embedded chapters still win). Its observation was recorded above whether or not
		// it yielded any; apply the chapters only when it did.
		if len(cueChapters) > 0 {
			bin.Chapters, bin.ChapterSource = cueChapters, "cue"
		}
		bin.AuxObservations = aux
		bin.PreferredItemPID = adoptedPID(sc, fm)
		bin.Diagnostics = diags
		bin.PreserveLocks = sc.preserveLocks
		bin.Derived = derived
		bin.LockKind, bin.KindForced = lockKind, forceKind != ""
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
			CustomTags:       tags.Custom,
			AuxObservations:  aux,
			PreferredItemPID: adoptedPID(sc, fm),
			Acquisition:      tags.Acquisition,
			Diagnostics:      diags,
			PreserveLocks:    sc.preserveLocks,
			Derived:          derived,
			LockKind:         lockKind,
			KindForced:       forceKind != "",
		})
	}
	if err != nil {
		return err
	}
	sc.promoted = append(sc.promoted, out.Promoted...)
	sc.last = out
	counted := countOutcome(res, out)

	// What the folder rule needs of the folder later: a book a stated album named, and a
	// file whose tags named no book of its own, unless a book read here took it in by name.
	// A part the folder's only book took as the walk passed is checked again at the settle.
	only := walked && by == byOnly
	if f := sc.folderState(folder, (!walked || only) && (loose || (isBook && album != ""))); f != nil {
		switch {
		case isBook && album != "" && !walked:
			f.addBook(folderBook{
				FolderBook: model.FolderBook{ItemPID: out.ItemPID, Key: bookKey, Title: cleanBookTitle(album)},
				strong:     lib.MediaType() == model.MediaAudiobook || tags.BookSignal == model.BookTagSignal,
			})
		case loose && (!walked || only):
			f.loose = append(f.loose, looseFile{path: path, album: album, track: looseTrack, part: part, only: only,
				item: out.ItemPID, counted: counted})
		}
	}
	if out.Relinked {
		res.Relinked++
		// The walk only ever looked up sc.index by the path it is currently visiting,
		// which is the file's new path, so the file's old-path entry is still sitting
		// in the index. Left there, end-of-walk reconciliation would read it as a
		// vanished file and mark the very item this scan just relinked as missing. The
		// store reports the path it moved from, which is that entry's key.
		if out.RelinkedFrom != "" {
			delete(sc.index, out.RelinkedFrom)
		}
	}
	return nil
}

// countOutcome adds a put's outcome to the result and returns the counter it took. The
// outcomes partition the files (see Result).
func countOutcome(res *Result, out *model.ScanItemResult) *int {
	switch {
	case out.ItemCreated:
		res.ItemsCreated++
		return &res.ItemsCreated
	case out.AttachedAsCopy && out.Joined:
		res.Copies++
		return &res.Copies
	case out.AttachedAsCopy && out.ContentChanged:
		res.ItemsUpdated++
		return &res.ItemsUpdated
	case out.AttachedAsCopy:
		res.Unchanged++
		return &res.Unchanged
	case out.ContentChanged, out.MetadataChanged, out.FileCreated, out.Joined:
		res.ItemsUpdated++
		return &res.ItemsUpdated
	case out.SidecarsChanged:
		// A sidecar-only change (an edited .lrc, a new cover) reaches the full path but
		// changes no audio bytes, so ItemCreated and ContentChanged are both false.
		// Without this case the scan reports changed=false, and watch mode's downstream
		// schedulers are silently skipped.
		res.SidecarsUpdated++
		return &res.SidecarsUpdated
	}
	res.Unchanged++
	return &res.Unchanged
}

// countAudio counts the audio files under root the walk would visit, by its own skip
// rules, for a first scan's progress denominator.
func countAudio(ctx context.Context, root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if model.IsTrashName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && isAudio(path) {
			n++
		}
		return nil
	})
	return n
}

// yearSuffix is the " (YYYY)" an album folder commonly ends with.
var yearSuffix = regexp.MustCompile(`\s+\(\d{4}\)$`)

// folderNames fills a track's artist and album from the folders above it, for a library
// with the folder fallback on, when its tags name none of artist, album artist or album:
// the grandparent folder is the artist and the parent the album, less a trailing year. A
// disc subfolder names the disc rather than the album, so the album and artist come from
// the two folders above it. rel is the path under the library root, and a file without
// both folders above it names nothing, since one folder cannot say which of the two it
// is. It returns the fields it filled.
func folderNames(tags *model.Tags, rel string) []string {
	if tags.Artist != "" || tags.AlbumArtist != "" || tags.Album != "" {
		return nil
	}
	dirs := strings.Split(filepath.ToSlash(filepath.Dir(rel)), "/")
	if _, ok := identity.DiscFolder(dirs[len(dirs)-1]); ok {
		dirs = dirs[:len(dirs)-1]
	}
	if len(dirs) < 2 {
		return nil
	}
	tags.Artist = dirs[len(dirs)-2]
	tags.Album = yearSuffix.ReplaceAllString(dirs[len(dirs)-1], "")
	return []string{"artist", "album"}
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
func bookInput(libraryID int64, file model.File, tags model.Tags, essenceHash string, cover *model.ArtImage, at place) model.PutScannedBookInput {
	title := BookTitle(tags)
	author := firstNonEmpty(tags.AlbumArtist, tags.Artist)
	key := BookIdentityKey(tags)
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
			Subtitle:   tags.Subtitle,
			Author:     author,
			AuthorSort: authorSort,
			Authors:    meta.SplitCredits(author),
			Narrators:  tags.Narrators,
			Narrator:   strings.Join(tags.Narrators, ", "),
			Series:     tags.Series,
			SeriesSeq:  tags.SeriesSeq,
			Year:       tags.Year,
			Publisher:  tags.Publisher,
			ASIN:       tags.ASIN,
			ISBN:       tags.ISBN,
			Edition:    tags.Edition,
			// Stored unvalidated like track.mbid: a scan mirrors what the file says.
			// It is also what gates the book arm of enrichment.
			MBID:        strings.TrimSpace(tags.MBReleaseID),
			Abridged:    tags.Abridged,
			Description: tags.Description,
			Genres:      tags.Genres,
			Genre:       tags.Genre,
			TrackTotal:  tags.TrackTotal,
		},
		Position:      at.position,
		DiscUnstated:  at.discUnstated,
		PlaceUnstated: at.placeUnstated,
		Chapters:      chapters,
		ChapterSource: chapterSource,
		CoverArt:      cover,
		CustomTags:    tags.Custom,
		Acquisition:   tags.Acquisition,
	}
}

// place is where a book part sits in its book, and which of it the file leaves unstated.
type place struct {
	position                    int
	discUnstated, placeUnstated bool
}

// partPlace is a book part's place as the file states it, an import's (given) filling
// what it does not; the catalog keeps what is still unstated for a part it already holds.
func partPlace(tags *model.Tags, root, path string, managed bool, given *int) place {
	disc, discStated := partDisc(tags, root, path, managed)
	at, placeStated := partPosition(tags, path, managed)
	if given != nil {
		givenDisc, givenPlace := model.SplitPartPosition(*given)
		if !discStated {
			disc, discStated = givenDisc, true
		}
		if !placeStated {
			at, placeStated = givenPlace, true
		}
	}
	return place{position: model.PartPosition(disc, at), discUnstated: !discStated, placeUnstated: !placeStated}
}

// carvedTracks composes a rip's windows and its .cue sheet into virtual tracks.
// Album-level fields prefer the cue header and fall back to the file's own tags, and
// each track's performer falls back to the album artist. Identity is offset-anchored
// via VirtualTrackKey, so a rescan re-keys the same tracks and a per-track title
// retag does not fork identity.
func carvedTracks(essenceHash string, tags model.Tags, sheet *meta.CueSheet, windows []meta.CueWindow) []model.VirtualTrack {
	album := firstNonEmpty(sheet.Title, tags.Album, tags.Title)
	albumArtist := firstNonEmpty(sheet.Performer, tags.AlbumArtist, tags.Artist)
	genre := firstNonEmpty(sheet.Genre, tags.Genre)
	year := sheet.Year
	if year == 0 {
		year = tags.Year
	}

	tracks := make([]model.VirtualTrack, 0, len(windows))
	for _, w := range windows {
		ct := w.Track
		title := ct.Title
		if title == "" {
			title = fmt.Sprintf("Track %02d", ct.Number)
		}
		vt := model.VirtualTrack{
			Item: model.PlayableItem{Title: title, IdentityKey: identity.VirtualTrackKey(essenceHash, ct.Number, w.StartFrames)},
			Track: model.Track{
				Artist: firstNonEmpty(ct.Performer, albumArtist), Album: album, AlbumArtist: albumArtist,
				TrackNo: ct.Number, Year: year, Genre: genre,
			},
			StartFrames: w.StartFrames,
			EndFrames:   w.EndFrames,
		}
		fillRipTrack(&vt, tags)
		tracks = append(tracks, vt)
	}
	return tracks
}

// fillRipTrack completes a virtual track from its own fields and the file's tags.
func fillRipTrack(vt *model.VirtualTrack, tags model.Tags) {
	vt.Item.Kind, vt.Item.State, vt.Item.SortKey = model.KindTrack, model.StatePresent, model.SortKey(vt.Item.Title)
	vt.Track.ArtistSort = model.SortKey(vt.Track.Artist)
	vt.Track.Genres = identity.SplitGenres(vt.Track.Genre)
	// From the file's own tags: a .cue has no vocabulary for these, and without them a
	// carved rip's album row is empty where a plain rip's is filled.
	//
	// BPM is left out on purpose. A cue sheet states no tempo, and the file's own value
	// describes the whole rip rather than the track being carved, so copying it down
	// would stamp one number onto every track. A per-track bpm is an edit away for
	// anyone who wants one.
	vt.Track.Barcode, vt.Track.Label, vt.Track.CatalogNumber = tags.Barcode, tags.Label, tags.CatalogNumber
	vt.Track.Media, vt.Track.Country = tags.Media, tags.Country
	// One rip is one disc, so every track it carves sits on the file's disc.
	vt.Track.DiscNo, vt.Track.DiscTotal = tags.DiscNo, tags.DiscTotal
}

// keptRip returns the virtual tracks a file was last carved into, each with its own
// stored identity key, fields and window, so re-putting them reconciles as a no-op.
// Rebuilding identity from the stored track number would fork a track the user
// renumbered, and one track's album fields would spread its edits to the rest.
func (s *Scanner) keptRip(ctx context.Context, path string, tags model.Tags) ([]model.VirtualTrack, error) {
	vts, err := s.cat.VirtualTracksForPath(ctx, []byte(path))
	if err != nil || len(vts) < 2 {
		return nil, err
	}
	for i := range vts {
		fillRipTrack(&vts[i], tags)
	}
	return vts, nil
}

// abridgedMarkerRe matches a trailing BRACKETED "(Unabridged)"/"[Abridged]" marker
// that audiobook taggers append to the title or album. parseAbridged in the meta
// adapter reads the flag; this strips it so the stored book title is clean. It
// requires the brackets (mirroring parseAbridged) so a title that genuinely ends in
// the word is not truncated, which would also shift the identity key.
var abridgedMarkerRe = regexp.MustCompile(`(?i)\s*[\(\[]\s*(?:un)?abridged\s*[\)\]]\s*$`)

// BookIdentityKey derives an audiobook's content identity key from its scanned tags: the
// same key PutScannedBook resolves the item by, from the title (ALBUM), author
// (ALBUMARTIST), and any identifiers. It is exported so the on-disk write-back re-anchor
// can recompute a book's identity from its file after a title or author edit lands there,
// keeping the catalog's stored key in step with what a rescan will derive. tags must have
// had meta.PromoteBookFields applied, which is what fills the identifiers and the edition.
// It returns "" for a book with no title, author, or identifier, in which case the scanner
// falls back to the essence hash, an identity a metadata edit does not disturb.
func BookIdentityKey(tags model.Tags) string {
	author := firstNonEmpty(tags.AlbumArtist, tags.Artist)
	return identity.BookKey(tags.ASIN, tags.ISBN, author, BookTitle(tags), tags.Edition)
}

// BookTitle is the title a book takes from a file's tags: the album, else the title, less
// a trailing abridged or unabridged marker.
func BookTitle(tags model.Tags) string {
	return cleanBookTitle(firstNonEmpty(tags.Album, tags.Title))
}

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
	// Tags.Artist is only the FIRST value, so a repeated ARTIST frame needs the whole
	// list joined, or a credit write-back loses every artist after the first on the
	// next scan. The separator matches syncCreditDenormTx's, so it round-trips.
	artistDisplay := tags.Artist
	if len(tags.Artists) > 1 {
		artistDisplay = strings.Join(tags.Artists, ", ")
	}
	artistForSort := artistDisplay
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
	// The composer sort mirrors the artist handling: a tagged COMPOSERSORT wins as
	// input, else the composer itself, folded through SortKey either way (an empty
	// composer yields an empty key).
	composerSortInput := tags.ComposerSort
	if composerSortInput == "" {
		composerSortInput = tags.Composer
	}
	return model.Track{
		Artist:           artistDisplay,
		Artists:          creditArtists(tags),
		ArtistSort:       artistSort,
		Album:            tags.Album,
		AlbumArtist:      tags.AlbumArtist,
		Composer:         tags.Composer,
		ComposerSort:     model.SortKey(composerSortInput),
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
		BPM:              tags.BPM,
		MBID:             tags.MBID,
		MBReleaseID:      tags.MBReleaseID,
		MBReleaseGroupID: tags.MBReleaseGroupID,
		MBArtistIDs:      tags.MBArtistIDs,
		MBAlbumArtistIDs: tags.MBAlbumArtistIDs,
		Barcode:          tags.Barcode,
		Label:            tags.Label,
		CatalogNumber:    tags.CatalogNumber,
		Media:            tags.Media,
		Country:          tags.Country,
	}
}

// creditArtists returns the credit list the file stated, or nil to let the store
// split the combined value. ID3v2.4 and Vorbis both carry true multi-valued tags, so
// a repeated frame is a statement and a split is a guess; the store treats them
// differently.
func creditArtists(tags model.Tags) []string {
	if len(tags.Artists) > 1 {
		return append([]string(nil), tags.Artists...)
	}
	return nil
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
	lrcPath := sidecarPath(audioPath, ".lrc")
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
					ly := &model.Lyrics{Source: model.SourceSidecar, Synced: synced}
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
	if obs := coverObservation(audioPath, cache); obs != nil {
		aux = append(aux, *obs)
	}
	return lyrics, aux, diags
}

// coverObservation is the observation of the directory cover resolveCover takes for a
// file: the first decodable one across coverDirs, else the first cover file present.
func coverObservation(audioPath string, cache *artCache) *model.AuxObservation {
	dirs := cache.coverDirs(audioPath)
	for _, d := range dirs {
		if obs := cache.dirCoverObs(d); obs != nil {
			return obs
		}
	}
	for _, d := range dirs {
		if obs := cache.dirCoverStat(d); obs != nil {
			return obs
		}
	}
	return nil
}

// coverDirs lists the folders a file's directory cover is looked for in: its own, then
// the album folder above when it sits in a disc subfolder of the library, since an album
// laid out in disc folders keeps its cover beside them. The library root is never taken
// for one, so the search stays inside the library.
func (c *artCache) coverDirs(path string) []string {
	dir := filepath.Dir(path)
	if _, ok := identity.FolderDisc(c.root, path); ok {
		return []string{dir, filepath.Dir(dir)}
	}
	return []string{dir}
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

// maxCueDropsShown bounds each list in a cue diagnostic, so a wholly malformed sheet
// yields a readable line rather than a hundred clauses.
const maxCueDropsShown = 3

// cueSheetDiag summarizes everything wrong with a sheet as one diagnostic: why it could
// not be applied at all, the lines that could not be read, the tracks dropped, and
// what became of the sheet, which survives the detail cap whole.
//
// One row per finding would be wrong twice over: file_diagnostic is keyed by
// (file_id, origin, code, tag_key), so rows sharing this code collide and all but one
// silently vanish; and an unbounded list is what lrcPartialDetail already declines to
// build for the same reason. Each list reports its count plus the first few.
func cueSheetDiag(sheet *meta.CueSheet, dropped []string, refusal, disposition string) []model.FileDiagnostic {
	var parts []string
	if refusal != "" {
		parts = append(parts, refusal)
	}
	if sheet != nil && len(sheet.Warnings) > 0 {
		lines := make([]string, len(sheet.Warnings))
		for i, w := range sheet.Warnings {
			lines[i] = w.String()
		}
		count := strconv.Itoa(len(lines))
		if sheet.WarningsTruncated {
			count = "at least " + count
		}
		parts = append(parts, fmt.Sprintf("%s line(s) of the cue sheet could not be read: %s",
			count, cueShown(lines, sheet.WarningsTruncated)))
	}
	if len(dropped) > 0 {
		parts = append(parts, fmt.Sprintf("%d cue TRACK(s) dropped from the sheet: %s", len(dropped), cueShown(dropped, false)))
	}
	if len(parts) == 0 {
		return nil
	}
	tail := ""
	if disposition != "" {
		tail = "; " + disposition
	}
	detail := model.CapDetailWithTail(strings.Join(parts, "; "), tail)
	return []model.FileDiagnostic{{Code: model.DiagCueTrackDropped, Severity: model.SeverityWarn, Detail: detail}}
}

// cueShown joins the first maxCueDropsShown entries and counts the rest, as a floor
// when the list itself was cut short.
func cueShown(list []string, cut bool) string {
	if len(list) <= maxCueDropsShown {
		return strings.Join(list, "; ")
	}
	more := strconv.Itoa(len(list) - maxCueDropsShown)
	if cut {
		more = "at least " + more
	}
	return fmt.Sprintf("%s (and %s more)", strings.Join(list[:maxCueDropsShown], "; "), more)
}

// sidecarPath is the same-basename sidecar of an audio file with the given extension.
func sidecarPath(audioPath, ext string) string {
	return strings.TrimSuffix(audioPath, filepath.Ext(audioPath)) + ext
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
// the skip from being invisible. It and a sheet that cannot be applied at all (the
// refusal says why) both report unread, which is different from a sheet that parsed
// to no tracks: an unread sheet says nothing about the tracks, so a rip keeps the
// ones it has. A sheet with unread lines is returned as read, carrying its warnings.
func scanCueSidecar(audioPath string) (sheet *meta.CueSheet, obs model.AuxObservation, diags []model.FileDiagnostic, refusal string, unread, ok bool) {
	cuePath := sidecarPath(audioPath, ".cue")
	// Stat before reading, so the same memory guard the .lrc read and the fast path
	// apply also covers the .cue.
	info, serr := os.Stat(cuePath)
	if serr != nil {
		return nil, model.AuxObservation{}, nil, "", false, false
	}
	if info.Size() > maxSidecarBytes {
		diags := []model.FileDiagnostic{{
			Code: model.DiagSidecarSkipped, Severity: model.SeverityWarn,
			Detail: sidecarSkippedDetail(model.AuxCue, info.Size()),
		}}
		return nil, statOnlyObs(model.AuxCue, cuePath, info), diags, "", true, true
	}
	data, err := os.ReadFile(cuePath)
	if err != nil {
		return nil, model.AuxObservation{}, nil, "", false, false
	}
	obs = model.AuxObservation{
		Kind: model.AuxCue, Path: []byte(cuePath), Hash: art.Hash(data),
		Size: info.Size(), MTimeNS: info.ModTime().UnixNano(),
	}
	sheet, perr := meta.ParseCueSheet(string(data))
	if perr != nil {
		return nil, obs, nil, errMsg(perr), true, true
	}
	return sheet, obs, nil, "", false, true
}

// errMsg is an error's message without the op it was raised under.
func errMsg(err error) string {
	var we *waxerr.Error
	if errors.As(err, &we) && we.Msg != "" {
		return we.Msg
	}
	return err.Error()
}

// reconcileFastPathSidecars re-checks an unchanged audio file's sidecars in one pass
// and returns needsFull=true when any of them changed, appeared, or vanished: the full
// path owns re-deriving what a sidecar feeds (lyrics, chapters, the virtual-track set,
// cover precedence) together with the diagnostics each one can raise. Each sidecar is
// stat'd once and none is read here.
func (s *Scanner) reconcileFastPathSidecars(path string, known model.ScopedFile, cache *artCache) (needsFull bool) {
	stored := make(map[string]model.AuxObservation, len(known.Aux))
	for _, o := range known.Aux {
		stored[o.Kind] = o
	}

	// The directory cover is stat-gated (no per-scan read+hash of an unchanged cover);
	// any change routes through the full path so resolveCover keeps embedded precedence.
	if coverChangedFast(path, stored, cache) {
		return true
	}

	// Any .lrc change routes to the full path, whatever the change is.
	//
	// That matters most in the repair direction. Re-deriving the lyrics_partial
	// diagnostic is full-path work, since the store replaces the scan's whole
	// diagnostic set there. Routing only a break would cover half the story: a .lrc
	// edited from partial back to clean would keep its now-false lyrics_partial row
	// indefinitely, which is the staleness the diagnostics design exists to prevent.
	//
	// The cost is bounded. The .lrc just changed, so one re-parse is cheap, and the
	// stat comparison short-circuits every later scan before touching the file again.
	// An oversized .lrc routes here too, once: the full path records its skip and a
	// stat-only observation, and that observation makes the next scan's size and mtime
	// comparison match.
	lrcPath := sidecarPath(path, ".lrc")
	switch statSidecar(lrcPath, model.AuxLyrics, stored) {
	case sidecarVanished, sidecarChanged, sidecarOversized:
		return true
	}

	// A .cue change routes there too, on a book as on a track: only the full path can
	// clear a cue_track_dropped once the sheet is fixed, and for a track it owns the
	// virtual-track set.
	cuePath := sidecarPath(path, ".cue")
	switch statSidecar(cuePath, model.AuxCue, stored) {
	case sidecarChanged, sidecarVanished, sidecarOversized:
		return true
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
	// No prior cover: a cover newly appearing is a change. dirCoverStat lists each
	// directory once (cached), without reading/hashing the image.
	for _, d := range cache.coverDirs(path) {
		if cache.dirCoverStat(d) != nil {
			return true
		}
	}
	return false
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
	root     string                           // the library root, the edge of the folders a cover is looked for in
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

// artCacheAt is a cache for a scan of the library rooted at root.
func artCacheAt(root string) *artCache {
	c := newArtCache()
	c.root = root
	return c
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
	for _, d := range cache.coverDirs(path) {
		if dir := cache.dirCover(d); dir != nil {
			return dir // a valid (decodable) directory cover
		}
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
// when there is none. The image is stamped "sidecar": it came off a companion file
// beside the audio, not out of the tags. Its path is not carried as a source URL;
// the on-disk location is recorded as the AuxCover observation instead.
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
		img := &model.ArtImage{Data: data, Attribution: model.Attribution{Source: model.SourceSidecar}}
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

// finalizeArt fills an image's content hash and, when the bytes are recognized, its
// format and pixel dimensions. It reports whether they were. The hash is always set, so
// undecodable bytes can still be stored as a last resort, but a recognized cover is
// preferred over one that is not. An AVIF/HEIC cover counts as recognized on its magic
// alone, which keeps a cover.avif from being skipped as unreadable even though its
// dimensions stay unknown. Empty bytes return false.
//
// Bytes nothing here recognizes keep whatever format they arrived with: an embedded BMP
// or TIFF picture has no pure-Go decoder, but its own tag frame's MIME already named it,
// and that is the only description the stored cover will ever have.
func finalizeArt(img *model.ArtImage) bool {
	if img == nil || len(img.Data) == 0 {
		return false
	}
	info := art.Describe(img.Data)
	img.Hash = info.Hash
	if info.Format == "" {
		return false
	}
	img.Format, img.Width, img.Height = info.Format, info.Width, info.Height
	return true
}

// unsupportedFormat reports whether the reader gave up on this file's container.
func unsupportedFormat(diags []model.FileDiagnostic) bool {
	for _, d := range diags {
		if d.Code == model.DiagUnsupportedFormat {
			return true
		}
	}
	return false
}

// probeProperties fills the stream properties no tag parser could read, from the
// decoder's own view of the container header. Left at zero they blank the display,
// drop the file out of duration rollups, and make the upgrade scan rank a 24/96
// file below a 16/44.1 one. A container the decoder cannot open either keeps
// the zeroes; there is nothing better to say about it, and a damaged file must
// still catalog so audit can name it. Only cancellation returns an error: the
// size+mtime fast path would freeze zeroes committed by an interrupted probe, so
// an interrupted file's scan must not commit at all. Other probe failures keep
// the zeroes and log at warn, since they too persist until a forced rescan.
func (s *Scanner) probeProperties(ctx context.Context, path string, tags *model.Tags) error {
	p, err := decode.Probe(ctx, path)
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		s.log.Warn("scan: no properties for an unparsed container", "path", path, "err", err)
		return nil
	}
	tags.DurationMS, tags.Bitrate = p.DurationMS, p.Bitrate
	tags.SampleRate, tags.Channels, tags.BitDepth = p.SampleRate, p.Channels, p.BitDepth
	return nil
}

// audioExts is the set of extensions the scanner picks up: what the tag library
// claims, minus excludedExts. A test pins the two against the library's own list, so a
// format added upstream surfaces here instead of being skipped in silence.
var audioExts = map[string]bool{
	".mp3": true, ".mpga": true, ".flac": true, ".wav": true, ".wave": true,
	".ogg": true, ".oga": true, ".opus": true,
	".m4a": true, ".m4b": true, ".m4r": true, ".mp4": true, ".alac": true,
	".aac": true, ".adts": true,
	".wma": true, ".aiff": true, ".aif": true, ".aifc": true, ".afc": true,
	".ape": true, ".wv": true, ".mpc": true, ".mp+": true,
	".mka": true,
}

// excludedExts are extensions the tag library claims that the scanner leaves alone.
// .mkv, .webm, .mk3d, .mks, .mov, and .asf share a container with an audio-only
// spelling but routinely carry video. A format WaxFlow could not decode at all would
// belong here too, since its files would sit in the analyze pass's retry set for good;
// .wma is not one, because every WMA generation the tag library names now decodes.
var excludedExts = map[string]bool{
	".mkv": true, ".webm": true, ".mk3d": true, ".mks": true, ".mov": true, ".asf": true,
}

func isAudio(path string) bool { return audioExts[strings.ToLower(filepath.Ext(path))] }

// IsAudio reports whether a path has a recognized audio extension. It is the one
// source of truth for the audio-file set, shared with the importer.
func IsAudio(path string) bool { return isAudio(path) }

// AudioExtensions lists the extensions IsAudio accepts, lowercase and sorted.
func AudioExtensions() []string { return slices.Sorted(maps.Keys(audioExts)) }

// CueRipTracks reports how many cue tracks a scan would carve from the sheet beside an
// audio file read as a track with no embedded chapters: none when there is no sheet, when
// it does not read clean, or when it carves fewer than two (the file then stays one track).
// codec and durationMS are the file's; a lossless file's length bounds the last track.
func CueRipTracks(audioPath, codec string, durationMS int64) int {
	sheet, _, _, refusal, unread, ok := scanCueSidecar(audioPath)
	if !ok || sheet == nil || unread || refusal != "" || len(sheet.Warnings) > 0 {
		return 0
	}
	var fileMS int64
	if model.LosslessCodec(codec) {
		fileMS = durationMS
	}
	carve, _, err := sheet.Carve(fileMS)
	if err != nil || len(carve) < 2 {
		return 0
	}
	return len(carve)
}

// spelledRel is path relative to root as the disk spells both, or "" when either
// cannot be resolved.
func spelledRel(root, path string) string {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return ""
	}
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(r, p)
	if err != nil {
		return ""
	}
	return rel
}
