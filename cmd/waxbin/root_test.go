package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/testcatalog"
	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
)

// TestMain clears every variable a command reads before any test runs, so nothing set in
// the shell running the tests reaches a command. A test that needs one hands it to its
// command through globals.getenv (envOf), never the process, which lets tests run side
// by side; TestCommandsReadTheProcessEnvironment is the one that sets the process's.
func TestMain(m *testing.M) {
	for _, key := range cliEnvKeys() {
		_ = os.Unsetenv(key)
	}
	os.Exit(m.Run())
}

// cliEnvKeys is every variable a command reads, collected by running the CLI's own reads
// against a recording environment, so the list cannot fall behind config.Load and leaves
// any other WAXBIN_ variable, a test's re-exec marker say, alone.
func cliEnvKeys() []string {
	var keys []string
	g := &globals{getenv: func(key string) string {
		keys = append(keys, key)
		return ""
	}}
	g.resolveConfigPath()
	_, _ = config.Load(config.Overrides{}, g.env)
	return keys
}

// TestCommandsReadTheProcessEnvironment: a command handed no environment reads the
// process's, as the shipped binary does: a setting itself (WAXBIN_DB) and the config
// file another names (WAXBIN_CONFIG). Sequential, since it sets the process's.
func TestCommandsReadTheProcessEnvironment(t *testing.T) {
	initOut := func() string {
		t.Helper()
		cmd := newRootCmd(&globals{})
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"init"})
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("init: %v", err)
		}
		return stdout.String()
	}
	viaEnv := testcatalog.Seed(t, filepath.Join(t.TempDir(), "env.db"))
	t.Setenv("WAXBIN_DB", viaEnv)
	if out := initOut(); !strings.Contains(out, viaEnv) {
		t.Errorf("init under WAXBIN_DB printed %q, want the catalog at %s", out, viaEnv)
	}

	viaFile := testcatalog.Seed(t, filepath.Join(t.TempDir(), "file.db"))
	cfg := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"db": %q}`, viaFile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAXBIN_DB", "")
	t.Setenv("WAXBIN_CONFIG", cfg)
	if out := initOut(); !strings.Contains(out, viaFile) {
		t.Errorf("init under WAXBIN_CONFIG printed %q, want the catalog at %s", out, viaFile)
	}
}

// envOf is an environment holding only the given key and value pairs.
func envOf(kv ...string) func(string) string {
	vars := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		vars[kv[i]] = kv[i+1]
	}
	return func(key string) string { return vars[key] }
}

// advertiseSocket writes the owner record a running server leaves beside the
// lockfile, pointing at sock, and returns the catalog path that advertises it.
func advertiseSocket(t *testing.T, sock string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	rec := fmt.Sprintf(`{"owner":"srv","ipc_socket":%q,"pid":1,"acquired_at_ns":1}`, sock)
	if err := os.WriteFile(dbPath+".waxlock.owner", []byte(rec), 0o600); err != nil {
		t.Fatalf("write owner record: %v", err)
	}
	return dbPath
}

// serveOneRefusal answers the first frame on sock with a CodeInvalid error carrying
// msg, the way an other-version server's gate answers a ping.
func serveOneRefusal(t *testing.T, sock, msg string) {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
			return
		}
		_, _ = fmt.Fprintf(conn, `{"ok":false,"error":{"code":"invalid","op":"proxy.dispatch","msg":%q}}`+"\n", msg)
	}()
}

// TestDialServerVersionMismatchIsAHardError: a server that refuses the ping on its
// protocol version is alive, not stale. Falling back to a direct open would only trip
// over its flock with a misleading held-lock error, so the refusal must surface as its
// own error naming both sides and the fix.
func TestDialServerVersionMismatchIsAHardError(t *testing.T) {
	t.Parallel()
	sock := testsock.Path(t)
	refusal := fmt.Sprintf("%s %d (this server speaks 14)", proxy.VersionMismatchPrefix, proxy.ProtocolVersion)
	serveOneRefusal(t, sock, refusal)
	dbPath := advertiseSocket(t, sock)

	px, err := dialServer(dbPath)
	if px != nil {
		t.Fatal("got a client for a version-refusing server")
	}
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("err = %v (code %s), want CodeInvalid", err, waxerr.CodeOf(err))
	}
	for _, want := range []string{refusal, strconv.Itoa(proxy.ProtocolVersion), "rebuild"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q does not mention %q", err, want)
		}
	}
}

// TestDialServerDeadSocketStillFallsBack: an advertisement whose socket nobody serves
// keeps yielding a silent nil, so the direct-open fallback is untouched.
func TestDialServerDeadSocketStillFallsBack(t *testing.T) {
	t.Parallel()
	dbPath := advertiseSocket(t, testsock.Path(t))
	px, err := dialServer(dbPath)
	if px != nil || err != nil {
		t.Fatalf("dialServer = %v, %v, want nil, nil for a dead socket", px, err)
	}
}

// TestOpenLibVersionMismatchSurfacesOverTheLockConflict: the maintenance hand-off is
// the other road to a running server. When its first frame is refused on protocol
// version the server is alive, so reporting the original flock conflict would point
// the operator at the lock instead of at the rebuild.
func TestOpenLibVersionMismatchSurfacesOverTheLockConflict(t *testing.T) {
	t.Parallel()
	sock := testsock.Path(t)
	refusal := fmt.Sprintf("%s %d (this server speaks 14)", proxy.VersionMismatchPrefix, proxy.ProtocolVersion)
	serveOneRefusal(t, sock, refusal)

	// A real store holds the flock and advertises the socket, the way a serving
	// waxbin does; the maintenance-begin frame then meets the refusing fake.
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	st, err := sqlite.Open(context.Background(), sqlite.OpenOptions{Path: testcatalog.Seed(t, dbPath), Owner: "srv", IPCSocket: sock})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	g := &globals{}
	cmd := newRootCmd(g)
	cmd.SetContext(context.Background())
	if err := cmd.ParseFlags([]string{"--db", dbPath}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	_, _, err = g.openLib(cmd, false)
	if err == nil {
		t.Fatal("openLib succeeded against a held lock and a version-refusing server")
	}
	if !waxerr.Is(err, waxerr.CodeInvalid) || !strings.Contains(err.Error(), refusal) {
		t.Fatalf("err = %v (code %s), want the protocol mismatch surfaced", err, waxerr.CodeOf(err))
	}
	if strings.Contains(err.Error(), "owned by another process") {
		t.Fatalf("err = %v, want the lock conflict replaced", err)
	}
}

// TestUnknownSubcommandIsAUsageError: a subcommand a command group does not know is
// refused as a usage error, while a group named alone still prints its help.
func TestUnknownSubcommandIsAUsageError(t *testing.T) {
	t.Parallel()
	run := func(args ...string) (string, error) {
		cmd := newRootCmd(&globals{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	for _, args := range [][]string{{"playlist", "bogus", "x"}, {"db", "bogus"}, {"completion", "bogus"}} {
		if _, err := run(args...); !waxerr.Is(err, waxerr.CodeInvalid) {
			t.Errorf("%v: err = %v, want CodeInvalid", args, err)
		}
	}
	if _, err := run("playlist", "set-ownr"); err == nil || !strings.Contains(err.Error(), "did you mean set-owner?") {
		t.Errorf("a near miss: err = %v, want it to name set-owner", err)
	}
	if out, err := run("playlist"); err != nil || !strings.Contains(out, "set-owner") {
		t.Errorf("playlist alone: err %v, printed %.80q; want its help", err, out)
	}
}

// TestUsageErrorsExitTwo: every malformed command line is a usage error (exit 2), the
// ones cobra raises itself included, while help still answers what it knows.
func TestUsageErrorsExitTwo(t *testing.T) {
	t.Parallel()
	run := func(args ...string) (string, error) {
		cmd := newRootCmd(&globals{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	for _, args := range [][]string{
		{"playlist", "show"},
		{"playlist", "list", "--bogus"},
		{"query", "--limit", "many"},
		{"acquisition", "set", "x", "--type", "manual", "--no-lock", "--keep-lock"},
		{"facet"},
		{"scna"},
		{"help", "bogus"},
		{"help", "playlist", "bogus"},
		{"edit", "--set", "title=x"},
		{"kind", "--to", "song", "x"},
		{"credit"},
		{"enrich", "--item", "x", "--entity", "artist:y"},
	} {
		out, err := run(args...)
		if !waxerr.Is(err, waxerr.CodeInvalid) || exitCodeFor(err) != exitUsage {
			t.Errorf("%v: err = %v (exit %d), want a usage error (exit %d); printed %.120q", args, err, exitCodeFor(err), exitUsage, out)
		}
	}
	if _, err := run("scna"); err == nil || !strings.Contains(err.Error(), "did you mean scan?") {
		t.Errorf("an unknown root command: err = %v, want it to name scan", err)
	}
	if _, err := run("playlist", "help", "bogus"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("playlist help bogus: err = %v, want an unknown topic", err)
	}
	if out, err := run("playlist", "help", "show"); err != nil || !strings.Contains(out, "playlist show PID") {
		t.Errorf("playlist help show: err %v, printed %.80q; want show's help", err, out)
	}
	for _, args := range [][]string{{}, {"playlist"}, {"help"}, {"help", "playlist"}, {"help", "playlist", "show"}, {"playlist", "help"}, {"db", "help"}} {
		if out, err := run(args...); err != nil || !strings.Contains(out, "Usage:") {
			t.Errorf("%v: err %v, printed %.80q; want help", args, err, out)
		}
	}
}

// TestARunnableGroupTakesTheGroupConventions: a command that runs and also holds
// subcommands (organize) answers `help <sub>` with that help, and an unknown word under it
// as any group does, a usage error naming the near miss.
func TestARunnableGroupTakesTheGroupConventions(t *testing.T) {
	t.Parallel()
	run := func(args ...string) (string, error) {
		cmd := newRootCmd(&globals{})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	if out, err := run("organize", "help", "undo"); err != nil || !strings.Contains(out, "organize undo") {
		t.Errorf("organize help undo: err %v, printed %.80q; want undo's help", err, out)
	}
	if out, err := run("organize", "help"); err != nil || !strings.Contains(out, "Usage:") {
		t.Errorf("organize help: err %v, printed %.80q; want organize's help", err, out)
	}
	_, err := run("organize", "histry")
	if !waxerr.Is(err, waxerr.CodeInvalid) || exitCodeFor(err) != exitUsage || !strings.Contains(err.Error(), "did you mean history?") {
		t.Errorf("organize histry: err = %v (exit %d), want a usage error naming history", err, exitCodeFor(err))
	}
	if _, err := run("organize", "help", "bogus"); !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Errorf("organize help bogus: err = %v, want an unknown topic", err)
	}
	// One that takes an argument of its own keeps its own refusal of a wrong count.
	if out, err := run("art", "help"); err != nil || !strings.Contains(out, "Usage:") {
		t.Errorf("art help: err %v, printed %.80q; want art's help", err, out)
	}
	if _, err := run("art", "a", "b"); !waxerr.Is(err, waxerr.CodeInvalid) || strings.Contains(err.Error(), "unknown command") {
		t.Errorf("art a b: err = %v, want its own usage error about the count", err)
	}
}
