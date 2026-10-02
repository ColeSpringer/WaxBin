package waxbin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// KindOptions controls SetItemKind.
type KindOptions struct {
	// WriteBack also writes the kind into the named items' files: MEDIATYPE 2 (the stik
	// atom in an MP4) for a book, and none for a track. A scan that ignores locks then reads
	// a book back as a book, and a track as a track only while nothing else says book (an
	// .m4b name, a narrator, an audiobook genre, an audiobook library). Without it the kind
	// lock alone keeps the change.
	WriteBack bool
	// Force changes an item whose kind is locked, which is otherwise refused with
	// CodeLocked.
	Force bool
}

// KindReport is what SetItemKind did. Converted lists the items that changed kind in place
// and kept their pids. Absorbed maps each item folded away to the item that took it in,
// with its plays, stars, bookmarks, sessions, queue and playlist entries: the tracks of one
// book after the first, or an item whose file joined one already holding its key. The
// change feed carries an absorbed pid as a plain item delete, so a host that keeps pids
// should persist this mapping. Created lists the items a book split minted for its parts
// after the primary. WriteBackFailures names the files whose tags could not be written;
// the catalog change stands either way. A change cut short by a failed file reports what
// it did before the failure.
type KindReport struct {
	Converted         []model.PID             `json:"converted,omitempty"`
	Absorbed          map[model.PID]model.PID `json:"absorbed,omitempty"`
	Created           []model.PID             `json:"created,omitempty"`
	WriteBackFailures []WriteBackFailure      `json:"writeBackFailures,omitempty"`
}

// SetItemKind turns the items pids names into kind, a track or a book, under a set-kind
// job on the shared file lease. Each item's files are read again as that kind
// (ScanFileAs), so an item changes kind in place and keeps everything keyed by its pid.
// Files whose tags name one book become that book: the first item named keeps its pid and
// the rest fold into it, as does a book that already holds that key. A book becoming
// tracks keeps its pid on its primary part, and a resume position or bookmark goes to the
// part that holds it, as an offset into that part. A copy whose file is not on disk goes
// with the file it copies. Every item the files land on is pinned with a kind lock, so
// later scans keep it. An item already of the kind is only pinned.
//
// An episode, a cue track, a file another item shares, a book file whose cue sheet would
// carve it into cue tracks once read as a track, a part not on disk and, without Force, an
// item whose kind is locked are refused before anything changes. A file that fails
// partway fails the job, and the report returned with the error says what changed before
// it.
func (l *Library) SetItemKind(ctx context.Context, pids []model.PID, kind model.Kind, opts KindOptions) (*KindReport, error) {
	const op = "waxbin.SetItemKind"
	if l.ReadOnly() {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "changing an item's kind requires a read-write library")
	}
	if _, err := l.kindTargets(ctx, op, pids, kind, opts.Force); err != nil {
		return nil, err
	}
	var rep *KindReport
	_, err := l.jobs.Run(ctx, kindSpec(pids), func(jctx context.Context, h *jobs.Handle) error {
		var err error
		rep, err = l.setKindWork(jctx, op, pids, kind, opts, h)
		return err
	})
	return rep, err
}

// StartSetItemKind submits SetItemKind as a background job and returns its pid; the
// finished job's Result holds the KindReport as JSON. The request is checked before the
// job starts, so a refused one starts none.
func (l *Library) StartSetItemKind(ctx context.Context, pids []model.PID, kind model.Kind, opts KindOptions) (model.PID, error) {
	const op = "waxbin.StartSetItemKind"
	if _, err := l.kindTargets(ctx, op, pids, kind, opts.Force); err != nil {
		return "", err
	}
	pids = slices.Clone(pids)
	return l.startJob(ctx, kindSpec(pids), func(jctx context.Context, h *jobs.Handle) error {
		_, err := l.setKindWork(jctx, op, pids, kind, opts, h)
		return err
	})
}

// kindSpec names a kind change's job, targeting the first item it names.
func kindSpec(pids []model.PID) jobs.Spec {
	spec := jobs.Spec{Kind: "set-kind", Scope: fsMutateScope, TargetType: "item"}
	if len(pids) > 0 {
		spec.TargetPID = pids[0]
	}
	return spec
}

