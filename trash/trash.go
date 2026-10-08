// Package trash plans and applies file removal without deleting catalog history.
// User deletes move files to a same-volume per-library trash with an undo
// journal; prune and permanent modes delete from disk immediately. The logical
// item is preserved and archived if it loses its last file. Restore and empty
// live on the facade, which re-scans a restored file.
package trash

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the deletion service needs (satisfied by store/sqlite).
type Store interface {
	TrashFile(ctx context.Context, in model.TrashFileInput) (*model.DetachResult, error)
	DetachFile(ctx context.Context, filePID model.PID) (*model.DetachResult, error)
	// ItemFiles returns every file backing an item, so a multi-file book's parts are
	// all planned for deletion, not just the representative primary.
	ItemFiles(ctx context.Context, itemPID model.PID) ([]model.ItemFileRef, error)
	// RipTracks returns the tracks a cue sheet carves out of a file.
	RipTracks(ctx context.Context, filePID model.PID) ([]model.ItemRef, error)
}

// Service plans and applies deletions.
type Service struct {
	store Store
	log   *slog.Logger
}

// New builds a deletion service.
func New(store Store, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, log: log}
}

// Action is one planned file deletion.
type Action struct {
	ItemPID  model.PID
	FilePID  model.PID
	Src      string // current absolute path
	SrcBytes []byte
	TrashDst string // destination in the trash (trash mode only)
	// Root is the root of the file's library, which no prune of the folders the deletion
	// empties passes, and InPlace marks an in-place library, whose folder companions keep
	// their folder.
	Root    string
	InPlace bool
	Skip    bool
	Reason  string
	// Tracks lists, for the file of a cue rip, every track it plays, which the deletion
	// archives with it; it is empty for any other file.
	Tracks []model.PID
}

// Plan is a set of deletions under one mode. Produced read-only (dry run) and
// applied separately.
type Plan struct {
	Mode    model.DeleteMode
	Actions []Action
	// Reason is what each trash row the plan writes records, in place of the mode's own
	// (see CheckReason); empty takes the mode's. A bypass mode writes no row.
	Reason string
	// SkippedPodcast counts items a caller dropped before planning because they are
	// podcast episodes, where `podcast unfetch` owns the bytes. Plan never sets it; the
	// caller does (see Library.PlanDelete), and the guard stays there rather than
	// moving into Plan because the two Library entry points need opposite answers:
	// PlanDelete skips so a mixed sweep still runs, while PlanDeletePIDs refuses,
	// because the caller named the item and a silent skip would lie. A Plan-side guard
	// could only ever skip.
	//
	// It is carried through to Report so a sweep can say what it did not cover instead
	// of reporting a smaller total with no explanation. The waxbin CLI has no
	// query-driven delete, so nothing there reads it yet; PlanDelete is a facade API
	// and an embedder driving a sweep is the consumer.
	SkippedPodcast int
	// SkippedReadOnly counts items a caller dropped before planning because their
	// library is read-only, carried to Report like SkippedPodcast.
	SkippedReadOnly int
	// SkippedRipTracks counts tracks a caller dropped before planning because the sweep
	// matched only some of the tracks a cue sheet carves out of their file (DropPartialRips),
	// carried to Report like SkippedPodcast.
	SkippedRipTracks int
}

// MaxReasonBytes bounds a caller's delete reason.
const MaxReasonBytes = 64

// CheckReason validates a caller's delete reason under mode: valid UTF-8, at most
// MaxReasonBytes, and no control, format or line-breaking characters (a right-to-left
// override, a zero-width space or joiner, a line or paragraph separator), since it is a
// label printed in one column of one `trash list` row and those would reorder, hide or
// break what it says; and none at all beside a mode that bypasses the trash, which writes
// no row to record it on.
func CheckReason(mode model.DeleteMode, reason string) error {
	const op = "trash.CheckReason"
	switch {
	case reason != "" && mode.BypassesTrash():
		return waxerr.New(waxerr.CodeInvalid, op, "a delete reason is recorded in the trash, which a "+string(mode)+" delete bypasses")
	case len(reason) > MaxReasonBytes:
		return waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("a delete reason is at most %d bytes, got %d", MaxReasonBytes, len(reason)))
	case !utf8.ValidString(reason):
		return waxerr.New(waxerr.CodeInvalid, op, "a delete reason must be valid UTF-8")
	case strings.IndexFunc(reason, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) }) >= 0:
		return waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("a delete reason cannot hold a control, format or line-breaking character: %q", reason))
	}
	return nil
}

