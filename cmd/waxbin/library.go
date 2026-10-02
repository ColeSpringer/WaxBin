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
	c.AddCommand(newLibraryListCmd(g), newLibraryAddCmd(g), newLibrarySetCmd(g))
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
			fmt.Fprintln(w, "PID\tMODE\tMEDIA\tPROFILE\tREAD-ONLY\tFOLDER-FALLBACK\tROOT")
			for _, l := range libs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.PID, l.Mode, l.MediaType(), l.Profile,
					yesNo(l.ReadOnly), yesNo(l.FolderFallback), l.DisplayRoot)
			}
			return w.Flush()
		},
	}
}

func newLibrarySetCmd(g *globals) *cobra.Command {
	var readOnly, writable, folders, noFolders bool
	cmd := &cobra.Command{
		Use:   "set <pid> --read-only|--writable|--folder-fallback|--no-folder-fallback",
		Short: "Set a library's read-only flag or folder fallback",
		Long: "A read-only library keeps WaxBin from writing under its root: edits still land in " +
			"the catalog, but no tag write-back, organize move, import into or out of it, " +
			"delete, or trash restore or purge touches its files, and a staged file bound for " +
			"it waits in the inbox rather than going to another library. Enrichment and ReplayGain " +
			"values stay owed and are written by the first write-back after the library is " +
			"made writable again; an edit's refused write-back is listed in `diagnostics` as " +
			"unsynced and has to be made again.\n\n" +
			"The folder fallback names a track that carries no artist, album artist, or album " +
			"from its folders: the grandparent folder as the artist and the parent as the " +
			"album, less a trailing \" (YYYY)\". Only an in-place library takes it, and files " +
			"already scanned take it on `scan --force`. Each call sets one of the two.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			set := 0
			for _, on := range []bool{readOnly, writable, folders, noFolders} {
				if on {
					set++
				}
			}
			if set != 1 {
				return waxerr.New(waxerr.CodeInvalid, "library set",
					"pass exactly one of --read-only, --writable, --folder-fallback and --no-folder-fallback")
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			var lib *model.Library
			if readOnly || writable {
				lib, err = m.SetLibraryReadOnly(ctx(cmd), pid, readOnly)
			} else {
				lib, err = m.SetLibraryFolderFallback(ctx(cmd), pid, folders)
			}
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, libViews([]*model.Library{lib})[0])
			}
			state := "writable"
			if lib.ReadOnly {
				state = "read-only"
			}
			fallback := "off"
			if lib.FolderFallback {
				fallback = "on"
			}
			fmt.Fprintf(out(cmd), "Library %s  %s  is %s, folder fallback %s\n", lib.PID, lib.DisplayRoot, state, fallback)
			return nil
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "keep WaxBin from writing under the library's root")
	cmd.Flags().BoolVar(&writable, "writable", false, "let WaxBin write under the library's root again")
	cmd.Flags().BoolVar(&folders, "folder-fallback", false, "name untagged tracks from their artist and album folders")
	cmd.Flags().BoolVar(&noFolders, "no-folder-fallback", false, "take no names from folders")
	return cmd
}

func newLibraryAddCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "add <path[:mode[:media[:profile]]]>",
		Short: "Register a new library root at runtime",
		Long: "Registers a library root in the running catalog without an init or restart. The " +
			"spec is validated against the registered roots, inbox folders, and podcast dir " +
			"(non-overlapping, like init). Re-adding an existing path updates its policy under " +
			"the same pid. Scan, organize, and import pick the root up immediately; a running " +
			"watch does not until it restarts. An audiobook root catalogs every file in it as a " +
			"book; music and mixed roots classify each file by its tags. Re-adding a root as " +
			"audiobook turns its tracks into books on the next scan; moving one off audiobook " +
			"takes `scan --force` to re-read its books' tags.",
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
