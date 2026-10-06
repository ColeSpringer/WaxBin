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
	PutAcquisitionForFile(ctx context.Context, path []byte, in model.AcquisitionInput) (model.PID, error)
	// BookByKey returns the book a library holds under an identity key, with its parts in
	// reading order, or nil.
	BookByKey(ctx context.Context, libraryID int64, key string) (*model.ItemView, []model.ItemFileRef, error)
	FileByPID(ctx context.Context, pid model.PID) (*model.File, error)
	// RespellFolder follows a folder a placement renamed to another spelling.
	RespellFolder(ctx context.Context, from, to string) (int, error)
	// ArtistNames returns the name the catalog keeps for each artist match key it holds,
	// and AlbumTitles the title of each album it holds under an identity key, so a staged
	// track lands under the spellings the catalog will file it by.
	ArtistNames(ctx context.Context, matchKeys []string) (map[string]string, error)
	AlbumTitles(ctx context.Context, keys []string) (map[string]string, error)
}

// Cataloger catalogs one file after it has been placed in the managed tree, honoring
// a forced media kind (empty classifies from tags), so an acquired book forced with
// --as book is cataloged as a book even when its tags do not say so, and the book the
// import's folder rule found a file with no album a part of.
type Cataloger interface {
	ScanFileWith(ctx context.Context, lib *model.Library, path string, opts scan.FileOptions) (*scan.Result, *model.ScanItemResult, error)
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
	Kind    model.Kind // classified/forced media kind (routes template + library)
	// KindForced says the request forced Kind. The cataloging scan then takes it too,
	// pinning it when the file's tags would say otherwise; a classified kind is left to
	// the scan's own rule at the destination.
	KindForced bool
	Library    *model.Library // target managed library for this file (kind-routed)
	Outcome    Outcome
	Reason     string
	// Album is the book the import's folder rule found a file whose tags name no album a
	// part of, which the cataloging scan joins it to (scan.FileOptions.Album).
	Album string
	// Book groups a book's staged files, which land together or not at all (HoldBooks);
	// Joined marks one the folder rule made a part of the book, and Alternate a copy or
	// another encoding of one of its parts.
	Book              string
	Joined, Alternate bool
	// Position is a book part's position in its book as the staged file gives it
	// (scan.PartPosition), which the cataloging scan keeps where the name the file is
	// placed under no longer states it.
	Position int
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
	// (none or several match) the file is quarantined, with the reason Route gives or
	// a default one.
	Route func(kind model.Kind) (*model.Library, string)
	// ProfileFor resolves the layout profile for a routed library, so a multi-root
	// import lays each file out under its own library's profile rather than one shared
	// profile. When nil, Profile is used for every file.
	ProfileFor func(lib *model.Library) organize.Profile
	// ForceKind overrides the kind rule (scan.EffectiveKind). Empty classifies by the
	// rule; the acquired single-file path forces a kind, which the cataloging scan pins
	// with a kind lock when the rule would say otherwise.
	ForceKind model.Kind
	// Acquisition, when set, records origin provenance on each imported item.
	Acquisition *model.AcquisitionInput
	// Inbox says Source is a configured inbox folder, which holds staged imports rather
	// than one book, so a file straight in it is in no book's folder.
	Inbox bool
	// Hold, when set, gives the reason a staged file stays where it is (a read-only
	// library holds it), or "".
	Hold func(src string) string
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
	// Inbox says Source is a configured inbox folder (Request.Inbox).
	Inbox bool
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
	// DirsPruned counts the staging folders the import emptied and removed.
	DirsPruned int
	Bytes      int64
	Failures   []Failure
	// Files says, for each file imported, the item it now backs and whether it joined an
	// existing one as an alternate (a DupAllow import of audio the catalog held).
	Files []FileOutcome
}

