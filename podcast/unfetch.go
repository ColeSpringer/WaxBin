package podcast

import (
	"context"
	"os"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// UnfetchResult reports what an Unfetch reclaimed. Unfetched is false when the
// episode held no file.
type UnfetchResult struct {
	EpisodePID     model.PID
	Unfetched      bool
	ReclaimedBytes int64
}

// Unfetch deletes a downloaded episode's file and returns the episode to remote,
// keeping the subscription, the episode row, and its play state. An episode that
// is not downloaded is a no-op, not an error; a pin does not block it (Pinned
// exempts from retention only, and survives).
//
// Disk first, like ApplyRetention: a crash between the two steps leaves the next
// Unfetch to skip the missing file and finish the catalog half. Catalog-first
// would orphan the bytes with nothing pointing at them.
func (s *Service) Unfetch(ctx context.Context, episodePID model.PID) (*UnfetchResult, error) {
	const op = "podcast.Unfetch"
	d, err := s.store.EpisodeByPID(ctx, episodePID)
	if err != nil {
		return nil, err
	}
	ep := d.Episode
	res := &UnfetchResult{EpisodePID: episodePID}
	// FilePID, not DisplayPath: the primary file edge is what DropEpisodeFile acts
	// on, so keying off the path would leave an episode whose file row carries no
	// display path present forever with no local bytes.
	if ep.FilePID == "" {
		return res, nil
	}

	if ep.DisplayPath != "" {
		res.ReclaimedBytes = onDiskSize(ep.DisplayPath)
		if err := os.Remove(pathx.Long(ep.DisplayPath)); err != nil && !os.IsNotExist(err) {
			return nil, waxerr.Wrapf(waxerr.CodeIO, op, err, "removing %s", ep.DisplayPath)
		}
	}
	if err := s.store.DropEpisodeFile(ctx, episodePID); err != nil {
		return nil, err
	}
	res.Unfetched = true
	return res, nil
}
