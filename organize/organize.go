package organize

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Action is one planned file move.
type Action struct {
	ItemPID  model.PID
	FilePID  model.PID
	Src      string // current absolute path
	SrcBytes []byte // raw bytes of the current path
	Dst      string // planned absolute path
	RelDst   string // destination relative to the library root
	Skip     bool   // already in place / nothing to do
	Reason   string
}

// Plan is a serializable set of moves for one library + profile. It is produced
// read-only (a dry run) and applied separately.
type Plan struct {
	Profile    string
	LibraryPID model.PID
	Root       string
	Actions    []Action
}

// Pending returns the actions that would actually move.
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
	Moved    int
	Skipped  int
	Errored  int
	Failures []Failure
}

// Failure records one action that could not be applied.
type Failure struct {
	FilePID model.PID
	Src     string
	Dst     string
	Err     string
}

// Organizer plans and applies moves against a catalog.
type Organizer struct {
	cat model.Catalog
	log *slog.Logger
}

// New builds an organizer.
func New(cat model.Catalog, log *slog.Logger) *Organizer {
	if log == nil {
		log = slog.Default()
	}
	return &Organizer{cat: cat, log: log}
}

// Plan computes the destination for each item under the profile. Items with no
// backing file are skipped; items already at their destination are marked Skip.
func (o *Organizer) Plan(lib *model.Library, p Profile, items []*model.ItemView) (*Plan, error) {
	root := string(lib.Root)
	plan := &Plan{Profile: p.Name, LibraryPID: lib.PID, Root: root}
	for _, it := range items {
		if it.FilePID == "" || it.DisplayPath == "" {
			continue
		}
		// Only organize files that belong to this library. Roots are validated
		// non-overlapping, so a path under this root cannot belong to another
		// library. That check is what keeps in-place library files out of move
		// plans.
		if !pathx.UnderRoot(root, it.DisplayPath) {
			continue
		}
		rel, err := RenderRelPath(p, it)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(root, rel)
		a := Action{
			ItemPID: it.PID, FilePID: it.FilePID,
			Src: it.DisplayPath, SrcBytes: it.Path, Dst: dst, RelDst: rel,
		}
		if filepath.Clean(a.Src) == filepath.Clean(dst) {
			a.Skip, a.Reason = true, "already in place"
		}
		plan.Actions = append(plan.Actions, a)
	}
	return plan, nil
}

// Execute applies the plan: each move happens on disk, then the catalog records
// the relocation (path update + organize_journal + change_log) in one
// transaction. A per-action failure is recorded and does not abort the run.
func (o *Organizer) Execute(ctx context.Context, plan *Plan, jobPID model.PID, hb func(progress float64, msg string) error) (*Report, error) {
	rep := &Report{}
	total := len(plan.Actions)
	for i := range plan.Actions {
		if ctx.Err() != nil {
			return rep, waxerr.FromContext("organize.Execute", ctx.Err(), waxerr.CodeIO)
		}
		a := &plan.Actions[i]
		if a.Skip {
			rep.Skipped++
			continue
		}
		if err := o.apply(ctx, a, jobPID); err != nil {
			rep.Errored++
			rep.Failures = append(rep.Failures, Failure{FilePID: a.FilePID, Src: a.Src, Dst: a.Dst, Err: err.Error()})
			o.log.Warn("organize action failed", "src", a.Src, "dst", a.Dst, "err", err)
		} else {
			rep.Moved++
		}
		if hb != nil {
			_ = hb(float64(i+1)/float64(max(total, 1)), "organized "+strconv.Itoa(i+1)+"/"+strconv.Itoa(total))
		}
	}
	return rep, nil
}

// apply journals the move as 'planned', performs it on disk, then commits the
// catalog update as 'committed'. If the move fails, it marks the journal row
// 'rolled_back'.
func (o *Organizer) apply(ctx context.Context, a *Action, jobPID model.PID) error {
	in := model.RelocateInput{
		FilePID:        a.FilePID,
		JobPID:         jobPID,
		SrcPath:        a.SrcBytes,
		NewPath:        []byte(a.Dst),
		NewDisplayPath: a.Dst,
		NewRelPath:     []byte(a.RelDst),
	}
	jpid, err := o.cat.PlanMove(ctx, in)
	if err != nil {
		return err
	}
	if err := moveFile(a.Src, a.Dst); err != nil {
		_ = o.cat.AbortMove(ctx, jpid)
		return err
	}
	return o.cat.CommitMove(ctx, jpid, in)
}

// moveFile moves src to dst, creating parent dirs, refusing to overwrite an
// existing destination, and falling back to copy+remove across filesystems.
func moveFile(src, dst string) error {
	const op = "organize.move"
	if src == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// The Lstat-then-rename guard is best-effort: os.Rename overwrites on POSIX,
	// so a file appearing at dst in the race window would be clobbered. Organize
	// runs single-writer under a scoped lease, which bounds this to an external
	// process racing the managed tree. A fully atomic no-clobber rename needs
	// platform-specific support such as renameat2/RENAME_NOREPLACE.
	if _, err := os.Lstat(dst); err == nil {
		return waxerr.New(waxerr.CodeConflict, op, "destination already exists: "+dst)
	} else if !os.IsNotExist(err) {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return copyAndRemove(src, dst)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}

// copyAndRemove implements a cross-device move: copy bytes, fsync, then delete
// the source. On any failure a single deferred cleanup removes the partial
// destination, so the tree is left as it was (source intact, no stray copy).
func copyAndRemove(src, dst string) error {
	const op = "organize.move"
	in, err := os.Open(src)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	var success bool
	defer func() {
		if !success {
			_ = out.Close() // harmless if already closed below
			_ = os.Remove(dst)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := out.Sync(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := out.Close(); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := os.Remove(src); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	success = true
	return nil
}
