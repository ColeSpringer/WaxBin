package main

import (
	"fmt"

	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newInitCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the catalog (run migrations) and register library roots",
		Long: "Creates the catalog database if absent, applies migrations, and " +
			"registers the configured library roots. Pass roots with --root path[:mode[:profile]].",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.readOnly {
				return waxerr.New(waxerr.CodeInvalid, "init", "cannot init in --read-only mode")
			}
			lib, cfg, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			libs, err := lib.Libraries(ctx(cmd))
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, struct {
					DBPath        string    `json:"dbPath"`
					SchemaVersion int       `json:"schemaVersion"`
					Libraries     []libView `json:"libraries"`
				}{cfg.DBPath, sqlite.SchemaVersion, libViews(libs)})
			}

			fmt.Fprintf(out(cmd), "Initialized catalog at %s (schema v%d)\n", cfg.DBPath, sqlite.SchemaVersion)
			if len(libs) == 0 {
				fmt.Fprintln(out(cmd), "No library roots registered (pass --root path[:mode[:profile]]).")
			}
			for _, l := range libs {
				fmt.Fprintf(out(cmd), "  %s  %s  [%s, %s]\n", l.PID, l.DisplayRoot, l.Mode, l.Profile)
			}
			return nil
		},
	}
}
