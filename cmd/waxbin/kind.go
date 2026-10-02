package main

import (
	"fmt"
	"slices"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newKindCmd(g *globals) *cobra.Command {
	var (
		to        string
		writeBack bool
		force     bool
		qf        queryFlags
		rulePath  string
		user      string
		dryRun    bool
		assumeYes bool
	)
	cmd := &cobra.Command{
		Use:   "kind [<pid> ...] --to book|track",
		Short: "Turn tracks into books or books into tracks",
		Long: "Changes items into books or tracks, keeping each item's pid, play state, bookmarks and " +
			"playlist entries. Tracks whose tags name one book become that book under the first item " +
			"named, the others folding into it. A book split into tracks keeps its pid on its primary " +
			"part and gets a new track for each other part, a place inside a part moving with it. " +
			"Every item the change leaves is locked to its kind so later scans keep it, which means " +
			"changing it again takes --force; a book the named tracks joined is locked too. " +
			"--write-back also writes the kind into the named items' files (MEDIATYPE 2 for a " +
			"book, none for a track), so a scan that ignores locks reads a book as a book, and a " +
			"track as a track unless its name (.m4b), a narrator, an audiobook genre or an " +
			"audiobook library says book.\n\n" +
			"Targets are explicit item pids, or the items the selection flags (--artist, --album, " +
			"--rule, ...) match. As with edit, one explicit pid applies at once, while several or a " +
			"selection preview the count and need --yes to apply (or --dry-run to just preview), " +
			"since an item absorbed into another loses its pid for good.",
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := model.Kind(to)
			if kind != model.KindBook && kind != model.KindTrack {
				return fmt.Errorf("--to must be book or track")
			}
			hasSelection := qf.title != "" || qf.artist != "" || qf.album != "" || qf.genre != "" ||
				qf.kind != "" || qf.source != "" || qf.year != 0 || rulePath != ""
			if len(args) > 0 && hasSelection {
				return fmt.Errorf("give explicit pids or selection filters, not both")
			}
			if len(args) == 0 && !hasSelection {
				return fmt.Errorf("specify item pids or a selection filter (--artist, --album, --rule, ...)")
			}
			targets, err := resolveEditTargets(cmd, g, args, hasSelection, rulePath, qf, user)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				fmt.Fprintln(out(cmd), "no items matched; nothing to change")
				return nil
			}
			if dryRun {
				fmt.Fprintf(out(cmd), "%d item(s) would change to a %s:\n", len(targets), kind)
				for _, pid := range targets {
					fmt.Fprintln(out(cmd), "  "+string(pid))
				}
				return nil
			}
			if (hasSelection || len(args) > 1) && !assumeYes {
				fmt.Fprintf(out(cmd), "%d item(s) selected; re-run with --yes to apply (or --dry-run to preview)\n", len(targets))
				return nil
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			rep, err := m.SetItemKind(ctx(cmd), targets, kind, waxbin.KindOptions{WriteBack: writeBack, Force: force},
				func(job model.PID) (*model.Job, error) { return g.tailJob(cmd, job) })
			// A change cut short still reports what it did before the error, since an item it
			// absorbed is gone either way.
			if rep != nil && (err == nil || len(rep.Converted)+len(rep.Absorbed)+len(rep.Created) > 0) {
				if perr := emitKindReport(cmd, g, kind, rep); err == nil {
					err = perr
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "the kind to change the items to: book or track")
	f.BoolVar(&writeBack, "write-back", false, "also write the kind into the named items' files")
	f.BoolVar(&force, "force", false, "change items whose kind is locked")
	f.StringVar(&qf.title, "title", "", "select items whose title matches (substring)")
	f.StringVar(&qf.artist, "artist", "", "select items whose artist matches (substring)")
	f.StringVar(&qf.album, "album", "", "select items whose album matches (substring)")
	f.StringVar(&qf.genre, "genre", "", "select items with this genre (exact)")
	f.StringVar(&qf.kind, "kind", "", "select items of this kind ("+kindList()+", exact)")
	f.StringVar(&qf.source, "source", "", "select items with this acquisition source (exact)")
	f.IntVar(&qf.year, "year", 0, "select items of this year (exact)")
	f.StringVar(&rulePath, "rule", "", "select items with a JSON rule document")
	f.StringVar(&user, "user", "", "user pid for per-user selection fields; empty = default user")
	f.BoolVar(&dryRun, "dry-run", false, "preview the selected items without changing them")
	f.BoolVar(&assumeYes, "yes", false, "apply a selection without the preview gate")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

// emitKindReport prints what a kind change did, and warns about each file whose tags it
// could not write.
func emitKindReport(cmd *cobra.Command, g *globals, kind model.Kind, rep *waxbin.KindReport) error {
	if g.jsonOut {
		return printJSON(cmd, rep)
	}
	fmt.Fprintf(out(cmd), "Changed to a %s: %d converted, %d absorbed, %d created\n",
		kind, len(rep.Converted), len(rep.Absorbed), len(rep.Created))
	for _, p := range rep.Converted {
		fmt.Fprintf(out(cmd), "  converted %s\n", p)
	}
	absorbed := make([]model.PID, 0, len(rep.Absorbed))
	for p := range rep.Absorbed {
		absorbed = append(absorbed, p)
	}
	slices.Sort(absorbed)
	for _, p := range absorbed {
		fmt.Fprintf(out(cmd), "  absorbed  %s -> %s\n", p, rep.Absorbed[p])
	}
	for _, p := range rep.Created {
		fmt.Fprintf(out(cmd), "  created   %s\n", p)
	}
	for _, f := range rep.WriteBackFailures {
		fmt.Fprintf(errOut(cmd), "warning: on-disk write-back skipped for %s: %s\n", f.Path, f.Reason)
	}
	return nil
}
