// Package inbox imports audio staged outside the library. Planning reads tags,
// renders destinations, checks duplicates and collisions, and totals byte cost.
// Execution performs the free-space preflight, places each file, catalogs it,
// and records an import batch with source attribution.
package inbox

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// Store is the persistence the importer needs (satisfied by store/sqlite).
type Store interface {
	FileByEssence(ctx context.Context, essence string) (*model.File, error)
	DisplayPathExistsFold(ctx context.Context, displayPath string) (bool, error)
	CreateImportBatch(ctx context.Context, b *model.ImportBatch) error
	UpdateImportBatch(ctx context.Context, b *model.ImportBatch) error
}

// Cataloger catalogs one file after it has been placed in the managed tree.
type Cataloger interface {
	ScanFile(ctx context.Context, lib *model.Library, path string) (*scan.Result, error)
}

// Service plans and applies imports.
type Service struct {
	store     Store
	reader    meta.Reader
	cataloger Cataloger
	log       *slog.Logger
}

// New builds an import service.
func New(store Store, reader meta.Reader, cataloger Cataloger, log *slog.Logger) *Service {
	if reader == nil {
		reader = meta.NewReader()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: store, reader: reader, cataloger: cataloger, log: log}
}

// Outcome is what an import plan would do with one file.
type Outcome string

const (
	OutcomeImport     Outcome = "import"     // bring it in
	OutcomeDuplicate  Outcome = "duplicate"  // already in the catalog (skipped under DupSkip)
	OutcomeQuarantine Outcome = "quarantine" // unreadable / undestinable / would collide; left in place
)

// Action is one planned import.
type Action struct {
	Src     string
	Dst     string // destination under the library (import outcome only)
	RelDst  string
	Size    int64
	Essence string
	Outcome Outcome
	Reason  string
}

// Request configures an import.
type Request struct {
	Source       string           // staging folder to import from
	Library      *model.Library   // target managed library
	Profile      organize.Profile // layout for placed files
	DupPolicy    model.DupPolicy  // how to treat catalog duplicates
	Copy         bool             // copy (keep originals) instead of move
	ReserveBytes int64            // free-space headroom to keep on the destination
}

// Plan is a reviewable set of import actions.
type Plan struct {
	Source     string
	Library    *model.Library
	Profile    string
	Copy       bool
	DupPolicy  model.DupPolicy
	Reserve    int64
	TotalBytes int64 // bytes the importable actions would bring in
	Actions    []Action
}

// Importable returns the number of actions that would actually import.
func (p *Plan) Importable() int {
	n := 0
	for i := range p.Actions {
		if p.Actions[i].Outcome == OutcomeImport {
			n++
		}
	}
	return n
}

// Report summarizes an applied import.
type Report struct {
	BatchPID    model.PID
	Imported    int
	Duplicates  int
	Quarantined int
	Errored     int
	Sidecars    int // companion files (lyrics/art/...) carried in with the audio
	Bytes       int64
	Failures    []Failure
}

// Failure records one import that could not be applied.
type Failure struct {
	Src string
	Err string
}

// Plan walks the source folder and classifies every audio file: importable (with
// its rendered destination), a catalog duplicate, or quarantined (unreadable, no
// destination, or a destination that would collide). It does not touch disk.
func (s *Service) Plan(ctx context.Context, req Request) (*Plan, error) {
	const op = "inbox.Plan"
	if req.Library == nil || req.Library.Mode != model.ModeManaged {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "import target must be a managed library")
	}
	if req.DupPolicy == "" {
		req.DupPolicy = model.DupSkip
	}
	if !req.DupPolicy.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "invalid duplicate policy: "+string(req.DupPolicy))
	}
	root := string(req.Library.Root)
	plan := &Plan{
		Source: req.Source, Library: req.Library, Profile: req.Profile.Name,
		Copy: req.Copy, DupPolicy: req.DupPolicy, Reserve: req.ReserveBytes,
	}

	// Claims made within this import: destinations (so two staged files never
	// target the same path) and audio essences (so under DupSkip two staged copies
	// of the same recording don't both import even when their tags, and thus their
	// destinations, differ).
	claims := &batchClaims{dst: map[string]bool{}, essence: map[string]bool{}}

	walkErr := filepath.WalkDir(req.Source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; the walk continues
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		if !isAudio(path) {
			return nil
		}
		plan.Actions = append(plan.Actions, s.classify(ctx, req, root, path, claims))
		return nil
	})
	if walkErr != nil {
		return nil, waxerr.FromContext(op, walkErr, waxerr.CodeIO)
	}
	for i := range plan.Actions {
		if plan.Actions[i].Outcome == OutcomeImport {
			plan.TotalBytes += plan.Actions[i].Size
		}
	}
	return plan, nil
}

