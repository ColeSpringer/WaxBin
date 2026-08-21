package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
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
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid, err := m.PlaylistCreate(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility), nil)
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
			counts, err := playlistCounts(cmd, lib, pls, model.PID(user))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, playlistViews(pls, counts))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tNAME\tKIND\tOWNER\tVIS\tITEMS")
			for _, p := range pls {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\n",
					p.PID, p.Name, p.Kind, p.OwnerName, p.Visibility, counts[p.PID])
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "viewer user pid (empty = default user)")
	return cmd
}

// playlistCounts resolves each listed playlist's member count, keyed by pid, for both
// the table and the JSON view so the two cannot disagree.
//
// A static playlist reuses the count ListPlaylists already selected, which is the same
// COUNT(*) over entries CountItems would run. Only a smart playlist costs a call, and
// there it is unavoidable: its membership is the rule, so its size is not known until
// the rule is evaluated. Calling CountItems for every row would instead turn one query
// into one per playlist for no new information.
func playlistCounts(cmd *cobra.Command, lib *waxbin.Library, pls []*model.Playlist, user model.PID) (map[model.PID]int, error) {
	counts := make(map[model.PID]int, len(pls))
	for _, p := range pls {
		if p.Kind != model.PlaylistSmart {
			counts[p.PID] = p.ItemCount
			continue
		}
		n, err := lib.Playlists().CountItems(ctx(cmd), p.PID, user, nil)
		if err != nil {
			return nil, err
		}
		counts[p.PID] = n
	}
	return counts, nil
}

func newPlaylistShowCmd(g *globals) *cobra.Command {
	var user string
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
			items, err := lib.Playlists().Items(ctx(cmd), model.PID(args[0]), model.PID(user))
			if err != nil {
				return err
			}
			if g.jsonOut {
				// len(items) is the count the text branch prints, and for a smart
				// playlist it is the only true one; pl.ItemCount reads 0 there.
				return printJSON(cmd, map[string]any{
					"playlist": toPlaylistView(pl, len(items)), "items": itemViews(items)})
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
	cmd.Flags().StringVar(&user, "user", "", "evaluate a smart playlist for this user pid (empty = default user)")
	return cmd
}

func newPlaylistAddCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add PID ITEMPID...",
		Short: "Append items to a static playlist",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.PlaylistAdd(ctx(cmd), model.PID(args[0]), pids(args[1:])...); err != nil {
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
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if byPos {
				if err := m.PlaylistRemoveAt(ctx(cmd), model.PID(args[0]), position); err != nil {
					return err
				}
			} else if err := m.PlaylistRemove(ctx(cmd), model.PID(args[0]), model.PID(args[1])); err != nil {
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
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.PlaylistDelete(ctx(cmd), model.PID(args[0])); err != nil {
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
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.PlaylistRename(ctx(cmd), model.PID(args[0]), args[1]); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "renamed")
			return nil
		},
	}
	return cmd
}

func newPlaylistExportCmd(g *globals) *cobra.Command {
	var outPath, user string
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
			return lib.Playlists().ExportM3U8(ctx(cmd), model.PID(args[0]), w, model.PID(user))
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "write the M3U8 to this file instead of stdout")
	cmd.Flags().StringVar(&user, "user", "", "evaluate a smart playlist for this user pid (empty = default user)")
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
			doc, err := io.ReadAll(r)
			if err != nil {
				return waxerr.Wrap(waxerr.CodeIO, "playlist import", err)
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			res, err := m.PlaylistImportM3U8(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility), doc)
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
	cmd.AddCommand(newSmartPlaylistCreateCmd(g), newSmartPlaylistSetRuleCmd(g), newSmartPlaylistExportNSPCmd(g))
	return cmd
}

