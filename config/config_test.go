package config_test

import (
	"path/filepath"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

func TestValidateRejectsOverlappingRoots(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{
		DBPath: filepath.Join(base, "c.db"),
		Roots: []config.Root{
			{Path: filepath.Join(base, "music"), Mode: model.ModeManaged},
			{Path: filepath.Join(base, "music", "rock"), Mode: model.ModeManaged}, // nested
		},
	}
	if err := cfg.Validate(); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for nested roots, got %v", err)
	}
}

func TestValidateAllowsSiblingRoots(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{
		DBPath: filepath.Join(base, "c.db"),
		Roots: []config.Root{
			{Path: filepath.Join(base, "music")},
			{Path: filepath.Join(base, "audiobooks")},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("sibling roots should validate: %v", err)
	}
	// Defaults applied.
	for _, r := range cfg.Roots {
		if r.Mode != model.ModeInPlace {
			t.Fatalf("default mode = %q, want in-place", r.Mode)
		}
		if r.Profile != "waxbin-native" {
			t.Fatalf("default profile = %q", r.Profile)
		}
		if !filepath.IsAbs(r.Path) {
			t.Fatalf("root not absolutized: %q", r.Path)
		}
	}
}

func TestValidateRequiresDBPath(t *testing.T) {
	cfg := &config.Config{}
	if err := cfg.Validate(); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("want CodeInvalid for empty db, got %v", err)
	}
}

func TestLoadPrecedenceFlagOverEnv(t *testing.T) {
	env := map[string]string{"WAXBIN_DB": "/env/path.db"}
	getenv := func(k string) string { return env[k] }

	flagDB := "/flag/path.db"
	cfg, err := config.Load(config.Overrides{DBPath: &flagDB}, getenv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.DBPath != flagDB {
		t.Fatalf("flag should win: got %q", cfg.DBPath)
	}

	// Without the flag, env wins over the (empty) default.
	cfg, err = config.Load(config.Overrides{}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != "/env/path.db" {
		t.Fatalf("env should win when no flag: got %q", cfg.DBPath)
	}
}

func TestParseRootSpec(t *testing.T) {
	r, err := config.ParseRootSpec("/music:managed:waxbin-native")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Path != "/music" || r.Mode != model.ModeManaged || r.Profile != "waxbin-native" {
		t.Fatalf("parsed = %+v", r)
	}

	r, err = config.ParseRootSpec("/audiobooks")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Path != "/audiobooks" || r.Mode != "" {
		t.Fatalf("bare path parsed = %+v", r)
	}

	// An unrecognized suffix is not a mode, so the whole spec is the path. This
	// is what lets a Unix path containing ':' be registered.
	r, err = config.ParseRootSpec("/data/My:Music")
	if err != nil {
		t.Fatalf("parse colon path: %v", err)
	}
	if r.Path != "/data/My:Music" || r.Mode != "" {
		t.Fatalf("colon path parsed = %+v", r)
	}
}

func TestParseRootSpecWindowsDrive(t *testing.T) {
	cases := []struct {
		spec    string
		path    string
		mode    model.Mode
		profile string
	}{
		{`C:\Music`, `C:\Music`, "", ""},
		{`C:\Music:managed`, `C:\Music`, model.ModeManaged, ""},
		{`D:/Audio:managed:waxbin-native`, `D:/Audio`, model.ModeManaged, "waxbin-native"},
	}
	for _, tc := range cases {
		r, err := config.ParseRootSpec(tc.spec)
		if err != nil {
			t.Fatalf("%s: %v", tc.spec, err)
		}
		if r.Path != tc.path || r.Mode != tc.mode || r.Profile != tc.profile {
			t.Fatalf("%s -> %+v, want path=%q mode=%q profile=%q", tc.spec, r, tc.path, tc.mode, tc.profile)
		}
	}
}