// reason is what the plan's trash rows record.
func (p *Plan) reason() string {
	if p.Reason != "" {
		return p.Reason
	}
	return p.Mode.Reason()
}

// Pending returns the number of actions that would actually delete.
func (p *Plan) Pending() int {
	n := 0
	for i := range p.Actions {
		if !p.Actions[i].Skip {
			n++
		}
	}
	return n
}

// Report summarizes an applied plan.
type Report struct {
	Trashed        int
	Deleted        int
	Skipped        int
	Errored        int
	ReclaimedBytes int64
	Failures       []Failure
	// SkippedPodcast is the plan's count carried through, so a report reads as a
	// complete account of the matched set. Skipped counts planned actions the
	// execution skipped; these never became actions at all.
	SkippedPodcast   int
	SkippedReadOnly  int
	SkippedRipTracks int
	// Promoted lists the alternates that took a removed file's place, for the caller
	// to re-read once the run is over.
	Promoted []model.PromotedFile
	// DirsPruned counts the folders the run emptied and removed.
	DirsPruned int
}

// Failure records one deletion that could not be applied.
type Failure struct {
	FilePID model.PID
	Src     string
	Err     string
}

// Plan computes the deletion for each item under the mode. Every backing file is
// planned, so a multi-file book is removed in full rather than losing only its
// primary part, and an item's alternates go with it. A file in a read-only library is
// planned as a skip, which leaves the item on that file. An item with no backing file
// is skipped; a trashed file's destination is placed in its library's trash directory
// under a unique sub-directory so same-named files never collide. The file of a cue rip
// is planned once for all its tracks, and only when every one of them is deleted, since
// deleting it takes every track it plays: one track alone is refused (CodeInvalid),
// naming the others.
func (s *Service) Plan(ctx context.Context, libs []*model.Library, items []*model.ItemView, mode model.DeleteMode) (*Plan, error) {
	if !mode.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, "trash.Plan", "invalid delete mode: "+string(mode))
	}
	return s.plan(ctx, newReads(s.store), libs, items, mode)
}

// PlanSweep is Plan for a sweep over a query's matches: rather than refuse a track of a cue
// rip whose other tracks the sweep did not match, it leaves out every track of that rip it
// matched, counting them in SkippedRipTracks, and deletes the rest.
func (s *Service) PlanSweep(ctx context.Context, libs []*model.Library, items []*model.ItemView, mode model.DeleteMode) (*Plan, error) {
	if !mode.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, "trash.PlanSweep", "invalid delete mode: "+string(mode))
	}
	r := newReads(s.store)
	items, dropped, err := dropPartialRips(ctx, r, items)
	if err != nil {
		return nil, err
	}
	plan, err := s.plan(ctx, r, libs, items, mode)
	if err != nil {
		return nil, err
	}
	plan.SkippedRipTracks = dropped
	return plan, nil
}

func (s *Service) plan(ctx context.Context, r *reads, libs []*model.Library, items []*model.ItemView, mode model.DeleteMode) (*Plan, error) {
	deleting := make(map[model.PID]bool, len(items))
	for _, it := range items {
		deleting[it.PID] = true
	}
	plan := &Plan{Mode: mode}
	planned := map[model.PID]bool{}
	var refused []string
	for _, it := range items {
		files, err := r.itemFiles(ctx, it.PID)
		if err != nil {
			return nil, err
		}
		for _, fl := range files {
			if fl.FilePID == "" || fl.DisplayPath == "" || planned[fl.FilePID] {
				continue
			}
			planned[fl.FilePID] = true
			a := planFile(libs, it.PID, fl, mode)
			if fl.Virtual {
				tracks, kept, err := r.ripTracks(ctx, fl.FilePID, deleting)
				if err != nil {
					return nil, err
				}
				if len(kept) > 0 {
					var names []string
					for _, tr := range kept {
						names = append(names, fmt.Sprintf("%q (%s)", tr.Title, tr.PID))
					}
					refused = append(refused, fmt.Sprintf("%q (%s) plays a window of the cue rip %s, which also plays %s",
						it.Title, it.PID, fl.DisplayPath, strings.Join(names, ", ")))
					continue
				}
				a.Tracks = tracks
			}
			plan.Actions = append(plan.Actions, a)
		}
	}
	if len(refused) > 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, "trash.Plan", "a cue rip's tracks are deleted together or not at all: "+strings.Join(refused, "; "))
	}
	return plan, nil
}

