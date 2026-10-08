package organize

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/colespringer/waxbin/identity"
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
	Root     string // the root of the library the move is made in
	Skip     bool   // not moving: in place already, or held for Code
	Code     HoldCode
	Reason   string
	// TagFields are the metadata fields to write into this file before the move
	// (album artist, track/disc numbers), computed lock-respectingly at plan time.
	// Empty unless the profile enables tag-write. Carried in the plan so a re-validated
	// executor writes exactly what was planned without re-reading the profile or item.
	TagFields []TagField
	// unseated marks an action whose file may no longer stand at Src (an undo's move of a
	// file the catalog has dropped), so its hold claims no path; Hold's look at the disk
	// finds a file still there.
	unseated bool
}

// HoldCode says why an action does not move: HoldInPlace marks a file already at its
// destination, and every other code a move held back.
type HoldCode string

const (
	HoldInPlace      HoldCode = "in-place"
	HoldCollision    HoldCode = "collision"     // another item of the plan takes the destination first, or keeps it
	HoldOccupied     HoldCode = "occupied"      // a file the plan does not move holds the destination, on disk or in the catalog
	HoldOccupiedCase HoldCode = "occupied-case" // a file named apart from the destination only by case or Unicode form stands beside it
	HoldUntagged     HoldCode = "untagged"      // no artist, album artist or album to file the item under
	HoldMissing      HoldCode = "missing"       // the file is not on disk
	HoldVirtual      HoldCode = "virtual"       // a cue rip's shared file, carrying several tracks
	HoldBookHeld     HoldCode = "book-held"     // another part of the book is held, and a book moves whole
	HoldReadOnly     HoldCode = "read-only"     // the file's library is read-only
)

// hold marks an action as not moving, for code.
func (a *Action) hold(code HoldCode, reason string) {
	a.Skip, a.Code, a.Reason = true, code, reason
}

// TagField is one metadata field the organize tag-write will set on disk and stamp
// with organize provenance. Field is the model metadata-field key (for lock and
// provenance); Key is the on-disk tag key; Value is the value to write. Substitute marks
// a value that is organize's own spelling and not the catalog's (a compilation's
// "Various Artists"), so writing it pays no edit the catalog holds for the field.
type TagField struct {
	Field      string
	Key        string
	Value      string
	Substitute bool
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
	// ReadOnlyLibraries counts the managed libraries the plan passed over because they
	// are read-only.
	ReadOnlyLibraries int
	// Undo marks a plan that undoes an organize job (UndoPlan): after its moves it takes
	// Companions, the inverse of the companion steps that job journaled, in place of the
	// covers and companions an organize carries, so they come back exactly.
	Undo       bool            `json:",omitempty"`
	Companions []CompanionStep `json:",omitempty"`
}

