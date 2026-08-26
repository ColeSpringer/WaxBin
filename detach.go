package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
)

// This file exposes the per-member detach on the Library facade: pulling one track off
// an album chain a MusicBrainz id pins it to and onto the heuristic chain its own tags
// and folder imply. The whole-entity counterpart is clearing that id through EditEntity.

// DetachOptions controls a detach.
type DetachOptions struct {
	// WriteBack also strips the release and release-group ids from the member's own
	// files. Without it the catalog change is the whole detach, and the file's tags
	// still name the release, so a scan that re-resolves the item adopts it back.
	WriteBack bool
}

// Detach moves one track off an album chain that carries a MusicBrainz id, its own
// release id or an mbid-keyed release group above it, and onto the heuristic album chain
// its own tags and folder imply, reporting the album it left and the album and release
// group it landed on. A non-track item, a member of a chain with no MusicBrainz id, and
// an album's last member are all refused with CodeInvalid; the last-member refusal names
// the whole-entity mbid clear to use instead.
//
// With opts.WriteBack the member's files also lose their MUSICBRAINZ_ALBUMID and
// MUSICBRAINZ_RELEASEGROUPID tags, which is what makes the detach survive a rescan.
// Write-back runs after the catalog change committed, so a file that cannot be written
// is reported through a *WriteBackError while the detach itself stands.
func (l *Library) Detach(ctx context.Context, itemPID model.PID, opts DetachOptions) (*model.DetachReport, error) {
	rep, err := l.store.DetachItemFromMBIDAlbum(ctx, itemPID)
	if err != nil {
		return nil, err
	}
	if !opts.WriteBack {
		return rep, nil
	}
	return rep, l.writeBackDetach(ctx, itemPID)
}

// writeBackDetach clears the two MusicBrainz release tags from a detached member's
// file. Only a track can be detached, so that is a single file, but it goes through the
// shared write-back engine like every other fan-out, which is what applies the
// shared-file guard and records the drift a refusal leaves behind.
func (l *Library) writeBackDetach(ctx context.Context, itemPID model.PID) error {
	files, err := l.store.ItemFiles(ctx, itemPID)
	if err != nil {
		return writeBackSetupFailure(itemPID, nil, err)
	}
	wbErr := &WriteBackError{ItemPID: itemPID}
	if len(files) == 0 {
		wbErr.Failures = append(wbErr.Failures, WriteBackFailure{Reason: "no backing files present to write"})
		return wbErr
	}
	edits := []meta.TagEdit{{Key: "MUSICBRAINZ_ALBUMID"}, {Key: "MUSICBRAINZ_RELEASEGROUPID"}}
	if err := l.writeBackFiles(ctx, "waxbin.Detach", files, wbErr,
		func(w *meta.Writer, path string) (*meta.WriteResult, error) {
			return w.Apply(ctx, path, edits)
		}); err != nil {
		return err
	}
	if len(wbErr.Failures) > 0 {
		return wbErr
	}
	return nil
}
