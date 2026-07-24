package waxbin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// The Library satisfies the proxy server's maintenance hook. Asserted here so a
// signature drift is a compile error at the wiring seam.
var _ proxy.Maintainer = (*Library)(nil)

// Serve runs the local control server: it listens on the unix socket at
// socketPath and dispatches proxied catalog mutations (and the reads a mutating
// CLI command needs for its confirmation output) to this Library, plus the
// maintenance-mode hand-off. It blocks until ctx is canceled, then returns nil.
//
// The socket is created owner-only (0600); the endpoint drives admin mutations, so
// a broader mode would let any local user issue unauthenticated writes. socketPath
// should match the Options.IPCSocket advertised in the lockfile so a CLI can
// discover it. Serve refuses on a read-only library.
func (l *Library) Serve(ctx context.Context, socketPath string) error {
	const op = "waxbin.Serve"
	if l.ReadOnly() {
		return waxerr.New(waxerr.CodeUnsupported, op, "serve requires a read-write library")
	}
	if strings.TrimSpace(socketPath) == "" {
		return waxerr.New(waxerr.CodeInvalid, op, "no socket path")
	}
	ln, err := proxy.Listen(socketPath)
	if err != nil {
		return err
	}
	srv := proxy.NewServer(l.proxyHandlers(), l, l.log)
	return srv.Serve(ctx, ln)
}

// BeginMaintenance suspends the Library and releases the write lock so a foreground
// process can take it. It implements proxy.Maintainer; the Library object is kept
// so EndMaintenance can restore it in place.
//
// It refuses while a background job is running: server-run scan/analyze/enrich/
// organize passes run in this process, and closing the store out from under one
// would abort it partway. The foreground caller gets a CodeConflict and should
// retry once the job completes. Unlike Close, it suspends (keeping in-process change
// subscribers alive) so an embedder's subscription survives the hand-off.
func (l *Library) BeginMaintenance(ctx context.Context) error {
	if running, err := l.store.HasRunningJob(ctx); err != nil {
		return err
	} else if running {
		return waxerr.New(waxerr.CodeConflict, "waxbin.BeginMaintenance",
			"a background job is running; retry after it completes")
	}
	l.log.Info("entering maintenance mode: releasing the write lock")
	// Flush buffered playback like Close does, then suspend (preserving subscribers).
	if l.playback != nil && !l.ReadOnly() {
		_ = l.playback.Flush(context.Background())
	}
	return l.store.Suspend()
}

// EndMaintenance reopens the Library after a maintenance hand-off. It implements
// proxy.Maintainer.
func (l *Library) EndMaintenance(ctx context.Context) error {
	if err := l.Reopen(ctx); err != nil {
		l.log.Error("leaving maintenance mode: reopen failed", "err", err)
		return err
	}
	l.log.Info("left maintenance mode: reacquired the write lock")
	return nil
}

// Reopen restores a Library that was Closed for a maintenance-mode hand-off. It
// reopens the store in place (every subsystem keeps its store handle) and
// re-ensures the configured roots, mirroring a read-write Open. It refuses on a
// read-only library.
func (l *Library) Reopen(ctx context.Context) error {
	if l.ReadOnly() {
		return waxerr.New(waxerr.CodeUnsupported, "waxbin.Reopen", "reopen requires a read-write library")
	}
	if err := l.store.Reopen(ctx); err != nil {
		return err
	}
	return l.ensureRoots(ctx)
}

// ReadLockOwner reads the write-owner metadata from a catalog's lockfile without
// opening the catalog. It is how a CLI discovers whether a server is running and
// on which socket, before deciding to proxy a mutation. It returns an error when
// the lockfile is absent or unreadable (no live owner).
func ReadLockOwner(dbPath string) (sqlite.OwnerInfo, error) {
	return sqlite.ReadOwnerInfo(dbPath + ".waxlock")
}