// reads keeps the item files and rip tracks one planning pass reads, so a rip's tracks are
// read once however many of them the pass meets.
type reads struct {
	store  Store
	files  map[model.PID][]model.ItemFileRef
	tracks map[model.PID][]model.ItemRef
}

func newReads(store Store) *reads {
	return &reads{store: store, files: map[model.PID][]model.ItemFileRef{}, tracks: map[model.PID][]model.ItemRef{}}
}

func (r *reads) itemFiles(ctx context.Context, item model.PID) ([]model.ItemFileRef, error) {
	if files, ok := r.files[item]; ok {
		return files, nil
	}
	files, err := r.store.ItemFiles(ctx, item)
	if err == nil {
		r.files[item] = files
	}
	return files, err
}

// ripTracks returns the tracks a cue sheet carves out of a file, and those of them not in
// deleting.
func (r *reads) ripTracks(ctx context.Context, filePID model.PID, deleting map[model.PID]bool) ([]model.PID, []model.ItemRef, error) {
	refs, ok := r.tracks[filePID]
	if !ok {
		var err error
		if refs, err = r.store.RipTracks(ctx, filePID); err != nil {
			return nil, nil, err
		}
		r.tracks[filePID] = refs
	}
	tracks := make([]model.PID, len(refs))
	var kept []model.ItemRef
	for i, tr := range refs {
		tracks[i] = tr.PID
		if !deleting[tr.PID] {
			kept = append(kept, tr)
		}
	}
	return tracks, kept, nil
}

// dropPartialRips leaves out of a sweep's items every track a cue sheet carves out of a
// file, a copy of the rip included, whose other tracks the sweep did not match, since
// deleting the file takes them all, and counts what it left out.
func dropPartialRips(ctx context.Context, r *reads, items []*model.ItemView) ([]*model.ItemView, int, error) {
	dropped := 0
	for {
		deleting := make(map[model.PID]bool, len(items))
		for _, it := range items {
			deleting[it.PID] = true
		}
		drop := map[model.PID]bool{}
		for _, it := range items {
			if drop[it.PID] {
				continue
			}
			files, err := r.itemFiles(ctx, it.PID)
			if err != nil {
				return nil, 0, err
			}
			for _, fl := range files {
				if !fl.Virtual {
					continue
				}
				tracks, kept, err := r.ripTracks(ctx, fl.FilePID, deleting)
				if err != nil {
					return nil, 0, err
				}
				if len(kept) == 0 {
					continue
				}
				for _, tr := range tracks {
					if deleting[tr] {
						drop[tr] = true
					}
				}
			}
		}
		if len(drop) == 0 {
			return items, dropped, nil
		}
		items = slices.DeleteFunc(slices.Clone(items), func(it *model.ItemView) bool { return drop[it.PID] })
		dropped += len(drop)
	}
}

// FileTarget is one file to delete on its own and the item it backs.
type FileTarget struct {
	ItemPID model.PID
	File    model.ItemFileRef
}

// PlanFiles computes the deletion of single files under the mode: an alternate goes
// alone, and a primary or part gives its place to an alternate when the item has one. The
// file of a cue rip names every track it plays, which its deletion archives.
func (s *Service) PlanFiles(ctx context.Context, libs []*model.Library, targets []FileTarget, mode model.DeleteMode) (*Plan, error) {
	if !mode.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, "trash.PlanFiles", "invalid delete mode: "+string(mode))
	}
	plan := &Plan{Mode: mode}
	for _, t := range targets {
		a := planFile(libs, t.ItemPID, t.File, mode)
		tracks, err := s.store.RipTracks(ctx, t.File.FilePID)
		if err != nil {
			return nil, err
		}
		for _, tr := range tracks {
			a.Tracks = append(a.Tracks, tr.PID)
		}
		plan.Actions = append(plan.Actions, a)
	}
	return plan, nil
}

