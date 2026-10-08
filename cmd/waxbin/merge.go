package main

import (
	"fmt"
	"strings"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newMergeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge <artist|release_group|album|genre|series> <survivor-pid> <loser-pid>...",
		Short: "Merge duplicate entities onto one survivor",
		Long: "Collapses one or more loser entities onto the survivor, re-pointing their " +
			"tracks, albums, books, genre links, and contributor credits (so play state and " +
			"provenance ride along), unioning MBID/enrichment state, recomputing rollups, " +
			"and deleting the losers. The survivor keeps its public id, and each loser's key " +
			"folds into it, so a file still spelled the loser's way resolves to the survivor " +
			"when it is read again (`entity folds` lists the keys, `entity unfold` forgets " +
			"one); a `db reset` starts the catalog over from the files, folds and all, so tag " +
			"them alike to keep a merge through one. Use `audit` to find duplicate artists/albums/genres to merge. An " +
			"album's key holds its folder, so merged albums stay merged while their files keep " +
			"their folders and tags; a retag or a move to another folder can split them again.",
		Args: cobra.MinimumNArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseMergeEntity("merge", args[0])
			if err != nil {
				return err
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
				fmt.Fprintf(w, "merged %s %s -> %s (%d children re-pointed; folds %s)\n",
					r.EntityType, r.Loser, r.Survivor, r.Children, strings.Join(quoteKeys(r.Folds), ", "))
			}
			fmt.Fprintf(w, "merged %d %s(s) into %s; %d children re-pointed\n",
				len(reports), et, survivor, total)
			fmt.Fprintln(w, mergeNote(et))
			return nil
		},
	}
	return cmd
}

// mergeNote is the caveat a merge prints. The loser's key folds into the survivor, so the
// spelling the merge did away with keeps resolving to it while the catalog holds the fold;
// a db reset starts over from the files, and an album's key holds its folder.
func mergeNote(et model.MergeEntity) string {
	if et == model.MergeAlbum {
		return "note: merged albums stay merged while their files keep their folders and tags; a retag, a move to another folder or a db reset can split them again"
	}
	return "note: files still spelled the loser's way resolve to the survivor, until a db reset starts the catalog over from them; tag them alike to keep the merge through one"
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
	EntityType string   `json:"entityType"`
	Survivor   string   `json:"survivor"`
	Loser      string   `json:"loser"`
	Children   int      `json:"children"`
	Folds      []string `json:"folds,omitempty"`
}

func toMergeViews(reports []*model.MergeReport) []mergeView {
	out := make([]mergeView, 0, len(reports))
	for _, r := range reports {
		out = append(out, mergeView{
			EntityType: string(r.EntityType),
			Survivor:   string(r.Survivor),
			Loser:      string(r.Loser),
			Children:   r.Children,
			Folds:      r.Folds,
		})
	}
	return out
}