// CompanionStep is one step with a folder companion: moved or copied from Src to Dst, or,
// for model.JournalCompanionDrop, the copy at Dst of the one at Src removed.
type CompanionStep struct {
	Kind     model.JournalKind
	Src, Dst string
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

// Held returns the actions held back from a move they need.
func (p *Plan) Held() int {
	n := 0
	for i := range p.Actions {
		if p.Actions[i].held() {
			n++
		}
	}
	return n
}

// held reports whether the action is held back, rather than moving or in place already.
func (a *Action) held() bool { return a.Skip && a.Code != "" && a.Code != HoldInPlace }

// HeldMove is a move an applied plan held back, with why.
type HeldMove struct {
	FilePID  model.PID
	Src, Dst string
	Code     HoldCode
	Reason   string
}

// Report summarizes an applied plan: Moved, Skipped (in place already), Held and Errored
// partition its actions.
type Report struct {
	Moved   int
	Skipped int
	Held    int
	// Holds lists the held moves, the ones the plan held and those a later look held.
	Holds         []HeldMove
	Errored       int
	SidecarsMoved int
	// DirsPruned counts the source folders the moves emptied and removed.
	DirsPruned int
	Failures   []Failure
	// Warnings records moves that succeeded but whose tag write-back did not fully
	// land. They are not failures and do not affect the exit code.
	Warnings []Warning
}

// RunResult is a Report plus the resolved profile name, the summary a server-run
// organize job records so a tailing client reports the same profile the direct
// path prints (the resolved name, not a placeholder).
type RunResult struct {
	Profile string `json:"profile"`
	Report  Report `json:"report"`
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
// concrete writer and stays testable. The options are part of the signature only so
// *meta.Writer satisfies it; organize passes none.
type TagWriter interface {
	Apply(ctx context.Context, path string, edits []meta.TagEdit, opts ...meta.ApplyOption) (*meta.WriteResult, error)
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

// PlanOptions carries what the catalog knows beyond an item's own tags.
type PlanOptions struct {
	// AlbumYears maps an album's pid to its year, which {year} renders for every track
	// on the album, so a track tagged a year apart from the rest files with its album
	// rather than in a folder the next scan would key as an album of its own.
	AlbumYears map[model.PID]int
	// Names maps an artist's pid to the name the catalog keeps for it and an album's to its
	// title, so tracks tagged with two spellings of one artist or album file under one
	// folder rather than two.
	Names map[model.PID]string
}

// Plan computes the destination for each item under the profile. Items with no
// backing file are skipped; items already at their destination are marked Skip. A
// multi-file audiobook expands into one move per part so the whole book is
// relocated together rather than split. Hold then holds back each move that cannot
// happen, and an item with no artist, album artist or album that its layout would file
// under the unknown buckets is held as untagged.
func (o *Organizer) Plan(ctx context.Context, lib *model.Library, p Profile, items []*model.ItemView, opts PlanOptions) (*Plan, error) {
	root := string(lib.Root)
	plan := &Plan{Profile: p.Name, LibraryPID: lib.PID, Root: root, TagWrite: p.TagWrite}
	rips := map[model.PID]bool{}
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
		// The tracks a cue sheet carves out of one file all play that file, which no one
		// track's path names, so the rip is held once.
		if it.Virtual {
			if !rips[it.FilePID] {
				rips[it.FilePID] = true
				a := Action{ItemPID: it.PID, FilePID: it.FilePID, Src: it.DisplayPath, SrcBytes: it.Path, Root: root}
				a.hold(HoldVirtual, "the file of a cue rip, which plays several tracks, stays where it is")
				plan.Actions = append(plan.Actions, a)
			}
			continue
		}
		acts, err := o.planItem(ctx, root, p, it, opts)
		if err != nil {
			return nil, err
		}
		plan.Actions = append(plan.Actions, acts...)
	}
	if err := o.Hold(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// planItem plans the moves of one item's files.
func (o *Organizer) planItem(ctx context.Context, root string, p Profile, it *model.ItemView, opts PlanOptions) ([]Action, error) {
	v := *it
	if y := opts.AlbumYears[it.AlbumPID]; it.AlbumPID != "" && y != 0 {
		v.Year = y
	}
	// The artist pair is the item's own text where it is not the entity's name in another
	// spelling: a joint credit's entity is only its first name.
	v.Artist = spelled(opts.Names, it.ArtistPID, it.Artist)
	v.AlbumArtist = spelled(opts.Names, it.AlbumArtistPID, it.AlbumArtist)
	if title := opts.Names[it.AlbumPID]; it.AlbumPID != "" && title != "" {
		v.Album = title
	}
	rel, buckets, err := renderRel(p, &v)
	if err != nil {
		return nil, err
	}
	acts, err := o.itemMoves(ctx, root, rel, p, it)
	if err != nil || !untagged(it, buckets) {
		return acts, err
	}
	for i := range acts {
		if !acts[i].Skip {
			acts[i].hold(HoldUntagged, "no artist, album artist or album to file it under")
		}
	}
	return acts, nil
}

// itemMoves plans an item's move to its rendered path, or each part's for a book.
func (o *Organizer) itemMoves(ctx context.Context, root, rel string, p Profile, it *model.ItemView) ([]Action, error) {
	// A book may be backed by several part files. The item view carries only the
	// representative primary, so moving just that would strand the other parts;
	// fetch them all and move every part into the rendered book folder. An
	// alternate copy of a part is not one, and stays where it is.
	if it.Kind == model.KindBook {
		files, err := o.cat.ItemFiles(ctx, it.PID)
		if err != nil {
			return nil, err
		}
		files = slices.DeleteFunc(files, func(f model.ItemFileRef) bool { return f.Role == "alternate" })
		if len(files) > 1 {
			return o.planBookParts(root, rel, it.PID, it.PartTotal, files), nil
		}
		// A lone part past the first is a part of a book whose other parts are not
		// here yet, named as it will be beside them; a lone first part is a
		// single-file book as often as not, and keeps the book's own name.
		if len(files) == 1 {
			if p := PartAt(files[0].Position); p.Track > 0 {
				if last, ok := LonePart(p, it.PartTotal); ok {
					rel = BookPartRelPath(rel, p, last, filepath.Ext(it.DisplayPath))
				}
			}
		}
	}
	dst := filepath.Join(root, rel)
	a := Action{
		ItemPID: it.PID, FilePID: it.FilePID,
		Src: it.DisplayPath, SrcBytes: it.Path, Dst: dst, RelDst: rel, Root: root,
	}
	if filepath.Clean(a.Src) == filepath.Clean(dst) {
		a.hold(HoldInPlace, "already in place")
	}
	// Tag write-back applies to music tracks (albumArtist / Various Artists /
	// disc-track numbering); a book's tag model is different and is left alone.
	if p.TagWrite && it.Kind == model.KindTrack {
		fields, err := o.tagFields(ctx, p, it)
		if err != nil {
			return nil, err
		}
		a.TagFields = fields
	}
	return []Action{a}, nil
}

// spelled is the name the catalog keeps for the entity pid where raw is that name in
// another spelling, else raw.
func spelled(names map[model.PID]string, pid model.PID, raw string) string {
	if name := names[pid]; pid != "" && name != "" && identity.MatchKey(name) == identity.MatchKey(raw) {
		return name
	}
	return raw
}

// untagged reports whether an item has no artist, album artist or album and the path
// rendered for it fell back on the unknown bucket of a field they give, which would file it
// with every other item tagged so.
func untagged(it *model.ItemView, buckets map[string]bool) bool {
	if firstNonEmpty(it.Artist, it.AlbumArtist, it.Album) != "" {
		return false
	}
	return slices.ContainsFunc(filingFields, func(f string) bool { return buckets[f] })
}

// tagFields computes the lock-respecting metadata edits organize will write into a
// track's file: the album artist (the profile's compilation folder for a compilation, as
// its path names it) and the disc/track numbers with their totals. A locked field is
// skipped so curated data survives.
func (o *Organizer) tagFields(ctx context.Context, p Profile, it *model.ItemView) ([]TagField, error) {
	// Load the item's locked fields once rather than one SELECT per candidate field.
	locked, err := o.cat.LockedFields(ctx, it.PID)
	if err != nil {
		return nil, err
	}
	var out []TagField
	// The tag key comes from meta.TagKeyForField, the one place the field-to-tag-key
	// mapping lives, shared with the catalog field-edit write-back.
	add := func(field, value string) {
		if value == "" || locked[field] {
			return
		}
		key, ok := meta.TagKeyForField(field)
		if !ok {
			return
		}
		out = append(out, TagField{Field: field, Key: key, Value: value})
	}

	albumArtist := it.AlbumArtist
	if it.Compilation {
		albumArtist = p.compilationArtist(it.AlbumArtist)
	}
	add("album_artist", albumArtist)
	if n := len(out); n > 0 && out[n-1].Field == "album_artist" && albumArtist != it.AlbumArtist {
		out[n-1].Substitute = true
	}
	// Each total rides beside its number, cleared when the catalog holds none, so a
	// renumbered file never keeps the old pair.
	total := func(field string, n int) {
		if n > 0 {
			add(field, strconv.Itoa(n))
		} else if key, ok := meta.TagKeyForField(field); ok && !locked[field] {
			out = append(out, TagField{Field: field, Key: key})
		}
	}
	if it.TrackNo > 0 {
		add("track_no", strconv.Itoa(it.TrackNo))
		total("track_total", it.TrackTotal)
	}
	if it.DiscNo > 0 {
		add("disc_no", strconv.Itoa(it.DiscNo))
		total("disc_total", it.DiscTotal)
	}
	return out, nil
}

// PartNumber is the number a book part's file name carries: its place on its disc, led
// by the disc when it has one.
type PartNumber struct{ Disc, Track int }

// PartAt is the place a book part's stored position gives it (scan.PartPosition).
func PartAt(position int) PartNumber {
	disc, place := model.SplitPartPosition(position)
	return PartNumber{Disc: disc, Track: place}
}

// NumberParts numbers a book's parts, given in reading order with the place each one's
// tags or name give it. When every part has a place of its own (a track from 1 to 999, no
// two alike) the parts keep their places, so a book imported a file at a time is named the
// way organize names it whole; otherwise they are numbered in reading order. It also
// returns the last number to pad them to: the highest disc, and the highest place or the
// part total the book's files are tagged with.
func NumberParts(places []PartNumber, trackTotal int) ([]PartNumber, PartNumber) {
	out := slices.Clone(places)
	last := PartNumber{Track: trackTotal}
	seen := make(map[PartNumber]bool, len(places))
	for _, p := range places {
		if p.Track < 1 || p.Track >= 1000 || seen[p] {
			for i := range out {
				out[i] = PartNumber{Track: i + 1}
			}
			return out, PartNumber{Track: len(places)}
		}
		seen[p] = true
		last = PartNumber{Disc: max(last.Disc, p.Disc), Track: max(last.Track, p.Track)}
	}
	return out, last
}

// LonePart says whether a book's only part is named by its number, as it will be beside
// the parts still to come, and the last number to pad it to: a place past the first, a
// disc past the first or a tagged part total past one says more parts are coming, where a
// lone first part is a single-file book as often as not.
func LonePart(place PartNumber, trackTotal int) (PartNumber, bool) {
	last := PartNumber{Disc: place.Disc, Track: max(place.Track, trackTotal)}
	return last, place.Track > 0 && place.Track < 1000 && (place.Track > 1 || place.Disc > 1 || trackTotal > 1)
}

// BookPartRelPath names one part of a multi-file book in the book's folder: rel is the
// book's rendered path, and the part is "<title> - NN<ext>", or "<title> - D-NN<ext>" on
// disc D, each number padded to the digits of last's (two at least for the part). A title
// near the length limit is cut to keep the number.
func BookPartRelPath(rel string, part, last PartNumber, ext string) string {
	return CopyRelPath(rel, part, last, 1, ext)
}

// CopyRelPath names the nth file of a book part, its first being the part itself
// (BookPartRelPath) and the others its copies and other encodings, told apart by " (n)";
// a zero part is a book's only part under the book's own name.
func CopyRelPath(rel string, part, last PartNumber, n int, ext string) string {
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	suffix := ""
	if part.Track > 0 {
		num := fmt.Sprintf("%0*d", max(2, len(strconv.Itoa(last.Track))), part.Track)
		if part.Disc > 0 {
			num = fmt.Sprintf("%0*d-%s", len(strconv.Itoa(max(last.Disc, part.Disc))), part.Disc, num)
		}
		suffix = " - " + num
	}
	if n > 1 {
		suffix += " (" + strconv.Itoa(n) + ")"
	}
	suffix += strings.ToLower(ext)
	if budget := maxSegmentBytes - len(suffix); len(stem) > budget {
		stem = strings.TrimRight(truncateUTF8(stem, budget), " .")
	}
	return filepath.Join(filepath.Dir(rel), sanitizeSegment(stem+suffix))
}

// planBookParts plans a move for every part of a multi-file book into the rendered
// book folder (the directory of the template output), each part named by NumberParts
// over the places its position gives it (files arrive in reading order). The numbers
// are unique, so two source parts that happen to share a basename across folders no
// longer render to the same destination and get silently dropped by collision
// detection, the split this function exists to prevent.
func (o *Organizer) planBookParts(root, rel string, itemPID model.PID, trackTotal int, files []model.ItemFileRef) []Action {
	// All-or-nothing: if any part cannot be placed (no path, or outside this managed
	// root), the parts here are held where they are rather than moving some parts and
	// stranding others, the split this function exists to prevent. Roots are
	// validated non-overlapping, so a legitimately-scanned book's parts are all under
	// one root; this guards a stray/cross-root edge.
	for _, fl := range files {
		if fl.DisplayPath != "" && pathx.UnderRoot(root, fl.DisplayPath) {
			continue
		}
		var held []Action
		for _, part := range files {
			if part.DisplayPath != "" && pathx.UnderRoot(root, part.DisplayPath) {
				a := Action{ItemPID: itemPID, FilePID: part.FilePID, Src: part.DisplayPath, SrcBytes: part.Path, Root: root}
				a.hold(HoldBookHeld, "a part of its book is outside this library: "+cmp.Or(fl.DisplayPath, string(fl.FilePID)))
				held = append(held, a)
			}
		}
		return held
	}
	places := make([]PartNumber, len(files))
	for i, fl := range files {
		places[i] = PartAt(fl.Position)
	}
	numbers, last := NumberParts(places, trackTotal)
	acts := make([]Action, 0, len(files))
	for i, fl := range files {
		partRel := BookPartRelPath(rel, numbers[i], last, filepath.Ext(fl.DisplayPath))
		dst := filepath.Join(root, partRel)
		a := Action{
			ItemPID: itemPID, FilePID: fl.FilePID,
			Src: fl.DisplayPath, SrcBytes: fl.Path, Dst: dst, RelDst: partRel, Root: root,
		}
		if filepath.Clean(a.Src) == filepath.Clean(dst) {
			a.hold(HoldInPlace, "already in place")
		}
		acts = append(acts, a)
	}
	return orderBookMoves(acts)
}

// orderBookMoves orders a book's moves so a part moves after the part whose name it takes
// has moved off it, where it can: a cycle of them (two parts trading numbers) keeps its
// order, one move per part, and Execute parks one part of it. A move to its own name in
// another case or Unicode form waits on nothing.
func orderBookMoves(acts []Action) []Action {
	from := make(map[string]int, len(acts))
	for i, a := range acts {
		if !a.Skip {
			from[pathx.CollisionKey(a.Src)] = i
		}
	}
	out := make([]Action, 0, len(acts))
	waiter := map[int]int{}
	var ready []int
	for i, a := range acts {
		switch j, taken := from[pathx.CollisionKey(a.Dst)]; {
		case a.Skip:
			out = append(out, a)
		case taken && j != i:
			waiter[j] = i
		default:
			ready = append(ready, i)
		}
	}
	done := make([]bool, len(acts))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		out, done[i] = append(out, acts[i]), true
		if w, ok := waiter[i]; ok {
			ready = append(ready, w)
		}
	}
	for i, a := range acts {
		if !a.Skip && !done[i] {
			out = append(out, a)
		}
	}
	return out
}

// Hold holds back each action of the plan that cannot move as disk and catalog stand. A
// file not on disk is missing, a file already in place included. A destination two moves
// claim goes to the first, and one a file the plan leaves in place keeps is a collision;
// collisions are decided afresh as other holds land, so a move that lost its destination to
// one since held claims it back. A file the plan does not move standing at the destination,
// or beside it under a name apart only by case or Unicode form, holds it, as does a catalog
// row for a file gone from that path; a destination another move of the plan vacates is
// free, since Execute waits that move, and a file's own entry under another spelling is no
// occupant. A held part holds the rest of its book. Plan runs it, and an apply runs it
// again over a plan made earlier (see Reopen).
func (o *Organizer) Hold(ctx context.Context, plan *Plan) error {
	var dsts [][]byte
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Skip && a.Code != HoldInPlace {
			continue
		}
		if _, err := os.Lstat(pathx.Long(a.Src)); errors.Is(err, fs.ErrNotExist) {
			a.hold(HoldMissing, "file is not on disk")
		} else if err != nil {
			a.hold(HoldMissing, "file cannot be looked at: "+err.Error())
		} else if !a.Skip {
			dsts = append(dsts, []byte(a.Dst))
		}
	}
	known, err := o.cat.FilePIDsByPath(ctx, dsts)
	if err != nil {
		return err
	}
	ls := fsx.NewLister()
	found := make([]*occupant, len(plan.Actions))
	collided := make([]bool, len(plan.Actions))
	for {
		for i := range collided {
			if collided[i] {
				a := &plan.Actions[i]
				a.Skip, a.Code, a.Reason, collided[i] = false, "", "", false
			}
		}
		markCollisions(plan, collided)
		moving := map[string][]string{}
		for _, a := range plan.Actions {
			if !a.Skip {
				k := pathx.CollisionKey(a.Src)
				moving[k] = append(moving[k], a.Src)
			}
		}
		changed := false
		for i := range plan.Actions {
			a := &plan.Actions[i]
			if a.Skip {
				continue
			}
			if found[i] == nil {
				oc, err := o.occupant(ctx, ls, known, a)
				if err != nil {
					return err
				}
				found[i] = &oc
			}
			oc := found[i]
			if oc.code == "" || vacated(moving, oc.path) {
				continue
			}
			a.hold(oc.code, oc.reason)
			changed = true
		}
		if holdBooks(plan) {
			changed = true
		}
		if !changed {
			return nil
		}
	}
}

// Reopen clears the holds that follow from how disk and catalog stood when the plan was
// made (a collision, an occupied destination, a missing file, a held book part, a read-only
// library), so a Hold run later decides them again as things then stand. A file in place,
// an untagged item, a cue rip and a book part outside its library stay as planned.
func (p *Plan) Reopen() {
	for i := range p.Actions {
		a := &p.Actions[i]
		switch a.Code {
		case HoldCollision, HoldOccupied, HoldOccupiedCase, HoldMissing, HoldBookHeld, HoldReadOnly:
			if a.Dst == "" {
				continue
			}
			a.Skip, a.Code, a.Reason = false, "", ""
			if filepath.Clean(a.Src) == filepath.Clean(a.Dst) {
				a.hold(HoldInPlace, "already in place")
			}
		}
	}
}

// vacated reports whether the file at path is the source of a move still in the plan,
// spelled as the plan has it or as the folder lists it after a rename by case that no scan
// has read yet; a hard link of that file named apart by case is another entry and stays.
func vacated(moving map[string][]string, path string) bool {
	for _, src := range moving[pathx.CollisionKey(path)] {
		if filepath.Clean(src) == filepath.Clean(path) || fsx.SpelledAlike(src, path) {
			return true
		}
	}
	return false
}

// occupant is what holds a destination: the path of a file there, or of the catalog row
// claiming it, and the code and reason it holds the move for. A zero occupant holds
// nothing.
type occupant struct {
	path   string
	code   HoldCode
	reason string
}

// occupant finds what holds an action's destination: an entry of its folder under its
// name or one apart only by case, else the catalog's row for the path. known holds the
// catalog's files at the plan's destinations, read at once.
func (o *Organizer) occupant(ctx context.Context, ls *fsx.Lister, known map[string]model.PID, a *Action) (occupant, error) {
	p, exact, err := ls.Occupant(a.Src, a.Dst)
	if err != nil {
		return occupant{path: a.Dst, code: HoldOccupied, reason: "destination cannot be looked at: " + err.Error()}, nil
	}
	if p == "" {
		what, err := o.cataloged(ctx, a.ItemPID, known[a.Dst])
		if what == "" || err != nil {
			return occupant{}, err
		}
		return occupant{path: a.Dst, code: HoldOccupied,
			reason: "destination is held in the catalog by " + what + ", which is not on disk: " + a.Dst}, nil
	}
	file, ok := known[p]
	if !ok {
		f, err := o.cat.FileByPath(ctx, []byte(p))
		switch {
		case err == nil:
			file = f.PID
		case !waxerr.Is(err, waxerr.CodeNotFound):
			return occupant{}, err
		}
	}
	what, err := o.cataloged(ctx, a.ItemPID, file)
	if err != nil {
		return occupant{}, err
	}
	if what == "" {
		what = "a file the catalog does not know"
	}
	if !exact {
		return occupant{path: p, code: HoldOccupiedCase, reason: "destination differs only by case from " + what + ": " + p}, nil
	}
	return occupant{path: p, code: HoldOccupied, reason: "destination holds " + what + ": " + p}, nil
}

// cataloged says whose the cataloged file is, as a reason names it, or nothing for no
// file.
func (o *Organizer) cataloged(ctx context.Context, self, file model.PID) (string, error) {
	if file == "" {
		return "", nil
	}
	item, _, err := o.cat.FileOwner(ctx, file)
	switch {
	case err != nil:
		return "", err
	case item == "":
		return "a cataloged file that backs no item", nil
	case item == self:
		return "another file of this item", nil
	default:
		return "the file of item " + string(item), nil
	}
}

// markCollisions holds every move whose destination collides with an earlier-planned one
// or with the path of a file the plan leaves where it is, marking each in held. Two items
// rendering to the same path (or to paths that differ only by case, which collide on a
// case-insensitive filesystem) cannot both be moved there, so all but the first are held
// back with a reason rather than silently overwriting. The key is cleaned and case-folded
// so a managed tree remains portable across case-sensitive and case-insensitive
// filesystems.
func markCollisions(plan *Plan, held []bool) {
	seen := make(map[string]int, len(plan.Actions))
	for i, a := range plan.Actions {
		if a.Skip && !a.unseated {
			seen[pathx.CollisionKey(a.Src)] = i
		}
	}
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if a.Skip {
			continue
		}
		key := pathx.CollisionKey(a.Dst)
		if j, ok := seen[key]; ok {
			a.hold(HoldCollision, "destination collides with "+plan.Actions[j].Src)
			if held != nil {
				held[i] = true
			}
			continue
		}
		seen[key] = i
	}
}

// holdBooks holds every move of an item one of whose actions is held, since a book's parts
// move together or not at all, reporting whether it held any.
func holdBooks(plan *Plan) bool {
	held := map[model.PID]string{}
	for _, a := range plan.Actions {
		if _, ok := held[a.ItemPID]; !ok && a.ItemPID != "" && a.held() {
			held[a.ItemPID] = a.Src
		}
	}
	changed := false
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if src, ok := held[a.ItemPID]; ok && !a.Skip {
			a.hold(HoldBookHeld, "another part of its book is held: "+src)
			changed = true
		}
	}
	return changed
}

