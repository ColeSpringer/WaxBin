// Package trash plans and applies file removal without deleting catalog history.
// User deletes move files to a same-volume per-library trash with an undo
// journal; prune and permanent modes delete from disk immediately. The logical
// item is preserved and archived if it loses its last file. Restore and empty
// live on the facade, which re-scans a restored file.
package trash

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the deletion service needs (satisfied by store/sqlite).
type Store interface {
	TrashFile(ctx context.Context, in model.TrashFileInput) (model.PID, error)
	DetachFile(ctx context.Context, filePID model.PID) error
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
	Skip     bool
	Reason   string
}

// Plan is a set of deletions under one mode. Produced read-only (dry run) and
// applied separately.
type Plan struct {
	Mode    model.DeleteMode
	Actions []Action
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
}

// Failure records one deletion that could not be applied.
type Failure struct {
	FilePID model.PID
	Src     string
	Err     string
}

// Plan computes the deletion for each item under the mode. An item with no
// backing file is skipped; a trashed file's destination is placed in its
// library's trash directory under a unique sub-directory so same-named files
// never collide.
func (s *Service) Plan(libs []*model.Library, items []*model.ItemView, mode model.DeleteMode) (*Plan, error) {
	if !mode.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, "trash.Plan", "invalid delete mode: "+string(mode))
	}
	plan := &Plan{Mode: mode}
	for _, it := range items {
		if it.FilePID == "" || it.DisplayPath == "" {
			continue
		}
		a := Action{ItemPID: it.PID, FilePID: it.FilePID, Src: it.DisplayPath, SrcBytes: it.Path}
		root, ok := rootFor(libs, it.DisplayPath)
		if !ok {
			a.Skip, a.Reason = true, "file is not under a known library root"
			plan.Actions = append(plan.Actions, a)
			continue
		}
		if !mode.BypassesTrash() {
			a.TrashDst = filepath.Join(root, model.TrashDirName, model.NewPID().String(), filepath.Base(it.DisplayPath))
		}
		plan.Actions = append(plan.Actions, a)
	}
	return plan, nil
}

// Execute applies the plan. A per-action failure is recorded and does not abort
// the run. Trash moves are same-volume renames (the trash lives under the root);
// pruning/permanent deletes remove the file outright and tally reclaimed bytes.
func (s *Service) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	rep := &Report{}
	for i := range plan.Actions {
		if ctx.Err() != nil {
			return rep, waxerr.FromContext("trash.Execute", ctx.Err(), waxerr.CodeIO)
		}
		a := &plan.Actions[i]
		if a.Skip {
			rep.Skipped++
			continue
		}
		size, err := s.apply(ctx, a, plan.Mode)
		if err != nil {
			rep.Errored++
			rep.Failures = append(rep.Failures, Failure{FilePID: a.FilePID, Src: a.Src, Err: err.Error()})
			s.log.Warn("delete action failed", "src", a.Src, "mode", plan.Mode, "err", err)
			continue
		}
		if plan.Mode.BypassesTrash() {
			rep.Deleted++
			rep.ReclaimedBytes += size
		} else {
			rep.Trashed++
		}
	}
	return rep, nil
}

// apply performs one deletion. For the trash mode it moves the file into the
// trash and then records the journal row; if the catalog write fails after the
// move, the file is moved back so disk and catalog stay consistent. For a bypass
// mode it removes the file and then detaches it.
func (s *Service) apply(ctx context.Context, a *Action, mode model.DeleteMode) (int64, error) {
	size := onDiskSize(a.Src)
	if mode.BypassesTrash() {
		if err := os.Remove(pathx.Long(a.Src)); err != nil && !os.IsNotExist(err) {
			return 0, waxerr.Wrap(waxerr.CodeIO, "trash.delete", err)
		}
		return size, s.store.DetachFile(ctx, a.FilePID)
	}

	// fsx.Move creates the unique trash sub-directory and is long-path-safe.
	if err := fsx.Move(a.Src, a.TrashDst); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "trash.move", err)
	}
	if _, err := s.store.TrashFile(ctx, model.TrashFileInput{
		FilePID: a.FilePID, Reason: mode.Reason(),
		TrashPath: []byte(a.TrashDst), TrashDisplay: a.TrashDst,
	}); err != nil {
		// Put the file back so a failed catalog write does not strand it in the trash.
		if back := fsx.Move(a.TrashDst, a.Src); back != nil {
			s.log.Error("trashed file stranded: catalog write and rollback both failed",
				"file", a.Src, "trash", a.TrashDst, "err", back)
		}
		return 0, err
	}
	return size, nil
}

// Restore ensures a trashed file is back at its original path. It is idempotent:
// if the file is already at the original path (a retry after a prior restore whose
// re-scan failed) it is a no-op, so the caller can safely re-run a failed restore.
// It refuses only when the original path is occupied by something else, or when
// the file is gone from both places. The caller re-scans the restored path to
// re-catalog it (un-archiving its item).
func (s *Service) Restore(entry model.TrashEntry) error {
	const op = "trash.Restore"
	orig, trashed := string(entry.OrigPath), string(entry.TrashPath)
	origHere, trashHere := fileExists(orig), fileExists(trashed)
	switch {
	case origHere && !trashHere:
		return nil // already restored on disk; nothing to move
	case !origHere && trashHere:
		if err := fsx.Move(trashed, orig); err != nil {
			if errors.Is(err, fsx.ErrExist) {
				return waxerr.New(waxerr.CodeConflict, op, "original path is occupied: "+entry.OrigDisplay)
			}
			return waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		return nil
	case origHere && trashHere:
		return waxerr.New(waxerr.CodeConflict, op, "original path is occupied: "+entry.OrigDisplay)
	default:
		return waxerr.New(waxerr.CodeNotFound, op, "trashed file is gone: "+entry.OrigDisplay)
	}
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

// rootFor returns the library root that contains path.
func rootFor(libs []*model.Library, path string) (string, bool) {
	for _, lib := range libs {
		root := lib.DisplayRoot
		if root == "" {
			root = string(lib.Root)
		}
		if pathx.UnderRoot(root, path) {
			return root, true
		}
	}
	return "", false
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
