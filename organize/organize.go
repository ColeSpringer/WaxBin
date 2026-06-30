package organize

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/internal/fsx"
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
	Moved         int
	Skipped       int
	Errored       int
	SidecarsMoved int
	Failures      []Failure
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
// backing file are skipped; items already at their destination are marked Skip. A
// multi-file audiobook expands into one move per part so the whole book is
// relocated together rather than split.
func (o *Organizer) Plan(ctx context.Context, lib *model.Library, p Profile, items []*model.ItemView) (*Plan, error) {
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
		// A book may be backed by several part files. The item view carries only the
		// representative primary, so moving just that would strand the other parts;
		// fetch them all and move every part into the rendered book folder.
		if it.Kind == model.KindBook {
			files, err := o.cat.ItemFiles(ctx, it.PID)
			if err != nil {
				return nil, err
			}
			if len(files) > 1 {
				o.planBookParts(plan, root, rel, it.PID, files)
				continue
			}
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
	markCollisions(plan)
	return plan, nil
}

// planBookParts plans a move for every part of a multi-file book into the rendered
// book folder (the directory of the template output). The audiobook template names
// a single file, so a multi-file book names each part "{book title} - NN.ext" using
// the part's 1-based reading-order index (files arrive in reading order). The index
// is a deterministic, unique disambiguator, so two source parts that happen to share
// a basename across folders no longer render to the same destination and get
// silently dropped by collision detection — the split this function exists to
// prevent.
func (o *Organizer) planBookParts(plan *Plan, root, rel string, itemPID model.PID, files []model.ItemFileRef) {
	// All-or-nothing: if any part cannot be placed (no path, or outside this managed
	// root), leave the WHOLE book where it is rather than moving some parts and
	// stranding others — the split this function exists to prevent. Roots are
	// validated non-overlapping, so a legitimately-scanned book's parts are all under
	// one root; this guards a stray/cross-root edge.
	for _, fl := range files {
		if fl.DisplayPath == "" || !pathx.UnderRoot(root, fl.DisplayPath) {
			o.log.Warn("skipping multi-file book organize: a part is not placeable",
				"item", itemPID, "part", fl.DisplayPath)
			return
		}
	}
	folder := filepath.Dir(rel)
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	width := len(strconv.Itoa(len(files)))
	if width < 2 {
		width = 2 // at least two digits ("01") so a small set still sorts on disk
	}
	for i, fl := range files {
		name := fmt.Sprintf("%s - %0*d%s", stem, width, i+1, strings.ToLower(filepath.Ext(fl.DisplayPath)))
		partRel := filepath.Join(folder, sanitizeSegment(name))
		dst := filepath.Join(root, partRel)
		a := Action{
			ItemPID: itemPID, FilePID: fl.FilePID,
			Src: fl.DisplayPath, SrcBytes: fl.Path, Dst: dst, RelDst: partRel,
		}
		if filepath.Clean(a.Src) == filepath.Clean(dst) {
			a.Skip, a.Reason = true, "already in place"
		}
		plan.Actions = append(plan.Actions, a)
	}
}

// markCollisions skips any action whose destination collides with an
// earlier-planned one. Two items rendering to the same path (or to paths that
// differ only by case, which collide on a case-insensitive filesystem) cannot
// both be moved there, so all but the first are held back with a reason rather
// than silently overwriting. The key is cleaned and case-folded so a managed tree
// remains portable across case-sensitive and case-insensitive filesystems.
func markCollisions(plan *Plan) {
	seen := make(map[string]int, len(plan.Actions))
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Skip {
			continue
		}
		key := strings.ToLower(filepath.Clean(a.Dst))
		if j, ok := seen[key]; ok {
			a.Skip = true
			a.Reason = "destination collides with " + plan.Actions[j].Src
			continue
		}
		seen[key] = i
	}
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
			// The audio is moved and recorded; now carry its sidecars (same-basename
			// lyrics/cue/art plus directory cover art) so a move does not leave them
			// behind.
			// Sidecars are not cataloged, so a failure here is logged, not fatal.
			rep.SidecarsMoved += o.moveSidecars(a.Src, a.Dst)
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

// moveFile moves src to dst via the shared long-path-safe mover (create parent,
// no-clobber, cross-device fallback), translating fsx's sentinel into WaxBin's
// typed conflict so a colliding destination is reported, not silently overwritten.
func moveFile(src, dst string) error {
	const op = "organize.move"
	if src == dst {
		return nil
	}
	if err := fsx.Move(src, dst); err != nil {
		if errors.Is(err, fsx.ErrExist) {
			return waxerr.New(waxerr.CodeConflict, op, "destination already exists: "+dst)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}
