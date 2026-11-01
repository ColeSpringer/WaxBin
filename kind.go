package waxbin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"time"

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
// book after the first, an item whose file joined one already holding its key, or, asked
// again after a crash, a target the crashed change absorbed. The change feed carries an
// absorbed pid as a plain item delete, so a host that keeps pids should persist this
// mapping. Created maps each item a book split minted for a part after the primary to the
// item the part's file came from, or the item that one ended in. WriteBackFailures names
// the files whose tags could not be written; the catalog change stands either way.
//
// Targets are the items the change works through, each once in the order named, a pid a
// change that ended early absorbed standing as the item it went to, and Reads is how many of
// them, from the first, it has finished. While it runs, the job row holds the targets as its
// params and Reads as its result, checkpointed at a target's end once kindCheckpointEvery
// files or a second have passed and when the reads end. A change cut off by a crash is
// completed when the catalog next opens read-write from what its writes recorded under its
// job in the same transactions (item_lineage): where every item it folded away went and
// every item it made, whatever the crash's moment, and the targets of the kind now; Reads
// stays its last checkpoint's. A change cut short by a failed file or a cancel reports what
// it did before it stopped. Every item a read lands a file on is pinned by the write that
// lands it. A change that finishes drops what its writes recorded.
type KindReport struct {
	Converted         []model.PID             `json:"converted,omitempty"`
	Absorbed          map[model.PID]model.PID `json:"absorbed,omitempty"`
	Created           map[model.PID]model.PID `json:"created,omitempty"`
	WriteBackFailures []WriteBackFailure      `json:"writeBackFailures,omitempty"`
	Targets           []model.PID             `json:"targets,omitempty"`
	Reads             int                     `json:"reads"`
}

// kindCheckpointEvery is how many files a kind change reads before it checkpoints its
// report at the next target's end, as it also does once kindCheckpointAfter has passed.
// The report is cumulative, so a checkpoint per file would write it whole per file.
const (
	kindCheckpointEvery = 50
	kindCheckpointAfter = time.Second
)

