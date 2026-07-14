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
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// WaxbinItemPIDKey is the tag key organize stamps with a backing item's stable
// WaxBin PID (a custom key, round-tripped as a native custom field), so a rebuild
// from tags can restore item identity. It is only a HINT: identity is essence-first
// and the tag is copyable, so rebuild adopts it only when a single essence-group
// unambiguously claims it.
const WaxbinItemPIDKey = model.TagWaxbinItemPID

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
	// TagFields are the metadata fields to write into this file before the move
	// (album artist, track/disc numbers), computed lock-respectingly at plan time.
	// Empty unless the profile enables tag-write. Carried in the plan so a re-validated
	// executor writes exactly what was planned without re-reading the profile or item.
	TagFields []TagField
}

// TagField is one metadata field the organize tag-write will set on disk and stamp
// with organize provenance. Field is the model metadata-field key (for lock and
// provenance); Key is the on-disk tag key; Value is the value to write.
type TagField struct {
	Field string
	Key   string
	Value string
}

// Plan is a serializable set of moves for one library + profile. It is produced
// read-only (a dry run) and applied separately.
type Plan struct {
	Profile    string
	LibraryPID model.PID
	Root       string
	Actions    []Action
	// TagWrite records whether the profile enabled lock-respecting tag write-back, so
	// the executor (which sees only the plan) knows to apply each action's TagFields.
	TagWrite bool
	// StampPID records whether to also stamp the backing item's WaxBin PID into a tag
	// before the move (managed-only; organize plans only managed-root files).
	StampPID bool
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
	// Warnings records moves that succeeded but whose tag write-back did not fully
	// land. They are not failures and do not affect the exit code.
	Warnings []Warning
}

// Failure records one action that could not be applied.
type Failure struct {
	FilePID model.PID
	Src     string
	Dst     string
	Err     string
}

// Warning records a non-fatal condition from an action that otherwise succeeded:
// a tag value the destination format could not store as asked. The file moved and
// the catalog is correct; the on-disk tag simply does not hold the planned value.
type Warning struct {
	FilePID model.PID
	Path    string
	Message string
}

// TagWriter applies tag edits to a file on disk and returns its new state. It is
// satisfied by *meta.Writer; injected so organize does not hard-depend on a
// concrete writer and stays testable.
type TagWriter interface {
	Apply(ctx context.Context, path string, edits []meta.TagEdit) (*meta.WriteResult, error)
}

// Organizer plans and applies moves against a catalog.
type Organizer struct {
	cat    model.Catalog
	writer TagWriter
	log    *slog.Logger
}

// New builds an organizer. writer may be nil when tag write-back is never used.
func New(cat model.Catalog, writer TagWriter, log *slog.Logger) *Organizer {
	if log == nil {
		log = slog.Default()
	}
	return &Organizer{cat: cat, writer: writer, log: log}
}

// Plan computes the destination for each item under the profile. Items with no
// backing file are skipped; items already at their destination are marked Skip. A
// multi-file audiobook expands into one move per part so the whole book is
// relocated together rather than split.
func (o *Organizer) Plan(ctx context.Context, lib *model.Library, p Profile, items []*model.ItemView) (*Plan, error) {
	root := string(lib.Root)
	plan := &Plan{Profile: p.Name, LibraryPID: lib.PID, Root: root, TagWrite: p.TagWrite}
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
		// Tag write-back applies to music tracks (albumArtist / Various Artists /
		// disc-track numbering); a book's tag model is different and is left alone.
		if p.TagWrite && it.Kind == model.KindTrack {
			fields, err := o.tagFields(ctx, it)
			if err != nil {
				return nil, err
			}
			a.TagFields = fields
		}
		plan.Actions = append(plan.Actions, a)
	}
	markCollisions(plan)
	return plan, nil
}

// tagFields computes the lock-respecting metadata edits organize will write into a
// track's file: the album artist (literal "Various Artists" for a compilation) and
// the disc/track numbers. A locked field is skipped so curated data survives.
func (o *Organizer) tagFields(ctx context.Context, it *model.ItemView) ([]TagField, error) {
	// Load the item's locked fields once rather than one SELECT per candidate field.
	locked, err := o.cat.LockedFields(ctx, it.PID)
	if err != nil {
		return nil, err
	}
	var out []TagField
	add := func(field, key, value string) {
		if value == "" || locked[field] {
			return
		}
		out = append(out, TagField{Field: field, Key: key, Value: value})
	}

	albumArtist := it.AlbumArtist
	if it.Compilation {
		albumArtist = "Various Artists"
	}
	add("album_artist", "ALBUMARTIST", albumArtist)
	if it.TrackNo > 0 {
		add("track_no", "TRACKNUMBER", strconv.Itoa(it.TrackNo))
	}
	if it.DiscNo > 0 {
		add("disc_no", "DISCNUMBER", strconv.Itoa(it.DiscNo))
	}
	return out, nil
}