// Execute applies the plan: each move happens on disk, then the catalog records
// the relocation (path update + organize_journal + change_log) in one
// transaction; a move a folder respell already carried into place logs no change. A
// per-action failure is recorded and does not abort the run.
//
// A move whose destination is the source of a move still to come waits for it, and a
// cycle of them (two parts trading numbers) is broken by parking one part under a free
// name first. Each folder a destination names in another spelling is respelled before the
// move lands in it, the catalog and the plan's later sources following, or the move fails
// with the folder as it was.
func (o *Organizer) Execute(ctx context.Context, plan *Plan, jobPID model.PID, hb func(progress float64, msg string) error) (*Report, error) {
	rep := &Report{}
	total := len(plan.Actions)
	// A plan can span the managed libraries, so each root has its own Speller.
	spellers := map[string]*fsx.Speller{}
	speller := func(root string) *fsx.Speller {
		if root == "" {
			root = plan.Root
		}
		if spellers[root] == nil {
			spellers[root] = fsx.NewSpeller(root, func(from, to string) error { return o.respelled(ctx, plan, from, to) })
		}
		return spellers[root]
	}
	// The directory covers are planned once the audio has actually moved, from what left
	// each directory and where it landed, so a split album gets a cover in each
	// destination and an emptied directory is not left holding one.
	// It runs on the cancellation path too: the audio already moved, so a directory
	// emptied before the cancel would otherwise keep a cover with nothing to hold it. The
	// source folders the moves emptied are then removed.
	var moved []SidecarMove
	roots := map[string]string{}
	sib := NewSiblings()
	placeCovers := func() {
		if plan.Undo {
			rep.SidecarsMoved += o.applyCompanionSteps(ctx, plan.Companions, moved, jobPID)
			rep.DirsPruned += o.prune(ctx, roots, nil, jobPID)
			return
		}
		byRoot := map[string][]SidecarMove{}
		for _, m := range moved {
			r := roots[filepath.Dir(m.Src)]
			byRoot[r] = append(byRoot[r], m)
		}
		var carried []model.CompanionMove
		for _, r := range slices.Sorted(maps.Keys(byRoot)) {
			carried = append(carried, o.applyCoverMoves(speller(r), CoverMoves(byRoot[r], PruneOptions(r, nil)))...)
		}
		rep.SidecarsMoved += len(carried)
		o.journalCompanions(ctx, jobPID, carried)
		rep.DirsPruned += o.prune(ctx, roots, moved, jobPID)
	}
	pending := map[string]int{}
	for _, a := range plan.Actions {
		if !a.Skip {
			pending[pathx.CollisionKey(a.Src)]++
		}
	}
	blocked := func(a *Action) bool {
		k := pathx.CollisionKey(a.Dst)
		return k != pathx.CollisionKey(a.Src) && pending[k] > 0
	}
	ran := 0
	run := func(a *Action) {
		sp := speller(a.Root)
		if err := o.apply(ctx, plan, a, jobPID, rep, sp); err != nil {
			rep.Errored++
			rep.Failures = append(rep.Failures, Failure{FilePID: a.FilePID, Src: a.Src, Dst: a.Dst, Err: err.Error()})
			o.log.Warn("organize action failed", "src", a.Src, "dst", a.Dst, "err", err)
		} else {
			rep.Moved++
			// The audio is moved and recorded; now carry its own companions (same-basename
			// lyrics/cue/art) so a move does not leave them behind. Sidecars are not
			// cataloged, so a failure here is logged, not fatal.
			sib.Left(a.Src)
			sib.Arrived(a.Dst)
			rep.SidecarsMoved += o.moveSidecars(sp, a.Src, a.Dst, sib)
			moved = append(moved, SidecarMove{Src: a.Src, Dst: a.Dst})
			roots[filepath.Dir(a.Src)] = cmp.Or(a.Root, plan.Root)
		}
		pending[pathx.CollisionKey(a.Src)]--
		if ran++; hb != nil {
			_ = hb(float64(ran)/float64(max(total, 1)), "organized "+strconv.Itoa(ran)+"/"+strconv.Itoa(total))
		}
	}
	var waiting []*Action
	for i := range plan.Actions {
		if ctx.Err() != nil {
			placeCovers()
			return rep, waxerr.FromContext("organize.Execute", ctx.Err(), waxerr.CodeIO)
		}
		a := &plan.Actions[i]
		switch {
		case a.held():
			rep.Held++
			rep.Holds = append(rep.Holds, HeldMove{FilePID: a.FilePID, Src: a.Src, Dst: a.Dst, Code: a.Code, Reason: a.Reason})
			ran++
		case a.Skip:
			rep.Skipped++
			ran++
		case blocked(a):
			waiting = append(waiting, a)
		default:
			run(a)
		}
	}
	for len(waiting) > 0 {
		if ctx.Err() != nil {
			placeCovers()
			return rep, waxerr.FromContext("organize.Execute", ctx.Err(), waxerr.CodeIO)
		}
		if i := slices.IndexFunc(waiting, func(a *Action) bool { return !blocked(a) }); i >= 0 {
			a := waiting[i]
			waiting = slices.Delete(waiting, i, i+1)
			run(a)
			continue
		}
		// Every move left waits on another, so they wait in a cycle: park the first under a
		// free name, which frees the name the move before it in the cycle takes.
		a := waiting[0]
		old := pathx.CollisionKey(a.Src)
		if err := o.park(ctx, plan, a, jobPID, speller(a.Root), sib); err != nil {
			waiting = waiting[1:]
			rep.Errored++
			rep.Failures = append(rep.Failures, Failure{FilePID: a.FilePID, Src: a.Src, Dst: a.Dst, Err: err.Error()})
			o.log.Warn("organize action failed", "src", a.Src, "dst", a.Dst, "err", err)
			pending[old]--
			continue
		}
		pending[old]--
		pending[pathx.CollisionKey(a.Src)]++
	}
	placeCovers()
	return rep, nil
}