// FileOutcome is what importing one file did. ItemPID is empty for a cue rip, whose
// file backs a track per cue entry.
type FileOutcome struct {
	Path           string
	ItemPID        model.PID
	AttachedAsCopy bool
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
	if abs, err := filepath.Abs(req.Source); err == nil {
		req.Source = abs
	}
	plan := &Plan{
		Source: req.Source, Inbox: req.Inbox, Library: req.Library, Profile: req.Profile.Name,
		Copy: req.Copy, DupPolicy: req.DupPolicy, Reserve: req.ReserveBytes,
		Acquisition: req.Acquisition,
	}

	var files []*staged
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
		files = append(files, s.classify(ctx, req, path))
		return nil
	})
	if walkErr != nil {
		return nil, waxerr.FromContext(op, walkErr, waxerr.CodeIO)
	}
	s.joinFolders(req, files)
	spelling, err := s.spellings(ctx, files)
	if err != nil {
		return nil, err
	}
	if err := s.alignAlbums(ctx, req, files, spelling); err != nil {
		return nil, err
	}
	plan.Actions = s.settle(ctx, req, s.numberBooks(ctx, files, spelling))
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
	files := []*staged{s.classify(ctx, req, path)}
	s.joinFolders(req, files)
	spelling, err := s.spellings(ctx, files)
	if err != nil {
		return nil, err
	}
	if err := s.alignAlbums(ctx, req, files, spelling); err != nil {
		return nil, err
	}
	plan.Actions = s.settle(ctx, req, s.numberBooks(ctx, files, spelling))
	if a := plan.Actions[0]; a.Outcome == OutcomeImport {
		plan.TotalBytes += a.Size
	}
	return plan, nil
}

// resolveLibrary picks the managed library a file targets. When a Route is set it is
// authoritative: a nil result means the kind has no unambiguous managed library (none
// matches, or several do), so the file is quarantined rather than silently sent to the
// default library. With no Route, every file targets the request's default Library.
func resolveLibrary(req Request, kind model.Kind) (*model.Library, string) {
	if req.Route != nil {
		return req.Route(kind)
	}
	return req.Library, ""
}

// batchClaims tracks the destinations and essences already claimed by earlier
// actions in one import plan.
type batchClaims struct {
	dst     map[string]bool
	essence map[string]bool
}

// file is the staged file as model.OtherEncoding and model.CompareQuality read it.
func (s *staged) file() model.File {
	return model.File{EssenceHash: s.Essence, Codec: s.tags.Codec, SampleRate: s.tags.SampleRate,
		BitDepth: s.tags.BitDepth, Bitrate: s.tags.Bitrate, DurationMS: s.tags.DurationMS}
}

// staged is a file classified for import before its destination is settled: its tags as
// read and as its destination renders from them and, for a book, the book's key and the
// file's position in it.
type staged struct {
	Action
	raw, tags   model.Tags
	prof        organize.Profile
	book        string
	place       int
	placeStated bool
	rel         string // the rendered destination before any part number
}

// classify decides one file's media kind, target library and rendered destination, or
// that it is a catalog duplicate or cannot be imported.
func (s *Service) classify(ctx context.Context, req Request, path string) *staged {
	st := &staged{Action: Action{Src: path, Size: onDiskSize(path)}}
	a := &st.Action

	fm, err := s.reader.Read(ctx, path)
	if err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "unreadable: "+err.Error()
		return st
	}
	// Determine the media kind (forced for an acquired file, else by the scan's rule, with
	// the target library when no route picks one by kind) and route to the matching managed
	// library, so a book lands in the audiobook root and a track in the music root.
	kind := req.ForceKind
	if kind == "" {
		var target *model.Library
		if req.Route == nil {
			target = req.Library
		}
		kind = scan.EffectiveKind(&fm.Tags, target, "", "")
	}
	a.Kind, a.KindForced = kind, req.ForceKind != ""
	st.raw = fm.Tags
	// A file renders under the names the scan at its destination will catalog it with.
	root := stagingRoot(req.Source, path)
	if kind == model.KindTrack {
		meta.DisplayFallbacks(&fm.Tags, root, path, fm.TitleFromName)
	} else {
		meta.PromoteBookFields(&fm.Tags)
		st.book = scan.BookIdentityKey(fm.Tags)
		st.place, st.placeStated = scan.PartPosition(&fm.Tags, root, path)
		a.Position = st.place
	}
	st.tags = fm.Tags
	lib, reason := resolveLibrary(req, kind)
	if lib == nil {
		if reason == "" {
			reason = "no unambiguous managed library for kind " + string(kind)
		}
		a.Outcome, a.Reason = OutcomeQuarantine, reason
		return st
	}
	a.Library = lib

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
			return st
		} else if !waxerr.Is(err, waxerr.CodeNotFound) {
			a.Outcome, a.Reason = OutcomeQuarantine, "dedup check failed: "+err.Error()
			return st
		}
	}
	if req.Hold != nil {
		if reason := req.Hold(path); reason != "" {
			a.Outcome, a.Reason = OutcomeQuarantine, reason
			return st
		}
	}

	// Render the destination under the routed library's own profile (multi-root import
	// may target several libraries with different profiles), falling back to the shared
	// request profile.
	prof := req.Profile
	if req.ProfileFor != nil {
		prof = req.ProfileFor(lib)
	}
	st.prof = prof
	rel, err := organize.RenderRelPath(prof, acquiredItemView(fm.Tags, path, kind))
	if err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "no destination: "+err.Error()
		return st
	}
	st.rel, a.RelDst, a.Outcome = rel, rel, OutcomeImport
	return st
}

