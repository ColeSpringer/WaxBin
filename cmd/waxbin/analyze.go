package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newAnalyzeCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "analyze",
		Short: "Decode + fingerprint files needing analysis (separate from scan)",
		Long: "Runs the resumable analyze pass over every audio file whose fingerprint " +
			"is missing or stale. This is the only stage that decodes PCM; scanning never " +
			"does. Files whose codec this build cannot decode are reported as skipped.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd) // mutating: takes the write lock
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Analyze(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toAnalyzeView(res))
			}
			w := out(cmd)
			fmt.Fprintf(w, "analyzed: %d\n", res.Result.Analyzed)
			fmt.Fprintf(w, "skipped:  %d (no decoder for codec)\n", res.Result.Skipped)
			fmt.Fprintf(w, "errored:  %d\n", res.Result.Errored)
			fmt.Fprintf(w, "job:      %s\n", res.JobPID)
			return nil
		},
	}
}