// proxyHandlers builds the map of method names to handlers the proxy server
// dispatches. Each handler unmarshals its params, calls the matching Library
// operation, and returns a value to marshal into the response. Handlers run
// concurrently across connections; the Library is safe for concurrent use.
func (l *Library) proxyHandlers() map[string]proxy.Handler {
	return map[string]proxy.Handler{
		proxy.MethodEditFields: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.EditFieldsParams](raw)
			if err != nil {
				return nil, err
			}
			editErr := l.EditFields(ctx, model.PID(p.ItemPID), p.Edits,
				EditOptions{WriteBack: p.WriteBack, Lock: p.Lock, Force: p.Force})
			// A write-back failure is a result, not a transport error: the catalog edit
			// committed and only the on-disk tags did not follow. Return the failures in
			// the response so the client rebuilds the same typed error the local path does.
			var wbErr *WriteBackError
			if errors.As(editErr, &wbErr) {
				return proxy.EditFieldsResult{WriteBackFailures: toProxyFailures(wbErr.Failures)}, nil
			}
			if editErr != nil {
				return nil, editErr
			}
			return proxy.EditFieldsResult{}, nil
		},
		proxy.MethodEditManyFields: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.EditManyFieldsParams](raw)
			if err != nil {
				return nil, err
			}
			pids := make([]model.PID, len(p.ItemPIDs))
			for i, s := range p.ItemPIDs {
				pids[i] = model.PID(s)
			}
			res, err := l.EditManyFields(ctx, pids, p.Edits,
				EditOptions{WriteBack: p.WriteBack, Lock: p.Lock, Force: p.Force, SkipLocked: p.SkipLocked})
			if err != nil {
				return nil, err
			}
			return toProxyBatchResult(res), nil
		},
		proxy.MethodEditBatch: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.EditBatchParams](raw)
			if err != nil {
				return nil, err
			}
			edits := make([]model.ItemFieldEdit, len(p.Items))
			for i, it := range p.Items {
				edits[i] = model.ItemFieldEdit{ItemPID: model.PID(it.ItemPID), Fields: it.Fields}
			}
			res, err := l.EditItemsFields(ctx, edits,
				EditOptions{WriteBack: p.WriteBack, Lock: p.Lock, Force: p.Force, SkipLocked: p.SkipLocked})
			if err != nil {
				return nil, err
			}
			return toProxyBatchResult(res), nil
		},
		proxy.MethodSetCredits: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetCreditsParams](raw)
			if err != nil {
				return nil, err
			}
			stored, editErr := l.SetCredits(ctx, model.PID(p.ItemPID), model.ContributorRole(p.Role), p.Names,
				CreditEditOptions{WriteBack: p.WriteBack, Lock: p.Lock, Force: p.Force})
			// A write-back failure is a result, not a transport error (the catalog edit stands).
			var wbErr *WriteBackError
			if errors.As(editErr, &wbErr) {
				return proxy.SetCreditsResult{Stored: stored, WriteBackFailures: toProxyFailures(wbErr.Failures)}, nil
			}
			if editErr != nil {
				return nil, editErr
			}
			return proxy.SetCreditsResult{Stored: stored}, nil
		},
		proxy.MethodSetLyrics: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetLyricsParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.SetLyrics(ctx, model.PID(p.ItemPID), p.Lyrics, p.Lock, p.Force)
		},
		proxy.MethodSetChapters: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetChaptersParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.SetChapters(ctx, model.PID(p.ItemPID), p.Chapters, p.Lock, p.Force)
		},
		proxy.MethodSetItemArt: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetItemArtParams](raw)
			if err != nil {
				return nil, err
			}
			// The wire carries a string; parse at the boundary ("" = front) so an
			// unknown role rejects here rather than minting an unreachable slot.
			role, ok := model.ParseArtRole(p.Role)
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, "serve.set_item_art", "unknown art role: "+p.Role)
			}
			artErr := l.SetItemArt(ctx, model.PID(p.ItemPID), role, p.Data, p.Lock, p.Force, p.WriteBack)
			// A write-back failure is a result, not a transport error (the catalog edit stands).
			var wbErr *WriteBackError
			if errors.As(artErr, &wbErr) {
				return proxy.SetItemArtResult{WriteBackFailures: toProxyFailures(wbErr.Failures)}, nil
			}
			if artErr != nil {
				return nil, artErr
			}
			return proxy.SetItemArtResult{}, nil
		},
		proxy.MethodSetEntityArt: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetEntityArtParams](raw)
			if err != nil {
				return nil, err
			}
			role, ok := model.ParseArtRole(p.Role)
			if !ok {
				return nil, waxerr.New(waxerr.CodeInvalid, "serve.set_entity_art", "unknown art role: "+p.Role)
			}
			artErr := l.SetEntityArt(ctx, model.ArtEntity(p.EntityType), model.PID(p.EntityPID), role, p.Data, p.WriteBack)
			var wbErr *WriteBackError
			if errors.As(artErr, &wbErr) {
				return proxy.SetEntityArtResult{WriteBackFailures: toProxyFailures(wbErr.Failures)}, nil
			}
			if artErr != nil {
				return nil, artErr
			}
			return proxy.SetEntityArtResult{}, nil
		},
		proxy.MethodEditEntity: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.EditEntityParams](raw)
			if err != nil {
				return nil, err
			}
			editErr := l.EditEntity(ctx, model.MergeEntity(p.EntityType), model.PID(p.EntityPID), p.Edits,
				EntityEditOptions{WriteBack: p.WriteBack, Lock: p.Lock, Force: p.Force})
			var wbErr *WriteBackError
			if errors.As(editErr, &wbErr) {
				return proxy.EditEntityResult{WriteBackFailures: toProxyFailures(wbErr.Failures)}, nil
			}
			if editErr != nil {
				return nil, editErr
			}
			return proxy.EditEntityResult{}, nil
		},
		proxy.MethodSetTag: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.SetTagParams](raw)
			if err != nil {
				return nil, err
			}
			key, stored, err := l.SetItemTag(ctx, model.PID(p.ItemPID), p.Key, p.Values, TagEditOptions{Lock: p.Lock, Force: p.Force})
			if err != nil {
				return nil, err
			}
			return proxy.SetTagResult{Key: key, Stored: stored}, nil
		},
		proxy.MethodLock: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.FieldsParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.Lock(ctx, model.PID(p.ItemPID), p.Fields...)
		},
		proxy.MethodUnlock: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.FieldsParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.Unlock(ctx, model.PID(p.ItemPID), p.Fields...)
		},
		proxy.MethodCreateUser: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.CreateUserParams](raw)
			if err != nil {
				return nil, err
			}
			return l.CreateUser(ctx, p.Name)
		},
		proxy.MethodUsers: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return l.Users(ctx)
		},
		proxy.MethodMerge: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.MergeParams](raw)
			if err != nil {
				return nil, err
			}
			losers := make([]model.PID, len(p.Losers))
			for i, s := range p.Losers {
				losers[i] = model.PID(s)
			}
			return l.MergeMany(ctx, model.MergeEntity(p.EntityType), model.PID(p.Survivor), losers)
		},
		proxy.MethodSetRating: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.RatingParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playback.SetRating(ctx, model.PID(p.UserPID), model.PID(p.ItemPID), p.Rating, proxy.AsOf(p.AsOfNS))
		},
		proxy.MethodSetStar: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.StarParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playback.SetStar(ctx, model.PID(p.UserPID), model.PID(p.ItemPID), p.Starred, proxy.AsOf(p.AsOfNS))
		},
		proxy.MethodMarkPlayed: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PlayedParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playback.MarkPlayed(ctx, model.PID(p.UserPID), model.PID(p.ItemPID), p.Finished)
		},
		proxy.MethodSetProgress: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.ProgressParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playback.Checkpoint(ctx, model.PID(p.UserPID), model.PID(p.ItemPID), p.PositionMS)
		},
		proxy.MethodPlayState: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.StateParams](raw)
			if err != nil {
				return nil, err
			}
			return l.playback.State(ctx, model.PID(p.UserPID), model.PID(p.ItemPID))
		},
		proxy.MethodProvenance: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.ItemParams](raw)
			if err != nil {
				return nil, err
			}
			return l.Provenance(ctx, model.PID(p.ItemPID))
		},
		proxy.MethodPlaylistAdd: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PlaylistAddParams](raw)
			if err != nil {
				return nil, err
			}
			items := make([]model.PID, len(p.ItemPIDs))
			for i, s := range p.ItemPIDs {
				items[i] = model.PID(s)
			}
			return nil, l.playlists.Add(ctx, model.PID(p.PlaylistPID), items...)
		},
		proxy.MethodPlaylistRemove: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PlaylistRemoveParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playlists.Remove(ctx, model.PID(p.PlaylistPID), model.PID(p.ItemPID))
		},
		proxy.MethodPlaylistRemoveAt: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PlaylistRemoveAtParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.playlists.RemoveAt(ctx, model.PID(p.PlaylistPID), p.Position)
		},
		proxy.MethodPlaylistSetRule: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PlaylistSetRuleParams](raw)
			if err != nil {
				return nil, err
			}
			q, err := query.ParseRule(p.Rule)
			if err != nil {
				return nil, err
			}
			return nil, l.playlists.SetRule(ctx, model.PID(p.PlaylistPID), q)
		},
		proxy.MethodPutTranscript: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.PutTranscriptParams](raw)
			if err != nil {
				return nil, err
			}
			return nil, l.podcasts.PutTranscript(ctx, model.PutTranscriptInput{
				EpisodePID: model.PID(p.EpisodePID), Format: p.Format,
				Body: string(p.Body), SourceURL: p.SourceURL,
			})
		},
		proxy.MethodFetchTranscript: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.FetchTranscriptParams](raw)
			if err != nil {
				return nil, err
			}
			// The fetch runs in this (server) process, under its network policy.
			return nil, l.podcasts.FetchTranscript(ctx, model.PID(p.EpisodePID))
		},
		proxy.MethodAddRoot: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.AddRootParams](raw)
			if err != nil {
				return nil, err
			}
			// AddRoot validates the spec (mode/media vocabulary, overlaps) against
			// this server's registered set, which is the catalog that matters: this
			// process is the one that scans.
			return l.AddRoot(ctx, config.Root{
				Path: p.Path, Mode: model.Mode(p.Mode), Media: model.MediaType(p.Media), Profile: p.Profile,
			})
		},
		proxy.MethodRunScan: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.ScanParams](raw)
			if err != nil {
				return nil, err
			}
			// ctx is the server's lifetime context, so the job outlives this request and
			// runs in-process while the server stays available.
			pid, err := l.StartScan(ctx, ScanRequest{
				LibraryPID: model.PID(p.LibraryPID), SubPath: p.SubPath, Force: p.Force,
				AdoptStampedPIDs: p.AdoptStampedPIDs, ForceReconcile: p.ForceReconcile,
				IgnoreLocks: p.IgnoreLocks,
			})
			if err != nil {
				return nil, err
			}
			return proxy.JobStartResult{JobPID: string(pid)}, nil
		},
		proxy.MethodRunAnalyze: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.AnalyzeParams](raw)
			if err != nil {
				return nil, err
			}
			pid, err := l.StartAnalyze(ctx, AnalyzeOptions{WriteReplayGainTags: p.WriteReplayGainTags})
			if err != nil {
				return nil, err
			}
			return proxy.JobStartResult{JobPID: string(pid)}, nil
		},
		proxy.MethodRunEnrich: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.EnrichParams](raw)
			if err != nil {
				return nil, err
			}
			pid, err := l.StartEnrich(ctx, EnrichOptions{
				Force: p.Force, Limit: p.Limit,
				ItemPID: model.PID(p.ItemPID), EntityType: read.EntityKind(p.EntityType), EntityPID: model.PID(p.EntityPID),
			})
			if err != nil {
				return nil, err
			}
			return proxy.JobStartResult{JobPID: string(pid)}, nil
		},
		proxy.MethodRunOrganize: func(ctx context.Context, raw json.RawMessage) (any, error) {
			p, err := decodeParams[proxy.OrganizeParams](raw)
			if err != nil {
				return nil, err
			}
			q, err := query.ParseRule(p.Rule)
			if err != nil {
				return nil, err
			}
			pid, err := l.RunOrganize(ctx, q, p.Profile)
			if err != nil {
				return nil, err
			}
			return proxy.JobStartResult{JobPID: string(pid)}, nil
		},
	}
}