// batchClaims tracks the destinations and essences already claimed by earlier
// actions in one import plan.
type batchClaims struct {
	dst     map[string]bool
	essence map[string]bool
}

// classify decides one file's outcome and, for an import, its destination.
func (s *Service) classify(ctx context.Context, req Request, root, path string, claims *batchClaims) Action {
	a := Action{Src: path, Size: onDiskSize(path)}

	fm, err := s.reader.Read(ctx, path)
	if err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "unreadable: "+err.Error()
		return a
	}
	// Match the scanner's essence rule so the duplicate check sees the same key the
	// catalog stores: the audio essence, or the content hash when there is none.
	essence := fm.EssenceHash
	if essence == "" {
		if ch, herr := identity.ContentHash(path); herr == nil {
			essence = ch
		}
	}
	a.Essence = essence

	if essence != "" && req.DupPolicy == model.DupSkip {
		if _, err := s.store.FileByEssence(ctx, essence); err == nil {
			a.Outcome, a.Reason = OutcomeDuplicate, "audio already in the catalog"
			return a
		} else if !waxerr.Is(err, waxerr.CodeNotFound) {
			a.Outcome, a.Reason = OutcomeQuarantine, "dedup check failed: "+err.Error()
			return a
		}
		// A second staged copy of the same recording (possibly tagged differently, so
		// a different destination) is still a duplicate under skip.
		if claims.essence[essence] {
			a.Outcome, a.Reason = OutcomeDuplicate, "duplicate of another file in this import"
			return a
		}
	}

	rel, err := organize.RenderRelPath(req.Profile, itemViewFromTags(fm.Tags, path))
	if err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "no destination: "+err.Error()
		return a
	}
	dst := filepath.Join(root, rel)
	key := caseFold(dst)
	if claims.dst[key] {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination already claimed by another staged file"
		return a
	}
	if pathExists(dst) {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination already exists in the library"
		return a
	}
	// A cataloged file whose path differs only by case would coexist here on Linux
	// but collide on a case-insensitive filesystem, so refuse it for portability.
	if exists, err := s.store.DisplayPathExistsFold(ctx, dst); err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "collision check failed: "+err.Error()
		return a
	} else if exists {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination case-collides with a cataloged file"
		return a
	}
	claims.dst[key] = true
	if essence != "" {
		claims.essence[essence] = true
	}
	a.Dst, a.RelDst, a.Outcome = dst, rel, OutcomeImport
	return a
}

