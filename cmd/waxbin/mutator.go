package main

import (
	"bytes"
	"context"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playlist"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
)

// mutator is how a mutating command reaches the catalog: either a directly-opened
// Library or a proxy connection to a running server. It exposes exactly the
// operations the proxied commands need (the fast catalog mutations plus the reads
// those commands use for confirmation output), dispatching each to whichever
// backing is set. This keeps the command bodies identical whether they run locally
// or through the socket.
type mutator struct {
	lib *waxbin.Library // non-nil for a direct open
	px  *proxy.Client   // non-nil for a proxied connection
}

// Close releases the backing. For a direct open it closes the Library (releasing
// the write lock); for a proxy it closes the connection.
func (m *mutator) Close() error {
	if m.px != nil {
		return m.px.Close()
	}
	if m.lib != nil {
		return m.lib.Close()
	}
	return nil
}

func (m *mutator) EditFields(ctx context.Context, pid model.PID, edits map[string]string, opts waxbin.EditOptions) error {
	if m.px != nil {
		res, err := m.px.EditFields(ctx, pid, edits, opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force)
		if err != nil {
			return err
		}
		return writeBackErr(pid, edits, res.WriteBackFailures)
	}
	return m.lib.EditFields(ctx, pid, edits, opts)
}

func (m *mutator) EditManyFields(ctx context.Context, pids []model.PID, edits map[string]string, opts waxbin.EditOptions) (*waxbin.BatchEditResult, error) {
	if m.px != nil {
		res, err := m.px.EditManyFields(ctx, pids, edits, opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
		if err != nil {
			return nil, err
		}
		out := &waxbin.BatchEditResult{Edited: toPIDs(res.Edited), Skipped: toPIDs(res.Skipped)}
		out.WriteBackErrors = writeBackErrs(res.WriteBackFailures, func(string) map[string]string { return edits })
		return out, nil
	}
	return m.lib.EditManyFields(ctx, pids, edits, opts)
}

func (m *mutator) EditItemsFields(ctx context.Context, edits []model.ItemFieldEdit, opts waxbin.EditOptions) (*waxbin.BatchEditResult, error) {
	if m.px != nil {
		items := make([]proxy.ItemFieldsEdit, len(edits))
		fieldsByPID := make(map[string]map[string]string, len(edits))
		for i, e := range edits {
			items[i] = proxy.ItemFieldsEdit{ItemPID: string(e.ItemPID), Fields: e.Fields}
			fieldsByPID[string(e.ItemPID)] = e.Fields
		}
		res, err := m.px.EditBatch(ctx, items, opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
		if err != nil {
			return nil, err
		}
		out := &waxbin.BatchEditResult{Edited: toPIDs(res.Edited), Skipped: toPIDs(res.Skipped)}
		out.WriteBackErrors = writeBackErrs(res.WriteBackFailures, func(pid string) map[string]string {
			return fieldsByPID[pid]
		})
		return out, nil
	}
	return m.lib.EditItemsFields(ctx, edits, opts)
}

func (m *mutator) SetCredits(ctx context.Context, pid model.PID, role model.ContributorRole, names []string, opts waxbin.CreditEditOptions) (int, bool, error) {
	if m.px != nil {
		res, err := m.px.SetCredits(ctx, pid, string(role), names,
			opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
		if err != nil {
			return 0, false, err
		}
		return res.Stored, res.Skipped, writeBackErr(pid, nil, res.WriteBackFailures)
	}
	return m.lib.SetCredits(ctx, pid, role, names, opts)
}

func (m *mutator) SetCreditsBatch(ctx context.Context, edits []model.ItemCreditEdit, opts waxbin.CreditEditOptions) (*waxbin.CreditBatchResult, error) {
	if m.px != nil {
		items := make([]proxy.ItemCreditsEdit, len(edits))
		for i, e := range edits {
			items[i] = proxy.ItemCreditsEdit{ItemPID: string(e.ItemPID), Role: string(e.Role), Names: e.Names}
		}
		res, err := m.px.SetCreditsBatch(ctx, items, opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force, opts.SkipLocked)
		if err != nil {
			return nil, err
		}
		out := &waxbin.CreditBatchResult{
			Edited:  toCreditEdits(res.Edited),
			Skipped: toCreditEdits(res.Skipped),
		}
		out.WriteBackErrors = writeBackErrs(res.WriteBackFailures, nil)
		return out, nil
	}
	return m.lib.SetCreditsBatch(ctx, edits, opts)
}

// toCreditEdits converts wire credit entries back to the facade's own shape.
func toCreditEdits(items []proxy.ItemCreditsEdit) []model.ItemCreditEdit {
	if len(items) == 0 {
		return nil
	}
	out := make([]model.ItemCreditEdit, len(items))
	for i, it := range items {
		out[i] = model.ItemCreditEdit{
			ItemPID: model.PID(it.ItemPID), Role: model.ContributorRole(it.Role), Names: it.Names,
		}
	}
	return out
}

func (m *mutator) SetLyrics(ctx context.Context, pid model.PID, ly *model.Lyrics, lock model.LockChange, force bool) error {
	if m.px != nil {
		return m.px.SetLyrics(ctx, pid, ly, lock, force)
	}
	return m.lib.SetLyrics(ctx, pid, ly, lock, force)
}

func (m *mutator) SetChapters(ctx context.Context, pid model.PID, chapters []model.Chapter, lock model.LockChange, force bool) error {
	if m.px != nil {
		return m.px.SetChapters(ctx, pid, chapters, lock, force)
	}
	return m.lib.SetChapters(ctx, pid, chapters, lock, force)
}

func (m *mutator) SetItemArt(ctx context.Context, pid model.PID, role model.ArtRole, data []byte, opts waxbin.ArtEditOptions) error {
	if m.px != nil {
		res, err := m.px.SetItemArt(ctx, pid, role, data, opts.Format, opts.Attribution(), opts.Lock, opts.Force, opts.WriteBack)
		if err != nil {
			return err
		}
		return writeBackErr(pid, nil, res.WriteBackFailures)
	}
	return m.lib.SetItemArt(ctx, pid, role, data, opts)
}

func (m *mutator) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role model.ArtRole, data []byte, opts waxbin.ArtEditOptions) error {
	if m.px != nil {
		res, err := m.px.SetEntityArt(ctx, entityType, entityPID, role, data, opts.Format,
			opts.Attribution(), opts.Lock, opts.Force, opts.WriteBack)
		if err != nil {
			return err
		}
		return writeBackErr(entityPID, nil, res.WriteBackFailures)
	}
	return m.lib.SetEntityArt(ctx, entityType, entityPID, role, data, opts)
}

func (m *mutator) SetArtLock(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role model.ArtRole, lock bool) (model.ArtLockChange, error) {
	if m.px != nil {
		return m.px.SetArtLock(ctx, entityType, entityPID, role, lock)
	}
	return m.lib.SetArtLock(ctx, entityType, entityPID, role, lock)
}

func (m *mutator) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, opts waxbin.EntityEditOptions) (*model.EntityEditReport, error) {
	if m.px != nil {
		res, err := m.px.EditEntity(ctx, entityType, entityPID, edits, opts.WriteBack, opts.Attribution(), opts.Lock, opts.Force)
		if err != nil {
			return nil, err
		}
		rep := &model.EntityEditReport{MergedInto: model.PID(res.MergedInto)}
		return rep, writeBackErr(entityPID, edits, res.WriteBackFailures)
	}
	return m.lib.EditEntity(ctx, entityType, entityPID, edits, opts)
}

func (m *mutator) RenameEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID,
	fields map[string]string, opts waxbin.RenameOptions) (*model.EntityRenameReport, error) {
	if m.px != nil {
		res, err := m.px.RenameEntity(ctx, entityType, entityPID, fields, opts.WriteBack,
			opts.Attribution(), opts.Lock, opts.Force)
		if err != nil {
			return nil, err
		}
		rep := &model.EntityRenameReport{
			EntityPID: entityPID, Outcome: model.EntityRenameOutcome(res.Outcome),
			MergedInto: model.PID(res.MergedInto), MovedAlbums: toPIDs(res.MovedAlbums),
			Members: res.Members, Credits: res.Credits,
		}
		return rep, writeBackErr(entityPID, fields, res.WriteBackFailures)
	}
	return m.lib.RenameEntity(ctx, entityType, entityPID, fields, opts)
}

