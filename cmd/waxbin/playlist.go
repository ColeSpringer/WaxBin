package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/playlist"
	"github.com/colespringer/waxbin/query"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newPlaylistCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "playlist", Short: "Manage static and smart playlists"}
	cmd.AddCommand(
		newPlaylistCreateCmd(g),
		newPlaylistListCmd(g),
		newPlaylistShowCmd(g),
		newPlaylistAddCmd(g),
		newPlaylistRemoveCmd(g),
		newPlaylistDeleteCmd(g),
		newPlaylistRenameCmd(g),
		newPlaylistExportCmd(g),
		newPlaylistImportCmd(g),
	)
	return cmd
}

func newPlaylistCreateCmd(g *globals) *cobra.Command {
	var user, visibility string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an empty static playlist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid, err := lib.Playlists().CreateStatic(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility))
			if err != nil {
				return err
			}
			return reportPID(cmd, g, "playlist", pid)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "owner user pid (empty = default user)")
	cmd.Flags().StringVar(&visibility, "visibility", "private", "visibility: private|shared")
	return cmd
}

func newPlaylistListCmd(g *globals) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List visible playlists",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pls, err := lib.Playlists().List(ctx(cmd), model.PID(user))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, playlistViews(pls))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tNAME\tKIND\tOWNER\tVIS\tITEMS")
			for _, p := range pls {
				// A smart playlist's membership is computed on read, so its stored count
				// is 0; show "-" rather than mislead the reader into thinking it is empty.
				count := fmt.Sprintf("%d", p.ItemCount)
				if p.Kind == model.PlaylistSmart {
					count = "-"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.PID, p.Name, p.Kind, p.OwnerName, p.Visibility, count)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "viewer user pid (empty = default user)")
	return cmd
}

func newPlaylistShowCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show PID",
		Short: "Show a playlist's items (a smart playlist is evaluated on read)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pl, err := lib.Playlists().Get(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			items, err := lib.Playlists().Items(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, map[string]any{"playlist": toPlaylistView(pl), "items": itemViews(items)})
			}
			fmt.Fprintf(out(cmd), "%s (%s, %s) - %d items\n", pl.Name, pl.Kind, pl.Visibility, len(items))
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tTITLE\tARTIST\tALBUM")
			for _, v := range items {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", v.PID, v.Title, v.Artist, v.Album)
			}
			return tw.Flush()
		},
	}
	return cmd
}

func newPlaylistAddCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add PID ITEMPID...",
		Short: "Append items to a static playlist",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Playlists().Add(ctx(cmd), model.PID(args[0]), pids(args[1:])...); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "added %d items\n", len(args)-1)
			return nil
		},
	}
	return cmd
}

func newPlaylistRemoveCmd(g *globals) *cobra.Command {
	var position int
	cmd := &cobra.Command{
		Use:   "remove PID [ITEMPID]",
		Short: "Remove items from a static playlist",
		Long: "Removes items from a static playlist: by ITEMPID (every occurrence), or " +
			"a single occurrence by --position N (so a duplicated item can be removed by " +
			"position).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			byPos := cmd.Flags().Changed("position")
			if byPos && len(args) != 1 {
				return waxerr.New(waxerr.CodeInvalid, "playlist remove", "--position takes only the playlist PID, not an item pid")
			}
			if !byPos && len(args) != 2 {
				return waxerr.New(waxerr.CodeInvalid, "playlist remove", "give an ITEMPID or --position N")
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if byPos {
				if err := lib.Playlists().RemoveAt(ctx(cmd), model.PID(args[0]), position); err != nil {
					return err
				}
			} else if err := lib.Playlists().Remove(ctx(cmd), model.PID(args[0]), model.PID(args[1])); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "removed")
			return nil
		},
	}
	cmd.Flags().IntVar(&position, "position", 0, "remove the single entry at this position instead of an item pid")
	return cmd
}

func newPlaylistDeleteCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete PID",
		Short: "Delete a playlist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Playlists().Delete(ctx(cmd), model.PID(args[0])); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "deleted")
			return nil
		},
	}
	return cmd
}

func newPlaylistRenameCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename PID NAME",
		Short: "Rename a playlist",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Playlists().Rename(ctx(cmd), model.PID(args[0]), args[1]); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "renamed")
			return nil
		},
	}
	return cmd
}

