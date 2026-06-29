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
	Path    string     `json:"path"`
	Mode    model.Mode `json:"mode"`
	Profile string     `json:"profile"`
}

// Config is the resolved configuration.
type Config struct {
	DBPath   string `json:"db"`
	Roots    []Root `json:"roots"`
	LogLevel string `json:"log_level"` // debug | info | warn | error

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
	return nil
}

// ParseRootSpec parses a CLI root spec "path[:mode[:profile]]" into a Root. The
// mode/profile suffix is recognized only when a colon-delimited field exactly
// matches a known mode keyword (managed|in-place); otherwise the whole spec is
// treated as the path. This handles Windows drive letters ("C:\Music") and
// Unix paths that legitimately contain a colon ("/data/My:Music") without a
// special case.
func ParseRootSpec(spec string) (Root, error) {
	if strings.TrimSpace(spec) == "" {
		return Root{}, waxerr.New(waxerr.CodeInvalid, "config.ParseRootSpec", "empty root spec")
	}

	parts := strings.Split(spec, ":")
	n := len(parts)
	r := Root{Path: spec}
	switch {
	case n >= 2 && model.Mode(parts[n-1]).Valid(): // path:mode
		r.Mode = model.Mode(parts[n-1])
		r.Path = strings.Join(parts[:n-1], ":")
	case n >= 3 && model.Mode(parts[n-2]).Valid(): // path:mode:profile
		r.Mode = model.Mode(parts[n-2])
		r.Profile = parts[n-1]
		r.Path = strings.Join(parts[:n-2], ":")
	}
	return r, nil
}

func pathsOverlap(a, b string) bool {
	return pathx.UnderRoot(a, b) || pathx.UnderRoot(b, a)
}