func (m *mutator) Detach(ctx context.Context, itemPID model.PID, opts waxbin.DetachOptions) (*model.DetachReport, error) {
	if m.px != nil {
		res, err := m.px.Detach(ctx, itemPID, opts.WriteBack)
		if err != nil {
			return nil, err
		}
		rep := &model.DetachReport{
			ItemPID: itemPID, OldAlbumPID: model.PID(res.OldAlbumPID),
			NewAlbumPID: model.PID(res.NewAlbumPID), NewReleaseGroupPID: model.PID(res.NewReleaseGroupPID),
		}
		return rep, writeBackErr(itemPID, nil, res.WriteBackFailures)
	}
	return m.lib.Detach(ctx, itemPID, opts)
}

func (m *mutator) SetAcquisition(ctx context.Context, itemPID model.PID, in model.AcquisitionInput, opts waxbin.AcquisitionEditOptions) error {
	if m.px != nil {
		res, err := m.px.SetAcquisition(ctx, itemPID, in, opts.Lock, opts.Force, opts.WriteBack)
		if err != nil {
			return err
		}
		return writeBackErr(itemPID, nil, res.WriteBackFailures)
	}
	return m.lib.SetAcquisition(ctx, itemPID, in, opts)
}

func (m *mutator) ClearAcquisition(ctx context.Context, itemPID model.PID, opts waxbin.AcquisitionEditOptions) error {
	if m.px != nil {
		res, err := m.px.ClearAcquisition(ctx, itemPID, opts.Lock, opts.Force, opts.WriteBack)
		if err != nil {
			return err
		}
		return writeBackErr(itemPID, nil, res.WriteBackFailures)
	}
	return m.lib.ClearAcquisition(ctx, itemPID, opts)
}

