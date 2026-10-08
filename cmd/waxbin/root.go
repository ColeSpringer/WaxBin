package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
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

	allowStale bool

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
		// Cobra checks required flags and flag groups after this hook and returns plain
		// errors; checking them first makes them usage errors.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			if err := c.ValidateRequiredFlags(); err != nil {
				return usageError(c, err)
			}
			if err := c.ValidateFlagGroups(); err != nil {
				return usageError(c, err)
			}
			return nil
		},
	}
	root.SetFlagErrorFunc(usageError)
	pf := root.PersistentFlags()
	pf.StringVar(&g.dbPath, "db", "", "path to the catalog database (env WAXBIN_DB)")
	pf.StringVar(&g.cfgPath, "config", "", "path to a JSON config file (env WAXBIN_CONFIG)")
	pf.StringArrayVar(&g.roots, "root", nil, "library root as path[:mode[:profile]] (repeatable)")
	pf.BoolVar(&g.jsonOut, "json", false, "emit JSON instead of text")
	pf.StringVar(&g.logLevel, "log-level", "", "log level: debug|info|warn|error")
	pf.BoolVar(&g.readOnly, "read-only", false, "open the catalog read-only")
	pf.BoolVar(&g.allowStale, "allow-stale", false,
		"read a catalog built from an older schema baseline instead of refusing it "+
			"(read-only opens only; commands touching a changed table still fail)")

	root.AddCommand(
		newInitCmd(g),
		newLibraryCmd(g),
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
		newProfilesCmd(g),
		newRmCmd(g),
		newTrashCmd(g),
		newMarkMissingCmd(g),
		newInboxCmd(g),
		newImportCmd(g),
		newEditCmd(g),
		newKindCmd(g),
		newEntityCmd(g),
		newCreditCmd(g),
		newDetachCmd(g),
		newTagCmd(g),
		newLockCmd(g),
		newUnlockCmd(g),
		newProvenanceCmd(g),
		newAcquisitionCmd(g),
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
	// Added now rather than at Execute, so the walks below reach them too.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	refuseUnknownSubcommands(root)
	classifyArgErrors(root)
	knowHelpTopics(root)
	return root
}

// usageError marks an error about the command line as a usage error (exit 2).
func usageError(c *cobra.Command, err error) error {
	return waxerr.Wrap(waxerr.CodeInvalid, commandOp(c), err)
}

// commandOp names a command in an error: its path below the root, or cli for the root.
func commandOp(c *cobra.Command) string {
	if !c.HasParent() {
		return "cli"
	}
	return strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
}

// classifyArgErrors makes every command's argument check a usage error; cobra's own
// checks return plain errors.
func classifyArgErrors(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		classifyArgErrors(c)
	}
	if check := cmd.Args; check != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := check(c, args); err != nil {
				return usageError(c, err)
			}
			return nil
		}
	}
}

// knowHelpTopics keeps `help` refusing a topic no command has. Cobra's help command
// reads that off a failed command lookup, and with the root and every group answering
// unknown words themselves the lookup no longer fails.
func knowHelpTopics(root *cobra.Command) {
	help, _, err := root.Find([]string{"help"})
	if err != nil || help == root {
		return
	}
	help.Run = nil
	help.RunE = func(c *cobra.Command, args []string) error { return helpTopic(c.Root(), args) }
}

// helpTopic prints the help of the command path names below from, or refuses a word
// left over at a command group as an unknown topic (a leaf's leftover words are its
// arguments).
func helpTopic(from *cobra.Command, path []string) error {
	target, rest, err := from.Find(path)
	if err != nil || (len(rest) > 0 && target.HasSubCommands()) {
		return waxerr.New(waxerr.CodeInvalid, "help", fmt.Sprintf("unknown help topic %q", strings.Join(path, " ")))
	}
	target.InitDefaultHelpFlag()
	return target.Help()
}