// SetItemKind turns the items pids names into kind, a track or a book, under a set-kind
// job on the shared file lease. Each item's files are read again as that kind
// (ScanFileAs), so an item changes kind in place and keeps everything keyed by its pid.
// Files whose tags name one book become that book: the first item named keeps its pid and
// the rest fold into it, as does a book that already holds that key. A book becoming
// tracks keeps its pid on its primary part, and a resume position or bookmark goes to the
// part that holds it, as an offset into that part. A copy whose file is not on disk goes
// with the file it copies. Every item the files land on is pinned with a kind lock as its
// file is read, so later scans keep it. An item already of the kind is only pinned. Asked
// again after a crash, the change takes a pid the crashed change absorbed as the item it
// went to (kindTargets).
//
// An episode, a cue track, a file another item shares, a book file whose cue sheet would
// carve it into cue tracks once read as a track, a part not on disk and, without Force, an
// item whose kind is locked are refused before anything changes, each with one of the
// ReasonKind reasons. A file that fails partway fails the job, and the report returned
// with the error says what changed before it.
func (l *Library) SetItemKind(ctx context.Context, pids []model.PID, kind model.Kind, opts KindOptions) (*KindReport, error) {
	const op = "waxbin.SetItemKind"
	if l.ReadOnly() {
		return nil, waxerr.New(waxerr.CodeUnsupported, op, "changing an item's kind requires a read-write library")
	}
	if _, _, err := l.kindTargets(ctx, op, pids, kind, opts.Force); err != nil {
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

// StartSetItemKind submits SetItemKind as a background job and returns its pid; the job's
// Result holds the KindReport as JSON, its last checkpoint while it runs or after a crash.
// The request is checked before the job starts, so a refused one starts none.
func (l *Library) StartSetItemKind(ctx context.Context, pids []model.PID, kind model.Kind, opts KindOptions) (model.PID, error) {
	const op = "waxbin.StartSetItemKind"
	if _, _, err := l.kindTargets(ctx, op, pids, kind, opts.Force); err != nil {
		return "", err
	}
	pids = slices.Clone(pids)
	return l.startJob(ctx, kindSpec(pids), func(jctx context.Context, h *jobs.Handle) error {
		_, err := l.setKindWork(jctx, op, pids, kind, opts, h)
		return err
	})
}

// The reasons a kind change is refused, each beside its refusal's class.
const (
	ReasonKindBadTarget     waxerr.Reason = "kind_bad_target" // a target kind other than track or book
	ReasonKindNoItems       waxerr.Reason = "kind_no_items"
	ReasonKindSourceDecides waxerr.Reason = "kind_source_decides" // an episode or other kind its source decides
	ReasonKindCueTrack      waxerr.Reason = "kind_cue_track"
	ReasonKindSharedFile    waxerr.Reason = "kind_shared_file"
	ReasonKindCueSheet      waxerr.Reason = "kind_cue_sheet" // a sheet beside the file would make it a cue rip
	ReasonKindNotOnDisk     waxerr.Reason = "kind_not_on_disk"
	ReasonKindNoFile        waxerr.Reason = "kind_no_file"
	ReasonKindLocked        waxerr.Reason = model.ReasonKindLocked // a scan that met a lock set since it read the item says so too
)

// KindRefusal is the cause of a kind refusal about one file: the item refused and the
// file's display path. The shared-file, cue-sheet and not-on-disk refusals carry one.
type KindRefusal struct {
	Item model.PID
	Path string
	msg  string
}

func (r *KindRefusal) Error() string {
	if r.msg == "" {
		return fmt.Sprintf("item %s cannot change kind because of %s", r.Item, r.Path)
	}
	return r.msg
}

// kindRefusalAt builds a refusal about one file of item, reading as msg.
func kindRefusalAt(code waxerr.Code, op string, reason waxerr.Reason, item model.PID, path, msg string) error {
	return &waxerr.Error{Code: code, Op: op, Err: &KindRefusal{Item: item, Path: path, msg: msg}, Reason: reason}
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
// order named. A pid gone from the catalog that a set-kind job which ended early (failed,
// canceled or crashed) had among its targets and folded away itself answers with the item
// it ended in, when that stands (cutOffAbsorbed), and the returned map records it, so the
// change asked again completes rather than refusing; the earlier job's unread targets are
// simply read again, reading a file as the kind it has being a pin and nothing more. The
// job table is never pruned, which this relies on: a prune would have to keep set-kind rows
// that ended early, and their lineage.
func (l *Library) kindTargets(ctx context.Context, op string, pids []model.PID, kind model.Kind, force bool) ([]model.KindTarget, map[model.PID]model.PID, error) {
	if kind != model.KindTrack && kind != model.KindBook {
		return nil, nil, waxerr.New(waxerr.CodeInvalid, op, "an item's kind can only be changed to track or book, not "+string(kind)).
			WithReason(ReasonKindBadTarget)
	}
	var uniq []model.PID
	for _, p := range pids {
		if !slices.Contains(uniq, p) {
			uniq = append(uniq, p)
		}
	}
	if len(uniq) == 0 {
		return nil, nil, waxerr.New(waxerr.CodeInvalid, op, "no items named").WithReason(ReasonKindNoItems)
	}
	targets, err := l.store.KindTargets(ctx, uniq)
	var retried map[model.PID]model.PID
	if waxerr.Is(err, waxerr.CodeNotFound) {
		if retried, err = l.cutOffAbsorbed(ctx, uniq); err != nil {
			return nil, nil, err
		}
		var asked []model.PID
		for _, p := range uniq {
			if into, ok := retried[p]; ok {
				p = into
			}
			if !slices.Contains(asked, p) {
				asked = append(asked, p)
			}
		}
		targets, err = l.store.KindTargets(ctx, asked)
	}
	if err != nil {
		return nil, nil, err
	}
	for _, t := range targets {
		if t.Kind != model.KindTrack && t.Kind != model.KindBook {
			return nil, nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s is a %s, whose kind its source decides", t.ItemPID, t.Kind)).
				WithReason(ReasonKindSourceDecides)
		}
		// An item already of the kind is not read, so its files refuse nothing.
		if t.Kind == kind {
			continue
		}
		parts := 0
		for _, f := range t.Files {
			switch {
			case f.Windowed:
				return nil, nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("item %s is a cue track, whose kind follows its rip file", t.ItemPID)).
					WithReason(ReasonKindCueTrack)
			case f.Shared:
				return nil, nil, kindRefusalAt(waxerr.CodeInvalid, op, ReasonKindSharedFile, t.ItemPID, f.DisplayPath,
					fmt.Sprintf("item %s shares the file %s with another item", t.ItemPID, f.DisplayPath))
			}
			// A book file read as a track takes a cue sheet beside it as its tracks, unless
			// its own chapters win, and cue tracks hold no kind lock.
			if kind == model.KindTrack && !f.Embedded && scan.CueRipTracks(string(f.Path), f.Codec, f.DurationMS) > 1 {
				return nil, nil, kindRefusalAt(waxerr.CodeInvalid, op, ReasonKindCueSheet, t.ItemPID, f.DisplayPath, fmt.Sprintf(
					"item %s has a cue sheet beside %s, which would make the file a cue rip; move the sheet aside first",
					t.ItemPID, f.DisplayPath))
			}
			if f.Role == "alternate" {
				continue
			}
			parts++
			present, err := onDisk(string(f.Path))
			if err != nil {
				return nil, nil, waxerr.Wrap(waxerr.CodeIO, op, err)
			}
			if !present {
				return nil, nil, kindRefusalAt(waxerr.CodeConflict, op, ReasonKindNotOnDisk, t.ItemPID, f.DisplayPath,
					fmt.Sprintf("item %s cannot be read as a %s: %s is not on disk", t.ItemPID, kind, f.DisplayPath))
			}
		}
		if parts == 0 {
			return nil, nil, waxerr.New(waxerr.CodeConflict, op, fmt.Sprintf("item %s has no file to read", t.ItemPID)).WithReason(ReasonKindNoFile)
		}
		if t.KindLocked && !force {
			return nil, nil, waxerr.New(waxerr.CodeLocked, op, fmt.Sprintf("item %s is locked as a %s (use force to override)", t.ItemPID, t.Kind)).
				WithReason(ReasonKindLocked)
		}
	}
	return targets, retried, nil
}

// cutOffAbsorbed maps each of pids gone from the catalog that a set-kind job which ended
// early had among its targets to the item that job folded it into, followed to where it
// ended (Store.AbsorbedInto), when that stands. A pid the job did not fold itself stays
// gone, so a retry never reaches an item some later write folded it into, which nobody
// named.
func (l *Library) cutOffAbsorbed(ctx context.Context, pids []model.PID) (map[model.PID]model.PID, error) {
	var gone []model.PID
	for _, p := range pids {
		_, err := l.store.ItemByPID(ctx, p)
		switch {
		case waxerr.Is(err, waxerr.CodeNotFound):
			gone = append(gone, p)
		case err != nil:
			return nil, err
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	ended, err := l.store.UnfinishedJobs(ctx, "set-kind")
	if err != nil {
		return nil, err
	}
	out := map[model.PID]model.PID{}
	for _, j := range ended {
		var p kindParams
		if json.Unmarshal([]byte(j.Params), &p) != nil {
			continue
		}
		targets := make(map[model.PID]bool, len(p.Targets))
		for _, t := range p.Targets {
			targets[t] = true
		}
		var named []model.PID
		for _, g := range gone {
			if _, done := out[g]; !done && targets[g] {
				named = append(named, g)
			}
		}
		if len(named) == 0 {
			continue
		}
		into, err := l.store.AbsorbedInto(ctx, j.PID, named)
		if err != nil {
			return nil, err
		}
		maps.Copy(out, into)
	}
	return out, nil
}

// kindParams is what a set-kind job records once its targets are read: the targets it
// works through, the kind, and the targets already of it, which it only pins.
type kindParams struct {
	Targets []model.PID `json:"targets"`
	Kind    model.Kind  `json:"kind"`
	Pinned  []model.PID `json:"pinned,omitempty"`
}

// completeCutOffKindChanges fills in the report of each kind change a crash cut off, from
// what it recorded when it began (kindParams), its last checkpoint's Reads, and the lineage
// its writes left under its job: each item it folded away and the item that ended in, each
// item it made and the item whose file it came from, and each target of the kind now that
// was not before, settled as a finished change's report is (model.SettleIdentities). A
// report already complete, or a job cut off before it named its targets, is left as it is.
func (l *Library) completeCutOffKindChanges(ctx context.Context) error {
	ended, err := l.store.UnfinishedJobs(ctx, "set-kind")
	if err != nil {
		return err
	}
	for _, j := range ended {
		if j.State != model.JobCrashed {
			continue
		}
		var last KindReport
		var p kindParams
		if j.Result != "" && json.Unmarshal([]byte(j.Result), &last) != nil || len(last.Targets) > 0 ||
			json.Unmarshal([]byte(j.Params), &p) != nil || len(p.Targets) == 0 {
			continue
		}
		rep := &KindReport{Targets: p.Targets, Reads: last.Reads}
		absorbed, err := l.store.AbsorbedInto(ctx, j.PID, nil)
		if err != nil {
			return err
		}
		created, err := l.store.MintedFrom(ctx, j.PID)
		if err != nil {
			return err
		}
		var converted []model.PID
		for _, t := range p.Targets {
			if _, gone := absorbed[t]; gone || slices.Contains(p.Pinned, t) {
				continue
			}
			v, err := l.store.ItemByPID(ctx, t)
			if err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
				return err
			}
			if err == nil && v.Kind == p.Kind {
				converted = append(converted, t)
			}
		}
		rep.Converted = model.SettleIdentities(converted, absorbed, created)
		if len(absorbed) > 0 {
			rep.Absorbed = absorbed
		}
		if len(created) > 0 {
			rep.Created = created
		}
		b, err := json.Marshal(rep)
		if err != nil {
			return err
		}
		if err := l.store.SaveJobResult(ctx, j.PID, b); err != nil {
			return err
		}
	}
	return nil
}

// onDisk reports whether a file is on disk; a failure to tell is an error.
func onDisk(path string) (bool, error) {
	_, err := os.Stat(pathx.Long(path))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// kindRun is what a kind change's reads did so far: the files read, how many targets it
// finished and which it found converted, each item folded away with the item it folded
// into, and each item minted with the item its file came from.
type kindRun struct {
	job       model.PID
	read      []model.PID
	reads     int
	converted []model.PID
	folded    map[model.PID]model.PID
	created   map[model.PID]model.PID
}

// report is the run so far as a KindReport over targets, with the pids a crashed change
// absorbed, settled (model.SettleIdentities).
func (r *kindRun) report(targets []model.KindTarget, retried map[model.PID]model.PID) *KindReport {
	rep := &KindReport{Reads: r.reads}
	for _, t := range targets {
		rep.Targets = append(rep.Targets, t.ItemPID)
	}
	absorbed := maps.Clone(r.folded)
	maps.Copy(absorbed, retried)
	created := maps.Clone(r.created)
	rep.Converted = model.SettleIdentities(slices.Clone(r.converted), absorbed, created)
	if len(absorbed) > 0 {
		rep.Absorbed = absorbed
	}
	if len(created) > 0 {
		rep.Created = created
	}
	return rep
}

// kindFile is a file a kind change reads, and for a copy not on disk, which it cannot
// read, the file it goes with instead.
type kindFile struct {
	model.KindTargetFile
	anchor model.PID
}

// setKindWork runs a kind change under its job: it records its targets, reads each target's
// files again as kind, pinning the item each file lands on as it goes, checkpoints how far it
// got on its cadence, then reports, pins and, when asked, writes back what landed
// (finishKind). The report is the job's result, and it is built and the pins written however
// the reads end, since an absorbed pid is gone either way.
func (l *Library) setKindWork(ctx context.Context, op string, pids []model.PID, kind model.Kind, opts KindOptions, h *jobs.Handle) (rep *KindReport, err error) {
	rep = &KindReport{}
	targets, retried, err := l.kindTargets(ctx, op, pids, kind, opts.Force)
	if err != nil {
		h.SetResult(l.jsonResult(rep))
		return rep, err
	}
	run := &kindRun{job: h.JobPID(), folded: map[model.PID]model.PID{}, created: map[model.PID]model.PID{}}
	defer func() {
		ferr := l.finishKind(context.WithoutCancel(ctx), op, targets, retried, kind, run, rep, err == nil && opts.WriteBack)
		if err == nil {
			err = ferr
		}
		h.SetResult(l.jsonResult(rep))
		// A change that finished leaves nothing for a retry or a crash's report to read.
		if err == nil {
			if derr := l.store.DropLineage(context.WithoutCancel(ctx), run.job); derr != nil {
				l.log.Warn("kind change: dropping its lineage", "job", run.job, "err", derr)
			}
		}
	}()
	params := kindParams{Kind: kind}
	for _, t := range targets {
		params.Targets = append(params.Targets, t.ItemPID)
		if t.Kind == kind {
			params.Pinned = append(params.Pinned, t.ItemPID)
		}
	}
	if err := h.SetParams(ctx, params); err != nil {
		return rep, err
	}
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return rep, err
	}
	byID := make(map[int64]*model.Library, len(libs))
	for _, lib := range libs {
		byID[lib.ID] = lib
	}
	total := 0
	for _, t := range targets {
		if t.Kind != kind {
			total += len(t.Files)
		}
	}
	every, after := l.kindCheckpointEvery, l.kindCheckpointAfter
	if every <= 0 {
		every = kindCheckpointEvery
	}
	if after <= 0 {
		after = kindCheckpointAfter
	}
	done, since, last, checked := 0, 0, time.Now(), 0
	for _, t := range targets {
		if t.Kind == kind {
			if err := l.store.LockKinds(ctx, []model.PID{t.ItemPID}); err != nil {
				return rep, err
			}
		} else {
			var follow []kindFile
			for _, f := range kindReadOrder(t) {
				if f.Role == "alternate" {
					if present, _ := onDisk(string(f.Path)); !present {
						follow = append(follow, f)
						continue
					}
				}
				if err := h.Heartbeat(ctx, float64(done)/float64(max(total, 1)),
					fmt.Sprintf("reading file %d of %d as a %s", done+1, total, kind)); err != nil {
					return rep, err
				}
				if err := l.readAsKind(ctx, byID, f, kind, run); err != nil {
					return rep, err
				}
				done, since = done+1, since+1
			}
			// A copy that cannot be read goes where the file it copies went, so its old
			// item is not kept alive by it.
			for _, f := range follow {
				into, folded, err := l.store.FollowFile(ctx, f.FilePID, f.anchor, run.job)
				if err != nil {
					return rep, err
				}
				for _, p := range folded {
					run.folded[p] = into
				}
			}
			if err := l.settleKindTarget(ctx, t, kind, run); err != nil {
				return rep, err
			}
		}
		run.reads++
		if since >= every || time.Since(last) >= after {
			if err := h.Checkpoint(ctx, KindReport{Reads: run.reads}); err != nil {
				return rep, err
			}
			since, last, checked = 0, time.Now(), run.reads
		}
	}
	if checked == run.reads {
		return rep, nil
	}
	return rep, h.Checkpoint(ctx, KindReport{Reads: run.reads})
}

// readAsKind reads one file again as kind, noting what the read folded away and minted,
// the re-reads of the files it promoted included. The write that lands the file pins the
// kind of its item and records the read's folds and mints under the change's job, so a
// crash leaves what it converted pinned and what it did on record.
func (l *Library) readAsKind(ctx context.Context, byID map[int64]*model.Library, f kindFile, kind model.Kind, run *kindRun) error {
	lib := byID[f.LibraryID]
	if lib == nil {
		return waxerr.New(waxerr.CodeInternal, "waxbin.SetItemKind", "no library holds "+f.DisplayPath)
	}
	res, _, err := l.scanner.ScanFileWith(ctx, lib, string(f.Path), scan.FileOptions{Kind: kind, Pin: true, Job: run.job})
	if err != nil {
		return err
	}
	run.read = append(run.read, f.FilePID)
	maps.Copy(run.folded, res.Absorbed)
	maps.Copy(run.created, res.Created)
	return nil
}

// settleKindTarget notes where a target stands once its files are read: converted when it
// is of the kind now, or, gone with no fold of it on record from the reads, absorbed into
// the item its files went to.
func (l *Library) settleKindTarget(ctx context.Context, t model.KindTarget, kind model.Kind, run *kindRun) error {
	v, err := l.store.ItemByPID(ctx, t.ItemPID)
	switch {
	case err == nil:
		if v.Kind == kind {
			run.converted = append(run.converted, t.ItemPID)
		}
		return nil
	case !waxerr.Is(err, waxerr.CodeNotFound):
		return err
	}
	if _, ok := run.folded[t.ItemPID]; ok {
		return nil
	}
	for _, f := range t.Files {
		p, err := l.store.WholeFileOwner(ctx, f.FilePID)
		if err != nil {
			return err
		}
		if p != "" {
			run.folded[t.ItemPID] = p
			return nil
		}
	}
	return nil
}

// finishKind fills the report from what the reads did, pins once more the item each read
// file ended on (a promoted copy's re-read can move one after its own pin), and with
// writeBack writes the kind into the named items' files. A pin that fails here leaves the
// report to go ahead and comes back joined with any other failure.
func (l *Library) finishKind(ctx context.Context, op string, targets []model.KindTarget, retried map[model.PID]model.PID, kind model.Kind, run *kindRun, rep *KindReport, writeBack bool) error {
	var errs []error
	var keep []model.PID
	for _, f := range run.read {
		p, err := l.store.WholeFileOwner(ctx, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p != "" && !slices.Contains(keep, p) {
			keep = append(keep, p)
		}
	}
	if err := l.store.LockKinds(ctx, keep); err != nil {
		errs = append(errs, err)
	}
	*rep = *run.report(targets, retried)
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