func (m *mutator) SetItemTag(ctx context.Context, itemPID model.PID, key string, values []string, opts waxbin.TagEditOptions) (string, int, error) {
	if m.px != nil {
		return m.px.SetTag(ctx, itemPID, key, values, opts.Attribution(), opts.Lock, opts.Force)
	}
	return m.lib.SetItemTag(ctx, itemPID, key, values, opts)
}

func (m *mutator) Provenance(ctx context.Context, pid model.PID) ([]model.FieldProvenance, error) {
	if m.px != nil {
		return m.px.Provenance(ctx, pid)
	}
	return m.lib.Provenance(ctx, pid)
}

func (m *mutator) Lock(ctx context.Context, pid model.PID, fields ...string) error {
	if m.px != nil {
		return m.px.Lock(ctx, pid, fields)
	}
	return m.lib.Lock(ctx, pid, fields...)
}

func (m *mutator) Unlock(ctx context.Context, pid model.PID, fields ...string) error {
	if m.px != nil {
		return m.px.Unlock(ctx, pid, fields)
	}
	return m.lib.Unlock(ctx, pid, fields...)
}

func (m *mutator) Users(ctx context.Context) ([]*model.User, error) {
	if m.px != nil {
		return m.px.Users(ctx)
	}
	return m.lib.Users(ctx)
}

