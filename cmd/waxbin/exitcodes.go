package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// exitCodeRef is the published exit-code contract.
var exitCodeRef = []struct {
	Code int    `json:"code"`
	Name string `json:"name"`
	Desc string `json:"description"`
}{
	{exitOK, "ok", "success"},
	{exitError, "error", "generic or internal error"},
	{exitUsage, "usage", "invalid arguments or configuration"},
	{exitNotFound, "not_found", "entity not found"},
	{exitConflict, "conflict", "write ownership or lease conflict"},
	{exitLocked, "locked", "user-locked field or immutable state"},
	{exitIO, "io", "filesystem or I/O failure"},
	{exitUnsupported, "unsupported", "operation unavailable (e.g. mutating in read-only)"},
	{exitCanceled, "canceled", "operation canceled or deadline exceeded"},
}

func newExitCodesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "exit-codes",
		Short: "Print the stable exit-code reference",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.jsonOut {
				return printJSON(cmd, exitCodeRef)
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "CODE\tNAME\tDESCRIPTION")
			for _, e := range exitCodeRef {
				fmt.Fprintf(tw, "%d\t%s\t%s\n", e.Code, e.Name, e.Desc)
			}
			return tw.Flush()
		},
	}
}
