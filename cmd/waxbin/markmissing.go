package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newMarkMissingCmd(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "mark-missing PID...",
		Short: "Record that an item's files are gone from disk",
		Long: "Marks each item missing so listings and workers stop treating it as playable. " +
			"The files' rows are kept, so a rescan that finds the bytes again restores the " +
			"item. Each pid is verified before it is written: an item whose files are still " +
			"on disk is reported files-present and left alone, and an unreadable library " +
			"root is refused as a dropped mount rather than recorded as a deletion. " +
			"--force skips both checks, for a caller whose own view of the filesystem is " +
			"the authoritative one. An archived or remote item is refused either way: both " +
			"already say there are no local bytes. Each pid's outcome is reported, since " +
			"the refusals differ in what they tell the caller to do next.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			// Non-nil so a run that fails on its first pid still answers with an empty
			// array rather than a null a consumer has to special-case.
			results := make([]markMissingView, 0, len(args))
			flush := func() error {
				if g.jsonOut {
					return printJSON(cmd, results)
				}
				return nil
			}
			// Stop at the first failure rather than tallying and continuing: the refusal
			// on an unreadable root exists to catch a mount that is not there, and
			// marking the remaining pids after seeing one is exactly the behavior the
			// guard is for. What was already marked is reported before the error, so a
			// partial run is not discarded.
			for _, a := range args {
				outcome, markErr := m.MarkMissing(ctx(cmd), model.PID(a), force)
				if markErr != nil {
					_ = flush()
					return markErr
				}
				results = append(results, markMissingView{PID: a, Outcome: string(outcome)})
				if !g.jsonOut {
					fmt.Fprintf(out(cmd), "%s\t%s\n", a, outcome)
				}
			}
			return flush()
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"skip the on-disk verification and the library-root check")
	return cmd
}

// markMissingView is one pid's outcome. The outcome is the payload a worker acts on,
// which is why the command answers in JSON rather than printing a bare confirmation.
type markMissingView struct {
	PID     string `json:"pid"`
	Outcome string `json:"outcome"`
}