// loadRule reads a smart-playlist rule from a JSON rule document or a Navidrome
// .nsp file; exactly one of the two paths must be given. Shared by create and
// set-rule so both accept the same rule sources. The report is what the .nsp
// mapping could not carry: empty for a --rule document, and empty for a failure
// that happened before the mapping ran. A refusal costs a second walk, since the
// strict import returns one sentence and the caller wants the list; a success
// does not, since the partial import hands its report back.
func loadRule(op, rulePath, nspPath string, partial bool) (query.Query, playlist.NSPReport, error) {
	var none playlist.NSPReport
	if (rulePath == "") == (nspPath == "") {
		return query.Query{}, none, waxerr.New(waxerr.CodeInvalid, op, "exactly one of --rule or --nsp is required")
	}
	if partial && nspPath == "" {
		return query.Query{}, none, waxerr.New(waxerr.CodeInvalid, op, "--partial applies to --nsp only")
	}
	path := rulePath
	if nspPath != "" {
		path = nspPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return query.Query{}, none, waxerr.Wrapf(waxerr.CodeIO, op, err, "reading %s", path)
	}
	if nspPath == "" {
		q, err := query.ParseRule(data)
		return q, none, err
	}
	if partial {
		imp, err := playlist.ImportNSPPartial(data)
		if err != nil {
			return query.Query{}, nspRefusalReport(data), err
		}
		return imp.Rule, imp.Report, nil
	}
	q, err := playlist.ImportNSP(data)
	if err != nil {
		return query.Query{}, nspRefusalReport(data), err
	}
	return q, playlist.NSPReport{Direction: playlist.NSPDirImport}, nil
}

// nspRefusalReport re-reads a refused document for the whole gap list. Its own
// error is dropped: the only one is unparseable JSON, which the refusal being
// reported already carries, and an empty report simply prints nothing.
func nspRefusalReport(data []byte) playlist.NSPReport {
	rep, _ := playlist.CheckNSPImport(data)
	return rep
}

func newSmartPlaylistCreateCmd(g *globals) *cobra.Command {
	var rulePath, nspPath, user, visibility string
	var partial bool
	cmd := &cobra.Command{
		Use:   "create NAME (--rule FILE | --nsp FILE)",
		Short: "Create a smart playlist from a JSON query rule or a Navidrome .nsp file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rule, rep, err := loadRule("smartplaylist create", rulePath, nspPath, partial)
			if err != nil {
				writeNSPReport(errOut(cmd), rep, false)
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid, err := m.PlaylistCreate(ctx(cmd), args[0], model.PID(user), model.PlaylistVisibility(visibility), &rule)
			if err != nil {
				return err
			}
			// A playlist created with rules quietly missing is the failure the report
			// exists to prevent, so the drop list prints on success too. After the
			// write, not before: nothing was dropped from a playlist that was never
			// created.
			writeNSPReport(errOut(cmd), rep, partial)
			return reportPID(cmd, g, "smartplaylist", pid)
		},
	}
	cmd.Flags().StringVar(&rulePath, "rule", "", "JSON query rule document")
	cmd.Flags().StringVar(&nspPath, "nsp", "", "Navidrome .nsp smart-playlist file")
	cmd.Flags().BoolVar(&partial, "partial", false,
		"with --nsp, import what maps and drop the parts with no WaxBin form instead of refusing")
	cmd.Flags().StringVar(&user, "user", "", "owner user pid (empty = default user)")
	cmd.Flags().StringVar(&visibility, "visibility", "private", "visibility: private|shared")
	return cmd
}

func newSmartPlaylistSetRuleCmd(g *globals) *cobra.Command {
	var rulePath, nspPath string
	var partial bool
	cmd := &cobra.Command{
		Use:   "set-rule PID (--rule FILE | --nsp FILE)",
		Short: "Replace a smart playlist's rule in place (the pid is unchanged)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rule, rep, err := loadRule("smartplaylist set-rule", rulePath, nspPath, partial)
			if err != nil {
				writeNSPReport(errOut(cmd), rep, false)
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.PlaylistSetRule(ctx(cmd), model.PID(args[0]), rule); err != nil {
				return err
			}
			writeNSPReport(errOut(cmd), rep, partial)
			fmt.Fprintln(out(cmd), "rule updated")
			return nil
		},
	}
	cmd.Flags().StringVar(&rulePath, "rule", "", "JSON query rule document")
	cmd.Flags().StringVar(&nspPath, "nsp", "", "Navidrome .nsp smart-playlist file")
	cmd.Flags().BoolVar(&partial, "partial", false,
		"with --nsp, import what maps and drop the parts with no WaxBin form instead of refusing")
	return cmd
}