// prune removes the source folders the moves emptied, and any folder above them that is
// then empty, up to each one's library root (roots maps a source folder to it); a
// companion left in one follows its audio as FolderDisposal says, journaled for an undo.
// With no moves (an undo, whose companions went back by their journal) a companion keeps
// its folder.
func (o *Organizer) prune(ctx context.Context, roots map[string]string, moved []SidecarMove, jobPID model.PID) int {
	if moved == nil {
		return fsx.PruneAll(slices.Sorted(maps.Keys(roots)), func(dir string) fsx.PruneOptions {
			return PruneOptions(roots[dir], nil)
		}, func(dir string, err error) { o.log.Warn("pruning an emptied folder", "dir", dir, "err", err) })
	}
	d := FolderDisposal(moved, func(src string) string { return roots[filepath.Dir(src)] })
	n := fsx.PruneAll(slices.Sorted(maps.Keys(roots)), func(dir string) fsx.PruneOptions {
		opts := PruneOptions(roots[dir], d.Dispose)
		opts.Undo = d.Undo
		return opts
	}, func(dir string, err error) { o.log.Warn("pruning an emptied folder", "dir", dir, "err", err) })
	o.journalCompanions(ctx, jobPID, d.Carried())
	return n
}

// journalCompanions records the companion steps a run took, so an undo can take them back;
// a failure is logged, as a companion's own failure is, since the files already moved.
func (o *Organizer) journalCompanions(ctx context.Context, jobPID model.PID, steps []model.CompanionMove) {
	if len(steps) == 0 {
		return
	}
	if err := o.cat.JournalCompanions(context.WithoutCancel(ctx), jobPID, steps); err != nil {
		o.log.Warn("journaling the companions carried", "err", err)
	}
}

