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

// diagnosticsSchemaVersion is the migration that introduced file_diagnostic and
// file.diag_version; doctor skips the diagnostic counts on an older catalog so it
// degrades to zero rather than failing with a no-such-table error.
//
// The store guards these reads too (the audit reaches them through a port that
// carries no schema knowledge), so this gate is the local early-out that matches the
// per-feature convention above rather than the only thing standing between an
// un-migrated catalog and a no-such-table error.
const diagnosticsSchemaVersion = sqlite.SchemaVersionFileDiagnostics

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

	// Diagnostic coverage: how many persisted file diagnostics exist, and how many
	// audio files have not had diagnostics derived under the current rule set. A
	// non-zero DiagnosticsStale is why "no diagnostics" must not be read as "clean".
	DiagnosticCount  int
	DiagnosticsStale int
	// EnrichmentEnabled reports whether a MusicBrainz contact is configured.
	EnrichmentEnabled bool

	// Detected optional helpers (never required for core use).
	FFmpeg bool
	Fpcalc bool
	// Exotic-image thumbnail support, detected per-format (an AVIF/HEIC cover is
	// otherwise found and served unscaled).
	AVIFThumbnails bool
	HEICThumbnails bool

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
	ic := caps.ImageSupport()
	rep := &DoctorReport{
		DBPath:             l.opts.DBPath,
		BuildSchemaVersion: sqlite.SchemaVersion,
		ReadOnly:           l.ReadOnly(),
		FFmpeg:             c.FFmpeg,
		Fpcalc:             c.Fpcalc,
		AVIFThumbnails:     ic.AVIF,
		HEICThumbnails:     ic.HEIC,
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

	// file_diagnostic and file.diag_version only exist from v25 on; skip on older
	// catalogs so an un-upgraded one reports zero instead of erroring.
	if version >= diagnosticsSchemaVersion {
		n, err := l.store.CountFileDiagnostics(ctx)
		if err != nil {
			return nil, err
		}
		rep.DiagnosticCount = n
		stale, _, err := l.store.DiagnosticCoverage(ctx)
		if err != nil {
			return nil, err
		}
		rep.DiagnosticsStale = stale
	}

	// The lockfile is read without taking the lock, so even a read-only doctor
	// can report who currently owns the catalog (empty when no one holds it).
	if info, err := l.store.OwnerInfo(); err == nil {
		rep.Owner = info
	}
	return rep, nil
}
