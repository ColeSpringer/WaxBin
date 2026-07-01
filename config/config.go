// Package config loads WaxBin's configuration with the precedence
// flag > env (WAXBIN_*) > json > default, and validates that library roots are
// absolute and non-overlapping (a file belongs to exactly one library).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Root is a registered library root and its handling policy.
type Root struct {
	Path string     `json:"path"`
	Mode model.Mode `json:"mode"`
	// Media is the content class a managed root holds (music/audiobook/mixed).
	// Empty defaults to mixed, allowing one root to hold both tracks and books.
	Media   model.MediaType `json:"media,omitempty"`
	Profile string          `json:"profile"`
}

// ProfileDef describes a layout profile from configuration. A profile with the
// same name as a built-in overrides it; empty template fields inherit from the
// built-in or native defaults. The facade converts this config-only shape into
// organize.Profile, keeping config independent of organize.
type ProfileDef struct {
	Name      string `json:"name"`
	Music     string `json:"music,omitempty"`
	Audiobook string `json:"audiobook,omitempty"`
	Podcast   string `json:"podcast,omitempty"`
	TagWrite  bool   `json:"tag_write,omitempty"`
}

// PodcastConfig controls podcast downloads and remote fetch limits. Dir is an
// internal library root, validated so it cannot overlap a user root or inbox.
type PodcastConfig struct {
	Dir               string `json:"dir,omitempty"`                 // download directory (downloads require it)
	UserAgent         string `json:"user_agent,omitempty"`          // HTTP User-Agent
	BlockPrivateIPs   bool   `json:"block_private_ips,omitempty"`   // SSRF guard (refuse private/loopback)
	TimeoutSeconds    int    `json:"timeout_seconds,omitempty"`     // per-request timeout (0 = default)
	MaxFeedBytes      int64  `json:"max_feed_bytes,omitempty"`      // cap on a feed/transcript body
	MaxEnclosureBytes int64  `json:"max_enclosure_bytes,omitempty"` // cap on an episode download
	DefaultRetention  int    `json:"default_retention,omitempty"`   // keep newest N per feed (0 = keep all)
}