// kindTargets reads and checks what a kind change applies to, each item once in the
// order named.
func (l *Library) kindTargets(ctx context.Context, op string, pids []model.PID, kind model.Kind, force bool) ([]model.KindTarget, error) {
	if kind != model.KindTrack && kind != model.KindBook {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "an item's kind can only be changed to track or book, not "+string(kind))
	}
	var uniq []model.PID
	for _, p := range pids {
		if !slices.Contains(uniq, p) {
			uniq = append(uniq, p)
		}
	}
	if len(uniq) == 0 {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no items named")
	}
	targets, err := l.store.KindTargets(ctx, uniq)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t.Kind != model.KindTrack && t.Kind != model.KindBook {
			return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s is a %s, whose kind its source decides", t.ItemPID, t.Kind))
		}
		// An item already of the kind is not read, so its files refuse nothing.
		if t.Kind == kind {
			continue
		}
		parts := 0
		for _, f := range t.Files {
			switch {
			case f.Windowed:
				return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s is a cue track, whose kind follows its rip file", t.ItemPID))
			case f.Shared:
				return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s shares the file %s with another item", t.ItemPID, f.DisplayPath))
			}
			// A book file read as a track takes a cue sheet beside it as its tracks, unless
			// its own chapters win, and cue tracks hold no kind lock.
			if kind == model.KindTrack && !f.Embedded && scan.CueRipTracks(string(f.Path), f.Codec, f.DurationMS) > 1 {
				return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf(
					"item %s has a cue sheet beside %s, which would make the file a cue rip; move the sheet aside first",
					t.ItemPID, f.DisplayPath))
			}
			if f.Role == "alternate" {
				continue
			}
			parts++
			present, err := onDisk(string(f.Path))
			if err != nil {
				return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if !present {
				return nil, waxerr.New(waxerr.CodeConflict, op, fmt.Sprintf("item %s cannot be read as a %s: %s is not on disk", t.ItemPID, kind, f.DisplayPath))
			}
		}
		if parts == 0 {
			return nil, waxerr.New(waxerr.CodeConflict, op, fmt.Sprintf("item %s has no file to read", t.ItemPID))
		}
		if t.KindLocked && !force {
			return nil, waxerr.New(waxerr.CodeLocked, op, fmt.Sprintf("item %s is locked as a %s (use force to override)", t.ItemPID, t.Kind))
		}
	}
	return targets, nil
}