// applyCompanionSteps takes an undo's companion steps, journaling each it took, and
// reports how many: a move or a copy where its source stands and its destination is free,
// and a removal of a copy that still holds its original's bytes, or, its original gone, a
// move of the copy into the original's place. A step goes only with audio this run moved
// back between its folders (companionMover), so a companion stays with audio that stays.
// Any other is left as it stands, a failure logged.
func (o *Organizer) applyCompanionSteps(ctx context.Context, steps []CompanionStep, moved []SidecarMove, jobPID model.PID) int {
	var took []model.CompanionMove
	for _, st := range steps {
		st, ok := companionMover(st, moved)
		if !ok {
			continue
		}
		var err error
		switch st.Kind {
		case model.JournalCompanion, model.JournalCompanionCopy:
			err = fsx.MoveOrCopy(st.Src, st.Dst, st.Kind == model.JournalCompanionCopy)
		case model.JournalCompanionDrop:
			if err = dropCopy(st.Src, st.Dst); errors.Is(err, fs.ErrNotExist) && isRegularFile(st.Dst) {
				st = CompanionStep{Kind: model.JournalCompanion, Src: st.Dst, Dst: st.Src}
				err = fsx.Move(st.Src, st.Dst)
			}
		default:
			continue
		}
		if err != nil {
			o.log.Warn("companion not put back", "kind", st.Kind, "src", st.Src, "dst", st.Dst, "err", err)
			continue
		}
		took = append(took, model.CompanionMove{Kind: st.Kind, Src: []byte(st.Src), Dst: []byte(st.Dst)})
	}
	o.journalCompanions(ctx, jobPID, took)
	return len(took)
}

