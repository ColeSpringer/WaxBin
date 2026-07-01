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
			if rep.NeedsMigration() {
				fmt.Fprintf(w, "                build supports v%d; run a read-write command (e.g. scan) to migrate\n",
					rep.BuildSchemaVersion)
			}
			fmt.Fprintf(w, "mode:           %s\n", readOnlyLabel(rep.ReadOnly))
			if rep.Owner.Owner != "" {
				fmt.Fprintf(w, "write owner:    %s (pid %d)\n", rep.Owner.Owner, rep.Owner.PID)
			}
			fmt.Fprintf(w, "libraries:      %d\n", rep.LibraryCount)
			fmt.Fprintf(w, "items:          %d\n", rep.ItemCount)
			fmt.Fprintf(w, "fingerprints:   %d\n", rep.FingerprintCount)
			fmt.Fprintf(w, "replaygain:     %d\n", rep.LoudnessCount)
			fmt.Fprintf(w, "podcasts:       %d\n", rep.PodcastCount)
			fmt.Fprintf(w, "enrichment:     %s (%d entities, %d matched)\n",
				enabledLabel(rep.EnrichmentEnabled), rep.EnrichedEntities, rep.EnrichedMatched)
			fmt.Fprintf(w, "ffmpeg:         %s\n", presentLabel(rep.FFmpeg))
			fmt.Fprintf(w, "fpcalc:         %s\n", presentLabel(rep.Fpcalc))
			fmt.Fprintln(w, "analyze decode coverage:")
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "  CODEC\tDECODER\tANALYSIS")
			for _, c := range rep.Coverage {
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Codec, c.Decoder, yesNo(c.Analysis))
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

func presentLabel(ok bool) string {
	if ok {
		return "detected"
	}
	return "not found (pure-Go baseline only)"
}

func enabledLabel(ok bool) string {
	if ok {
		return "enabled"
	}
	return "disabled (set enrichment.contact)"
}

func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}
