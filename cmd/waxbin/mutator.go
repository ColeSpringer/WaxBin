package main

import (
	"context"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
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
		res, err := m.px.EditFields(ctx, pid, edits, opts.WriteBack, opts.Lock, opts.Force)
		if err != nil {
			return err
		}
		// Rebuild the typed write-back error so the CLI reports a partial on-disk sync
		// exactly as the local path does (catalog updated, tags not followed).
		if len(res.WriteBackFailures) > 0 {
			return &waxbin.WriteBackError{ItemPID: pid, Edits: edits, Failures: fromProxyFailures(res.WriteBackFailures)}
		}
		return nil
	}
	return m.lib.EditFields(ctx, pid, edits, opts)
}

func (m *mutator) EditManyFields(ctx context.Context, pids []model.PID, edits map[string]string, opts waxbin.EditOptions) (*waxbin.BatchEditResult, error) {
	if m.px != nil {
		res, err := m.px.EditManyFields(ctx, pids, edits, opts.WriteBack, opts.Lock, opts.Force, opts.SkipLocked)
		if err != nil {
			return nil, err
		}
		out := &waxbin.BatchEditResult{Edited: toPIDs(res.Edited), Skipped: toPIDs(res.Skipped)}
		if len(res.WriteBackFailures) > 0 {
			out.WriteBackErrors = make(map[model.PID]*waxbin.WriteBackError, len(res.WriteBackFailures))
			for pid, fails := range res.WriteBackFailures {
				out.WriteBackErrors[model.PID(pid)] = &waxbin.WriteBackError{
					ItemPID: model.PID(pid), Edits: edits, Failures: fromProxyFailures(fails),
				}
			}
		}
		return out, nil
	}
	return m.lib.EditManyFields(ctx, pids, edits, opts)
}

func (m *mutator) SetCredits(ctx context.Context, pid model.PID, role model.ContributorRole, names []string, opts waxbin.CreditEditOptions) (int, error) {
	if m.px != nil {
		res, err := m.px.SetCredits(ctx, pid, string(role), names, opts.WriteBack, opts.Lock, opts.Force)
		if err != nil {
			return 0, err
		}
		if len(res.WriteBackFailures) > 0 {
			return res.Stored, &waxbin.WriteBackError{ItemPID: pid, Failures: fromProxyFailures(res.WriteBackFailures)}
		}
		return res.Stored, nil
	}
	return m.lib.SetCredits(ctx, pid, role, names, opts)
}

func (m *mutator) SetLyrics(ctx context.Context, pid model.PID, ly *model.Lyrics, lock, force bool) error {
	if m.px != nil {
		return m.px.SetLyrics(ctx, pid, ly, lock, force)
	}
	return m.lib.SetLyrics(ctx, pid, ly, lock, force)
}

func (m *mutator) SetChapters(ctx context.Context, pid model.PID, chapters []model.Chapter, lock, force bool) error {
	if m.px != nil {
		return m.px.SetChapters(ctx, pid, chapters, lock, force)
	}
	return m.lib.SetChapters(ctx, pid, chapters, lock, force)
}

func (m *mutator) SetItemArt(ctx context.Context, pid model.PID, data []byte, lock, force bool) error {
	if m.px != nil {
		return m.px.SetItemArt(ctx, pid, data, lock, force)
	}
	return m.lib.SetItemArt(ctx, pid, data, lock, force)
}

func (m *mutator) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role string, data []byte) error {
	if m.px != nil {
		return m.px.SetEntityArt(ctx, entityType, entityPID, role, data)
	}
	return m.lib.SetEntityArt(ctx, entityType, entityPID, role, data)
}

func (m *mutator) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, opts waxbin.EntityEditOptions) error {
	if m.px != nil {
		return m.px.EditEntity(ctx, entityType, entityPID, edits, opts.Lock, opts.Force)
	}
	return m.lib.EditEntity(ctx, entityType, entityPID, edits, opts)
}

func (m *mutator) SetItemTag(ctx context.Context, itemPID model.PID, key string, values []string, opts waxbin.TagEditOptions) (string, int, error) {
	if m.px != nil {
		return m.px.SetTag(ctx, itemPID, key, values, opts.Lock, opts.Force)
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

func (m *mutator) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error {
	if m.px != nil {
		return m.px.SetRating(ctx, userPID, itemPID, rating)
	}
	return m.lib.Playback().SetRating(ctx, userPID, itemPID, rating)
}

func (m *mutator) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error {
	if m.px != nil {
		return m.px.SetStar(ctx, userPID, itemPID, starred)
	}
	return m.lib.Playback().SetStar(ctx, userPID, itemPID, starred)
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
