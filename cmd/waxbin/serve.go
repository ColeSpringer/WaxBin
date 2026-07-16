package main

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newServeCmd(g *globals) *cobra.Command {
	var socket string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a local server that proxies catalog mutations over a unix socket",
		Long: "Opens the catalog read-write (taking the write lock) and serves a local " +
			"control socket. While it runs, other waxbin commands in this catalog no longer " +
			"fail with a write-ownership conflict: fast mutations (edit, lock, play state, " +
			"ratings/stars, playlist membership, user, merge) are proxied through the server, " +
			"and other mutating commands borrow the lock through maintenance mode. Read " +
			"commands always run directly. The socket is created owner-only (0600). Runs until " +
			"interrupted (Ctrl-C / SIGTERM).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.loadConfig(cmd)
			if err != nil {
				return err
			}
			if socket == "" {
				socket = defaultSocketPath(cfg.DBPath)
			}
			if g.readOnly {
				return waxerr.New(waxerr.CodeUnsupported, "serve", "serve requires a read-write catalog")
			}
			opts := waxbin.OptionsFromConfig(cfg, g.logger(cfg))
			opts.IPCSocket = socket
			lib, err := waxbin.Open(cmd.Context(), opts)
			if err != nil {
				return err
			}
			defer lib.Close()

			// Serve until interrupted; a clean shutdown returns nil from Serve.
			ctx, stop := signal.NotifyContext(ctx(cmd), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			fmt.Fprintf(errOut(cmd), "waxbin: serving on %s (Ctrl-C to stop)\n", socket)
			return lib.Serve(ctx, socket)
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "unix socket path (default <db>.waxsock)")
	return cmd
}

// defaultSocketPath is the conventional control socket beside the catalog file.
func defaultSocketPath(dbPath string) string { return dbPath + ".waxsock" }
