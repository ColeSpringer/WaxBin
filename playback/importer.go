package playback

import (
	"context"

	"github.com/colespringer/waxbin/model"
)

// PlayStateRecord is one external play-state datum to import: a resume position and
// optional played/rating/star signals for one item and user. It is the neutral shape
// a concrete adapter maps a foreign export (a prior media server, a companion app)
// into before handing it to a PlayStateImporter. The changed-at stamps (unix
// nanoseconds, 0 = unknown) say when the star or rating last changed value on the
// foreign side.
//
// An adapter passes these stamps straight into SetStar/SetRating as their asOf
// argument (the address of the changed-at field), and the engine then enforces
// recorded-time last-writer-wins: a record older than local state is skipped. The
// comparison is the engine's once the adapter supplies the recorded time, so this
// seam no longer holds the replay guard itself. The engine treats a 0 stamp (the
// seam's "unknown" value) as no recorded time, stamping at server-now and ordering
// against nothing, so an adapter can pass every record's stamp straight through
// without special-casing the unknown ones.
type PlayStateRecord struct {
	UserPID          model.PID
	ItemPID          model.PID
	PositionMS       int64
	Played           bool
	HasRating        bool
	Rating           int // 0..100 when HasRating
	Starred          bool
	LastPlayedNS     int64
	RatingChangedNS  int64
	StarredChangedNS int64
}

// PlayStateImportResult tallies an import pass.
type PlayStateImportResult struct {
	Imported int
	Skipped  int
}

// PlayStateImporter ingests external play state into the catalog. It is the §9
// import-adapter seam: v1.0 ships the interface and a no-op default so an embedder
// can supply a concrete adapter (mapping, say, a Plex or Jellyfin export) without an
// engine change. The default imports nothing.
type PlayStateImporter interface {
	ImportPlayState(ctx context.Context, records []PlayStateRecord) (PlayStateImportResult, error)
}

// noopImporter is the default importer: it accepts records and imports none,
// reporting them all skipped, until a concrete adapter is wired in.
type noopImporter struct{}

func (noopImporter) ImportPlayState(_ context.Context, records []PlayStateRecord) (PlayStateImportResult, error) {
	return PlayStateImportResult{Skipped: len(records)}, nil
}

// Importer returns the configured play-state importer (the no-op default until a
// concrete adapter is set with SetImporter).
func (s *Service) Importer() PlayStateImporter { return s.importer }

// SetImporter installs a concrete play-state import adapter, replacing the no-op
// default. A nil importer restores the no-op default.
func (s *Service) SetImporter(imp PlayStateImporter) {
	if imp == nil {
		imp = noopImporter{}
	}
	s.importer = imp
}
