package main

import (
	"fmt"
	"runtime"

	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/spf13/cobra"
)

// version is overridable at build time via -ldflags "-X main.version=...".
var version = "0.0.0-dev"

func newVersionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version and schema information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.jsonOut {
				return printJSON(cmd, struct {
					Version       string `json:"version"`
					SchemaVersion int    `json:"schemaVersion"`
					CLISchema     int    `json:"cliSchemaVersion"`
					Go            string `json:"go"`
				}{version, sqlite.SchemaVersion, cliSchemaVersion, runtime.Version()})
			}
			fmt.Fprintf(out(cmd), "waxbin %s\n", version)
			fmt.Fprintf(out(cmd), "  storage schema: v%d\n", sqlite.SchemaVersion)
			fmt.Fprintf(out(cmd), "  cli schema:     v%d\n", cliSchemaVersion)
			fmt.Fprintf(out(cmd), "  go:             %s\n", runtime.Version())
			return nil
		},
	}
}