func (m *mutator) CreateUser(ctx context.Context, name string) (*model.User, error) {
	if m.px != nil {
		return m.px.CreateUser(ctx, name)
	}
	return m.lib.CreateUser(ctx, name)
}

func (m *mutator) MergeMany(ctx context.Context, et model.MergeEntity, survivor model.PID, losers []model.PID) ([]*model.MergeReport, error) {
	if m.px != nil {
		return m.px.Merge(ctx, et, survivor, losers)
	}
	return m.lib.MergeMany(ctx, et, survivor, losers)
}

func (m *mutator) MarkMissing(ctx context.Context, itemPID model.PID, force bool) (model.MarkMissingOutcome, error) {
	if m.px != nil {
		return m.px.MarkMissing(ctx, itemPID, force)
	}
	return m.lib.MarkMissing(ctx, itemPID, waxbin.MarkMissingOptions{Force: force})
}

// The four star/rating mutations report whether they changed anything. The commands
// discard it: each re-reads and prints the resulting state, and a no-op is silent by
// convention.
func (m *mutator) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int, asOf *int64) (bool, error) {
	if m.px != nil {
		return m.px.SetRating(ctx, userPID, itemPID, rating, asOf)
	}
	return m.lib.Playback().SetRating(ctx, userPID, itemPID, rating, asOf)
}

func (m *mutator) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool, asOf *int64) (bool, error) {
	if m.px != nil {
		return m.px.SetStar(ctx, userPID, itemPID, starred, asOf)
	}
	return m.lib.Playback().SetStar(ctx, userPID, itemPID, starred, asOf)
}

func (m *mutator) SetEntityStar(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, starred bool, asOf *int64) (bool, error) {
	if m.px != nil {
		return m.px.SetEntityStar(ctx, userPID, kind, entityPID, starred, asOf)
	}
	return m.lib.SetEntityStar(ctx, userPID, kind, entityPID, starred, asOf)
}

func (m *mutator) SetEntityRating(ctx context.Context, userPID model.PID, kind model.MergeEntity, entityPID model.PID, rating *int, asOf *int64) (bool, error) {
	if m.px != nil {
		return m.px.SetEntityRating(ctx, userPID, kind, entityPID, rating, asOf)
	}
	return m.lib.SetEntityRating(ctx, userPID, kind, entityPID, rating, asOf)
}

func (m *mutator) SetPlayed(ctx context.Context, userPID, itemPID model.PID,
	played, finished bool, playCount *int, asOf *int64) (bool, error) {
	if m.px != nil {
		return m.px.SetPlayed(ctx, userPID, itemPID, played, finished, playCount, asOf)
	}
	return m.lib.Playback().SetPlayed(ctx, userPID, itemPID, played, finished, playCount, asOf)
}

func (m *mutator) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	if m.px != nil {
		return m.px.MarkPlayed(ctx, userPID, itemPID, finished)
	}
	return m.lib.Playback().MarkPlayed(ctx, userPID, itemPID, finished)
}

func (m *mutator) Checkpoint(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	if m.px != nil {
		return m.px.SetProgress(ctx, userPID, itemPID, positionMS)
	}
	return m.lib.Playback().Checkpoint(ctx, userPID, itemPID, positionMS)
}

func (m *mutator) PlayState(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error) {
	if m.px != nil {
		return m.px.PlayState(ctx, userPID, itemPID)
	}
	return m.lib.Playback().State(ctx, userPID, itemPID)
}

// PlaylistCreate creates a static playlist, or a smart one when rule is non-nil.
func (m *mutator) PlaylistCreate(ctx context.Context, name string, owner model.PID, vis model.PlaylistVisibility, rule *query.Query) (model.PID, error) {
	if m.px != nil {
		var doc []byte
		if rule != nil {
			var err error
			if doc, err = marshalRuleForWire(*rule); err != nil {
				return "", err
			}
		}
		return m.px.PlaylistCreate(ctx, name, owner, string(vis), doc)
	}
	if rule != nil {
		return m.lib.Playlists().CreateSmart(ctx, name, owner, vis, *rule)
	}
	return m.lib.Playlists().CreateStatic(ctx, name, owner, vis)
}

