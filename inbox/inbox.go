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
	PutAcquisitionForFile(ctx context.Context, path []byte, in model.AcquisitionInput) error
}

// Cataloger catalogs one file after it has been placed in the managed tree, honoring
// a forced media kind (empty classifies from tags), so an acquired book forced with
// --as book is cataloged as a book even when its tags do not say so.
type Cataloger interface {
	ScanFileAs(ctx context.Context, lib *model.Library, path string, kind model.Kind) (*scan.Result, error)
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
	Kind    model.Kind     // classified/forced media kind (routes template + library)
	Library *model.Library // target managed library for this file (kind-routed)
	Outcome Outcome
	Reason  string
}

// Request configures an import.
type Request struct {
	Source       string           // staging folder to import from (or a single file for PlanFile)
	Library      *model.Library   // default target managed library
	Profile      organize.Profile // layout for placed files
	DupPolicy    model.DupPolicy  // how to treat catalog duplicates
	Copy         bool             // copy (keep originals) instead of move
	ReserveBytes int64            // free-space headroom to keep on the destination
	// Route picks the target managed library for a file by its classified kind, for
	// media-typed multi-root import (a book to the audiobook root, a track to the
	// music root). When nil the file targets Library; when it returns nil for a kind
	// (none or several match) the file is quarantined.
	Route func(kind model.Kind) *model.Library
	// ProfileFor resolves the layout profile for a routed library, so a multi-root
	// import lays each file out under its own library's profile rather than one shared
	// profile. When nil, Profile is used for every file.
	ProfileFor func(lib *model.Library) organize.Profile
	// ForceKind overrides tag-based kind classification. Empty classifies from tags
	// (audiobook -> book, else track); the acquired single-file path forces a kind.
	ForceKind model.Kind
	// Acquisition, when set, records origin provenance on each imported item.
	Acquisition *model.AcquisitionInput
}

// Plan is a reviewable set of import actions.
type Plan struct {
	Source      string
	Library     *model.Library
	Profile     string
	Copy        bool
	DupPolicy   model.DupPolicy
	Reserve     int64
	TotalBytes  int64 // bytes the importable actions would bring in
	Actions     []Action
	Acquisition *model.AcquisitionInput // recorded on each imported item when set
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
	plan := &Plan{
		Source: req.Source, Library: req.Library, Profile: req.Profile.Name,
		Copy: req.Copy, DupPolicy: req.DupPolicy, Reserve: req.ReserveBytes,
		Acquisition: req.Acquisition,
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
		plan.Actions = append(plan.Actions, s.classify(ctx, req, path, claims))
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

// PlanFile plans a single acquired file (not a folder) into the request's target
// library, forcing kind. It is the ImportAcquired path for tracks and books: the file
// is classified against the same duplicate and collision rules as a folder import,
// and Execute records the request's acquisition provenance. The caller has already
// chosen the library, so this targets req.Library directly.
func (s *Service) PlanFile(ctx context.Context, req Request, path string, kind model.Kind) (*Plan, error) {
	const op = "inbox.PlanFile"
	if req.Library == nil || req.Library.Mode != model.ModeManaged {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "import target must be a managed library")
	}
	if !isAudio(path) {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "not a recognized audio file: "+path)
	}
	if req.DupPolicy == "" {
		req.DupPolicy = model.DupSkip
	}
	if !req.DupPolicy.Valid() {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "invalid duplicate policy: "+string(req.DupPolicy))
	}
	req.ForceKind = kind
	plan := &Plan{
		Source: path, Library: req.Library, Profile: req.Profile.Name,
		Copy: req.Copy, DupPolicy: req.DupPolicy, Reserve: req.ReserveBytes,
		Acquisition: req.Acquisition,
	}
	claims := &batchClaims{dst: map[string]bool{}, essence: map[string]bool{}}
	a := s.classify(ctx, req, path, claims)
	plan.Actions = append(plan.Actions, a)
	if a.Outcome == OutcomeImport {
		plan.TotalBytes += a.Size
	}
	return plan, nil
}

// resolveLibrary picks the managed library a file targets. When a Route is set it is
// authoritative: a nil result means the kind has no unambiguous managed library (none
// matches, or several do), so the file is quarantined rather than silently sent to the
// default library. With no Route, every file targets the request's default Library.
func resolveLibrary(req Request, kind model.Kind) *model.Library {
	if req.Route != nil {
		return req.Route(kind)
	}
	return req.Library
}

