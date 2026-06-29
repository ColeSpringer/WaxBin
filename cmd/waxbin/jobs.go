package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newJobsCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "List recent jobs and their state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			list, err := lib.Jobs(ctx(cmd), limit)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, jobViews(list))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tKIND\tSCOPE\tSTATE\tPROGRESS\tMESSAGE")
			for _, j := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.0f%%\t%s\n",
					j.PID, j.Kind, j.Scope, j.State, j.Progress*100, j.Message)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max jobs to list")
	return cmd
}
