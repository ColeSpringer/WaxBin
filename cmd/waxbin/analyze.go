package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

func newAnalyzeCmd(g *globals) *cobra.Command {
	var writeReplayGain bool
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Decode + fingerprint files needing analysis (separate from scan)",
		Long: "Runs the resumable analyze pass over every audio file whose fingerprint " +
			"is missing or stale. This is the only stage that decodes PCM; scanning never " +
			"does. Files whose codec this build cannot decode are reported as skipped.\n\n" +
			"With --write-replaygain, the computed track and album ReplayGain is also " +
			"written back into the files on disk after album aggregation (off by default; " +
			"the catalog is always authoritative, and the audio essence is preserved).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd) // mutating: takes the write lock
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Analyze(ctx(cmd), waxbin.AnalyzeOptions{WriteReplayGainTags: writeReplayGain})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toAnalyzeView(res))
			}
			w := out(cmd)
			fmt.Fprintf(w, "analyzed:   %d\n", res.Result.Analyzed)
			fmt.Fprintf(w, "replaygain: %d\n", res.Result.LoudnessMeasured)
			if res.Result.ReplayGainTagsWritten > 0 {
				fmt.Fprintf(w, "rg tags:    %d written to disk\n", res.Result.ReplayGainTagsWritten)
			}
			// Printed whenever non-zero, independent of the written count: a pass where
			// every write failed writes nothing, and must not look like a pass with
			// nothing to write.
			if res.Result.ReplayGainTagsFailed > 0 {
				fmt.Fprintf(w, "rg tags:    %d failed to write\n", res.Result.ReplayGainTagsFailed)
			}
			if res.Result.ReplayGainTagsUnrepresented > 0 {
				fmt.Fprintf(w, "rg tags:    %d files with a value the format could not store\n", res.Result.ReplayGainTagsUnrepresented)
			}
			fmt.Fprintf(w, "skipped:    %d (cannot decode; retried later)\n", res.Result.Skipped)
			if res.Result.MeasureFailed > 0 {
				fmt.Fprintf(w, "no loudness: %d (fingerprint stored; measurement failed)\n", res.Result.MeasureFailed)
			}
			fmt.Fprintf(w, "errored:    %d\n", res.Result.Errored)
			fmt.Fprintf(w, "job:        %s\n", res.JobPID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&writeReplayGain, "write-replaygain", false, "write computed ReplayGain back into files on disk")
	return cmd
}
