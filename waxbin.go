package waxbin

import (
	"context"
	"io"
	"log/slog"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/jobs"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// Library is the public handle to a WaxBin catalog. It is safe for concurrent
// use. A read-only Library refuses mutating operations.
type Library struct {
	store     *sqlite.Store
	jobs      *jobs.Manager
	scanner   *scan.Scanner
	organizer *organize.Organizer
	log       *slog.Logger
	opts      Options
}

// Open opens (creating if needed) the catalog and wires the subsystems. A
// read-write open acquires the write lock, migrates, reclaims orphaned jobs, and
// ensures the configured roots; a read-only open does none of those.
func Open(ctx context.Context, opts Options) (*Library, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	owner := opts.WriteOwner
	if owner == "" {
		owner = defaultOwner()
	}

	// Validate and normalize roots here so embedders get the same root isolation
	// as the CLI. Overlapping roots could otherwise let organize move files from
	// an in-place library.
	cfg := &config.Config{DBPath: opts.DBPath, Roots: opts.Roots}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts.DBPath, opts.Roots = cfg.DBPath, cfg.Roots

	st, err := sqlite.Open(ctx, sqlite.OpenOptions{
		Path:          opts.DBPath,
		ReadOnly:      opts.ReadOnly,
		Owner:         owner,
		IPCSocket:     opts.IPCSocket,
		Logger:        log,
		BusyTimeoutMS: opts.BusyTimeoutMS,
		CacheSizeKB:   opts.CacheSizeKB,
		MmapSizeBytes: opts.MmapSizeBytes,
		ReadPoolSize:  opts.ReadPoolSize,
	})
	if err != nil {
		return nil, err
	}

	l := &Library{
		store:     st,
		jobs:      jobs.NewManager(st, owner, log),
		scanner:   scan.New(st, meta.NewReader(), log),
		organizer: organize.New(st, log),
		log:       log,
		opts:      opts,
	}

	if !opts.ReadOnly {
		if err := l.ensureRoots(ctx); err != nil {
			_ = st.Close()
			return nil, err
		}
	}
	return l, nil
}

// Close releases the catalog and the write lock.
func (l *Library) Close() error { return l.store.Close() }

// ReadOnly reports whether the library was opened read-only.
func (l *Library) ReadOnly() bool { return l.store.ReadOnly() }

func (l *Library) ensureRoots(ctx context.Context) error {
	for _, r := range l.opts.Roots {
		if _, err := l.store.EnsureLibrary(ctx, &model.Library{
			Root:        []byte(r.Path),
			DisplayRoot: r.Path,
			Mode:        r.Mode,
			Profile:     r.Profile,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Libraries lists registered library roots.
func (l *Library) Libraries(ctx context.Context) ([]*model.Library, error) {
	return l.store.Libraries(ctx)
}

// Query runs a compiled selection and returns matching item views.
func (l *Library) Query(ctx context.Context, q query.Query) ([]*model.ItemView, error) {
	return l.store.QueryItems(ctx, q)
}

// Count returns the number of items matching q.
func (l *Library) Count(ctx context.Context, q query.Query) (int, error) {
	return l.store.CountItems(ctx, q)
}

// Get returns a single item by public id.
func (l *Library) Get(ctx context.Context, pid model.PID) (*model.ItemView, error) {
	return l.store.ItemByPID(ctx, pid)
}

// Changes returns change_log rows after seq.
func (l *Library) Changes(ctx context.Context, sinceSeq int64) ([]model.Change, error) {
	return l.store.ChangesSince(ctx, sinceSeq)
}

// Jobs lists recent jobs, newest first.
func (l *Library) Jobs(ctx context.Context, limit int) ([]*model.Job, error) {
	return l.jobs.List(ctx, limit)
}

// OwnerInfo returns the current write-owner metadata from the lockfile.
func (l *Library) OwnerInfo() (sqlite.OwnerInfo, error) { return l.store.OwnerInfo() }

// ScanRequest selects what to scan.
type ScanRequest struct {
	LibraryPID model.PID // empty scans every library
	SubPath    string    // optional sub-path under a single library's root
}

// ScanResult reports a scan, including the job it ran under.
type ScanResult struct {
	JobPID model.PID
	Total  scan.Result
	Runs   []scan.Result
}

// Scan indexes the selected libraries under a single "scan"-scoped job.
func (l *Library) Scan(ctx context.Context, req ScanRequest) (*ScanResult, error) {
	libs, err := l.resolveLibraries(ctx, req.LibraryPID)
	if err != nil {
		return nil, err
	}
	out := &ScanResult{}
	job, runErr := l.jobs.Run(ctx, "scan", "scan", func(ctx context.Context, h *jobs.Handle) error {
		for _, lib := range libs {
			r, err := l.scanner.Scan(ctx, scan.Request{Library: lib, SubPath: req.SubPath},
				func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
			if err != nil {
				return err
			}
			out.Runs = append(out.Runs, *r)
			addResult(&out.Total, r)
		}
		return nil
	})
	if job != nil {
		out.JobPID = job.PID
	}
	return out, runErr
}

// PlanOrganize computes a dry-run move plan for the selected items under a
// profile. This build organizes one managed library at a time.
func (l *Library) PlanOrganize(ctx context.Context, q query.Query, profileName string) (*organize.Plan, error) {
	lib, err := l.singleManagedLibrary(ctx)
	if err != nil {
		return nil, err
	}
	// Default to the library's configured profile so a root registered
	// `:managed:plex-music` lays out as plex-music without repeating --profile.
	if profileName == "" {
		profileName = lib.Profile
	}
	prof, err := organize.ProfileByName(profileName)
	if err != nil {
		return nil, err
	}
	items, err := l.store.QueryItems(ctx, q)
	if err != nil {
		return nil, err
	}
	return l.organizer.Plan(lib, prof, items)
}

// ApplyOrganize executes a plan under an "organize"-scoped job.
func (l *Library) ApplyOrganize(ctx context.Context, plan *organize.Plan) (*organize.Report, error) {
	var rep *organize.Report
	_, err := l.jobs.Run(ctx, "organize", "organize", func(ctx context.Context, h *jobs.Handle) error {
		r, err := l.organizer.Execute(ctx, plan, h.JobPID(),
			func(p float64, msg string) error { return h.Heartbeat(ctx, p, msg) })
		rep = r
		return err
	})
	return rep, err
}

func (l *Library) resolveLibraries(ctx context.Context, pid model.PID) ([]*model.Library, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	if pid == "" {
		if len(libs) == 0 {
			return nil, waxerr.New(waxerr.CodeInvalid, "Library.Scan", "no library roots configured")
		}
		return libs, nil
	}
	for _, lib := range libs {
		if lib.PID == pid {
			return []*model.Library{lib}, nil
		}
	}
	return nil, waxerr.New(waxerr.CodeNotFound, "Library.Scan", "no such library: "+string(pid))
}

func (l *Library) singleManagedLibrary(ctx context.Context) (*model.Library, error) {
	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	var managed []*model.Library
	for _, lib := range libs {
		if lib.Mode == model.ModeManaged {
			managed = append(managed, lib)
		}
	}
	switch len(managed) {
	case 1:
		return managed[0], nil
	case 0:
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.Organize", "no managed library to organize")
	default:
		return nil, waxerr.New(waxerr.CodeInvalid, "Library.Organize",
			"this build organizes a single managed library; multiple are configured")
	}
}

func addResult(dst *scan.Result, src *scan.Result) {
	dst.FilesSeen += src.FilesSeen
	dst.AudioFiles += src.AudioFiles
	dst.ItemsCreated += src.ItemsCreated
	dst.ItemsUpdated += src.ItemsUpdated
	dst.Relinked += src.Relinked
	dst.Skipped += src.Skipped
	dst.Errored += src.Errored
}
