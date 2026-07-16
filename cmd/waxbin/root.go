package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// cliSchemaVersion versions the CLI's JSON output envelope, independent of the
// storage schema.
const cliSchemaVersion = 1

// globals holds parsed persistent flags shared by all commands.
type globals struct {
	dbPath   string
	cfgPath  string
	roots    []string
	jsonOut  bool
	logLevel string
	readOnly bool
}

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "waxbin",
		Short:         "WaxBin catalog and organization engine",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.dbPath, "db", "", "path to the catalog database (env WAXBIN_DB)")
	pf.StringVar(&g.cfgPath, "config", "", "path to a JSON config file (env WAXBIN_CONFIG)")
	pf.StringArrayVar(&g.roots, "root", nil, "library root as path[:mode[:profile]] (repeatable)")
	pf.BoolVar(&g.jsonOut, "json", false, "emit JSON instead of text")
	pf.StringVar(&g.logLevel, "log-level", "", "log level: debug|info|warn|error")
	pf.BoolVar(&g.readOnly, "read-only", false, "open the catalog read-only")

	root.AddCommand(
		newInitCmd(g),
		newScanCmd(g),
		newWatchCmd(g),
		newAnalyzeCmd(g),
		newEnrichCmd(g),
		newQueryCmd(g),
		newFacetCmd(g),
		newBrowseCmd(g),
		newSearchCmd(g),
		newLyricsCmd(g),
		newArtCmd(g),
		newShowCmd(g),
		newBookCmd(g),
		newChaptersCmd(g),
		newOrganizeCmd(g),
		newRmCmd(g),
		newTrashCmd(g),
		newInboxCmd(g),
		newImportCmd(g),
		newEditCmd(g),
		newLockCmd(g),
		newUnlockCmd(g),
		newProvenanceCmd(g),
		newUserCmd(g),
		newStateCmd(g),
		newStatsCmd(g),
		newPlaylistCmd(g),
		newSmartPlaylistCmd(g),
		newPodcastCmd(g),
		newOPMLCmd(g),
		newBackupCmd(g),
		newRestoreCmd(g),
		newExportCmd(g),
		newManifestCmd(g),
		newRebuildCmd(g),
		newJobsCmd(g),
		newMergeCmd(g),
		newAuditCmd(g),
		newUpgradeCmd(g),
		newDBCmd(g),
		newDoctorCmd(g),
		newVersionCmd(g),
		newExitCodesCmd(g),
	)
	return root
}

// loadConfig resolves configuration with flag > env > json > default precedence
// and validates it (normalizing + checking non-overlapping roots).
func (g *globals) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	ov := config.Overrides{ConfigPath: g.resolveConfigPath()}
	if cmd.Flags().Changed("db") {
		ov.DBPath = &g.dbPath
	}
	if cmd.Flags().Changed("log-level") {
		ov.LogLevel = &g.logLevel
	}
	if cmd.Flags().Changed("root") {
		roots := make([]config.Root, 0, len(g.roots))
		for _, spec := range g.roots {
			r, err := config.ParseRootSpec(spec)
			if err != nil {
				return nil, err
			}
			roots = append(roots, r)
		}
		ov.Roots = roots
	}

	cfg, err := config.Load(ov, os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (g *globals) resolveConfigPath() string {
	if g.cfgPath != "" {
		return g.cfgPath
	}
	return os.Getenv("WAXBIN_CONFIG")
}

// open resolves config and opens the library for a mutating command (read-write
// unless --read-only forces otherwise).
func (g *globals) open(cmd *cobra.Command) (*waxbin.Library, *config.Config, error) {
	return g.openLib(cmd, false)
}

// openRead opens the library read-only, so read commands take no write lock and
// run concurrently with a scan/organize that owns the catalog.
func (g *globals) openRead(cmd *cobra.Command) (*waxbin.Library, *config.Config, error) {
	return g.openLib(cmd, true)
}

func (g *globals) openLib(cmd *cobra.Command, forceReadOnly bool) (*waxbin.Library, *config.Config, error) {
	cfg, err := g.loadConfig(cmd)
	if err != nil {
		return nil, nil, err
	}
	opts := waxbin.OptionsFromConfig(cfg, g.logger(cfg))
	opts.ReadOnly = forceReadOnly || g.readOnly
	lib, err := waxbin.Open(cmd.Context(), opts)
	if err != nil {
		return nil, nil, err
	}
	return lib, cfg, nil
}

func (g *globals) logger(cfg *config.Config) *slog.Logger {
	level := cfg.LogLevel
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

// ctx returns the command context (background-rooted by cobra).
func ctx(cmd *cobra.Command) context.Context {
	if c := cmd.Context(); c != nil {
		return c
	}
	return context.Background()
}

// printJSON writes a versioned JSON envelope to the command's stdout.
func printJSON(cmd *cobra.Command, data any) error {
	env := struct {
		SchemaVersion int    `json:"schemaVersion"`
		Command       string `json:"command"`
		Data          any    `json:"data"`
	}{cliSchemaVersion, cmd.Name(), data}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "cli.json", err)
	}
	return nil
}

func out(cmd *cobra.Command) io.Writer { return cmd.OutOrStdout() }

// errOut is the stream for advisory warnings that must not pollute --json stdout.
func errOut(cmd *cobra.Command) io.Writer { return cmd.ErrOrStderr() }