// Recheck skips each planned deletion of a file whose cue rip plays other tracks than the
// plan named, a whole file having become a rip or a rip having gained or lost a track since
// the plan was made, since deleting it would archive tracks no one chose.
func (s *Service) Recheck(ctx context.Context, plan *Plan) error {
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Skip {
			continue
		}
		refs, err := s.store.RipTracks(ctx, a.FilePID)
		if err != nil {
			return err
		}
		now := make([]model.PID, len(refs))
		for j, tr := range refs {
			now[j] = tr.PID
		}
		if !slices.Equal(now, a.Tracks) {
			a.Skip, a.Reason = true, "the cue rip's tracks changed since the plan; plan the delete again"
		}
	}
	return nil
}

// planFile plans one file's deletion.
func planFile(libs []*model.Library, item model.PID, fl model.ItemFileRef, mode model.DeleteMode) Action {
	a := Action{ItemPID: item, FilePID: fl.FilePID, Src: fl.DisplayPath, SrcBytes: fl.Path}
	lib, ok := libFor(libs, fl.DisplayPath)
	switch {
	case !ok:
		a.Skip, a.Reason = true, "file is not under a known library root"
	case lib.ReadOnly:
		a.Skip, a.Reason = true, "library is read-only"
	default:
		a.Root, a.InPlace = lib.RootPath(), lib.Mode == model.ModeInPlace
		if !mode.BypassesTrash() {
			a.TrashDst = filepath.Join(a.Root, model.TrashDirName, model.NewPID().String(), filepath.Base(fl.DisplayPath))
		}
	}
	return a
}

// Execute applies the plan. A per-action failure is recorded and does not abort
// the run. Trash moves are same-volume renames (the trash lives under the root);
// pruning/permanent deletes remove the file outright and tally reclaimed bytes. The
// folders the run empties are removed once it ends, a canceled run included.
func (s *Service) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	if err := CheckReason(plan.Mode, plan.Reason); err != nil {
		return nil, err
	}
	rep := &Report{SkippedPodcast: plan.SkippedPodcast, SkippedReadOnly: plan.SkippedReadOnly, SkippedRipTracks: plan.SkippedRipTracks}
	emptied := map[string]*Action{}
	sib := organize.NewSiblings()
	for i := range plan.Actions {
		if ctx.Err() != nil {
			rep.DirsPruned = s.prune(emptied, plan.Mode)
			return rep, waxerr.FromContext("trash.Execute", ctx.Err(), waxerr.CodeIO)
		}
		a := &plan.Actions[i]
		if a.Skip {
			rep.Skipped++
			continue
		}
		size, promoted, err := s.apply(ctx, a, plan.Mode, plan.reason(), sib)
		rep.Promoted = append(rep.Promoted, promoted...)
		if err != nil {
			rep.Errored++
			rep.Failures = append(rep.Failures, Failure{FilePID: a.FilePID, Src: a.Src, Err: err.Error()})
			s.log.Warn("delete action failed", "src", a.Src, "mode", plan.Mode, "err", err)
			continue
		}
		emptied[filepath.Dir(a.Src)] = a
		if plan.Mode.BypassesTrash() {
			rep.Deleted++
			rep.ReclaimedBytes += size
		} else {
			rep.Trashed++
		}
	}
	rep.DirsPruned = s.prune(emptied, plan.Mode)
	return rep, nil
}

// foldersDir names the trash's store of folder companions, beside the entries. A run keeps
// the companions of the folders it emptied there, under its own key and at their paths
// below the root, so they belong to the folder rather than to any one entry: a restore
// into the folder brings them back from whichever run kept them, and Sweep drops them
// once no active entry came from the folder.
const foldersDir = "folders"

