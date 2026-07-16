package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newMergeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge <artist|release_group|album|genre> <survivor-pid> <loser-pid>...",
		Short: "Merge duplicate entities onto one survivor",
		Long: "Collapses one or more loser entities onto the survivor, re-pointing their " +
			"tracks, albums, genre links, and contributor credits (so play state and " +
			"provenance ride along), unioning MBID/enrichment state, recomputing rollups, " +
			"and deleting the losers. The survivor keeps its public id. Use `audit` to find " +
			"duplicate artists/albums/genres to merge.",
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.MergeEntity(args[0])
			if !et.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "merge",
					"unknown entity type "+args[0]+" (want artist|release_group|album|genre)")
			}
			survivor := model.PID(args[1])
			// Dedup the losers and drop any that equal the survivor: each merge deletes
			// its loser, so a repeated (or self-) loser would fail with CodeNotFound on
			// the second pass and leave the command half-applied.
			losers := dedupLosers(args[2:], survivor)
			if len(losers) == 0 {
				return waxerr.New(waxerr.CodeInvalid, "merge",
					"no distinct loser entities to merge into the survivor")
			}

			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			// One atomic batch: a bad PID rolls the whole merge back rather than
			// leaving earlier losers merged and the command aborted mid-way.
			reports, err := m.MergeMany(ctx(cmd), et, survivor, losers)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toMergeViews(reports))
			}
			w := out(cmd)
			var total int
			for _, r := range reports {
				total += r.Children
				fmt.Fprintf(w, "merged %s %s -> %s (%d children re-pointed)\n",
					r.EntityType, r.Loser, r.Survivor, r.Children)
			}
			fmt.Fprintf(w, "merged %d %s(s) into %s; %d children re-pointed\n",
				len(reports), et, survivor, total)
			return nil
		},
	}
	return cmd
}

// dedupLosers returns the distinct loser PIDs in input order, excluding any equal
// to the survivor.
func dedupLosers(args []string, survivor model.PID) []model.PID {
	seen := map[model.PID]bool{survivor: true}
	out := make([]model.PID, 0, len(args))
	for _, a := range args {
		p := model.PID(a)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

type mergeView struct {
	EntityType string `json:"entityType"`
	Survivor   string `json:"survivor"`
	Loser      string `json:"loser"`
	Children   int    `json:"children"`
}

func toMergeViews(reports []*model.MergeReport) []mergeView {
	out := make([]mergeView, 0, len(reports))
	for _, r := range reports {
		out = append(out, mergeView{
			EntityType: string(r.EntityType),
			Survivor:   string(r.Survivor),
			Loser:      string(r.Loser),
			Children:   r.Children,
		})
	}
	return out
}
