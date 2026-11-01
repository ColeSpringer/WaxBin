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
	// FilesByEssence returns the cataloged files holding an audio essence, and
	// FileOccupants those at a display path folded for case, each saying whether it is on
	// disk and which item it backs.
	FilesByEssence(ctx context.Context, essence string) ([]model.FileOccupant, error)
	FileOccupants(ctx context.Context, displayPath string) ([]model.FileOccupant, error)
	CreateImportBatch(ctx context.Context, b *model.ImportBatch) error
	UpdateImportBatch(ctx context.Context, b *model.ImportBatch) error
	PutAcquisitionForFile(ctx context.Context, path []byte, in model.AcquisitionInput, made []model.PID) (model.PID, error)
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

// HoldCode says why a staged file does not import, beside the reason that says it in
// words. A duplicate carries one too.
type HoldCode string

const (
	HoldUnreadable       HoldCode = "unreadable"         // its tags or audio could not be read
	HoldNoLibrary        HoldCode = "no-library"         // no one managed library takes its kind
	HoldDedupFailed      HoldCode = "dedup-failed"       // the duplicate check could not run
	HoldReadOnlySource   HoldCode = "read-only-source"   // a move would take it out of a read-only library
	HoldNoDestination    HoldCode = "no-destination"     // the layout renders no path for it
	HoldDuplicateCatalog HoldCode = "duplicate"          // a cataloged file on disk holds its audio
	HoldDuplicateBatch   HoldCode = "duplicate-in-batch" // an earlier file of the import holds its audio
	HoldClaimed          HoldCode = "claimed"            // an earlier file of the import takes its destination
	HoldExists           HoldCode = "exists"             // a file stands at its destination
	HoldOccupiedCase     HoldCode = "occupied-case"      // a file named apart only by case or Unicode form stands there
	HoldCataloged        HoldCode = "cataloged"          // the catalog holds its destination for a file not on disk
	HoldCollisionCheck   HoldCode = "collision-check"    // its destination could not be looked at
	HoldBook             HoldCode = "book-held"          // its book's own part will not land
	HoldAlbum            HoldCode = "album-held"         // a track of its album will not land, and the request holds units
	HoldReadOnlyLibrary  HoldCode = "read-only-library"  // its library was flagged read-only since the plan
	HoldLandedSince      HoldCode = "landed-since"       // its audio reached the catalog since the plan
)

// Occupant is what turned an action away: the file at Path (with the catalog's file and
// item when it holds one), or a staged file of the same import, StagedSrc.
type Occupant struct {
	Path      string    `json:"path,omitempty"`
	FilePID   model.PID `json:"filePid,omitempty"`
	ItemPID   model.PID `json:"itemPid,omitempty"`
	StagedSrc string    `json:"stagedSrc,omitempty"`
}