func (m *mutator) PlaylistDelete(ctx context.Context, playlistPID model.PID) error {
	if m.px != nil {
		return m.px.PlaylistDelete(ctx, playlistPID)
	}
	return m.lib.Playlists().Delete(ctx, playlistPID)
}

func (m *mutator) PlaylistRename(ctx context.Context, playlistPID model.PID, name string) error {
	if m.px != nil {
		return m.px.PlaylistRename(ctx, playlistPID, name)
	}
	return m.lib.Playlists().Rename(ctx, playlistPID, name)
}

// PlaylistImportM3U8 imports an M3U8 document as a new static playlist. The
// document is read whole rather than streamed: the proxied form has to put it in
// a frame anyway, and a playlist file is small.
func (m *mutator) PlaylistImportM3U8(ctx context.Context, name string, owner model.PID, vis model.PlaylistVisibility, doc []byte) (*playlist.ImportResult, error) {
	if m.px != nil {
		res, err := m.px.PlaylistImportM3U8(ctx, name, owner, string(vis), doc)
		if err != nil {
			return nil, err
		}
		return &playlist.ImportResult{
			PlaylistPID: model.PID(res.PlaylistPID), Matched: res.Matched,
			Unmatched: res.Unmatched, UnmatchedPaths: res.UnmatchedPaths,
		}, nil
	}
	return m.lib.Playlists().ImportM3U8(ctx, name, owner, vis, bytes.NewReader(doc))
}

func (m *mutator) PlaylistAdd(ctx context.Context, playlistPID model.PID, itemPIDs ...model.PID) error {
	if m.px != nil {
		return m.px.PlaylistAdd(ctx, playlistPID, itemPIDs)
	}
	return m.lib.Playlists().Add(ctx, playlistPID, itemPIDs...)
}

func (m *mutator) PlaylistRemove(ctx context.Context, playlistPID, itemPID model.PID) error {
	if m.px != nil {
		return m.px.PlaylistRemove(ctx, playlistPID, itemPID)
	}
	return m.lib.Playlists().Remove(ctx, playlistPID, itemPID)
}

func (m *mutator) PlaylistRemoveAt(ctx context.Context, playlistPID model.PID, position int) error {
	if m.px != nil {
		return m.px.PlaylistRemoveAt(ctx, playlistPID, position)
	}
	return m.lib.Playlists().RemoveAt(ctx, playlistPID, position)
}

func (m *mutator) PlaylistSetRule(ctx context.Context, playlistPID model.PID, rule query.Query) error {
	if m.px != nil {
		data, err := marshalRuleForWire(rule)
		if err != nil {
			return err
		}
		return m.px.PlaylistSetRule(ctx, playlistPID, data)
	}
	return m.lib.Playlists().SetRule(ctx, playlistPID, rule)
}

// marshalRuleForWire encodes a rule for a proxied playlist call. The wrap is what
// keeps the op honest: envelope.Wrap stamps its own, so a rule the codec cannot
// encode would otherwise surface as an envelope failure with no sign of which
// command asked for it.
func marshalRuleForWire(rule query.Query) ([]byte, error) {
	data, err := query.MarshalRule(rule)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeInternal, "playlist", err)
	}
	return data, nil
}

func (m *mutator) PutTranscript(ctx context.Context, in model.PutTranscriptInput) error {
	if m.px != nil {
		return m.px.PutTranscript(ctx, in.EpisodePID, in.Format, []byte(in.Body), in.SourceURL)
	}
	return m.lib.Podcasts().PutTranscript(ctx, in)
}

func (m *mutator) FetchTranscript(ctx context.Context, episodePID model.PID) error {
	if m.px != nil {
		return m.px.FetchTranscript(ctx, episodePID)
	}
	return m.lib.Podcasts().FetchTranscript(ctx, episodePID)
}