// prune removes the folders a run emptied and every folder above them it leaves empty, up
// to the library root; emptied maps a folder to the last file it gave up. Junk goes with
// a folder. A managed library's companions go into the folder store under the key of that
// file's entry, or in a bypass mode are held there until their folder is gone and then
// deleted, so a folder that stays after all gets them back. An in-place library's
// companions are the user's and keep their folder.
func (s *Service) prune(emptied map[string]*Action, mode model.DeleteMode) int {
	var held []string
	defer func() {
		for _, h := range held {
			_ = os.RemoveAll(pathx.Long(h))
			_ = os.Remove(pathx.Long(filepath.Dir(h)))
			_ = os.Remove(pathx.Long(filepath.Dir(filepath.Dir(h))))
		}
	}()
	return fsx.PruneAll(slices.Sorted(maps.Keys(emptied)), func(dir string) fsx.PruneOptions {
		a := emptied[dir]
		opts := organize.PruneOptions(a.Root, nil)
		if a.InPlace {
			return opts
		}
		key := filepath.Base(filepath.Dir(a.TrashDst))
		if mode.BypassesTrash() {
			key = "pruning-" + model.NewPID().String()
		}
		store := filepath.Join(a.Root, model.TrashDirName, foldersDir, key)
		if mode.BypassesTrash() {
			held = append(held, store)
		}
		opts.Dispose, opts.Undo = moveUnder(a.Root, store)
		return opts
	}, func(dir string, err error) { s.log.Warn("pruning an emptied folder", "dir", dir, "err", err) })
}

// moveUnder returns a Dispose that moves a companion under dir at its path below root,
// and the Undo that moves it back.
func moveUnder(root, dir string) (dispose, undo func(string) error) {
	at := func(p string) (string, error) {
		rel, err := filepath.Rel(root, p)
		return filepath.Join(dir, rel), err
	}
	dispose = func(p string) error {
		q, err := at(p)
		if err != nil {
			return err
		}
		return fsx.Move(p, q)
	}
	undo = func(p string) error {
		q, err := at(p)
		if err != nil {
			return err
		}
		return fsx.Move(q, p)
	}
	return dispose, undo
}

// apply performs one deletion. For the trash mode it moves the file into the
// trash and then records the journal row; if the catalog write fails after the
// move, the file is moved back so disk and catalog stay consistent. For a bypass
// mode it removes the file and then detaches it. It returns the alternates promoted in
// the file's place.
func (s *Service) apply(ctx context.Context, a *Action, mode model.DeleteMode, reason string, sib *organize.Siblings) (int64, []model.PromotedFile, error) {
	size := onDiskSize(a.Src)
	if mode.BypassesTrash() {
		if err := os.Remove(pathx.Long(a.Src)); err != nil && !os.IsNotExist(err) {
			return 0, nil, waxerr.Wrap(waxerr.CodeIO, "trash.delete", err)
		}
		res, err := s.store.DetachFile(ctx, a.FilePID)
		if err != nil {
			return size, nil, err
		}
		s.sidecars(a.Src, "", sib)
		return size, res.Promoted, nil
	}

	// fsx.Move creates the unique trash sub-directory and is long-path-safe.
	if err := fsx.Move(a.Src, a.TrashDst); err != nil {
		return 0, nil, waxerr.Wrap(waxerr.CodeIO, "trash.move", err)
	}
	res, err := s.store.TrashFile(ctx, model.TrashFileInput{
		FilePID: a.FilePID, Reason: reason,
		TrashPath: []byte(a.TrashDst), TrashDisplay: a.TrashDst,
	})
	if err != nil {
		// Put the file back so a failed catalog write does not strand it in the trash.
		if back := fsx.Move(a.TrashDst, a.Src); back != nil {
			s.log.Error("trashed file stranded: catalog write and rollback both failed",
				"file", a.Src, "trash", a.TrashDst, "err", back)
		}
		return 0, nil, err
	}
	s.sidecars(a.Src, a.TrashDst, sib)
	return size, res.Promoted, nil
}

// Sidecars lists, for each action, the file's own sidecars its deletion takes, replaying
// the run in order as Execute does: a sidecar another file of its name shares stays while
// that file does, and goes with the last of them the plan deletes.
func (p *Plan) Sidecars() [][]string {
	out := make([][]string, len(p.Actions))
	sib := organize.NewSiblings()
	for i := range p.Actions {
		a := &p.Actions[i]
		if a.Skip {
			continue
		}
		for _, m := range organize.SidecarMoves(a.Src, a.Src, sib) {
			if !m.Shared {
				out[i] = append(out[i], m.Src)
			}
		}
		sib.Left(a.Src)
	}
	return out
}