// Execute applies a plan: a free-space preflight first, then each importable file
// is placed and cataloged, and an auditable import batch is recorded. A per-file
// failure is tallied and does not abort the run.
func (s *Service) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	const op = "inbox.Execute"
	if err := preflight(string(plan.Library.Root), plan.TotalBytes, plan.Reserve); err != nil {
		return nil, err
	}

	batch := &model.ImportBatch{
		Source: plan.Source, LibraryID: plan.Library.ID,
		State: model.ImportRunning, StartedAt: nowNS(),
	}
	if err := s.store.CreateImportBatch(ctx, batch); err != nil {
		return nil, err
	}
	rep := &Report{BatchPID: batch.PID}

	for i := range plan.Actions {
		if ctx.Err() != nil {
			s.finalize(ctx, batch, rep, model.ImportFailed)
			return rep, waxerr.FromContext(op, ctx.Err(), waxerr.CodeIO)
		}
		a := &plan.Actions[i]
		switch a.Outcome {
		case OutcomeDuplicate:
			rep.Duplicates++
		case OutcomeQuarantine:
			rep.Quarantined++
		case OutcomeImport:
			sidecars, err := s.importOne(ctx, plan, a)
			if err != nil {
				rep.Errored++
				rep.Failures = append(rep.Failures, Failure{Src: a.Src, Err: err.Error()})
				s.log.Warn("import failed", "src", a.Src, "dst", a.Dst, "err", err)
				continue
			}
			rep.Imported++
			rep.Bytes += a.Size
			rep.Sidecars += sidecars
		}
	}
	s.finalize(ctx, batch, rep, model.ImportDone)
	return rep, nil
}

// importOne places one file in the managed tree, catalogs it, and carries its
// sidecars in alongside it. It returns the number of sidecars placed.
func (s *Service) importOne(ctx context.Context, plan *Plan, a *Action) (int, error) {
	if err := os.MkdirAll(pathx.Long(filepath.Dir(a.Dst)), 0o755); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "inbox.import", err)
	}
	if err := placeFile(a.Src, a.Dst, plan.Copy); err != nil {
		return 0, err
	}
	if _, err := s.cataloger.ScanFile(ctx, plan.Library, a.Dst); err != nil {
		return 0, err
	}
	return s.relocateSidecars(plan, a), nil
}

// relocateSidecars carries an imported file's companions (same-basename
// lyrics/art, directory cover art) into the managed tree, moving or copying them
// to match the audio. Sidecars are not cataloged, so a conflict or failure is
// logged and skipped, never fatal (the audio is already imported). The same
// discovery as organize is used, so import and organize keep one sidecar set.
func (s *Service) relocateSidecars(plan *Plan, a *Action) int {
	moved := 0
	for _, m := range organize.SidecarMoves(a.Src, a.Dst) {
		switch err := fsx.MoveOrCopy(m.Src, m.Dst, plan.Copy); {
		case err == nil:
			moved++
		case errors.Is(err, fsx.ErrExist):
			s.log.Warn("inbox sidecar not placed: destination exists", "src", m.Src, "dst", m.Dst)
		default:
			s.log.Warn("inbox sidecar placement failed", "src", m.Src, "dst", m.Dst, "err", err)
		}
	}
	return moved
}

func (s *Service) finalize(ctx context.Context, batch *model.ImportBatch, rep *Report, state model.ImportBatchState) {
	batch.State = state
	batch.Imported, batch.Duplicates = rep.Imported, rep.Duplicates
	batch.Quarantined, batch.Errored = rep.Quarantined, rep.Errored
	batch.Bytes, batch.FinishedAt = rep.Bytes, nowNS()
	// Use a cancel-free context: a batch interrupted by cancellation must still be
	// recorded as failed/done rather than left forever "running".
	if err := s.store.UpdateImportBatch(context.WithoutCancel(ctx), batch); err != nil {
		s.log.Warn("finalizing import batch", "err", err)
	}
}

// preflight refuses an import that would leave the destination volume below its
// reserve. It is best-effort: where the free-space probe is unsupported, the
// import proceeds.
func preflight(root string, need, reserve int64) error {
	if need <= 0 && reserve <= 0 {
		return nil
	}
	avail, err := diskfree.Available(root)
	if err != nil {
		if errors.Is(err, diskfree.ErrUnsupported) {
			return nil
		}
		return waxerr.Wrap(waxerr.CodeIO, "inbox.preflight", err)
	}
	if int64(avail) < need+reserve {
		return waxerr.New(waxerr.CodeIO, "inbox.preflight",
			"insufficient free space for import (need "+itoa(need+reserve)+" bytes, have "+itoa(int64(avail))+")")
	}
	return nil
}
