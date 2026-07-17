package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/scan"
	"github.com/spf13/cobra"
)

func newScanCmd(g *globals) *cobra.Command {
	var (
		subPath        string
		libraryPID     string
		force          bool
		reconcileDelet bool
		ignoreLocks    bool
	)
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Index library roots into the catalog (never decodes PCM)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// When a server holds the write lock, submit the scan to it: the server runs
			// the whole pass in its own process and stays available, and we follow the
			// job through the read-only job row rather than pausing it.
			px, err := g.jobServer(cmd)
			if err != nil {
				return err
			}
			if px != nil {
				defer px.Close()
				jobPID, err := px.RunScan(ctx(cmd), proxy.ScanParams{
					LibraryPID: libraryPID, SubPath: subPath, Force: force, ForceReconcile: reconcileDelet,
					IgnoreLocks: ignoreLocks,
				})
				if err != nil {
					return err
				}
				job, err := g.tailJob(cmd, jobPID)
				if err != nil {
					return err
				}
				var t scan.Result
				if err := unmarshalJobResult(job, &t); err != nil {
					return err
				}
				return renderScanResult(cmd, g, jobPID, t)
			}

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
				IgnoreLocks:    ignoreLocks,
			})
			if err != nil {
				return err
			}
			return renderScanResult(cmd, g, res.JobPID, res.Total)
		},
	}
	var full bool
	cmd.Flags().StringVar(&subPath, "sub-path", "", "scan only this sub-path under a library root")
	cmd.Flags().StringVar(&libraryPID, "library", "", "scan only the library with this pid")
	cmd.Flags().BoolVar(&force, "force", false, "re-hash and re-parse every file, bypassing the incremental fast-path")
	cmd.Flags().BoolVar(&full, "full", false, "alias for --force")
	cmd.Flags().BoolVar(&reconcileDelet, "reconcile-deletions", false,
		"reconcile deletions even past the survival-gate floor (recovery for a deliberate large deletion)")
	cmd.Flags().BoolVar(&ignoreLocks, "ignore-locks", false,
		"re-derive locked fields from disk too, discarding curated edits (use with --force)")
	cmd.PreRun = func(*cobra.Command, []string) { force = force || full }
	return cmd
}

// renderScanResult prints a scan's totals, shared by the direct run and the
// server-run (job-tailed) path.
func renderScanResult(cmd *cobra.Command, g *globals, jobPID model.PID, t scan.Result) error {
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
			string(jobPID), t.FilesSeen, t.AudioFiles, t.ItemsCreated, t.ItemsUpdated,
			t.Relinked, t.Unchanged, t.SidecarsUpdated, t.Missing, t.Skipped, t.Errored,
		})
	}
	fmt.Fprintf(out(cmd), "Scan complete (job %s)\n", jobPID)
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
}