// onDisk reports whether a file is on disk; a failure to tell is an error.
func onDisk(path string) (bool, error) {
	_, err := os.Stat(pathx.Long(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// kindRun is what a kind change's reads did: the files read, the items created, and each
// item folded away with the item it folded into.
type kindRun struct {
	read, created []model.PID
	folded        map[model.PID]model.PID
}

// kindFile is a file a kind change reads, and for a copy not on disk, which it cannot
// read, the file it goes with instead.
type kindFile struct {
	model.KindTargetFile
	anchor model.PID
}

// setKindWork runs a kind change under its job: it reads the items' files again as kind,
// then reports, locks and, when asked, writes back what landed (finishKind). The report is
// the job's result, and it is built and the locks written however the reads end, since
// an absorbed pid is gone either way.
func (l *Library) setKindWork(ctx context.Context, op string, pids []model.PID, kind model.Kind, opts KindOptions, h *jobs.Handle) (rep *KindReport, err error) {
	rep = &KindReport{}
	targets, err := l.kindTargets(ctx, op, pids, kind, opts.Force)
	if err != nil {
		h.SetResult(l.jsonResult(rep))
		return rep, err
	}
	run := &kindRun{folded: map[model.PID]model.PID{}}
	defer func() {
		ferr := l.finishKind(context.WithoutCancel(ctx), op, targets, kind, run, rep, err == nil && opts.WriteBack)
		if err == nil {
			err = ferr
		}
		h.SetResult(l.jsonResult(rep))
	}()
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return rep, err
	}
	byID := make(map[int64]*model.Library, len(libs))
	for _, lib := range libs {
		byID[lib.ID] = lib
	}
	var files, follow []kindFile
	for _, t := range targets {
		if t.Kind == kind {
			continue
		}
		for _, f := range kindReadOrder(t) {
			if f.Role == "alternate" {
				if present, _ := onDisk(string(f.Path)); !present {
					follow = append(follow, f)
					continue
				}
			}
			files = append(files, f)
		}
	}
	for i, f := range files {
		if err := h.Heartbeat(ctx, float64(i)/float64(len(files)),
			fmt.Sprintf("reading file %d of %d as a %s", i+1, len(files), kind)); err != nil {
			return rep, err
		}
		lib := byID[f.LibraryID]
		if lib == nil {
			return rep, waxerr.New(waxerr.CodeInternal, op, "no library holds "+f.DisplayPath)
		}
		_, out, err := l.scanner.ScanFileAs(ctx, lib, string(f.Path), kind)
		if err != nil {
			return rep, err
		}
		run.read = append(run.read, f.FilePID)
		if out == nil {
			continue
		}
		if out.ItemCreated {
			run.created = append(run.created, out.ItemPID)
		}
		for _, p := range out.Folded {
			run.folded[p] = out.ItemPID
		}
	}
	// A copy that cannot be read goes where the file it copies went, so its old item is
	// not kept alive by it.
	for _, f := range follow {
		into, folded, err := l.store.FollowFile(ctx, f.FilePID, f.anchor)
		if err != nil {
			return rep, err
		}
		for _, p := range folded {
			run.folded[p] = into
		}
	}
	return rep, nil
}

// finishKind fills the report from what the reads did and where the files landed, pins
// the kind of every item they landed on, and with writeBack writes the kind into the
// named items' files. A read that fails here leaves the rest of the report and the locks
// to go ahead, and comes back joined with any other.
func (l *Library) finishKind(ctx context.Context, op string, targets []model.KindTarget, kind model.Kind, run *kindRun, rep *KindReport, writeBack bool) error {
	var errs []error
	absorbed := make(map[model.PID]model.PID, len(run.folded))
	for p, into := range run.folded {
		for range len(run.folded) {
			next, ok := run.folded[into]
			if !ok {
				break
			}
			into = next
		}
		absorbed[p] = into
	}
	var keep []model.PID
	add := func(p model.PID) {
		if p != "" && !slices.Contains(keep, p) {
			keep = append(keep, p)
		}
	}
	for _, t := range targets {
		v, err := l.store.ItemByPID(ctx, t.ItemPID)
		switch {
		case err == nil:
			if v.Kind == kind {
				if t.Kind != kind {
					rep.Converted = append(rep.Converted, t.ItemPID)
				}
				add(t.ItemPID)
			}
		case waxerr.Is(err, waxerr.CodeNotFound):
			if _, ok := absorbed[t.ItemPID]; ok {
				continue
			}
			// No put here folded it: the scanner did, re-reading a promoted copy, so it went
			// where its files went, to an item a fold reaches (a cue track's window is not one).
			for _, f := range t.Files {
				p, err := l.store.WholeFileOwner(ctx, f.FilePID)
				if err != nil {
					errs = append(errs, err)
					break
				}
				if p != "" {
					absorbed[t.ItemPID] = p
					break
				}
			}
		default:
			errs = append(errs, err)
		}
	}
	for _, f := range run.read {
		p, err := l.store.WholeFileOwner(ctx, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		add(p)
	}
	if len(absorbed) > 0 {
		rep.Absorbed = absorbed
	}
	for _, p := range run.created {
		if _, gone := absorbed[p]; !gone && !slices.Contains(rep.Created, p) {
			rep.Created = append(rep.Created, p)
		}
	}
	if err := l.store.LockKinds(ctx, keep); err != nil {
		errs = append(errs, err)
	}
	if !writeBack || len(errs) > 0 {
		return errors.Join(errs...)
	}
	edit := meta.TagEdit{Key: "MEDIATYPE"}
	if kind == model.KindBook {
		edit.Values = []string{"2"}
	}
	var refs []model.ItemFileRef
	for _, t := range targets {
		for _, f := range t.Files {
			if !f.Windowed {
				refs = append(refs, model.ItemFileRef{FilePID: f.FilePID, Path: f.Path, DisplayPath: f.DisplayPath, Role: f.Role, Position: f.Position})
			}
		}
	}
	wb := &WriteBackError{}
	if err := l.writeBackFiles(ctx, op, model.OriginEdit, refs, wb, nil, nil, func(w *meta.Writer, path string) (*meta.WriteResult, error) {
		return w.Apply(ctx, path, []meta.TagEdit{edit})
	}); err != nil {
		return err
	}
	rep.WriteBackFailures = wb.Failures
	return nil
}

// kindReadOrder is the order a kind change reads an item's files in, each alternate with
// the file it goes with. A book becoming tracks reads its parts before its primary, so the
// primary is the book's last part and turns the book into its track in place, keeping the
// pid. A part's other encodings come just before it, so the places inside the part land on
// the part's own track (an encoding with no recording id in common becomes a track of its
// own, a track's encodings being linked by recording id), and its copies just after, which
// then join the part's track. The primary's encodings come after it instead: read first,
// one sharing its recording id would make a track the primary then joins, taking the book's
// pid with it. A track's alternates follow its primary.
func kindReadOrder(t model.KindTarget) []kindFile {
	var primary, parts, alts []model.KindTargetFile
	for _, f := range t.Files {
		switch f.Role {
		case "primary":
			primary = append(primary, f)
		case "alternate":
			alts = append(alts, f)
		default:
			parts = append(parts, f)
		}
	}
	var out []kindFile
	if t.Kind != model.KindBook {
		for _, f := range append(primary, parts...) {
			out = append(out, kindFile{KindTargetFile: f})
		}
		for _, a := range alts {
			anchor := model.PID("")
			if len(primary) > 0 {
				anchor = primary[0].FilePID
			}
			out = append(out, kindFile{KindTargetFile: a, anchor: anchor})
		}
		return out
	}
	taken := make([]bool, len(alts))
	take := func(p model.KindTargetFile, encodings bool) {
		for i, a := range alts {
			if !taken[i] && a.Position == p.Position && (a.Essence != p.Essence) == encodings {
				out, taken[i] = append(out, kindFile{KindTargetFile: a, anchor: p.FilePID}), true
			}
		}
	}
	for _, p := range parts {
		take(p, true)
		out = append(out, kindFile{KindTargetFile: p})
		take(p, false)
	}
	for _, p := range primary {
		out = append(out, kindFile{KindTargetFile: p})
		take(p, true)
		take(p, false)
	}
	for i, a := range alts {
		if !taken[i] {
			anchor := model.PID("")
			if len(primary) > 0 {
				anchor = primary[0].FilePID
			}
			out = append(out, kindFile{KindTargetFile: a, anchor: anchor})
		}
	}
	return out
}