// settle decides each staged file's destination in the order the files will land: a
// second staged copy of audio another file claimed is a duplicate under DupSkip, and a
// destination another staged file claimed, one already on disk, or one a cataloged file
// holds under another case is quarantined.
func (s *Service) settle(ctx context.Context, req Request, files []*staged) []Action {
	// Claims made within this import: destinations (so two staged files never
	// target the same path) and audio essences (so under DupSkip two staged copies
	// of the same recording don't both import even when their tags, and thus their
	// destinations, differ).
	claims := &batchClaims{dst: map[string]bool{}, essence: map[string]bool{}}
	out := make([]Action, len(files))
	for i, f := range files {
		a := f.Action
		if a.Outcome == OutcomeImport {
			s.claim(ctx, req, &a, claims)
		}
		if a.Outcome != OutcomeImport {
			a.Dst, a.RelDst = "", ""
		}
		out[i] = a
	}
	HoldBooks(out)
	return out
}

// claim takes an importable action's destination and audio for this import, or turns it
// into a duplicate or a quarantine.
func (s *Service) claim(ctx context.Context, req Request, a *Action, claims *batchClaims) {
	// A second staged copy of the same recording (possibly tagged differently, so a
	// different destination) is still a duplicate under skip.
	if a.Essence != "" && req.DupPolicy == model.DupSkip && claims.essence[a.Essence] {
		a.Outcome, a.Reason = OutcomeDuplicate, "duplicate of another file in this import"
		return
	}
	dst := filepath.Join(string(a.Library.Root), a.RelDst)
	key := pathx.CollisionKey(dst)
	if claims.dst[key] {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination already claimed by another staged file"
		return
	}
	if pathExists(dst) {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination already exists in the library"
		return
	}
	// A cataloged file whose path differs only by case would coexist here on Linux
	// but collide on a case-insensitive filesystem, so refuse it for portability.
	if exists, err := s.store.DisplayPathExistsFold(ctx, dst); err != nil {
		a.Outcome, a.Reason = OutcomeQuarantine, "collision check failed: "+err.Error()
		return
	} else if exists {
		a.Outcome, a.Reason = OutcomeQuarantine, "destination case-collides with a cataloged file"
		return
	}
	claims.dst[key] = true
	if a.Essence != "" {
		claims.essence[a.Essence] = true
	}
	a.Dst = dst
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

	// The staging directories' covers are planned once the audio has landed, so a folder
	// routed to more than one library leaves a cover in each destination. The album
	// folders are the source and the folders below it, or only those below a configured
	// inbox, which holds staged imports; a file handed over alone carries nothing from its
	// folder. It runs on the cancellation path too, before the batch is finalized: the audio
	// already landed, so a staging directory emptied before the cancel would otherwise
	// keep a cover with nothing to hold it. The staging folders the import emptied are
	// then removed; a copier still writing into one has a file there, which keeps it.
	var placed []organize.SidecarMove
	placeCovers := func() {
		root := filepath.Dir(plan.Source)
		if plan.Inbox {
			root = plan.Source
		}
		rep.Sidecars += s.placeCovers(organize.CoverMoves(placed, organize.PruneOptions(root, nil)), plan.Copy)
		rep.DirsPruned += s.pruneStaging(plan.Source, placed)
	}
	sib := organize.NewSiblings()
	spellers := map[int64]*fsx.Speller{}
	speller := func(lib *model.Library) *fsx.Speller {
		if spellers[lib.ID] == nil {
			spellers[lib.ID] = fsx.NewSpeller(string(lib.Root), func(from, to string) error {
				_, err := s.store.RespellFolder(context.WithoutCancel(ctx), from, to)
				return err
			})
		}
		return spellers[lib.ID]
	}
	for i := range plan.Actions {
		if ctx.Err() != nil {
			placeCovers()
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
			lib := a.Library
			if lib == nil {
				lib = plan.Library
			}
			sidecars, outcome, err := s.importOne(ctx, plan, a, lib, speller(lib), sib)
			if err != nil {
				rep.Errored++
				rep.Failures = append(rep.Failures, Failure{Src: a.Src, Err: err.Error()})
				s.log.Warn("import failed", "src", a.Src, "dst", a.Dst, "err", err)
				continue
			}
			rep.Files = append(rep.Files, outcome)
			rep.Imported++
			rep.Bytes += a.Size
			rep.Sidecars += sidecars
			placed = append(placed, organize.SidecarMove{Src: a.Src, Dst: a.Dst})
		}
	}
	placeCovers()
	s.finalize(ctx, batch, rep, model.ImportDone)
	return rep, nil
}