// companionMover finds the audio move a companion step goes with: one from the folder the
// step leaves (from) or one below it to the folder it reaches (to) or one below it, an album
// folder above its discs being either, compared as the library lays paths out
// (pathx.CollisionKey). The step is returned with its leaving side in that move's spelling
// of the folder, which a respell since may have changed; ok is false when no audio moved
// between them, and the step stays.
func companionMover(st CompanionStep, moved []SidecarMove) (CompanionStep, bool) {
	leaving, reaching := &st.Src, st.Dst
	if st.Kind == model.JournalCompanionDrop {
		leaving, reaching = &st.Dst, st.Src
	}
	from, to := pathx.CollisionKey(filepath.Dir(*leaving)), pathx.CollisionKey(filepath.Dir(reaching))
	within := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+string(filepath.Separator)) }
	for _, m := range moved {
		at := filepath.Dir(m.Src)
		if !within(pathx.CollisionKey(at), from) || !within(pathx.CollisionKey(filepath.Dir(m.Dst)), to) {
			continue
		}
		for pathx.CollisionKey(at) != from && at != filepath.Dir(at) {
			at = filepath.Dir(at)
		}
		*leaving = filepath.Join(at, filepath.Base(*leaving))
		return st, true
	}
	return st, false
}

// errCopyDiffers refuses to remove a copy that no longer holds its original's bytes.
var errCopyDiffers = errors.New("the copy differs from the file it was copied from")

