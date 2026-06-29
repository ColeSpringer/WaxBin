package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newDoctorCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report catalog health and detected capabilities",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			rep, err := lib.Doctor(ctx(cmd))
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, rep)
			}
			w := out(cmd)
			fmt.Fprintf(w, "catalog:        %s (schema v%d)\n", rep.DBPath, rep.SchemaVersion)
			fmt.Fprintf(w, "mode:           %s\n", readOnlyLabel(rep.ReadOnly))
			if rep.Owner.Owner != "" {
				fmt.Fprintf(w, "write owner:    %s (pid %d)\n", rep.Owner.Owner, rep.Owner.PID)
			}
			fmt.Fprintf(w, "libraries:      %d\n", rep.LibraryCount)
			fmt.Fprintf(w, "items:          %d\n", rep.ItemCount)
			fmt.Fprintln(w, "decode coverage:")
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "  FORMAT\tCATALOG\tANALYSIS")
			for _, c := range rep.Coverage {
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Format, c.Catalog, c.Analysis)
			}
			return tw.Flush()
		},
	}
}

func readOnlyLabel(ro bool) string {
	if ro {
		return "read-only"
	}
	return "read-write (write lock held)"
}