// placeCovers carries the staging directories' covers into the managed tree. A
// copy-mode import never removes a staging cover, so the plan's copy flag overrides the
// move the batch plan would otherwise make.
func (s *Service) placeCovers(moves []organize.CoverMove, copyMode bool) int {
	moved := 0
	for _, m := range moves {
		switch err := fsx.MoveOrCopy(m.Src, m.Dst, m.Copy || copyMode); {
		case err == nil:
			moved++
		case errors.Is(err, fsx.ErrExist):
			s.log.Warn("inbox cover not placed: destination exists", "src", m.Src, "dst", m.Dst)
		default:
			s.log.Warn("inbox cover placement failed", "src", m.Src, "dst", m.Dst, "err", err)
		}
	}
	return moved
}

// pruneStaging removes the staging folders the import emptied, and any folder above them
// then empty, below the staging root only; a companion left in one follows its audio as
// organize.FolderDisposal says.
func (s *Service) pruneStaging(root string, placed []organize.SidecarMove) int {
	dirs := make([]string, 0, len(placed))
	for _, m := range placed {
		dirs = append(dirs, filepath.Dir(m.Src))
	}
	dispose, undo := organize.FolderDisposal(placed, func(string) string { return root })
	return fsx.PruneAll(dirs, func(string) fsx.PruneOptions {
		opts := organize.PruneOptions(root, dispose)
		opts.Undo = undo
		return opts
	}, func(dir string, err error) { s.log.Warn("pruning an emptied staging folder", "dir", dir, "err", err) })
}

// importOne places one file in the managed tree, catalogs it under its target
// library, records any acquisition provenance, and carries its sidecars in alongside
// it. It returns the number of sidecars placed and what cataloging the file did.
func (s *Service) importOne(ctx context.Context, plan *Plan, a *Action, lib *model.Library, sp *fsx.Speller, sib *organize.Siblings) (int, FileOutcome, error) {
	outcome := FileOutcome{Path: a.Dst}
	if err := os.MkdirAll(pathx.Long(filepath.Dir(a.Dst)), 0o755); err != nil {
		return 0, outcome, waxerr.Wrap(waxerr.CodeIO, "inbox.import", err)
	}
	if err := placeFile(sp, a.Src, a.Dst, plan.Copy); err != nil {
		return 0, outcome, err
	}
	var forced model.Kind
	if a.KindForced {
		forced = a.Kind
	}
	opts := scan.FileOptions{Kind: forced, Album: a.Album}
	if a.Kind == model.KindBook {
		opts.Position = &a.Position
	}
	_, out, err := s.cataloger.ScanFileWith(ctx, lib, a.Dst, opts)
	if err != nil {
		return 0, outcome, err
	}
	if out != nil {
		outcome.ItemPID, outcome.AttachedAsCopy = out.ItemPID, out.AttachedAsCopy
	}
	// Record origin provenance on the item that now backs the placed file. This is
	// attribution only: a failure to record it must not fail an import whose audio
	// already landed and cataloged. A file that joined an existing item as a copy
	// records none, since that item was acquired before it.
	if plan.Acquisition != nil && !outcome.AttachedAsCopy {
		copyOf, err := s.store.PutAcquisitionForFile(ctx, []byte(a.Dst), *plan.Acquisition)
		if err != nil {
			s.log.Warn("recording acquisition provenance", "dst", a.Dst, "err", err)
		}
		if copyOf != "" {
			outcome.ItemPID, outcome.AttachedAsCopy = copyOf, true
		}
	}
	if !plan.Copy {
		sib.Left(a.Src)
	}
	return s.relocateSidecars(plan, a, sib), outcome, nil
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

// relocateSidecars carries an imported file's own companions (same-basename lyrics and
// art) into the managed tree, moving or copying them to match the audio. Sidecars are
// not cataloged, so a conflict or failure is logged and skipped, never fatal (the audio
// is already imported). The same discovery as organize is used, so import and organize
// keep one sidecar set; the directory's own cover is carried by placeCovers once the
// batch is done.
func (s *Service) relocateSidecars(plan *Plan, a *Action, sib *organize.Siblings) int {
	moved := 0
	for _, m := range organize.SidecarMoves(a.Src, a.Dst, sib) {
		switch err := fsx.MoveOrCopy(m.Src, m.Dst, plan.Copy || m.Shared); {
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