// dropCopy removes copy when it still holds the bytes of orig, which still stands.
func dropCopy(orig, copy string) error {
	a, err := os.ReadFile(pathx.Long(orig))
	if err != nil {
		return err
	}
	b, err := os.ReadFile(pathx.Long(copy))
	if err != nil {
		return err
	}
	if !bytes.Equal(a, b) {
		return errCopyDiffers
	}
	return os.Remove(pathx.Long(copy))
}

// park moves an action's file to a free name in its own folder, journaled like any move,
// and points the action at it, so the move waiting on its name can land first.
func (o *Organizer) park(ctx context.Context, plan *Plan, a *Action, jobPID model.PID, sp *fsx.Speller, sib *Siblings) error {
	tmp, err := fsx.FreeName(filepath.Dir(a.Src), strings.ToLower(filepath.Ext(a.Src)))
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "organize.park", err)
	}
	rel := pathx.RelUnder(cmp.Or(a.Root, plan.Root), tmp)
	in := model.RelocateInput{FilePID: a.FilePID, JobPID: jobPID, SrcPath: a.SrcBytes,
		NewPath: []byte(tmp), NewDisplayPath: tmp, NewRelPath: []byte(rel)}
	jpid, err := o.cat.PlanMove(ctx, in)
	if err != nil {
		return err
	}
	if err := moveFile(sp, a.Src, tmp); err != nil {
		_ = o.cat.AbortMove(ctx, jpid)
		return err
	}
	if err := o.cat.CommitMove(ctx, jpid, in); err != nil {
		return err
	}
	sib.Left(a.Src)
	sib.Arrived(tmp)
	o.moveSidecars(sp, a.Src, tmp, sib)
	a.Src, a.SrcBytes = tmp, []byte(tmp)
	return nil
}

