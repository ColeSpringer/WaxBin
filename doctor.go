package waxbin

import (
	"context"
	"fmt"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// DoctorReport summarizes catalog health and detected capabilities.
type DoctorReport struct {
	DBPath string
	// SchemaVersion is the catalog's actual applied version; BuildSchemaVersion is
	// what this binary supports. They differ when a catalog has not yet been
	// migrated by a read-write command (doctor itself never migrates).
	SchemaVersion      int
	BuildSchemaVersion int
	// BaselineStamp is the schema stamp this build writes into a catalog it creates
	// (port.BaselineStamp, as eight hex digits), the one a backup must hold to restore.
	BaselineStamp    string
	ReadOnly         bool
	Owner            sqlite.OwnerInfo
	LibraryCount     int // library roots, the internal podcast library left out
	ItemCount        int
	FingerprintCount int
	LoudnessCount    int // files with a stored ReplayGain measurement
	PodcastCount     int // subscribed feeds

	// Enrichment coverage: entities looked up, and how many a provider matched.
	EnrichedEntities int
	EnrichedMatched  int
	// Lyrics coverage over the present tracks: how many hold lyrics from any source,
	// and how many have none because a lookup answered that there are none
	// (EnrichmentCoverage).
	TrackCount        int
	TracksWithLyrics  int
	TracksLyricsAsked int

	// Diagnostic coverage: how many persisted file diagnostics exist, and how many
	// audio files have not had diagnostics derived under the current rule set. A
	// non-zero DiagnosticsStale is why "no diagnostics" must not be read as "clean".
	DiagnosticCount  int
	DiagnosticsStale int
	// EnrichmentEnabled reports whether any enrichment phase can run: a MusicBrainz
	// contact is configured, or an injected provider gates a phase of its own.
	// EnrichmentPhases are those phases in run order, the list Library.EnrichmentPhases
	// reports.
	EnrichmentEnabled bool
	EnrichmentPhases  []model.EnrichPhase

	// FpcalcPath is fpcalc, the sole remaining optional helper (Chromaprint for grouping
	// and AcoustID), as the catalog runs it: the configured binary or the one found on
	// PATH when the catalog opened, empty for none. It is never required for core use.
	// FpcalcError says why a configured fpcalc is not usable, which the analyze pass
	// refuses on. Decoding is pure-Go via WaxFlow, so there is no ffmpeg capability to
	// report. Coverage below is the honest "what can this build decode" answer.
	FpcalcPath  string
	FpcalcError string

	// Coverage reports, per codec, how the analyze pass decodes it in this build.
	Coverage []decode.FormatSupport
}

// NeedsMigration reports whether the catalog is behind the build (a read-write
// command would upgrade it).
func (r *DoctorReport) NeedsMigration() bool {
	return r.SchemaVersion > 0 && r.SchemaVersion < r.BuildSchemaVersion
}

// Doctor reports catalog stats and capability coverage. It is read-only: it
// never migrates, and it reports the catalog's applied schema version next to
// the build's so an operator can see when a read-write command would upgrade.
func (l *Library) Doctor(ctx context.Context) (*DoctorReport, error) {
	rep := &DoctorReport{
		DBPath:             l.opts.DBPath,
		BuildSchemaVersion: sqlite.SchemaVersion,
		ReadOnly:           l.ReadOnly(),
		FpcalcPath:         l.fpcalc,
		Coverage:           l.Coverage(),
	}

	if l.fpcalcErr != nil {
		rep.FpcalcError = l.fpcalcErr.Error()
	}

	version, err := l.store.CatalogVersion(ctx)
	if err != nil {
		return nil, err
	}
	rep.SchemaVersion = version
	stamp, err := port.BaselineStamp()
	if err != nil {
		return nil, err
	}
	rep.BaselineStamp = fmt.Sprintf("%08x", uint32(stamp))

	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	rep.LibraryCount = len(model.UserLibraries(libs))

	n, err := l.store.CountItems(ctx, query.New(query.EntityItems).Build(), "")
	if err != nil {
		return nil, err
	}
	rep.ItemCount = n

	// Every initialized catalog carries the full v1 baseline, so these counts
	// need no per-feature schema gates. The first post-1.0 migration that adds
	// a table changes that: doctor never migrates, so a count that reads the
	// new table must check rep.SchemaVersion first or it breaks with "no such
	// table" against a catalog no read-write command has upgraded yet.
	fps, err := l.store.CountFingerprints(ctx)
	if err != nil {
		return nil, err
	}
	rep.FingerprintCount = fps

	loud, err := l.store.CountLoudness(ctx)
	if err != nil {
		return nil, err
	}
	rep.LoudnessCount = loud

	pods, err := l.store.Podcasts(ctx)
	if err != nil {
		return nil, err
	}
	rep.PodcastCount = len(pods)

	// One read of the phase list, so the flag and the list cannot disagree when the
	// provider list hook answers differently between two reads.
	rep.EnrichmentPhases = l.EnrichmentPhases()
	rep.EnrichmentEnabled = len(rep.EnrichmentPhases) > 0
	cov, err := l.EnrichmentCoverage(ctx)
	if err != nil {
		return nil, err
	}
	rep.EnrichedEntities = cov.Artists + cov.ReleaseGroups + cov.Books
	rep.EnrichedMatched = cov.Matched
	rep.TrackCount = cov.Tracks
	rep.TracksWithLyrics = cov.TracksWithLyrics
	rep.TracksLyricsAsked = cov.TracksLyricsAsked

	diags, err := l.store.CountFileDiagnostics(ctx)
	if err != nil {
		return nil, err
	}
	rep.DiagnosticCount = diags
	stale, _, err := l.store.DiagnosticCoverage(ctx)
	if err != nil {
		return nil, err
	}
	rep.DiagnosticsStale = stale

	// The owner record is read without taking the lock, so even a read-only doctor
	// can report who currently owns the catalog (empty when no one holds it).
	if info, err := l.store.OwnerInfo(); err == nil {
		rep.Owner = info
	}
	return rep, nil
}