// refuseUnknownSubcommands makes the root and every command group answer an unknown
// subcommand with a usage error and print their help when named alone (a group that also
// runs on its own runs instead, runnableGroup). Cobra refuses
// an unknown command at the root with a plain error and shows a group's help before it
// reads the arguments, so `waxbin playlist typo` printed help and exited 0.
func refuseUnknownSubcommands(cmd *cobra.Command) {
	for _, c := range cmd.Commands() {
		refuseUnknownSubcommands(c)
	}
	if !cmd.HasSubCommands() {
		return
	}
	if cmd.Runnable() {
		runnableGroup(cmd)
		return
	}
	cmd.Args = cobra.ArbitraryArgs
	cmd.DisableFlagsInUseLine = true
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return c.Help()
		}
		// `waxbin playlist help show` asks for help as `waxbin help playlist show` does.
		if args[0] == "help" {
			return helpTopic(c, args[1:])
		}
		return unknownCommand(c, args[0])
	}
}

// runnableGroup gives a command that runs on its own and holds subcommands (organize, art)
// the group conventions: `help` answers as it does under any group, and so does a word,
// as an unknown command, where the command takes no word of its own; one that takes an
// argument (art's item) keeps its own refusals.
func runnableGroup(cmd *cobra.Command) {
	declared, run := cmd.Args, cmd.RunE
	if run == nil {
		plain := cmd.Run
		run = func(c *cobra.Command, args []string) error {
			plain(c, args)
			return nil
		}
		cmd.Run = nil
	}
	cmd.Args = func(c *cobra.Command, args []string) error {
		if len(args) > 0 && args[0] == "help" {
			return nil
		}
		if declared == nil {
			return nil
		}
		if err := declared(c, args); err != nil {
			if len(args) > 0 && declared(c, args[:1]) != nil {
				return unknownCommand(c, args[0])
			}
			return err
		}
		return nil
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 && args[0] == "help" {
			return helpTopic(c, args[1:])
		}
		return run(c, args)
	}
}

// unknownCommand is the usage error for a word no subcommand of c answers, naming the near
// misses.
func unknownCommand(c *cobra.Command, word string) error {
	if c.SuggestionsMinimumDistance <= 0 {
		c.SuggestionsMinimumDistance = 2 // cobra's own default for the root
	}
	msg := fmt.Sprintf("unknown command %q", word)
	if near := c.SuggestionsFor(word); len(near) > 0 {
		msg += " (did you mean " + strings.Join(near, " or ") + "?)"
	}
	return waxerr.New(waxerr.CodeInvalid, commandOp(c), msg)
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
	lib, err := g.openLoaded(cmd, cfg, forceReadOnly)
	if err != nil {
		return nil, nil, err
	}
	return lib, cfg, nil
}