// sidecars takes a removed file's own sidecars with it: into its trash entry beside it,
// or deleted when trashDst is empty. One another file of its name still uses stays, and a
// failure leaves one where it was.
func (s *Service) sidecars(src, trashDst string, sib *organize.Siblings) {
	sib.Left(src)
	dst := trashDst
	if dst == "" {
		dst = src
	}
	for _, m := range organize.SidecarMoves(src, dst, sib) {
		if m.Shared {
			continue
		}
		var err error
		if trashDst == "" {
			err = os.Remove(pathx.Long(m.Src))
		} else {
			err = fsx.Move(m.Src, m.Dst)
		}
		if err != nil {
			s.log.Warn("sidecar left in place", "file", m.Src, "err", err)
		}
	}
}

// Restore ensures a trashed file is back at its original path, with its sidecars and its
// folder's companions. It is idempotent: when the file is already at the original path (a
// retry after a prior restore whose re-scan failed) it only finishes bringing back what
// came with it, and nothing when the file there is not the one the entry trashed (its
// size differs), so the caller can safely re-run a failed restore. It refuses only when
// the original path is occupied by something else, or when the file is gone from both
// places. The caller re-scans the restored path to re-catalog it (un-archiving its item).
func (s *Service) Restore(entry model.TrashEntry) error {
	const op = "trash.Restore"
	orig, trashed := string(entry.OrigPath), string(entry.TrashPath)
	origHere, trashHere := fileExists(orig), fileExists(trashed)
	switch {
	case origHere && !trashHere:
		// Already back, by a restore that stopped before what came with the file; another
		// file at the path is none of this entry's, and keeps the extras out.
		if entry.Size > 0 && onDiskSize(orig) != entry.Size {
			return nil
		}
	case !origHere && trashHere:
		if err := fsx.Move(trashed, orig); err != nil {
			if errors.Is(err, fsx.ErrExist) {
				return waxerr.New(waxerr.CodeConflict, op, "original path is occupied: "+entry.OrigDisplay)
			}
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
	case origHere && trashHere:
		return waxerr.New(waxerr.CodeConflict, op, "original path is occupied: "+entry.OrigDisplay)
	default:
		return waxerr.New(waxerr.CodeNotFound, op, "trashed file is gone: "+entry.OrigDisplay)
	}
	s.restoreExtras(orig, trashed)
	return nil
}

// restoreExtras moves back what the trash took with a file: its sidecars, beside it in its
// entry, and the companions of its folder and the folders above it, from the folder store.
// Junk is dropped, a name taken since leaves the file in the trash, and what it empties
// goes.
func (s *Service) restoreExtras(orig, trashed string) {
	entry := filepath.Dir(trashed)
	trash := filepath.Dir(entry)
	if !model.IsTrashName(filepath.Base(trash)) {
		return
	}
	root := filepath.Dir(trash)
	s.restoreFiles(entry, filepath.Dir(orig))
	_ = os.Remove(pathx.Long(entry))
	store := filepath.Join(trash, foldersDir)
	runs, err := os.ReadDir(pathx.Long(store))
	if err != nil {
		return
	}
	for _, run := range runs {
		for dir := filepath.Dir(orig); pathx.UnderRoot(root, dir) && !pathx.SamePath(root, dir); dir = filepath.Dir(dir) {
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				break
			}
			kept := filepath.Join(store, run.Name(), rel)
			s.restoreFiles(kept, dir)
			_ = os.Remove(pathx.Long(kept))
		}
		_ = os.Remove(pathx.Long(filepath.Join(store, run.Name())))
	}
	_ = os.Remove(pathx.Long(store))
}

// restoreFiles moves the files directly in from back into to, never over a file there,
// and deletes the junk among them.
func (s *Service) restoreFiles(from, to string) {
	entries, err := os.ReadDir(pathx.Long(from))
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(from, e.Name())
		switch {
		case !e.Type().IsRegular():
		case fsx.IsJunk(p):
			_ = os.Remove(pathx.Long(p))
		default:
			if err := fsx.Move(p, filepath.Join(to, e.Name())); err != nil {
				s.log.Warn("trash restore left a file in the trash", "file", p, "dst", filepath.Join(to, e.Name()), "err", err)
			}
		}
	}
}