func (m *mutator) Unfetch(ctx context.Context, episodePID model.PID) (*podcast.UnfetchResult, error) {
	if m.px != nil {
		res, err := m.px.Unfetch(ctx, episodePID)
		if err != nil {
			return nil, err
		}
		return &podcast.UnfetchResult{
			EpisodePID: episodePID, Unfetched: res.Unfetched, ReclaimedBytes: res.ReclaimedBytes,
		}, nil
	}
	return m.lib.Podcasts().Unfetch(ctx, episodePID)
}

func (m *mutator) PodcastRemove(ctx context.Context, podcastPID model.PID) error {
	if m.px != nil {
		return m.px.PodcastRemove(ctx, podcastPID)
	}
	return m.lib.Podcasts().Remove(ctx, podcastPID)
}

func (m *mutator) AddRoot(ctx context.Context, spec config.Root) (*model.Library, error) {
	if m.px != nil {
		return m.px.AddRoot(ctx, proxy.AddRootParams{
			Path: spec.Path, Mode: string(spec.Mode), Media: string(spec.Media), Profile: spec.Profile,
		})
	}
	return m.lib.AddRoot(ctx, spec)
}

// toPIDs converts a wire string slice into a PID slice.
func toPIDs(ss []string) []model.PID {
	if len(ss) == 0 {
		return nil
	}
	out := make([]model.PID, len(ss))
	for i, s := range ss {
		out[i] = model.PID(s)
	}
	return out
}

// fromProxyFailures converts wire write-back failures back into the facade shape.
// writeBackErr rebuilds the typed error a proxied write-back's failures stand for, or nil
// when every file was written. It is the one place a proxied command turns a partial
// on-disk sync back into what the local path raises, so the CLI reports the same thing
// either way: catalog updated, tags not followed. edits is the field map to record on the
// error, nil for a surface that has none.
func writeBackErr(pid model.PID, edits map[string]string, failures []proxy.WriteBackFailure) error {
	if len(failures) == 0 {
		return nil
	}
	return &waxbin.WriteBackError{ItemPID: pid, Edits: edits, Failures: fromProxyFailures(failures)}
}

// writeBackErrs is the batch form: the per-item errors a proxied batch's failure map
// stands for, nil when it is empty. editsOf names the edit map to record against one
// item, and is nil for a surface with none (a credit carries its names on the entry
// rather than in a field map).
func writeBackErrs(failures map[string][]proxy.WriteBackFailure,
	editsOf func(string) map[string]string) map[model.PID]*waxbin.WriteBackError {
	if len(failures) == 0 {
		return nil
	}
	out := make(map[model.PID]*waxbin.WriteBackError, len(failures))
	for pid, fails := range failures {
		var edits map[string]string
		if editsOf != nil {
			edits = editsOf(pid)
		}
		out[model.PID(pid)] = &waxbin.WriteBackError{
			ItemPID: model.PID(pid), Edits: edits, Failures: fromProxyFailures(fails),
		}
	}
	return out
}

func fromProxyFailures(failures []proxy.WriteBackFailure) []waxbin.WriteBackFailure {
	out := make([]waxbin.WriteBackFailure, len(failures))
	for i, f := range failures {
		out[i] = waxbin.WriteBackFailure{FilePID: model.PID(f.FilePID), Path: f.Path, Reason: f.Reason}
	}
	return out
}

// userLister is the read a command needs to map a --user name to a pid. Both the
// Library and the mutator satisfy it, so resolveUser works on either.
type userLister interface {
	Users(ctx context.Context) ([]*model.User, error)
}

// provenanceReader is the read reportProvenance needs, satisfied by both the
// Library and the mutator.
type provenanceReader interface {
	Provenance(ctx context.Context, pid model.PID) ([]model.FieldProvenance, error)
}
