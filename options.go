package waxbin

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/colespringer/waxbin/config"
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
	// Podcasts configures the podcast engine (download dir + network policy).
	Podcasts config.PodcastConfig
	// Enrichment configures the metadata enrichment pass (MusicBrainz/CAA/AcoustID).
	Enrichment config.EnrichConfig
	// SourceProviders are injected acquisition providers, such as a youtube provider
	// supplied by another module. The built-in netsafe rss provider is always
	// registered; these register under their own source types. The default CLI build
	// does not ship extra providers.
	SourceProviders []source.Provider

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
