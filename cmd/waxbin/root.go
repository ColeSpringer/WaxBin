package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/proxy"
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

	// maintConn holds the proxy connection of an in-progress maintenance-mode
	// hand-off, kept open for the command's lifetime; closing it (in cleanup, or on
	// process exit) tells the server to reopen. See openViaMaintenance.
	maintConn *proxy.Client
}

func newRootCmd(g *globals) *cobra.Command {
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
		newEntityCmd(g),
		newCreditCmd(g),
		newTagCmd(g),
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
		newDiagnosticsCmd(g),
		newUpgradeCmd(g),
		newServeCmd(g),
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
		// A read-write open that conflicts with a running server: hand off through
		// maintenance mode so the command still runs holding the lock itself. Read-only
		// opens never take the lock, so they never reach here.
		if !opts.ReadOnly && waxerr.Is(err, waxerr.CodeConflict) {
			if sock := advertisedSocket(cfg.DBPath); sock != "" {
				if lib2, err2 := g.openViaMaintenance(cmd, opts, sock); err2 == nil {
					return lib2, cfg, nil
				}
			}
		}
		return nil, nil, err
	}
	return lib, cfg, nil
}

// openMutator resolves how a mutating command reaches the catalog. When a server
// advertises a reachable socket, the command's mutations are proxied through it
// (no write-lock contention); otherwise it opens the catalog directly, which for a
// read-write open falls back to a maintenance-mode hand-off on a conflict. It is
// the single interception point the proxied mutation commands use in place of
// open.
func (g *globals) openMutator(cmd *cobra.Command) (*mutator, *config.Config, error) {
	cfg, err := g.loadConfig(cmd)
	if err != nil {
		return nil, nil, err
	}
	if !g.readOnly {
		if px := dialServer(cfg.DBPath); px != nil {
			return &mutator{px: px}, cfg, nil
		}
	}
	lib, _, err := g.openLib(cmd, false)
	if err != nil {
		return nil, nil, err
	}
	return &mutator{lib: lib}, cfg, nil
}

// openViaMaintenance performs the maintenance-mode hand-off: it asks the server to
// close its Library and release the write lock, opens the catalog directly (the
// server now yielding the lock), and keeps the proxy connection open on g so the
// command's lifetime brackets the hand-off. cleanup (or, on a crash, the dropped
// connection) tells the server to reopen.
func (g *globals) openViaMaintenance(cmd *cobra.Command, opts waxbin.Options, sock string) (*waxbin.Library, error) {
	px, err := proxy.Dial(sock)
	if err != nil {
		return nil, err
	}
	if err := px.MaintenanceBegin(cmd.Context()); err != nil {
		_ = px.Close()
		return nil, err
	}
	// The server has released the lock; open directly. A brief retry covers the
	// filesystem race where the flock is not yet observably free.
	lib, err := openReadWriteRetry(cmd.Context(), opts)
	if err != nil {
		// Best effort: return the server to service before giving up.
		_ = px.MaintenanceEnd(context.Background())
		_ = px.Close()
		return nil, err
	}
	fmt.Fprintln(errOut(cmd), "waxbin: server is running; took the lock via maintenance mode")
	g.maintConn = px
	return lib, nil
}

// openReadWriteRetry opens the catalog read-write, retrying a transient conflict
// with bounded exponential backoff to cover the flock hand-off race after a server
// releases the lock. The server releases the flock synchronously before answering
// maintenance-begin, but a heavy WAL checkpoint or a loaded filesystem can delay
// when the lock is observably free, so the wait is generous (a few seconds) rather
// than a fixed 200ms that could fail a slow hand-off. It mirrors the daemon-side
// acquireWriteLockRetry so both ends of the hand-off tolerate the same lag.
func openReadWriteRetry(ctx context.Context, opts waxbin.Options) (*waxbin.Library, error) {
	const maxAttempts = 40
	const maxBackoff = 200 * time.Millisecond
	backoff := 5 * time.Millisecond
	for attempt := 0; ; attempt++ {
		lib, err := waxbin.Open(ctx, opts)
		if err == nil {
			return lib, nil
		}
		if !waxerr.Is(err, waxerr.CodeConflict) || attempt >= maxAttempts {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, waxerr.FromContext("cli.openReadWrite", ctx.Err(), waxerr.CodeConflict)
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

// cleanup ends any in-progress maintenance hand-off, telling the server to reopen.
// It runs after the command (and its deferred lib.Close, which releases the lock),
// so the server reacquires a lock that is already free. It is best effort: on a
// crash the dropped connection triggers the same reopen on the server side.
func (g *globals) cleanup() {
	if g.maintConn == nil {
		return
	}
	// Use a fresh context: the command's context may already be canceled (a Ctrl-C
	// that interrupted the command must still return the server to service).
	_ = g.maintConn.MaintenanceEnd(context.Background())
	_ = g.maintConn.Close()
	g.maintConn = nil
}

// advertisedSocket returns the IPC socket a running server advertises in the
// catalog's lockfile, or "" when no server is advertised.
func advertisedSocket(dbPath string) string {
	info, err := waxbin.ReadLockOwner(dbPath)
	if err != nil {
		return ""
	}
	return info.IPCSocket
}

// dialServer connects to an advertised server socket and confirms it is live,
// returning a client or nil. A stale advertisement (the server died leaving the
// lockfile) yields nil, so the caller falls back to a direct open.
func dialServer(dbPath string) *proxy.Client {
	sock := advertisedSocket(dbPath)
	if sock == "" {
		return nil
	}
	px, err := proxy.Dial(sock)
	if err != nil {
		return nil
	}
	// Bound the liveness probe: a stale or wedged server must not hang command
	// startup. On timeout, fall back to a direct open.
	pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := px.Ping(pctx); err != nil {
		_ = px.Close()
		return nil
	}
	return px
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