// respelled follows a folder the Speller renamed to another spelling: the catalog's paths
// below it, and the sources of the moves still to come. An error leaves the plan as it
// was, and has the Speller rename the folder back.
func (o *Organizer) respelled(ctx context.Context, plan *Plan, from, to string) error {
	if _, err := o.cat.RespellFolder(context.WithoutCancel(ctx), from, to); err != nil {
		return err
	}
	prefix := from + string(filepath.Separator)
	for i := range plan.Actions {
		a := &plan.Actions[i]
		if strings.HasPrefix(a.Src, prefix) {
			a.Src = to + a.Src[len(from):]
		}
		if bytes.HasPrefix(a.SrcBytes, []byte(prefix)) {
			a.SrcBytes = append([]byte(to), a.SrcBytes[len(from):]...)
		}
	}
	return nil
}

// apply optionally re-tags the source (before the move, so a tag-write failure
// aborts the action cleanly with the file still in place), journals the move as
// 'planned', performs it on disk, then commits the catalog update as 'committed'
// plus a paired file-state update recording the re-tag's new hash/mtime. If the
// move fails, it marks the journal row 'rolled_back' (and records the re-tag at the
// un-moved source so the catalog reflects the bytes on disk).
func (o *Organizer) apply(ctx context.Context, plan *Plan, a *Action, jobPID model.PID, rep *Report, sp *fsx.Speller) error {
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
	// The writer's one exception (a landed write whose hash could not be read) also
	// aborts here, and the next scan of the un-moved source heals its row.
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
	if err := moveFile(sp, a.Src, a.Dst); err != nil {
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
	if retag == nil {
		return nil
	}
	var landed []string
	for _, tf := range a.TagFields {
		if lost[tf.Key] {
			// The value did not land, so stamping organize provenance would name
			// organize as the source of a value the file does not hold. That source is
			// read by `waxbin provenance <pid>`, and skipping the stamp keeps the
			// display truthful. It gates nothing else, since enrichment never reads
			// source and never writes these fields.
			continue
		}
		if !tf.Substitute {
			landed = append(landed, tf.Field)
		}
		// A cleared total names no value organize set, so it leaves no stamp.
		if !retag.Changed || tf.Value == "" {
			continue
		}
		if err := o.cat.SetFieldProvenance(ctx, a.ItemPID, tf.Field, model.Attribution{Source: model.SourceOrganize}, tf.Value, false); err != nil {
			o.log.Warn("organize provenance stamp", "item", a.ItemPID, "field", tf.Field, "err", err)
		}
	}
	// The file now carries what the catalog holds for these fields, so an edit of one of
	// them is no longer owed to it.
	if err := o.cat.SettleTagWriteOwed(ctx, a.FilePID, landed); err != nil {
		o.log.Warn("organize owed settle", "item", a.ItemPID, "err", err)
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
				// The retag landed and only a post-commit step failed; surface it in
				// the log without gating anything.
				if w.Code == meta.PostWriteWarningCode {
					o.log.Warn("organize tag post-write", "path", a.Dst, "warning", w.Message)
				}
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
	if err := o.cat.PutFileDiagnostics(ctx, a.FilePID, model.OriginOrganize, meta.MergeKeylessDiagnostics(diags)); err != nil {
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
			e := meta.TagEdit{Key: tf.Key}
			if tf.Value != "" {
				e.Values = []string{tf.Value}
			}
			edits = append(edits, e)
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
func moveFile(sp *fsx.Speller, src, dst string) error {
	const op = "organize.move"
	if src == dst {
		return nil
	}
	if err := sp.Move(src, dst); err != nil {
		if errors.Is(err, fsx.ErrExist) {
			return waxerr.New(waxerr.CodeConflict, op, "destination already exists: "+dst)
		}
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return nil
}
