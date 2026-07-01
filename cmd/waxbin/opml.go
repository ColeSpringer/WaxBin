package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/colespringer/waxbin/waxerr"
)

func newOPMLCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "opml", Short: "Import or export podcast subscriptions as OPML"}
	cmd.AddCommand(newOPMLImportCmd(g), newOPMLExportCmd(g))
	return cmd
}

func newOPMLImportCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Subscribe to every feed in an OPML file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return waxerr.Wrapf(waxerr.CodeIO, "opml.import", err, "reading %s", args[0])
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			results, err := lib.Podcasts().ImportOPML(ctx(cmd), data)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, results)
			}
			ok, failed := 0, 0
			for _, r := range results {
				if r.Err != "" {
					failed++
					fmt.Fprintf(out(cmd), "  failed: %s: %s\n", r.FeedURL, r.Err)
				} else {
					ok++
				}
			}
			fmt.Fprintf(out(cmd), "Imported %d feeds (%d failed)\n", ok, failed)
			return nil
		},
	}
}

func newOPMLExportCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "export [file]",
		Short: "Export subscriptions as OPML (to a file or stdout)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if len(args) == 1 {
				f, err := os.Create(args[0])
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "opml.export", err, "creating %s", args[0])
				}
				if err := lib.Podcasts().ExportOPML(ctx(cmd), f); err != nil {
					_ = f.Close()
					return err
				}
				// Close explicitly so late write or flush errors, such as ENOSPC, are
				// reported instead of calling a truncated file exported.
				if err := f.Close(); err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "opml.export", err, "closing %s", args[0])
				}
				fmt.Fprintf(out(cmd), "Exported subscriptions to %s\n", args[0])
				return nil
			}
			return lib.Podcasts().ExportOPML(ctx(cmd), out(cmd))
		},
	}
}
