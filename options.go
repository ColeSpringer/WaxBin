package waxbin

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/enrich"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
)

// Options configures opening a Library.
type Options struct {
	// DBPath is the catalog database (local filesystem only).
	DBPath string
	// Roots are library roots to ensure on open (upserted; never deleted here).
	Roots []config.Root
	// ReadOnly opens without taking the write lock and forbids mutations.
	ReadOnly bool
	// AllowStaleBaseline opens a catalog built from an older schema baseline instead
	// of refusing it, for best-effort salvage: read-only opens only, and a read
	// touching a table the baseline edit changed still fails.
	AllowStaleBaseline bool
	// Logger receives structured logs; nil discards (the library never prints).
	Logger *slog.Logger
	// WriteOwner identifies this owner in the lockfile and job rows; defaulted
	// from hostname + pid when empty.
	WriteOwner string
	// IPCSocket, if set, is advertised in the lockfile for local proxy support.
	IPCSocket string

	// Profiles defines additional organization profiles and built-in overrides.
	Profiles []config.ProfileDef
	// Inbox folders are staging directories importable into a managed library.
	Inbox []string
	// FreeSpaceReserveBytes is headroom an import preflight keeps free.
	FreeSpaceReserveBytes int64
	// WriteReplayGainTags mirrors computed ReplayGain into files after analyze (off
	// by default; the catalog stays authoritative).
	WriteReplayGainTags bool
	// WriteEnrichmentTags writes what enrichment filled back into files after an
	// enrich pass (off by default), which is also what makes it survive a rescan.
	WriteEnrichmentTags bool
	// StampItemPID stamps the backing item's WaxBin PID into a tag during organize's
	// tag write-back on managed roots (off by default).
	StampItemPID bool
	// Podcasts configures the podcast engine (download dir + network policy).
	Podcasts config.PodcastConfig
	// Enrichment configures the metadata enrichment pass (MusicBrainz/CAA/AcoustID).
	Enrichment config.EnrichConfig
	// SourceProviders are injected acquisition providers, such as a youtube provider
	// supplied by another module. The built-in netsafe rss provider is always
	// registered; these register under their own source types. The default CLI build
	// does not ship extra providers.
	SourceProviders []source.Provider
	// EnrichmentProviders are injected metadata-enrichment providers (Discogs, Last.fm,
	// Audnexus, Hardcover, fanart.tv, ...) supplied by an embedding module. A pass asks
	// them in the order given, ahead of the key-free built-ins (Cover Art Archive,
	// ListenBrainz, LRCLIB), unless EnrichmentProviderList reorders them, and the first
	// to answer wins a value conflict; the MusicBrainz identity spine still resolves the
	// anchoring MBID first. Each needs a name of its own: one repeating another's or a
	// built-in's is dropped with a warning. The default CLI build ships none.
	EnrichmentProviders []enrich.Provider
	// EnrichmentProviderList, when set, decides which providers each enrichment pass
	// consults and in what order, so a settings screen can rank, add, or switch off
	// providers without reopening the Library. It is handed the fixed list, the
	// EnrichmentProviders ahead of the built-ins (find those by the enrich.Provider*
	// names, or list them with EnrichmentBuiltins), and returns the list a pass uses. It
	// is read once at the start of every pass, and again whenever a status surface asks
	// which phases would run, so it has to be cheap and safe for concurrent use. Leaving
	// out the enrich.ProviderMusicBrainz entry drops MusicBrainz's own genres from the
	// genre merge; the identity spine itself runs regardless. A provider switched off is
	// simply not asked: a release group resolved meanwhile keeps no front from it until
	// its phase is forced again (see enrich.Config.ProviderList). Nil keeps the fixed list.
	EnrichmentProviderList func(fixed []enrich.Provider) []enrich.Provider
	// SecretCipher, when set, seals secret-table values (private-feed passwords) at
	// rest; an embedder supplies one to own the key. Nil keeps secrets in plaintext.
	SecretCipher model.SecretCipher
	// SecretKeyID labels the key/epoch sealed values are written under; defaults to
	// "1" when a cipher is set without one. Change it when rotating to a new key.
	SecretKeyID string

	// Storage tuning; zero values fall back to library defaults.
	BusyTimeoutMS int
	CacheSizeKB   int
	MmapSizeBytes int64
	ReadPoolSize  int
}

// OptionsFromConfig derives Options from a resolved Config.
func OptionsFromConfig(cfg *config.Config, log *slog.Logger) Options {
	return Options{
		DBPath:                cfg.DBPath,
		Roots:                 cfg.Roots,
		Profiles:              cfg.Profiles,
		Inbox:                 cfg.Inbox,
		FreeSpaceReserveBytes: cfg.FreeSpaceReserveBytes,
		WriteReplayGainTags:   cfg.WriteReplayGainTags,
		WriteEnrichmentTags:   cfg.WriteEnrichmentTags,
		StampItemPID:          cfg.StampItemPID,
		Podcasts:              cfg.Podcasts,
		Enrichment:            cfg.Enrichment,
		Logger:                log,
		BusyTimeoutMS:         cfg.BusyTimeoutMS,
		CacheSizeKB:           cfg.CacheSizeKB,
		MmapSizeBytes:         cfg.MmapSizeBytes,
		ReadPoolSize:          cfg.ReadPoolSize,
	}
}

// defaultOwner builds an owner label for the lockfile and job rows. It is
// informational only; liveness comes from the OS flock, not this string.
func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/pid-%d", host, os.Getpid())
}
