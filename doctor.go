package waxbin

import (
	"context"

	"github.com/colespringer/waxbin/decode"
	"github.com/colespringer/waxbin/internal/caps"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/store/sqlite"
)

// fingerprintSchemaVersion is the migration that introduced the fingerprint
// table; doctor skips the fingerprint count on a catalog older than this.
const fingerprintSchemaVersion = 3

// loudnessSchemaVersion is the migration that introduced the loudness table;
// doctor skips the ReplayGain count on a catalog older than this.
const loudnessSchemaVersion = 7

// podcastSchemaVersion is the lowest schema at which a podcast read succeeds: the
// podcast tables landed in v18, but Podcasts() reads the v19 source_type column, so
// doctor skips the subscription count on a catalog older than v19 to avoid a
// no-such-column error on a read-only catalog that has not been migrated yet.
const podcastSchemaVersion = 19

// enrichmentSchemaVersion is the migration that introduced the entity_enrichment
// table; doctor skips the enrichment coverage on an older, un-migrated catalog.
const enrichmentSchemaVersion = 20

// DoctorReport summarizes catalog health and detected capabilities.
type DoctorReport struct {
	DBPath string
	// SchemaVersion is the catalog's actual applied version; BuildSchemaVersion is
	// what this binary supports. They differ when a catalog has not yet been
	// migrated by a read-write command (doctor itself never migrates).
	SchemaVersion      int
	BuildSchemaVersion int
	ReadOnly           bool
	Owner              sqlite.OwnerInfo
	LibraryCount       int
	ItemCount          int
	FingerprintCount   int
	LoudnessCount      int // files with a stored ReplayGain measurement
	PodcastCount       int // subscribed feeds

	// Enrichment coverage: entities looked up, and how many a provider matched.
	EnrichedEntities int
	EnrichedMatched  int
	// EnrichmentEnabled reports whether a MusicBrainz contact is configured.
	EnrichmentEnabled bool

	// Detected optional helpers (never required for core use).
	FFmpeg bool
	Fpcalc bool

	// Coverage reports, per codec, how the analyze pass decodes it in this build.
	Coverage []decode.FormatSupport
}

// NeedsMigration reports whether the catalog is behind the build (a read-write
// command would upgrade it).
func (r *DoctorReport) NeedsMigration() bool {
	return r.SchemaVersion > 0 && r.SchemaVersion < r.BuildSchemaVersion
}

// Doctor reports catalog stats and capability coverage. It is read-only, and
// tolerates a catalog older than this build (it reports the actual version and
// skips checks that depend on not-yet-applied migrations) so the diagnostic
// never fails on an un-upgraded catalog.
func (l *Library) Doctor(ctx context.Context) (*DoctorReport, error) {
	c := caps.Detect()
	rep := &DoctorReport{
		DBPath:             l.opts.DBPath,
		BuildSchemaVersion: sqlite.SchemaVersion,
		ReadOnly:           l.ReadOnly(),
		FFmpeg:             c.FFmpeg,
		Fpcalc:             c.Fpcalc,
		Coverage:           l.Coverage(),
	}

	version, err := l.store.CatalogVersion(ctx)
	if err != nil {
		return nil, err
	}
	rep.SchemaVersion = version

	libs, err := l.store.Libraries(ctx)
	if err != nil {
		return nil, err
	}
	rep.LibraryCount = len(libs)

	n, err := l.store.CountItems(ctx, query.New(query.EntityItems).Build())
	if err != nil {
		return nil, err
	}
	rep.ItemCount = n

	// The fingerprint table only exists from v3 on; querying it against an older
	// catalog would error, so skip the count there (it reports as 0).
	if version >= fingerprintSchemaVersion {
		fps, err := l.store.CountFingerprints(ctx)
		if err != nil {
			return nil, err
		}
		rep.FingerprintCount = fps
	}

	// The loudness table only exists from v7 on; skip its count on older catalogs.
	if version >= loudnessSchemaVersion {
		n, err := l.store.CountLoudness(ctx)
		if err != nil {
			return nil, err
		}
		rep.LoudnessCount = n
	}

	// The podcast tables only exist from v18 on; skip on older catalogs.
	if version >= podcastSchemaVersion {
		pods, err := l.store.Podcasts(ctx)
		if err != nil {
			return nil, err
		}
		rep.PodcastCount = len(pods)
	}

	// The enrichment tables only exist from v20 on; skip on older catalogs.
	rep.EnrichmentEnabled = l.enricher.Enabled()
	if version >= enrichmentSchemaVersion {
		cov, err := l.EnrichmentCoverage(ctx)
		if err != nil {
			return nil, err
		}
		rep.EnrichedEntities = cov.Artists + cov.ReleaseGroups + cov.Books
		rep.EnrichedMatched = cov.Matched
	}

	// The lockfile is read without taking the lock, so even a read-only doctor
	// can report who currently owns the catalog (empty when no one holds it).
	if info, err := l.store.OwnerInfo(); err == nil {
		rep.Owner = info
	}
	return rep, nil
}
