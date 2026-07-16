package main

import (
	"fmt"

	"github.com/colespringer/waxbin/organize"
	"github.com/colespringer/waxbin/query"
	"github.com/spf13/cobra"
)

func newOrganizeCmd(g *globals) *cobra.Command {
	var (
		profile string
		apply   bool
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "organize",
		Short: "Plan (and with --apply, execute) moves for the managed library",
		Long: "Computes destination paths for items under an organization profile and " +
			"moves the files when --apply is given. Without --apply it is a dry run.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Organize every item; the template engine picks the per-kind layout
			// (music vs audiobook), so books are laid out by the audiobook template
			// alongside tracks rather than being excluded.
			b := query.New(query.EntityItems)
			if limit > 0 {
				b.Limit(limit)
			}
			q := b.Build()

			// A dry run only reads; open read-only so it never contends with a running
			// server (and needs no maintenance hand-off).
			if !apply {
				lib, _, err := g.openRead(cmd)
				if err != nil {
					return err
				}
				defer lib.Close()
				plan, err := lib.PlanOrganize(ctx(cmd), q, profile)
				if err != nil {
					return err
				}
				return emitPlan(cmd, g, plan)
			}

			// --apply mutates. Submit to a running server (it plans and moves in its own
			// process, staying available) and tail the job; otherwise run directly.
			px, err := g.jobServer(cmd)
			if err != nil {
				return err
			}
			if px != nil {
				defer px.Close()
				rule, err := query.MarshalRule(q)
				if err != nil {
					return err
				}
				jobPID, err := px.RunOrganize(ctx(cmd), rule, profile)
				if err != nil {
					return err
				}
				job, err := g.tailJob(cmd, jobPID)
				if err != nil {
					return err
				}
				var rr organize.RunResult
				if err := unmarshalJobResult(job, &rr); err != nil {
					return err
				}
				return emitReport(cmd, g, rr.Profile, &rr.Report)
			}

			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			plan, err := lib.PlanOrganize(ctx(cmd), q, profile)
			if err != nil {
				return err
			}
			rep, err := lib.ApplyOrganize(ctx(cmd), plan)
			if err != nil {
				return err
			}
			return emitReport(cmd, g, plan.Profile, rep)
		},
	}
	cmd.Flags().StringVar(&profile, "profile", "", "organization profile (default: the library's configured profile)")
	cmd.Flags().BoolVar(&apply, "apply", false, "execute the moves (default is a dry run)")
	cmd.Flags().IntVar(&limit, "limit", 0, "limit number of items (0 = all)")
	return cmd
}

func emitPlan(cmd *cobra.Command, g *globals, plan *organize.Plan) error {
	if g.jsonOut {
		return printJSON(cmd, planJSON(plan))
	}
	fmt.Fprintf(out(cmd), "Plan (profile %s): %d action(s), %d would move\n",
		plan.Profile, len(plan.Actions), plan.Pending())
	for _, a := range plan.Actions {
		if a.Skip {
			fmt.Fprintf(out(cmd), "  skip  %s (%s)\n", a.Src, a.Reason)
			continue
		}
		fmt.Fprintf(out(cmd), "  move  %s\n        -> %s\n", a.Src, a.Dst)
	}
	fmt.Fprintln(out(cmd), "(dry run; pass --apply to execute)")
	return nil
}

func emitReport(cmd *cobra.Command, g *globals, profile string, rep *organize.Report) error {
	if g.jsonOut {
		return printJSON(cmd, struct {
			Profile       string             `json:"profile"`
			Moved         int                `json:"moved"`
			Skipped       int                `json:"skipped"`
			Errored       int                `json:"errored"`
			SidecarsMoved int                `json:"sidecarsMoved"`
			Failures      []organize.Failure `json:"failures,omitempty"`
			Warnings      []organize.Warning `json:"warnings,omitempty"`
		}{profile, rep.Moved, rep.Skipped, rep.Errored, rep.SidecarsMoved, rep.Failures, rep.Warnings})
	}
	fmt.Fprintf(out(cmd), "Organized (profile %s): moved %d, skipped %d, errored %d, sidecars %d\n",
		profile, rep.Moved, rep.Skipped, rep.Errored, rep.SidecarsMoved)
	for _, f := range rep.Failures {
		fmt.Fprintf(out(cmd), "  FAIL %s -> %s: %s\n", f.Src, f.Dst, f.Err)
	}
	// A warning is not a failure: the move succeeded, so the exit code is unchanged.
	for _, w := range rep.Warnings {
		fmt.Fprintf(out(cmd), "  WARN %s: %s\n", w.Path, w.Message)
	}
	return nil
}

type planActionJSON struct {
	ItemPID string `json:"itemPid"`
	FilePID string `json:"filePid"`
	Src     string `json:"src"`
	Dst     string `json:"dst"`
	Skip    bool   `json:"skip"`
	Reason  string `json:"reason,omitempty"`
}

func planJSON(plan *organize.Plan) any {
	actions := make([]planActionJSON, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		actions = append(actions, planActionJSON{
			ItemPID: string(a.ItemPID), FilePID: string(a.FilePID),
			Src: a.Src, Dst: a.Dst, Skip: a.Skip, Reason: a.Reason,
		})
	}
	return struct {
		Profile string           `json:"profile"`
		Pending int              `json:"pending"`
		Actions []planActionJSON `json:"actions"`
	}{plan.Profile, plan.Pending(), actions}
}
