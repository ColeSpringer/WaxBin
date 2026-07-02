package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newScanCmd(g *globals) *cobra.Command {
	var (
		subPath        string
		libraryPID     string
		force          bool
		reconcileDelet bool
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Index library roots into the catalog (never decodes PCM)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			res, err := lib.Scan(ctx(cmd), waxbin.ScanRequest{
				LibraryPID:     model.PID(libraryPID),
				SubPath:        subPath,
				Force:          force,
				ForceReconcile: reconcileDelet,
			})
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, struct {
					JobPID          string `json:"jobPid"`
					FilesSeen       int    `json:"filesSeen"`
					AudioFiles      int    `json:"audioFiles"`
					ItemsCreated    int    `json:"itemsCreated"`
					ItemsUpdated    int    `json:"itemsUpdated"`
					Relinked        int    `json:"relinked"`
					Unchanged       int    `json:"unchanged"`
					SidecarsUpdated int    `json:"sidecarsUpdated"`
					Missing         int    `json:"missing"`
					Skipped         int    `json:"skipped"`
					Errored         int    `json:"errored"`
				}{
					string(res.JobPID), res.Total.FilesSeen, res.Total.AudioFiles,
					res.Total.ItemsCreated, res.Total.ItemsUpdated, res.Total.Relinked,
					res.Total.Unchanged, res.Total.SidecarsUpdated, res.Total.Missing,
					res.Total.Skipped, res.Total.Errored,
				})
			}

			t := res.Total
			fmt.Fprintf(out(cmd), "Scan complete (job %s)\n", res.JobPID)
			fmt.Fprintf(out(cmd), "  files seen:   %d\n", t.FilesSeen)
			fmt.Fprintf(out(cmd), "  audio files:  %d\n", t.AudioFiles)
			fmt.Fprintf(out(cmd), "  created:      %d\n", t.ItemsCreated)
			fmt.Fprintf(out(cmd), "  updated:      %d\n", t.ItemsUpdated)
			fmt.Fprintf(out(cmd), "  unchanged:    %d\n", t.Unchanged)
			if t.SidecarsUpdated > 0 {
				fmt.Fprintf(out(cmd), "  sidecars:     %d updated\n", t.SidecarsUpdated)
			}
			fmt.Fprintf(out(cmd), "  re-linked:    %d\n", t.Relinked)
			fmt.Fprintf(out(cmd), "  missing:      %d\n", t.Missing)
			fmt.Fprintf(out(cmd), "  skipped:      %d\n", t.Skipped)
			fmt.Fprintf(out(cmd), "  errored:      %d\n", t.Errored)
			return nil
		},
	}
	var full bool
	cmd.Flags().StringVar(&subPath, "sub-path", "", "scan only this sub-path under a library root")
	cmd.Flags().StringVar(&libraryPID, "library", "", "scan only the library with this pid")
	cmd.Flags().BoolVar(&force, "force", false, "re-hash and re-parse every file, bypassing the incremental fast-path")
	cmd.Flags().BoolVar(&full, "full", false, "alias for --force")
	cmd.Flags().BoolVar(&reconcileDelet, "reconcile-deletions", false,
		"reconcile deletions even past the survival-gate floor (recovery for a deliberate large deletion)")
	cmd.PreRun = func(*cobra.Command, []string) { force = force || full }
	return cmd
}