// openLoaded opens the library a loaded config describes.
func (g *globals) openLoaded(cmd *cobra.Command, cfg *config.Config, forceReadOnly bool) (*waxbin.Library, error) {
	opts := waxbin.OptionsFromConfig(cfg, g.logger(cfg))
	opts.ReadOnly = forceReadOnly || g.readOnly
	opts.AllowStaleBaseline = g.allowStale
	lib, err := waxbin.Open(cmd.Context(), opts)
	if err != nil {
		// A read-write open that conflicts with a running server: hand off through
		// maintenance mode so the command still runs holding the lock itself. Read-only
		// opens never take the lock, so they never reach here.
		if !opts.ReadOnly && waxerr.Is(err, waxerr.CodeConflict) {
			if sock := advertisedSocket(cfg.DBPath); sock != "" {
				lib2, err2 := g.openViaMaintenance(cmd, opts, sock)
				if err2 == nil {
					return lib2, nil
				}
				// A failed hand-off normally defers to the original conflict, but a
				// version refusal on the maintenance-begin frame means the server is
				// alive and will refuse everything, so the held-lock message would only
				// misdirect the operator at the flock.
				if msg, ok := versionRefusal(err2); ok {
					return nil, protocolMismatch("cli.open", msg)
				}
			}
		}
		return nil, err
	}
	return lib, nil
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
		px, err := dialServer(cfg.DBPath)
		if err != nil {
			return nil, nil, err
		}
		if px != nil {
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
	px, err := beginMaintenance(cmd.Context(), sock)
	if err != nil {
		return nil, err
	}
	if px == nil {
		return nil, waxerr.New(waxerr.CodeIO, "cli.openViaMaintenance", "no server answered on "+sock)
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

// beginMaintenance asks the server on sock to suspend and release the write lock,
// returning the connection that holds the hand-off. A nil client with a nil error
// means no server answered, as with a stale advertisement.
func beginMaintenance(ctx context.Context, sock string) (*proxy.Client, error) {
	px, err := proxy.Dial(sock)
	if err != nil {
		return nil, nil
	}
	if err := px.MaintenanceBegin(ctx); err != nil {
		_ = px.Close()
		return nil, err
	}
	return px, nil
}

// openReadWriteRetry opens the catalog read-write, retrying a transient conflict
// with bounded exponential backoff to cover the flock hand-off race after a server
// releases the lock. The server releases the flock synchronously before answering
// maintenance-begin, but a heavy WAL checkpoint or a loaded filesystem can delay
// when the lock is observably free, so the wait is generous (a few seconds) rather
// than a fixed 200ms that could fail a slow hand-off. It mirrors the daemon-side
// acquireWriteLockRetry so both ends of the hand-off tolerate the same lag.
func openReadWriteRetry(ctx context.Context, opts waxbin.Options) (*waxbin.Library, error) {
	var lib *waxbin.Library
	err := retryConflict(ctx, func() error {
		var err error
		lib, err = waxbin.Open(ctx, opts)
		return err
	})
	return lib, err
}

// retryConflict runs try until it stops returning CodeConflict, with the bounded
// backoff openReadWriteRetry describes.
func retryConflict(ctx context.Context, try func() error) error {
	const maxAttempts = 40
	const maxBackoff = 200 * time.Millisecond
	backoff := 5 * time.Millisecond
	for attempt := 0; ; attempt++ {
		err := try()
		if err == nil {
			return nil
		}
		if !waxerr.Is(err, waxerr.CodeConflict) || attempt >= maxAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return waxerr.FromContext("cli.openReadWrite", ctx.Err(), waxerr.CodeConflict)
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
	_ = g.endMaintenance()
}

// endMaintenance ends a maintenance hand-off this command took and returns the
// server's answer, which is an error when it could not reopen the catalog. It uses a
// fresh context: the command's may already be canceled (a Ctrl-C that interrupted the
// command must still return the server to service).
func (g *globals) endMaintenance() error {
	if g.maintConn == nil {
		return nil
	}
	err := g.maintConn.MaintenanceEnd(context.Background())
	_ = g.maintConn.Close()
	g.maintConn = nil
	return err
}

// advertisedSocket returns the IPC socket a running server advertises beside the
// catalog's lockfile, or "" when no server is advertised.
func advertisedSocket(dbPath string) string {
	info, err := waxbin.ReadLockOwner(dbPath)
	if err != nil {
		return ""
	}
	return info.IPCSocket
}

// dialServer connects to an advertised server socket and confirms it is live. A nil
// client with a nil error means no server to proxy through, so the caller falls back
// to a direct open: no advertisement, a stale one (the server died leaving its owner
// record behind), or a wedged server. A server that answers the ping by refusing this
// client's protocol version is alive, and the fallback would only trip over its flock
// with a misleading held-lock error, so that refusal comes back as a hard error
// naming both versions instead.
func dialServer(dbPath string) (*proxy.Client, error) {
	sock := advertisedSocket(dbPath)
	if sock == "" {
		return nil, nil
	}
	px, err := proxy.Dial(sock)
	if err != nil {
		return nil, nil
	}
	// Bound the liveness probe: a stale or wedged server must not hang command
	// startup. On timeout, fall back to a direct open.
	pctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := px.Ping(pctx); err != nil {
		_ = px.Close()
		if msg, ok := versionRefusal(err); ok {
			return nil, protocolMismatch("cli.dialServer", msg)
		}
		return nil, nil
	}
	return px, nil
}

// protocolMismatch is the operator-facing form of a server's version refusal: it names
// this client's version, quotes the server's answer (which names the server's), and
// says what to do about it.
func protocolMismatch(op, msg string) error {
	return waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf(
		"the serving waxbin speaks a different protocol version than this client (version %d): %s; rebuild and restart the server so both sides match",
		proxy.ProtocolVersion, msg))
}

// versionRefusal reports whether a proxied call's failure is the server's
// protocol-version gate rather than a dead or wedged socket, returning the server's
// own message when it is. The gate answers CodeInvalid under a stable prefix; matching
// on both keeps every other invalid answer (and every transport failure) on the
// fallback path.
func versionRefusal(err error) (string, bool) {
	var e *waxerr.Error
	if !errors.As(err, &e) || e.Code != waxerr.CodeInvalid ||
		!strings.HasPrefix(e.Msg, proxy.VersionMismatchPrefix) {
		return "", false
	}
	return e.Msg, true
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

// errText is an error's message for a JSON document's error member, empty for none.
// A report printed by a run that then fails carries it, so no host reads the report as
// a success.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// reply answers a command: data in the JSON envelope under --json, else the text as is.
func reply(cmd *cobra.Command, g *globals, data any, text string) error {
	if g.jsonOut {
		return printJSON(cmd, data)
	}
	fmt.Fprint(out(cmd), text)
	return nil
}

// selectionView answers a command that resolved its items and stopped short of
// changing them: nothing matched, a --dry-run, or a selection waiting on --yes.
type selectionView struct {
	Items   []model.PID `json:"items"`
	Applied bool        `json:"applied"`
}

// previewSelection answers with the items a command would change: as JSON, or head
// and one pid per line.
func previewSelection(cmd *cobra.Command, g *globals, targets []model.PID, head string) error {
	if g.jsonOut {
		return printJSON(cmd, selectionView{Items: append([]model.PID{}, targets...)})
	}
	fmt.Fprint(out(cmd), head)
	for _, pid := range targets {
		fmt.Fprintln(out(cmd), "  "+string(pid))
	}
	return nil
}

// awaitYes answers a multi-item change run without --yes: the items it would change,
// and how to apply them (on stderr under --json, so stdout stays one document).
func awaitYes(cmd *cobra.Command, g *globals, targets []model.PID) error {
	const hint = "re-run with --yes to apply (or --dry-run to preview)"
	if g.jsonOut {
		fmt.Fprintln(errOut(cmd), hint)
		return printJSON(cmd, selectionView{Items: append([]model.PID{}, targets...)})
	}
	fmt.Fprintf(out(cmd), "%d item(s) selected; %s\n", len(targets), hint)
	return nil
}

// printJSONStream writes the envelope printJSON does around a document produce writes
// itself, so a large one (the logical export) is never held whole.
func printJSONStream(cmd *cobra.Command, produce func(io.Writer) error) error {
	name, err := json.Marshal(cmd.Name())
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInternal, "cli.json", err)
	}
	w := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(w, "{\n  \"schemaVersion\": %d,\n  \"command\": %s,\n  \"data\": ", cliSchemaVersion, name); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "cli.json", err)
	}
	if err := produce(w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "}\n"); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "cli.json", err)
	}
	return nil
}

func out(cmd *cobra.Command) io.Writer { return cmd.OutOrStdout() }

// errOut is the stream for advisory warnings that must not pollute --json stdout.
func errOut(cmd *cobra.Command) io.Writer { return cmd.ErrOrStderr() }