// EnrichConfig controls the metadata enrichment pass (MusicBrainz + Cover Art
// Archive + optional AcoustID). MusicBrainz requires an identifying contact, so
// enrichment is disabled unless Contact (or an explicit UserAgent) is set. The
// base-URL fields default to the public services and exist mainly for tests and
// private mirrors.
type EnrichConfig struct {
	Contact         string `json:"contact,omitempty"`           // MB contact (email/URL); enables enrichment
	UserAgent       string `json:"user_agent,omitempty"`        // overrides the built User-Agent
	AcoustIDKey     string `json:"acoustid_key,omitempty"`      // enables the AcoustID fallback (needs fpcalc)
	CoverArt        *bool  `json:"cover_art,omitempty"`         // fetch release-group covers (default on)
	BlockPrivateIPs bool   `json:"block_private_ips,omitempty"` // SSRF guard for provider requests
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`   // per-request timeout (0 = default)

	MusicBrainzBaseURL string `json:"musicbrainz_base_url,omitempty"`
	CoverArtBaseURL    string `json:"cover_art_base_url,omitempty"`
	AcoustIDBaseURL    string `json:"acoustid_base_url,omitempty"`
}

// Config is the resolved configuration.
type Config struct {
	DBPath   string `json:"db"`
	Roots    []Root `json:"roots"`
	LogLevel string `json:"log_level"` // debug | info | warn | error

	// Profiles defines additional organization profiles and built-in overrides.
	Profiles []ProfileDef `json:"profiles,omitempty"`
	// Inbox folders are staging directories imported into a managed library.
	Inbox []string `json:"inbox,omitempty"`
	// FreeSpaceReserveBytes is the headroom an import preflight keeps free on the
	// destination volume (0 disables the check).
	FreeSpaceReserveBytes int64 `json:"free_space_reserve_bytes,omitempty"`

	// Podcasts configures the podcast engine: download directory and network policy.
	Podcasts PodcastConfig `json:"podcasts,omitempty"`

	// Enrichment configures the metadata enrichment pass (MusicBrainz/CAA/AcoustID).
	Enrichment EnrichConfig `json:"enrichment,omitempty"`

	// Storage tuning (applied as SQLite pragmas by store/sqlite).
	BusyTimeoutMS int   `json:"busy_timeout_ms"`
	CacheSizeKB   int   `json:"cache_size_kb"` // SQLite cache_size, in KiB
	MmapSizeBytes int64 `json:"mmap_size_bytes"`
	ReadPoolSize  int   `json:"read_pool_size"` // max concurrent read connections
}

// Default returns the baseline configuration before any overlay.
func Default() *Config {
	return &Config{
		LogLevel:      "info",
		BusyTimeoutMS: 10000,
		CacheSizeKB:   16000,     // ~16 MiB page cache
		MmapSizeBytes: 256 << 20, // 256 MiB mmap window (evaluated per the plan)
		ReadPoolSize:  8,
	}
}

// Overrides carries values resolved from the highest-precedence sources (flags,
// then env at the CLI layer). A nil pointer means "not set, defer to lower
// precedence"; a non-nil Roots slice replaces any roots from the json file.
type Overrides struct {
	ConfigPath string // explicit --config / WAXBIN_CONFIG; "" => no json file
	DBPath     *string
	LogLevel   *string
	Roots      []Root
}

// Load resolves configuration applying defaults, then the optional json file,
// then env vars, then the supplied overrides. getenv is injectable for testing.
func Load(ov Overrides, getenv func(string) string) (*Config, error) {
	cfg := Default()

	if ov.ConfigPath != "" {
		if err := mergeJSON(cfg, ov.ConfigPath); err != nil {
			return nil, err
		}
	}

	// env overlay (lower precedence than explicit flag overrides below)
	if v := getenv("WAXBIN_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := getenv("WAXBIN_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := getenv("WAXBIN_PODCAST_DIR"); v != "" {
		cfg.Podcasts.Dir = v
	}
	if v := getenv("WAXBIN_ENRICH_CONTACT"); v != "" {
		cfg.Enrichment.Contact = v
	}
	if v := getenv("WAXBIN_ACOUSTID_KEY"); v != "" {
		cfg.Enrichment.AcoustIDKey = v
	}

	// flag overrides (highest precedence)
	if ov.DBPath != nil {
		cfg.DBPath = *ov.DBPath
	}
	if ov.LogLevel != nil {
		cfg.LogLevel = *ov.LogLevel
	}
	if ov.Roots != nil {
		cfg.Roots = ov.Roots
	}

	return cfg, nil
}

func mergeJSON(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return waxerr.Wrapf(waxerr.CodeIO, "config.Load", err, "reading %s", path)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return waxerr.Wrapf(waxerr.CodeInvalid, "config.Load", err, "parsing %s", path)
	}
	return nil
}

// Validate normalizes roots (absolute + cleaned, default mode/profile) and
// rejects an empty DB path or overlapping/nested roots. It mutates the receiver
// in place so callers see normalized paths.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.DBPath) == "" {
		return waxerr.New(waxerr.CodeInvalid, "config.Validate", "db path is required")
	}
	if abs, err := filepath.Abs(c.DBPath); err == nil {
		c.DBPath = abs
	}

	for i := range c.Roots {
		r := &c.Roots[i]
		if strings.TrimSpace(r.Path) == "" {
			return waxerr.New(waxerr.CodeInvalid, "config.Validate", "library root path is empty")
		}
		abs, err := filepath.Abs(r.Path)
		if err != nil {
			return waxerr.Wrapf(waxerr.CodeInvalid, "config.Validate", err, "resolving root %q", r.Path)
		}
		r.Path = filepath.Clean(abs)
		if r.Mode == "" {
			r.Mode = model.ModeInPlace // conservative default: never move files unless asked
		}
		if !r.Mode.Valid() {
			return waxerr.New(waxerr.CodeInvalid, "config.Validate",
				fmt.Sprintf("root %q has invalid mode %q", r.Path, r.Mode))
		}
		if r.Media == "" {
			r.Media = model.MediaMixed // default: one content-classified tree
		}
		if !r.Media.Valid() {
			return waxerr.New(waxerr.CodeInvalid, "config.Validate",
				fmt.Sprintf("root %q has invalid media type %q", r.Path, r.Media))
		}
		if r.Profile == "" {
			r.Profile = "waxbin-native"
		}
	}

	for i := 0; i < len(c.Roots); i++ {
		for j := i + 1; j < len(c.Roots); j++ {
			if pathsOverlap(c.Roots[i].Path, c.Roots[j].Path) {
				return waxerr.New(waxerr.CodeInvalid, "config.Validate",
					fmt.Sprintf("library roots overlap: %q and %q", c.Roots[i].Path, c.Roots[j].Path))
			}
		}
	}

	// Normalize inbox staging folders to absolute, cleaned paths and reject any
	// nested inside a library root: a file belongs to exactly one place, and an
	// inbox under a root would be re-imported on every scan.
	for i := range c.Inbox {
		if strings.TrimSpace(c.Inbox[i]) == "" {
			return waxerr.New(waxerr.CodeInvalid, "config.Validate", "inbox folder path is empty")
		}
		abs, err := filepath.Abs(c.Inbox[i])
		if err != nil {
			return waxerr.Wrapf(waxerr.CodeInvalid, "config.Validate", err, "resolving inbox %q", c.Inbox[i])
		}
		c.Inbox[i] = filepath.Clean(abs)
		for _, r := range c.Roots {
			if pathx.UnderRoot(r.Path, c.Inbox[i]) || pathx.UnderRoot(c.Inbox[i], r.Path) {
				return waxerr.New(waxerr.CodeInvalid, "config.Validate",
					fmt.Sprintf("inbox %q overlaps library root %q", c.Inbox[i], r.Path))
			}
		}
	}

	// Keep podcast downloads outside user roots and inboxes. Scan should never see
	// the podcast cache as ordinary music.
	if strings.TrimSpace(c.Podcasts.Dir) != "" {
		abs, err := filepath.Abs(c.Podcasts.Dir)
		if err != nil {
			return waxerr.Wrapf(waxerr.CodeInvalid, "config.Validate", err, "resolving podcast dir %q", c.Podcasts.Dir)
		}
		c.Podcasts.Dir = filepath.Clean(abs)
		for _, r := range c.Roots {
			if pathsOverlap(r.Path, c.Podcasts.Dir) {
				return waxerr.New(waxerr.CodeInvalid, "config.Validate",
					fmt.Sprintf("podcast dir %q overlaps library root %q", c.Podcasts.Dir, r.Path))
			}
		}
		for _, in := range c.Inbox {
			if pathsOverlap(in, c.Podcasts.Dir) {
				return waxerr.New(waxerr.CodeInvalid, "config.Validate",
					fmt.Sprintf("podcast dir %q overlaps inbox %q", c.Podcasts.Dir, in))
			}
		}
	}
	return nil
}

// ParseRootSpec parses a CLI root spec "path[:mode[:media[:profile]]]" into a Root.
// The suffix is recognized only when a colon-delimited field exactly matches a known
// mode keyword (managed|in-place). If the following slot is a known media keyword
// (music|audiobook|mixed), it is parsed as media; otherwise the whole spec is treated
// as the path. This handles Windows drive letters ("C:\Music") and Unix paths that
// legitimately contain a colon ("/data/My:Music") without a special case.
// A profile literally named after a media keyword must be written with an explicit
// media slot ("path:managed:mixed:mixed").
func ParseRootSpec(spec string) (Root, error) {
	if strings.TrimSpace(spec) == "" {
		return Root{}, waxerr.New(waxerr.CodeInvalid, "config.ParseRootSpec", "empty root spec")
	}

	parts := strings.Split(spec, ":")
	n := len(parts)
	isMode := func(i int) bool { return model.Mode(parts[i]).Valid() }
	isMedia := func(i int) bool { return model.MediaType(parts[i]).Valid() }
	r := Root{Path: spec}
	switch {
	case n >= 4 && isMode(n-3) && isMedia(n-2): // path:mode:media:profile
		r.Mode = model.Mode(parts[n-3])
		r.Media = model.MediaType(parts[n-2])
		r.Profile = parts[n-1]
		r.Path = strings.Join(parts[:n-3], ":")
	case n >= 3 && isMode(n-2) && isMedia(n-1): // path:mode:media
		r.Mode = model.Mode(parts[n-2])
		r.Media = model.MediaType(parts[n-1])
		r.Path = strings.Join(parts[:n-2], ":")
	case n >= 3 && isMode(n-2): // path:mode:profile
		r.Mode = model.Mode(parts[n-2])
		r.Profile = parts[n-1]
		r.Path = strings.Join(parts[:n-2], ":")
	case n >= 2 && isMode(n-1): // path:mode
		r.Mode = model.Mode(parts[n-1])
		r.Path = strings.Join(parts[:n-1], ":")
	}
	return r, nil
}

func pathsOverlap(a, b string) bool {
	return pathx.UnderRoot(a, b) || pathx.UnderRoot(b, a)
}