// Sweep drops what the trash under root keeps that the journal is done with: the folders
// of entries it records as restored, which a restore that could not put a file back
// leaves, and stored folder companions no active entry came from. A folder the journal
// does not know may hold a user's only copy (a file whose rollback failed, a catalog
// restored from an older backup or kept by another catalog), so it stays, with what its
// run stored. Folder paths are compared without regard to case or Unicode form, so a
// spelling that differs from the journal's keeps what it holds.
func (s *Service) Sweep(root string, active, restored []model.TrashEntry) {
	trash := filepath.Join(root, model.TrashDirName)
	live, held, done := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, e := range active {
		live[entryName(e)] = true
		for dir := filepath.Dir(e.OrigDisplay); pathx.UnderRoot(root, dir) && !pathx.SamePath(root, dir); dir = filepath.Dir(dir) {
			if rel, err := filepath.Rel(root, dir); err == nil {
				held[pathx.CollisionKey(rel)] = true
			}
		}
	}
	for _, e := range restored {
		if n := entryName(e); !live[n] {
			done[n] = true
		}
	}
	dirs, err := os.ReadDir(pathx.Long(trash))
	if err != nil {
		return
	}
	for _, d := range dirs {
		p := filepath.Join(trash, d.Name())
		switch {
		case !d.IsDir():
		case d.Name() == foldersDir:
			// A run whose entry is still on disk and not restored belongs to an entry that
			// is active, whose folders are held anyway, or one the journal does not know.
			s.sweepStore(p, held, func(run string) bool { return !done[run] && fileExists(filepath.Join(trash, run)) })
		case done[d.Name()]:
			if err := os.RemoveAll(pathx.Long(p)); err != nil {
				s.log.Warn("sweeping a restored trash entry", "dir", p, "err", err)
			}
		}
	}
}

func entryName(e model.TrashEntry) string { return filepath.Base(filepath.Dir(e.TrashDisplay)) }

// sweepStore removes each run's folders that no active entry came from, keeping a run
// whole when keep says so, then the runs and the store once empty.
func (s *Service) sweepStore(store string, held map[string]bool, keep func(run string) bool) {
	runs, err := os.ReadDir(pathx.Long(store))
	if err != nil {
		return
	}
	var sweep func(dir, rel string)
	sweep = func(dir, rel string) {
		entries, err := os.ReadDir(pathx.Long(dir))
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			r, p := filepath.Join(rel, e.Name()), filepath.Join(dir, e.Name())
			if !held[pathx.CollisionKey(r)] {
				if err := os.RemoveAll(pathx.Long(p)); err != nil {
					s.log.Warn("sweeping stored folder companions", "dir", p, "err", err)
				}
				continue
			}
			sweep(p, r)
		}
	}
	for _, run := range runs {
		if keep(run.Name()) {
			continue
		}
		p := filepath.Join(store, run.Name())
		sweep(p, "")
		_ = os.Remove(pathx.Long(p))
	}
	_ = os.Remove(pathx.Long(store))
}

// Purge permanently removes a trashed file (and its unique trash sub-directory)
// from disk, returning the bytes reclaimed. A missing file is not an error: the
// goal is that it is gone.
func (s *Service) Purge(entry model.TrashEntry) (int64, error) {
	size := onDiskSize(string(entry.TrashPath))
	// trash_path is <root>/.waxbin-trash/<unique>/<basename>; removing the unique
	// sub-directory cleans up the file and its container in one step.
	if err := os.RemoveAll(pathx.Long(filepath.Dir(string(entry.TrashPath)))); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "trash.Purge", err)
	}
	return size, nil
}

// libFor returns the library whose root contains path.
func libFor(libs []*model.Library, path string) (*model.Library, bool) {
	for _, lib := range libs {
		if pathx.UnderRoot(lib.RootPath(), path) {
			return lib, true
		}
	}
	return nil, false
}

func onDiskSize(path string) int64 {
	if info, err := os.Stat(pathx.Long(path)); err == nil {
		return info.Size()
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(pathx.Long(path))
	return err == nil
}