// Action is one planned import. A quarantine keeps the destination it would have taken
// when one was rendered; a duplicate, and a file held with its book or album (HoldUnits),
// have none.
type Action struct {
	Src     string
	Dst     string // destination under the library
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
	// Code says why a duplicate or a quarantine does not import, and Occupant names what
	// holds its audio or its destination, when something does.
	Code     HoldCode
	Occupant *Occupant
	// Album is the book the import's folder rule found a file whose tags name no album a
	// part of, which the cataloging scan joins it to (scan.FileOptions.Album).
	Album string
	// Unit groups staged files that land together or not at all (HoldUnits): a book's
	// files always, an album's tracks when the request holds units. Joined marks a file
	// the folder rule made a part of a book, and Alternate a copy or another encoding of
	// one of a book's parts.
	Unit              string
	Joined, Alternate bool
	// Position is a book part's position in its book as the staged file gives it
	// (scan.PartPosition), which the cataloging scan keeps where the name the file is
	// placed under no longer states it.
	Position int
	// Restores names the item whose gone file held this audio, which the import brings
	// back: no file of the audio is on disk, so it is no duplicate under either policy.
	Restores model.PID
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
	// HoldUnits keeps an album's tracks where they are when one of them is quarantined,
	// before or after it has a destination, as a book's files always are when one of its
	// parts will not land, and every file with a unit when a refused file's tags cannot be
	// read (planUnits).
	HoldUnits bool
	// Acquired says the files were handed over by a host (ImportAcquired) rather than
	// staged in a folder the import owns: their folders name no book, and the import
	// carries nothing from them and removes none.
	Acquired bool
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
	// Inbox says Source is a configured inbox folder (Request.Inbox), and Acquired that
	// the files were handed over (Request.Acquired), Source being the folder they share.
	Inbox, Acquired bool
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

// Report summarizes an applied import. Holds names each file counted in Duplicates and
// Quarantined, the plan's refusals and those the apply's recheck made alike.
type Report struct {
	BatchPID    model.PID
	Imported    int
	Duplicates  int
	Quarantined int
	Holds       []Held
	Errored     int
	Sidecars    int // companion files (lyrics/art/...) carried in with the audio
	// DirsPruned counts the staging folders the import emptied and removed.
	DirsPruned int
	Bytes      int64
	Failures   []Failure
	// Files says what importing each file did (FileOutcome).
	Files []FileOutcome
}

// Held is a file an applied import did not bring in, with why and what held it.
type Held struct {
	Src      string    `json:"src"`
	Dst      string    `json:"dst,omitempty"`
	Outcome  Outcome   `json:"outcome"`
	Code     HoldCode  `json:"code"`
	Reason   string    `json:"reason"`
	Occupant *Occupant `json:"occupant,omitempty"`
}

// FileOutcome is what importing one file did: the item it backs (empty for a cue rip, whose
// file backs a track per cue entry), what cataloging it did by the scan's own rule
// (scan.Verdict: a new item, a part or encoding joining a standing one, a copy of it, a
// missing one back), whether it took back a gone file's row by its audio (Relinked), the
// items it left with no file, each with the item that took it in (Absorbed), and the
// alternates it promoted on an item it left, which the import read again. AttachedAsCopy
// marks an alternate.
type FileOutcome struct {
	Path           string
	ItemPID        model.PID
	AttachedAsCopy bool
	Verdict        scan.Verdict
	Relinked       bool
	Absorbed       map[model.PID]model.PID
	Promoted       []model.PromotedFile
	// Restores is the item the planned import named (Action.Restores) whose gone file this
	// one brought back, set only when the file landed on that item, its verdict saying what
	// else the put did.
	Restores model.PID
}

// Failure records one import that could not be applied. Dst names where the file landed
// when it failed after the move, and is empty when it never left Src.
type Failure struct {
	Src string `json:"src"`
	Dst string `json:"dst,omitempty"`
	Err string `json:"error"`
}

// Plan walks the source folder and classifies every audio file: importable (with
// its rendered destination), a catalog duplicate, or quarantined (unreadable, no
// destination, or a destination that would collide). It does not touch disk.
func (s *Service) Plan(ctx context.Context, req Request) (*Plan, error) {
	const op = "inbox.Plan"
	if err := checkRequest(op, &req); err != nil {
		return nil, err
	}
	if abs, err := filepath.Abs(req.Source); err == nil {
		req.Source = abs
	}
	var paths []string
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
		if isAudio(path) {
			paths = append(paths, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, waxerr.FromContext(op, walkErr, waxerr.CodeIO)
	}
	return s.PlanPaths(ctx, req, paths, req.ForceKind)
}

// PlanFile plans a single acquired file (not a folder) into the request's target
// library, forcing kind. It is the ImportAcquired path for tracks and books: the file
// is classified against the same duplicate and collision rules as a folder import,
// and Execute records the request's acquisition provenance. The caller has already
// chosen the library, so this targets req.Library directly.
func (s *Service) PlanFile(ctx context.Context, req Request, path string, kind model.Kind) (*Plan, error) {
	req.Acquired = true
	return s.PlanPaths(ctx, req, []string{path}, kind)
}

// PlanPaths plans the files at paths as one import, forcing kind unless it is empty: the
// body Plan and PlanFile share, and the way a host plans a unit it acquired (an album's
// tracks, a book's parts) so its files see each other. The folder rule, the artist and
// album spellings, the book numbering and the claims on destinations and audio each read
// every file of the plan at once. Source is req.Source, or for handed-over files
// (req.Acquired) the one file or the deepest folder holding them all. A path that is not
// audio, or is named twice, is refused.
func (s *Service) PlanPaths(ctx context.Context, req Request, paths []string, kind model.Kind) (*Plan, error) {
	const op = "inbox.PlanPaths"
	if err := checkRequest(op, &req); err != nil {
		return nil, err
	}
	if kind != "" {
		req.ForceKind = kind
	}
	if req.Acquired {
		req.Source, req.Inbox = "", false
	}
	plan := &Plan{
		Source: req.Source, Inbox: req.Inbox, Acquired: req.Acquired, Library: req.Library, Profile: req.Profile.Name,
		Copy: req.Copy, DupPolicy: req.DupPolicy, Reserve: req.ReserveBytes, Acquisition: req.Acquisition,
	}
	files := make([]*staged, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, waxerr.FromContext(op, ctx.Err(), waxerr.CodeIO)
		}
		if !isAudio(path) {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "not a recognized audio file: "+path)
		}
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		if seen[path] {
			return nil, waxerr.New(waxerr.CodeInvalid, op, "a file is named twice: "+path)
		}
		seen[path] = true
		files = append(files, s.classify(ctx, req, path))
	}
	if req.Acquired {
		plan.Source = sharedFolder(files)
		if len(files) == 1 {
			plan.Source = files[0].Src
		}
	}
	s.joinFolders(req, files)
	spelling, err := s.spellings(ctx, files)
	if err != nil {
		return nil, err
	}
	if err := s.alignAlbums(ctx, req, files, spelling); err != nil {
		return nil, err
	}
	numbered := s.numberBooks(ctx, files, spelling)
	if req.HoldUnits {
		s.planUnits(ctx, numbered)
	}
	plan.Actions = s.settle(ctx, req, numbered)
	for i := range plan.Actions {
		if plan.Actions[i].Outcome == OutcomeImport {
			plan.TotalBytes += plan.Actions[i].Size
		}
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

// Refuse turns an action away under outcome for code, naming what holds it. A refused
// action restores nothing, and it keeps the destination it rendered only when held there
// or by its library or source (keepsDestination), so the plan still says where it would
// have landed; a duplicate, a file held with its unit, and one with no library or no
// destination keep none.
func (a *Action) Refuse(outcome Outcome, code HoldCode, reason string, occ *Occupant) {
	a.Outcome, a.Code, a.Reason, a.Occupant, a.Restores = outcome, code, reason, occ, ""
	if !code.keepsDestination() {
		a.Dst, a.RelDst = "", ""
	}
}

// keepsDestination says whether an action held for c keeps the destination it rendered:
// held by what stands there or the check of it, or by a read-only library or source.
func (c HoldCode) keepsDestination() bool {
	switch c {
	case HoldClaimed, HoldExists, HoldOccupiedCase, HoldCataloged, HoldCollisionCheck, HoldReadOnlySource, HoldReadOnlyLibrary:
		return true
	}
	return false
}

// batchClaims tracks the destinations and essences earlier actions of one import plan
// claimed, each with the staged file that claimed it, and lists destination folders once.
type batchClaims struct {
	dst     map[string]string
	essence map[string]string
	ls      *fsx.Lister
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
	if info, err := os.Stat(pathx.Long(path)); err != nil || !info.Mode().IsRegular() {
		reason := "unreadable: not a file"
		if err != nil {
			reason = "unreadable: " + err.Error()
		}
		a.Refuse(OutcomeQuarantine, HoldUnreadable, reason, nil)
		return st
	}

	fm, err := s.reader.Read(ctx, path)
	if err != nil {
		a.Refuse(OutcomeQuarantine, HoldUnreadable, "unreadable: "+err.Error(), nil)
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
		a.Refuse(OutcomeQuarantine, HoldNoLibrary, reason, nil)
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

	// A cataloged file of the audio on disk makes this a duplicate under DupSkip; one that
	// is gone is the file of an item the import restores, under either policy.
	if essence != "" {
		occ, err := s.store.FilesByEssence(ctx, essence)
		if err != nil {
			a.Refuse(OutcomeQuarantine, HoldDedupFailed, "dedup check failed: "+err.Error(), nil)
			return st
		}
		held, gone := heldAudio(occ, kind, lib.ID)
		switch {
		case held != nil && req.DupPolicy == model.DupSkip:
			a.Refuse(OutcomeDuplicate, HoldDuplicateCatalog, "audio already in the catalog", occupantOf(*held))
			return st
		case held == nil && gone != nil:
			a.Restores = gone.ItemPID
		}
	}
	if req.Hold != nil {
		if reason := req.Hold(path); reason != "" {
			a.Refuse(OutcomeQuarantine, HoldReadOnlySource, reason, nil)
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
		a.Refuse(OutcomeQuarantine, HoldNoDestination, "no destination: "+err.Error(), nil)
		return st
	}
	st.rel, a.RelDst, a.Outcome = rel, rel, OutcomeImport
	return st
}

// settle decides each staged file's destination in the order the files will land: a
// second staged copy of audio another file claimed is a duplicate under DupSkip, and a
// destination another staged file claimed, one a file stands at or beside under another
// case, or one the catalog holds for other audio is quarantined, keeping the destination.
func (s *Service) settle(ctx context.Context, req Request, files []*staged) []Action {
	// Claims made within this import: destinations (so two staged files never
	// target the same path) and audio essences (so under DupSkip two staged copies
	// of the same recording don't both import even when their tags, and thus their
	// destinations, differ).
	claims := &batchClaims{dst: map[string]string{}, essence: map[string]string{}, ls: fsx.NewLister()}
	out := make([]Action, len(files))
	for i, f := range files {
		a := f.Action
		if a.Outcome == OutcomeImport {
			s.claim(ctx, req, &a, claims)
		}
		out[i] = a
	}
	HoldUnits(out)
	return out
}

// claim takes an importable action's destination and audio for this import, or turns it
// into a duplicate or a quarantine.
func (s *Service) claim(ctx context.Context, req Request, a *Action, claims *batchClaims) {
	// A second staged copy of the same recording (possibly tagged differently, so a
	// different destination) is still a duplicate under skip.
	if first, ok := claims.essence[a.Essence]; ok && a.Essence != "" && req.DupPolicy == model.DupSkip {
		a.Refuse(OutcomeDuplicate, HoldDuplicateBatch, "duplicate of another file in this import", &Occupant{StagedSrc: first})
		return
	}
	a.Dst = filepath.Join(string(a.Library.Root), a.RelDst)
	key := pathx.CollisionKey(a.Dst)
	if first, ok := claims.dst[key]; ok {
		a.Refuse(OutcomeQuarantine, HoldClaimed, "destination already claimed by another staged file", &Occupant{Path: a.Dst, StagedSrc: first})
		return
	}
	occ, err := s.store.FileOccupants(ctx, a.Dst)
	if err != nil {
		a.Refuse(OutcomeQuarantine, HoldCollisionCheck, "collision check failed: "+err.Error(), nil)
		return
	}
	if code, reason, o := DestinationHold(claims.ls, a, occ); code != "" {
		a.Refuse(OutcomeQuarantine, code, reason, o)
		return
	}
	claims.dst[key] = a.Src
	if a.Essence != "" {
		claims.essence[a.Essence] = a.Src
	}
}

// restored is the item an import planned to restore (Action.Restores) when its file landed
// on it: the item the put names, or for a cue rip's file, whose put names no one item, an
// item the file backs now.
func (s *Service) restored(ctx context.Context, a *Action, out *model.ScanItemResult) model.PID {
	if a.Restores == "" {
		return ""
	}
	if out.ItemPID != "" {
		if out.ItemPID == a.Restores {
			return a.Restores
		}
		return ""
	}
	occ, err := s.store.FileOccupants(ctx, a.Dst)
	if err != nil {
		s.log.Warn("import: reading what a restored rip backs", "dst", a.Dst, "err", err)
		return ""
	}
	for _, o := range occ {
		if o.ItemPID == a.Restores {
			return a.Restores
		}
	}
	return ""
}

// DestinationHold says what holds an action's destination, given the cataloged files at
// it folded for case (Store.FileOccupants): a file standing there or beside it under
// another case, which its folder's listing shows on any filesystem, or a cataloged file
// whose path differs only by case, which would coexist here on Linux but collide on a
// case-insensitive filesystem. A gone file of the staged file's own audio is the row the
// import takes back, while another item's gone file still holds its path, which a
// different file landing there would take over. A plan and its apply check alike by it.
func DestinationHold(ls *fsx.Lister, a *Action, occ []model.FileOccupant) (HoldCode, string, *Occupant) {
	if fsx.SameFile(a.Src, a.Dst) {
		return HoldExists, "the file is already at its destination", occupantAt(a.Dst, occ)
	}
	path, exact, err := ls.Occupant(a.Src, a.Dst)
	switch {
	case err != nil:
		return HoldCollisionCheck, "destination cannot be looked at: " + err.Error(), &Occupant{Path: a.Dst}
	case path != "" && exact:
		return HoldExists, "destination already exists in the library", occupantAt(path, occ)
	case path != "":
		return HoldOccupiedCase, "destination differs only by case from " + path, occupantAt(path, occ)
	}
	for _, o := range occ {
		switch {
		case !o.OnDisk && a.Essence != "" && o.Essence == a.Essence:
		case o.OnDisk:
			return HoldOccupiedCase, "destination case-collides with the cataloged file " + o.DisplayPath, occupantOf(o)
		default:
			return HoldCataloged, "destination is held in the catalog by " + heldBy(o) + ", which is not on disk", occupantOf(o)
		}
	}
	return "", "", nil
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
	// inbox, which holds staged imports; files a host handed over carry nothing from their
	// folders. It runs on the cancellation path too, before the batch is finalized: the audio
	// already landed, so a staging directory emptied before the cancel would otherwise
	// keep a cover with nothing to hold it. The staging folders the import emptied are
	// then removed; a copier still writing into one has a file there, which keeps it.
	var placed []organize.SidecarMove
	placeCovers := func() {
		if plan.Acquired {
			return
		}
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
		case OutcomeDuplicate, OutcomeQuarantine:
			if a.Outcome == OutcomeDuplicate {
				rep.Duplicates++
			} else {
				rep.Quarantined++
			}
			rep.Holds = append(rep.Holds, Held{Src: a.Src, Dst: a.Dst, Outcome: a.Outcome, Code: a.Code, Reason: a.Reason, Occupant: a.Occupant})
		case OutcomeImport:
			lib := a.Library
			if lib == nil {
				lib = plan.Library
			}
			sidecars, outcome, err := s.importOne(ctx, plan, a, lib, speller(lib), sib)
			if err != nil {
				rep.Errored++
				rep.Failures = append(rep.Failures, Failure{Src: a.Src, Dst: outcome.Path, Err: err.Error()})
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
	d := organize.FolderDisposal(placed, func(string) string { return root })
	return fsx.PruneAll(dirs, func(string) fsx.PruneOptions {
		opts := organize.PruneOptions(root, d.Dispose)
		opts.Undo = d.Undo
		return opts
	}, func(dir string, err error) { s.log.Warn("pruning an emptied staging folder", "dir", dir, "err", err) })
}

// importOne places one file in the managed tree, catalogs it under its target
// library, records any acquisition provenance, and carries its sidecars in alongside
// it. It returns the number of sidecars placed and what cataloging the file did, whose
// Path is set once the file has landed, a failure after the move included.
func (s *Service) importOne(ctx context.Context, plan *Plan, a *Action, lib *model.Library, sp *fsx.Speller, sib *organize.Siblings) (int, FileOutcome, error) {
	var outcome FileOutcome
	if err := os.MkdirAll(pathx.Long(filepath.Dir(a.Dst)), 0o755); err != nil {
		return 0, outcome, waxerr.Wrap(waxerr.CodeIO, "inbox.import", err)
	}
	if err := placeFile(sp, a.Src, a.Dst, plan.Copy); err != nil {
		return 0, outcome, err
	}
	outcome.Path = a.Dst
	var forced model.Kind
	if a.KindForced {
		forced = a.Kind
	}
	opts := scan.FileOptions{Kind: forced, Album: a.Album}
	if a.Kind == model.KindBook {
		opts.Position = &a.Position
	}
	res, out, err := s.cataloger.ScanFileWith(ctx, lib, a.Dst, opts)
	if err != nil {
		return 0, outcome, err
	}
	var made []model.PID
	if out != nil {
		outcome.ItemPID, outcome.AttachedAsCopy = out.ItemPID, out.AttachedAsCopy
		outcome.Verdict, outcome.Relinked, outcome.Promoted = scan.VerdictOf(out), out.Relinked, out.Promoted
		outcome.Restores = s.restored(ctx, a, out)
		made = out.Made
	}
	// The scan's own result settles every fold it made, a promoted alternate's re-read
	// included.
	if res != nil {
		outcome.Absorbed = res.Absorbed
	}
	// Record origin provenance on the item that now backs the placed file. This is
	// attribution only: a failure to record it must not fail an import whose audio
	// already landed and cataloged. A file that joined an existing item as a copy
	// records none, since that item was acquired before it, and an item that stood before
	// the file, or took one in, only fills what its record lacks: every item the file backs
	// but those the put made from nothing.
	if plan.Acquisition != nil && !outcome.AttachedAsCopy {
		copyOf, err := s.store.PutAcquisitionForFile(ctx, []byte(a.Dst), *plan.Acquisition, made)
		if err != nil {
			s.log.Warn("recording acquisition provenance", "dst", a.Dst, "err", err)
		}
		if copyOf != "" {
			outcome.ItemPID, outcome.AttachedAsCopy, outcome.Verdict = copyOf, true, scan.VerdictCopy
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