// planBookParts plans a move for every part of a multi-file book into the rendered
// book folder (the directory of the template output). The audiobook template names
// a single file, so a multi-file book names each part "{book title} - NN.ext" using
// the part's 1-based reading-order index (files arrive in reading order). The index
// is a deterministic, unique disambiguator, so two source parts that happen to share
// a basename across folders no longer render to the same destination and get
// silently dropped by collision detection, the split this function exists to
// prevent.
func (o *Organizer) planBookParts(plan *Plan, root, rel string, itemPID model.PID, files []model.ItemFileRef) {
	// All-or-nothing: if any part cannot be placed (no path, or outside this managed
	// root), leave the WHOLE book where it is rather than moving some parts and
	// stranding others, the split this function exists to prevent. Roots are
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
		if err := o.apply(ctx, plan, a, jobPID, rep); err != nil {
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

// apply optionally re-tags the source (before the move, so a tag-write failure
// aborts the action cleanly with the file still in place), journals the move as
// 'planned', performs it on disk, then commits the catalog update as 'committed'
// plus a paired file-state update recording the re-tag's new hash/mtime. If the
// move fails, it marks the journal row 'rolled_back' (and records the re-tag at the
// un-moved source so the catalog reflects the bytes on disk).
func (o *Organizer) apply(ctx context.Context, plan *Plan, a *Action, jobPID model.PID, rep *Report) error {
	in := model.RelocateInput{
		FilePID:        a.FilePID,
		JobPID:         jobPID,
		SrcPath:        a.SrcBytes,
		NewPath:        []byte(a.Dst),
		NewDisplayPath: a.Dst,
		NewRelPath:     []byte(a.RelDst),
	}

	// Re-tag the source first. The write is essence-preserving, so item identity is
	// unchanged; a failure leaves the file untouched (atomic) and aborts the move.
	edits := o.buildEdits(plan, a)
	var retag *meta.WriteResult
	var prevSize, prevMtime int64
	if len(edits) > 0 && o.writer != nil {
		// The file's current size/mtime anchor the post-retag optimistic update. If it
		// cannot be read, abort BEFORE touching the file rather than proceed with a
		// zero anchor (which would silently match no row and never record the new hash).
		f, ferr := o.cat.FileByPath(ctx, a.SrcBytes)
		if ferr != nil {
			return ferr
		}
		prevSize, prevMtime = f.Size, f.MTimeNS
		res, err := o.writer.Apply(ctx, a.Src, edits)
		if err != nil {
			return err
		}
		retag = res
	}

	jpid, err := o.cat.PlanMove(ctx, in)
	if err != nil {
		return err
	}
	if err := moveFile(a.Src, a.Dst); err != nil {
		_ = o.cat.AbortMove(ctx, jpid)
		// The retag succeeded but the move did not: record the new bytes at the
		// un-moved source so the next scan does not re-hash it, then surface the move error.
		o.recordRetag(ctx, a.FilePID, prevSize, prevMtime, retag)
		return err
	}
	if err := o.cat.CommitMove(ctx, jpid, in); err != nil {
		return err
	}
	// CommitMove updated only the path; record the re-tag's new size/mtime/hash and
	// stamp organize provenance for the fields we wrote.
	o.recordRetag(ctx, a.FilePID, prevSize, prevMtime, retag)
	// Collected before the Changed gate. A write whose only effect was a value the
	// format could not store leaves the bytes unchanged, so it reports Changed=false
	// while being the case most worth surfacing.
	lost := o.noteUnrepresented(ctx, a, retag, rep)
	if retag != nil && retag.Changed {
		for _, tf := range a.TagFields {
			if lost[tf.Key] {
				// The value did not land, so stamping organize provenance would name
				// organize as the source of a value the file does not hold. That source is
				// read by `waxbin provenance <pid>`, and skipping the stamp keeps the
				// display truthful. It gates nothing else, since enrichment never reads
				// source and never writes these fields.
				continue
			}
			if err := o.cat.SetFieldProvenance(ctx, a.ItemPID, tf.Field, model.SourceOrganize, tf.Value, false); err != nil {
				o.log.Warn("organize provenance stamp", "item", a.ItemPID, "field", tf.Field, "err", err)
			}
		}
	}
	return nil
}

// noteUnrepresented appends a report warning for every tag value the write-back
// reported as not landing, and returns the set of affected on-disk tag keys so the
// caller can withhold their provenance stamp.
//
// It records a warning even when the warning matches no TagField. WAXBIN_ITEM_PID is
// not a TagField and a keyless warning matches nothing, so filtering to the planned
// fields would let a dropped PID stamp fail in silence, and rebuild-by-PID depends on
// that stamp. The writer carries benign warnings through, but they gate nothing here.
func (o *Organizer) noteUnrepresented(ctx context.Context, a *Action, retag *meta.WriteResult, rep *Report) map[string]bool {
	var lost map[string]bool
	var diags []model.FileDiagnostic
	// retag is nil when the profile writes no tags. That is a reason to reach the
	// replace below with an empty set, not a reason to skip it.
	if retag != nil {
		for _, w := range retag.Warnings {
			if !w.Unrepresented {
				continue
			}
			if w.Key != "" {
				if lost == nil {
					lost = make(map[string]bool, len(retag.Warnings))
				}
				lost[w.Key] = true
			}
			// The file has moved by now, so report where it actually lives: that is the
			// path the user would act on.
			rep.Warnings = append(rep.Warnings, Warning{FilePID: a.FilePID, Path: a.Dst, Message: w.Message})
			o.log.Warn("organize tag value unrepresented", "path", a.Dst, "key", w.Key, "warning", w.Message)
			diags = append(diags, model.FileDiagnostic{
				Code: model.DiagTagWriteLost, Severity: model.SeverityWarn,
				TagKey: w.Key, Detail: w.Message,
			})
		}
	}
	// Called on every applied action, including one that wrote no tags. This writer
	// replaces its own rows wholesale, so an organize that finds nothing has to clear
	// what a prior organize left. Skipping the call when the profile has tag-write
	// turned off would strand a tag_write_lost finding that describes a write no longer
	// being attempted. A replace with nothing to do costs a read, not a transaction.
	//
	// Keyed by FilePID rather than path, since the file has just moved and
	// file_diagnostic is keyed by file_id, which follows the move on its own.
	if err := o.cat.PutFileDiagnostics(ctx, a.FilePID, model.OriginOrganize, diags); err != nil {
		o.log.Warn("organize diagnostics", "file", a.FilePID, "err", err)
	}
	return lost
}

// buildEdits assembles the on-disk tag edits for an action: the planned metadata
// fields plus, when PID stamping is enabled, the item's WaxBin PID.
func (o *Organizer) buildEdits(plan *Plan, a *Action) []meta.TagEdit {
	if !plan.TagWrite && !plan.StampPID {
		return nil
	}
	var edits []meta.TagEdit
	if plan.TagWrite {
		for _, tf := range a.TagFields {
			edits = append(edits, meta.TagEdit{Key: tf.Key, Values: []string{tf.Value}})
		}
	}
	if plan.StampPID && a.ItemPID != "" {
		edits = append(edits, meta.TagEdit{Key: WaxbinItemPIDKey, Values: []string{string(a.ItemPID)}})
	}
	return edits
}

// recordRetag updates the file row's size/mtime/content_hash to the re-tagged
// values, only if the stored size/mtime still match what we read before the write
// (optimistic concurrency). A no-op or absent re-tag does nothing.
func (o *Organizer) recordRetag(ctx context.Context, filePID model.PID, prevSize, prevMtime int64, retag *meta.WriteResult) {
	if retag == nil || !retag.Changed {
		return
	}
	if _, err := o.cat.UpdateFileStateIfUnchanged(ctx, model.FileStateUpdate{
		FilePID:         filePID,
		ExpectedSize:    prevSize,
		ExpectedMTimeNS: prevMtime,
		NewSize:         retag.Size,
		NewMTimeNS:      retag.MTimeNS,
		NewContentHash:  retag.ContentHash,
	}); err != nil {
		o.log.Warn("organize file-state update after retag", "file", filePID, "err", err)
	}
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