func newSmartPlaylistExportNSPCmd(g *globals) *cobra.Command {
	var outPath string
	var partial bool
	cmd := &cobra.Command{
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
			var data []byte
			var rep playlist.NSPReport
			if partial {
				e, perr := playlist.ExportNSPPartial(*pl.Rule)
				if perr != nil {
					return refuseNSP(cmd, g, playlist.CheckNSPExport(*pl.Rule), partial, perr)
				}
				data, rep = e.Data, e.Report
			} else {
				// The strict export returns one sentence, so the report is walked for
				// itself here: on a refusal it holds the rest of the list, and on a
				// clean export it holds the notes.
				rep = playlist.CheckNSPExport(*pl.Rule)
				if data, err = playlist.ExportNSP(*pl.Rule); err != nil {
					return refuseNSP(cmd, g, rep, partial, err)
				}
			}
			if outPath != "" {
				if err := os.WriteFile(outPath, data, 0o644); err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "smartplaylist export-nsp", err, "writing %s", outPath)
				}
			}
			if g.jsonOut {
				// With --out the document is already on disk, so the report goes to
				// stdout on its own rather than being written twice.
				doc := data
				if outPath != "" {
					doc = nil
				}
				return printJSON(cmd, nspReportJSON(rep, doc, partial))
			}
			writeNSPReport(errOut(cmd), rep, partial)
			if outPath != "" {
				fmt.Fprintf(out(cmd), "wrote %d bytes to %s\n", len(data), outPath)
				return nil
			}
			fmt.Fprintln(out(cmd), string(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&outPath, "out", "", "write the .nsp document to this file instead of stdout")
	cmd.Flags().BoolVar(&partial, "partial", false,
		"render what maps, dropping the parts of the rule with no .nsp form, instead of refusing")
	return cmd
}

// refuseNSP shows the whole refusal before returning it, so the terminal sees
// every gap rather than only the sentence the error carries. The error itself is
// returned untouched, leaving the exit-code mapping alone.
func refuseNSP(cmd *cobra.Command, g *globals, rep playlist.NSPReport, partial bool, cause error) error {
	if g.jsonOut {
		if err := printJSON(cmd, nspReportJSON(rep, nil, partial)); err != nil {
			return err
		}
		return cause
	}
	// A refusal wrote nothing, so the list is headed as what has no counterpart
	// rather than as what was dropped.
	writeNSPReport(errOut(cmd), rep, false)
	return cause
}

// nspReportJSON is the --json shape of a conversion: the report, and the
// document when there is one for stdout to carry. The document nests as JSON
// rather than as a string holding JSON, so a caller can read into it directly.
func nspReportJSON(rep playlist.NSPReport, doc []byte, partial bool) map[string]any {
	m := map[string]any{"report": rep, "partial": partial}
	if doc != nil {
		m["nsp"] = json.RawMessage(doc)
	}
	return m
}

// writeNSPReport lists a conversion report one entry per line, each with the
// JSON Pointer that locates it in the rule or the document. It writes to the
// advisory stream so a --json stdout stays parseable, and it says nothing at all
// when the mapping carried everything.
func writeNSPReport(w io.Writer, rep playlist.NSPReport, dropped bool) {
	if len(rep.Gaps) > 0 {
		head := "nsp: these parts have no counterpart:"
		if dropped {
			head = "nsp: dropped these parts:"
		}
		fmt.Fprintln(w, head)
		for _, gap := range rep.Gaps {
			fmt.Fprintf(w, "  %s at %s: %s\n", gap.Kind, nspGapAt(gap.Path), gap.Reason)
		}
	}
	for _, note := range rep.Notes {
		fmt.Fprintf(w, "nsp: note: %s at %s: %s\n", note.Kind, nspGapAt(note.Path), note.Reason)
	}
}

func nspGapAt(path string) string {
	if path == "" {
		return "the document"
	}
	return path
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
