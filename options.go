package waxbin

import (
	"context"
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
	// Roots are library roots to ensure on open (upserted; never deleted here). A
	// root's read-only and folder fallback flags are the catalog's alone
	// (Library.SetLibraryReadOnly, Library.SetLibraryFolderFallback), so ensuring a root
	// keeps whatever flags it has. Since every open registers these again,
	// Library.RemoveRoot refuses one of them unless forced.
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
	// enrich pass (off by default), so other players see it too. The catalog keeps a
	// fill through rescans either way, until the file states a value of its own.
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

	// OnSuspend and OnReopen are the maintenance hooks, for a host that caches catalog
	// state. OnSuspend runs when a maintenance hand-off begins, after the running-job
	// refusal and before the Library suspends, so the host can quiesce its own work.
	// OnReopen answers it once the Library is open again: as the last step of the
	// reopen ending the hand-off (or the one a dropped maintenance connection triggers),
	// after a hand-off refused once the hook ran (a job the host started during it, or
	// ctx canceled), and after a suspend that failed and was reopened on the spot. A
	// reopen whose last steps fail leaves its OnReopen owed; the next Reopen or
	// BeginMaintenance finishes it, so a host hears each OnReopen before the next
	// OnSuspend. It never runs for the initial Open, or while the Library stays closed
	// after a reopen that failed outright or a server that shut down mid-hand-off.
	//
	// The proxy server holds its lock across both hooks, so proxied requests wait until
	// OnReopen returns, which is what lets a host rebuild its own maps before the next
	// proxied read; a hook must not call back through the proxy. The host's own
	// goroutines are not held back, and between the two hooks every call that touches
	// the catalog fails with CodeUnsupported. OnReopen may read the catalog; keep it
	// short, since proxied clients wait on it.
	OnSuspend func(ctx context.Context)
	OnReopen  func(ctx context.Context, ev ReopenEvent)

	// Storage tuning; zero values fall back to library defaults.
	BusyTimeoutMS int
	CacheSizeKB   int
	MmapSizeBytes int64
	ReadPoolSize  int
}

// ReopenEvent describes a finished reopen. Replaced reports a catalog that is not the
// one the hand-off suspended (see model.ChangeCatalog): drop what was derived from it,
// reload, and resume the change feed from Seq, the catalog row's seq. Otherwise the
// feed ran on across the hand-off, and the rows other processes wrote meanwhile follow
// the host's own cursor, so it resumes from that; Seq is then the head as the reopen
// finished. Either way this process may write more rows before the hook runs, so Seq
// is where to resume from, not necessarily the head.
type ReopenEvent struct {
	Seq      int64
	Replaced bool
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