// decodeParams unmarshals a request's params frame into T, mapping a malformed
// frame to a CodeInvalid error.
func decodeParams[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, waxerr.Wrap(waxerr.CodeInvalid, "waxbin.proxy", err)
	}
	return v, nil
}

// toProxyFailures converts facade write-back failures into their wire form.
func toProxyFailures(failures []WriteBackFailure) []proxy.WriteBackFailure {
	out := make([]proxy.WriteBackFailure, len(failures))
	for i, f := range failures {
		out[i] = proxy.WriteBackFailure{FilePID: string(f.FilePID), Path: f.Path, Reason: f.Reason}
	}
	return out
}

// toProxyBatchResult converts a facade batch-edit result into its wire form,
// shared by the edit_many_fields and edit_batch handlers.
func toProxyBatchResult(res *BatchEditResult) proxy.EditManyFieldsResult {
	out := proxy.EditManyFieldsResult{Edited: pidStrings(res.Edited), Skipped: pidStrings(res.Skipped)}
	if len(res.WriteBackErrors) > 0 {
		out.WriteBackFailures = make(map[string][]proxy.WriteBackFailure, len(res.WriteBackErrors))
		for pid, wbe := range res.WriteBackErrors {
			out.WriteBackFailures[string(pid)] = toProxyFailures(wbe.Failures)
		}
	}
	return out
}

// pidStrings converts a PID slice to a plain string slice for a wire payload.
func pidStrings(pids []model.PID) []string {
	if len(pids) == 0 {
		return nil
	}
	out := make([]string, len(pids))
	for i, p := range pids {
		out[i] = string(p)
	}
	return out
}
