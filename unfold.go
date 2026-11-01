package waxbin

import (
	"context"
	"slices"

	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
)

// UnfoldOptions tunes Unfold and StartUnfold.
type UnfoldOptions struct {
	// ReResolve reads again the file of each item a fold kept on its entity (the items
	// Store.ItemsByEntityKeyedSpelling names), so each is filed under the name it states:
	// an unchanged file is otherwise filed where the fold put it until it changes.
	ReResolve bool
}

// UnfoldReport is what an unfold did: the folds it forgot, and with ReResolve the items
// whose file it read again and those it did not, the file offline, missing or unreadable.
type UnfoldReport struct {
	Folds   int         `json:"folds"`
	Reread  []model.PID `json:"reread,omitempty"`
	Skipped []model.PID `json:"skipped,omitempty"`
}

// Unfold forgets folds as UnfoldEntity does, all of them or none, and with ReResolve reads
// again the file of each item a fold kept, under an "unfold" job holding the filesystem
// lease. A read that cannot be made skips its item and names it in the report.
func (l *Library) Unfold(ctx context.Context, entityType model.MergeEntity, keys []string, opts UnfoldOptions) (*UnfoldReport, error) {
	if err := l.checkUnfold(ctx, entityType, keys); err != nil {
		return nil, err
	}
	var out *UnfoldReport
	_, err := l.jobs.Run(ctx, jobs.Spec{Kind: "unfold", Scope: fsMutateScope}, l.unfoldWork(entityType, keys, opts, &out))
	return out, err
}

// StartUnfold submits Unfold as a background job and returns its PID; the client reads the
// UnfoldReport from the job's result.
func (l *Library) StartUnfold(ctx context.Context, entityType model.MergeEntity, keys []string, opts UnfoldOptions) (model.PID, error) {
	if err := l.checkUnfold(ctx, entityType, keys); err != nil {
		return "", err
	}
	return l.startJob(ctx, jobs.Spec{Kind: "unfold", Scope: fsMutateScope},
		l.unfoldWork(entityType, slices.Clone(keys), opts, new(*UnfoldReport)))
}

// checkUnfold refuses an unfold before its job starts: a bad type, no keys, or a key no
// fold holds.
func (l *Library) checkUnfold(ctx context.Context, et model.MergeEntity, keys []string) error {
	switch {
	case !et.Valid():
		return waxerr.New(waxerr.CodeInvalid, "Library.Unfold", "unknown entity type: "+string(et))
	case len(keys) == 0:
		return waxerr.New(waxerr.CodeInvalid, "Library.Unfold", "no keys to unfold")
	}
	return l.store.CheckFolds(ctx, et, keys...)
}

// unfoldWork forgets the folds inside a job and reads again what they kept, leaving the
// report in out. The items are found while the folds still name their entity.
func (l *Library) unfoldWork(et model.MergeEntity, keys []string, opts UnfoldOptions, out **UnfoldReport) jobFn {
	return func(ctx context.Context, h *jobs.Handle) error {
		var items []model.PID
		if opts.ReResolve {
			for _, key := range keys {
				pids, err := l.store.ItemsByEntityKeyedSpelling(ctx, et, key)
				if err != nil {
					return err
				}
				items = append(items, pids...)
			}
			slices.Sort(items)
			items = slices.Compact(items)
		}
		if err := l.store.UnfoldEntity(ctx, et, keys...); err != nil {
			return err
		}
		rep := &UnfoldReport{Folds: len(slices.Compact(slices.Sorted(slices.Values(keys))))}
		*out = rep
		defer func() { h.SetResult(l.jsonResult(rep)) }()
		if len(items) == 0 {
			return nil
		}
		reads, skipped, err := l.unfoldReads(ctx, items)
		rep.Skipped = skipped
		if err != nil {
			return err
		}
		reread, skipped, err := l.rereadAll(ctx, reads, func(done, total int) error {
			return h.Heartbeat(ctx, float64(done)/float64(total), "reading files again")
		})
		rep.Reread, rep.Skipped = reread, append(rep.Skipped, skipped...)
		slices.Sort(rep.Reread)
		slices.Sort(rep.Skipped)
		return err
	}
}

// heartbeatEvery is how many files an unfold reads between heartbeats.
const heartbeatEvery = 50

// unfoldRead is a file an unfold reads again, the primary of each of items.
type unfoldRead struct {
	path  string
	lib   *model.Library
	items []model.PID
}

// unfoldReads groups items by the primary file an unfold reads for them, a cue rip's once
// for all the tracks it plays, from the catalog alone, and names the items with no file to
// read: none, or none in a library.
func (l *Library) unfoldReads(ctx context.Context, items []model.PID) ([]unfoldRead, []model.PID, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, nil, err
	}
	byPID := map[model.PID]*model.Library{}
	for _, lib := range libs {
		byPID[lib.PID] = lib
	}
	primaries, err := l.store.PrimaryFiles(ctx, items)
	if err != nil {
		return nil, nil, err
	}
	byFile := map[model.PID]int{}
	var reads []unfoldRead
	var skipped []model.PID
	for _, item := range items {
		f, ok := primaries[item]
		lib := byPID[f.LibraryPID]
		switch n, read := byFile[f.FilePID]; {
		case !ok || lib == nil:
			skipped = append(skipped, item)
		case read:
			reads[n].items = append(reads[n].items, item)
		default:
			byFile[f.FilePID] = len(reads)
			reads = append(reads, unfoldRead{path: string(f.Path), lib: lib, items: []model.PID{item}})
		}
	}
	return reads, skipped, nil
}

// rereadAll reads each file of reads again, beat heartbeating between them, and returns the
// items read and those skipped: the file's root does not answer (checked once per root,
// and a file under one that does not is never statted), or the file is gone or cannot be
// read. A cancel ends it with the cancel.
func (l *Library) rereadAll(ctx context.Context, reads []unfoldRead, beat func(done, total int) error) (reread, skipped []model.PID, err error) {
	const op = "Library.Unfold"
	gate := fsx.RootGate{Checker: l.roots}
	for i, r := range reads {
		if err := ctx.Err(); err != nil {
			return reread, skipped, waxerr.FromContext(op, err, waxerr.CodeIO)
		}
		if i%heartbeatEvery == 0 {
			if err := beat(i, len(reads)); err != nil {
				return reread, skipped, err
			}
		}
		_, err := gate.Stat(ctx, rawRoot(r.lib), r.path)
		if err == nil {
			_, _, err = l.scanner.ScanFileWith(ctx, r.lib, r.path, scan.FileOptions{ReResolve: true})
			if err != nil && ctx.Err() == nil {
				l.log.Warn("unfold: could not read a file again", "file", r.path, "err", err)
			}
		}
		if ctx.Err() != nil {
			return reread, skipped, waxerr.FromContext(op, ctx.Err(), waxerr.CodeIO)
		}
		if err != nil {
			skipped = append(skipped, r.items...)
			continue
		}
		reread = append(reread, r.items...)
	}
	return reread, skipped, nil
}