// batchClaims tracks the destinations and essences already claimed by earlier
// actions in one import plan.
type batchClaims struct {
	dst     map[string]bool
	essence map[string]bool
}

// classify decides one file's outcome and, for an import, its media kind, target
// library, and destination.
func (s *Service) classify(ctx context.Context, req Request, path string, claims *batchClaims) Action {
	a := Action{Src: path, Size: onDiskSize(path)}

	fm, err := s.reader.Read(ctx, path)
	if err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "unreadable: "+err.Error()
		return a
	}
	// Determine the media kind (forced for an acquired file, else tag-classified) and
	// route to the matching managed library, so a book lands in the audiobook root and
	// a track in the music root.
	kind := req.ForceKind
	if kind == "" {
		kind = classifyKind(fm.Tags)
	}
	a.Kind = kind
	lib := resolveLibrary(req, kind)
	if lib == nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "no unambiguous managed library for kind "+string(kind)
		return a
	}
	a.Library = lib
	root := string(lib.Root)

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

	// Render the destination under the routed library's own profile (multi-root import
	// may target several libraries with different profiles), falling back to the shared
	// request profile.
	prof := req.Profile
	if req.ProfileFor != nil {
		prof = req.ProfileFor(lib)
	}
	rel, err := organize.RenderRelPath(prof, acquiredItemView(fm.Tags, path, kind))
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

// Execute applies a plan: a free-space preflight first, then each importable file is
// placed and cataloged, and the import batch is recorded for later review. A per-file
// failure is tallied and does not abort the run.
func (s *Service) Execute(ctx context.Context, plan *Plan) (*Report, error) {
	const op = "inbox.Execute"
	// A plan can span managed roots (a book to the audiobook root, a track to the
	// music root), so preflight each distinct destination volume for the bytes it
	// receives rather than the whole plan against one root.
	if err := preflightPlan(plan); err != nil {
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

// importOne places one file in the managed tree, catalogs it under its target
// library, records any acquisition provenance, and carries its sidecars in alongside
// it. It returns the number of sidecars placed.
func (s *Service) importOne(ctx context.Context, plan *Plan, a *Action) (int, error) {
	if err := os.MkdirAll(pathx.Long(filepath.Dir(a.Dst)), 0o755); err != nil {
		return 0, waxerr.Wrap(waxerr.CodeIO, "inbox.import", err)
	}
	if err := placeFile(a.Src, a.Dst, plan.Copy); err != nil {
		return 0, err
	}
	lib := a.Library
	if lib == nil {
		lib = plan.Library
	}
	if _, err := s.cataloger.ScanFileAs(ctx, lib, a.Dst, a.Kind); err != nil {
		return 0, err
	}
	// Record origin provenance on the item that now backs the placed file. This is
	// attribution only: a failure to record it must not fail an import whose audio
	// already landed and cataloged.
	if plan.Acquisition != nil {
		if err := s.store.PutAcquisitionForFile(ctx, []byte(a.Dst), *plan.Acquisition); err != nil {
			s.log.Warn("recording acquisition provenance", "dst", a.Dst, "err", err)
		}
	}
	return s.relocateSidecars(plan, a), nil
}

// preflightPlan refuses an import that would leave any destination volume below its
// reserve, summing each importable action's bytes against the library root it targets.
func preflightPlan(plan *Plan) error {
	byRoot := map[string]int64{}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Outcome != OutcomeImport {
			continue
		}
		lib := a.Library
		if lib == nil {
			lib = plan.Library
		}
		byRoot[string(lib.Root)] += a.Size
	}
	for root, need := range byRoot {
		if err := preflight(root, need, plan.Reserve); err != nil {
			return err
		}
	}
	// No importable actions but a reserve is set: still check the default root so an
	// empty import on a full disk is reported consistently with the prior behavior.
	if len(byRoot) == 0 && plan.Reserve > 0 && plan.Library != nil {
		return preflight(string(plan.Library.Root), 0, plan.Reserve)
	}
	return nil
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
// reserve. If the platform cannot report free space, the import proceeds.
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