func newPlaylistExportCmd(g *globals) *cobra.Command {
	var outPath string
	cmd := &cobra.Command{
		Use:   "export PID",
		Short: "Export a playlist as an M3U8 file (stdout or --out)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			w := out(cmd)
			if outPath != "" {
				f, err := os.Create(outPath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "playlist export", err, "creating %s", outPath)
				}
				defer f.Close()
				w = f
			}
			return lib.Playlists().ExportM3U8(ctx(cmd), model.PID(args[0]), w)
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "write the M3U8 to this file instead of stdout")
	return cmd
}

func newPlaylistImportCmd(g *globals) *cobra.Command {
	var file, user, visibility string
	cmd := &cobra.Command{
		Use:   "import NAME",
		Short: "Import an M3U8 file as a new static playlist",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := cmd.InOrStdin()
			if file != "" {
				f, err := os.Open(file)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "playlist import", err, "opening %s", file)
				}
				defer f.Close()
				r = f
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			res, err := lib.Playlists().ImportM3U8(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility), r)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, map[string]any{
					"playlistPid": string(res.PlaylistPID), "matched": res.Matched,
					"unmatched": res.Unmatched, "unmatchedPaths": res.UnmatchedPaths,
				})
			}
			fmt.Fprintf(out(cmd), "%s: matched %d, unmatched %d\n", res.PlaylistPID, res.Matched, res.Unmatched)
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "read the M3U8 from this file instead of stdin")
	cmd.Flags().StringVar(&user, "user", "", "owner user pid (empty = default user)")
	cmd.Flags().StringVar(&visibility, "visibility", "private", "visibility: private|shared")
	return cmd
}

func newSmartPlaylistCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "smartplaylist", Short: "Manage smart (query-driven) playlists"}
	cmd.AddCommand(newSmartPlaylistCreateCmd(g), newSmartPlaylistExportNSPCmd(g))
	return cmd
}

func newSmartPlaylistCreateCmd(g *globals) *cobra.Command {
	var rulePath, nspPath, user, visibility string
	cmd := &cobra.Command{
		Use:   "create NAME (--rule FILE | --nsp FILE)",
		Short: "Create a smart playlist from a JSON query rule or a Navidrome .nsp file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (rulePath == "") == (nspPath == "") {
				return waxerr.New(waxerr.CodeInvalid, "smartplaylist create", "exactly one of --rule or --nsp is required")
			}
			var rule query.Query
			if nspPath != "" {
				data, err := os.ReadFile(nspPath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "smartplaylist create", err, "reading %s", nspPath)
				}
				if rule, err = playlist.ImportNSP(data); err != nil {
					return err
				}
			} else {
				data, err := os.ReadFile(rulePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "smartplaylist create", err, "reading %s", rulePath)
				}
				if rule, err = query.ParseRule(data); err != nil {
					return err
				}
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid, err := lib.Playlists().CreateSmart(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility), rule)
			if err != nil {
				return err
			}
			return reportPID(cmd, g, "smartplaylist", pid)
		},
	}
	cmd.Flags().StringVar(&rulePath, "rule", "", "JSON query rule document")
	cmd.Flags().StringVar(&nspPath, "nsp", "", "Navidrome .nsp smart-playlist file")
	cmd.Flags().StringVar(&user, "user", "", "owner user pid (empty = default user)")
	cmd.Flags().StringVar(&visibility, "visibility", "private", "visibility: private|shared")
	return cmd
}

func newSmartPlaylistExportNSPCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "export-nsp PID",
		Short: "Export a smart playlist's rule as a Navidrome .nsp document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pl, err := lib.Playlists().Get(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if pl.Rule == nil {
				return waxerr.New(waxerr.CodeInvalid, "smartplaylist export-nsp", "not a smart playlist: "+args[0])
			}
			data, err := playlist.ExportNSP(*pl.Rule)
			if err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), string(data))
			return nil
		},
	}
}

// pids converts string args to a model.PID slice.
func pids(args []string) []model.PID {
	out := make([]model.PID, len(args))
	for i, a := range args {
		out[i] = model.PID(a)
	}
	return out
}

// reportPID prints a created entity's pid as text or JSON.
func reportPID(cmd *cobra.Command, g *globals, kind string, pid model.PID) error {
	if g.jsonOut {
		return printJSON(cmd, map[string]string{kind: string(pid)})
	}
	fmt.Fprintln(out(cmd), pid)
	return nil
}
