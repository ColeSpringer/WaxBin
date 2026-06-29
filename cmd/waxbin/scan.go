package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newScanCmd(g *globals) *cobra.Command {
	var (
		subPath    string
		libraryPID string
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
				LibraryPID: model.PID(libraryPID),
				SubPath:    subPath,
			})
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, struct {
					JobPID       string `json:"jobPid"`
					FilesSeen    int    `json:"filesSeen"`
					AudioFiles   int    `json:"audioFiles"`
					ItemsCreated int    `json:"itemsCreated"`
					ItemsUpdated int    `json:"itemsUpdated"`
					Relinked     int    `json:"relinked"`
					Skipped      int    `json:"skipped"`
					Errored      int    `json:"errored"`
				}{
					string(res.JobPID), res.Total.FilesSeen, res.Total.AudioFiles,
					res.Total.ItemsCreated, res.Total.ItemsUpdated, res.Total.Relinked,
					res.Total.Skipped, res.Total.Errored,
				})
			}

			t := res.Total
			fmt.Fprintf(out(cmd), "Scan complete (job %s)\n", res.JobPID)
			fmt.Fprintf(out(cmd), "  files seen:   %d\n", t.FilesSeen)
			fmt.Fprintf(out(cmd), "  audio files:  %d\n", t.AudioFiles)
			fmt.Fprintf(out(cmd), "  created:      %d\n", t.ItemsCreated)
			fmt.Fprintf(out(cmd), "  updated:      %d\n", t.ItemsUpdated)
			fmt.Fprintf(out(cmd), "  re-linked:    %d\n", t.Relinked)
			fmt.Fprintf(out(cmd), "  skipped:      %d\n", t.Skipped)
			fmt.Fprintf(out(cmd), "  errored:      %d\n", t.Errored)
			return nil
		},
	}
	cmd.Flags().StringVar(&subPath, "sub-path", "", "scan only this sub-path under a library root")
	cmd.Flags().StringVar(&libraryPID, "library", "", "scan only the library with this pid")
	return cmd
}
