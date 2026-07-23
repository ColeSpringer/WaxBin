package main

import (
	"fmt"
	"path/filepath"
	"text/tabwriter"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newLibraryCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "library",
		Short: "List and manage library roots",
	}
	c.AddCommand(newLibraryListCmd(g), newLibraryAddCmd(g))
	return c
}

func newLibraryListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered library roots",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			libs, err := lib.Libraries(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, libViews(libs))
			}
			if len(libs) == 0 {
				fmt.Fprintln(out(cmd), "(no library roots registered)")
				return nil
			}
			w := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "PID\tMODE\tMEDIA\tPROFILE\tROOT")
			for _, l := range libs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", l.PID, l.Mode, l.MediaType(), l.Profile, l.DisplayRoot)
			}
			return w.Flush()
		},
	}
}

func newLibraryAddCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "add <path[:mode[:media[:profile]]]>",
		Short: "Register a new library root at runtime",
		Long: "Registers a library root in the running catalog without an init or restart. The " +
			"spec is validated against the registered roots, inbox folders, and podcast dir " +
			"(non-overlapping, like init). Re-adding an existing path updates its policy under " +
			"the same pid. Scan, organize, and import pick the root up immediately; a running " +
			"watch does not until it restarts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := config.ParseRootSpec(args[0])
			if err != nil {
				return err
			}
			// Resolve the path here: a proxied add validates on the server, where a
			// relative path would resolve against the server's working directory.
			abs, err := filepath.Abs(spec.Path)
			if err != nil {
				return waxerr.Wrapf(waxerr.CodeInvalid, "library add", err, "resolving %q", spec.Path)
			}
			spec.Path = abs
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			lib, err := m.AddRoot(ctx(cmd), spec)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, libViews([]*model.Library{lib})[0])
			}
			fmt.Fprintf(out(cmd), "Registered library root %s  %s  [%s, %s, %s]\n",
				lib.PID, lib.DisplayRoot, lib.Mode, lib.MediaType(), lib.Profile)
			return nil
		},
	}
}
