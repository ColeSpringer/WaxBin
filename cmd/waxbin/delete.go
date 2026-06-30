package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/trash"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newRmCmd(g *globals) *cobra.Command {
	var (
		permanent bool
		prune     bool
		apply     bool
	)
	cmd := &cobra.Command{
		Use:   "rm <item-pid>...",
		Short: "Delete items (to the trash by default) while keeping their catalog history",
		Long: "Removes the files backing the given items. By default files go to the " +
			"library's same-volume trash and can be restored with `trash restore`. " +
			"--prune or --permanent bypass the trash to reclaim space. The logical item " +
			"is always preserved (archived when it loses its last file). Dry run unless --apply.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if permanent && prune {
				return waxerr.New(waxerr.CodeInvalid, "rm", "--permanent and --prune are mutually exclusive")
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			mode := model.DeleteTrash
			switch {
			case permanent:
				mode = model.DeletePermanent
			case prune:
				mode = model.DeletePrune
			}

			pids := make([]model.PID, len(args))
			for i, a := range args {
				pids[i] = model.PID(a)
			}
			plan, err := lib.PlanDeletePIDs(ctx(cmd), pids, mode)
			if err != nil {
				return err
			}
			if !apply {
				return emitDeletePlan(cmd, g, plan)
			}
			rep, err := lib.ApplyDelete(ctx(cmd), plan)
			if err != nil {
				return err
			}
			return emitDeleteReport(cmd, g, plan, rep)
		},
	}
	cmd.Flags().BoolVar(&permanent, "permanent", false, "delete from disk immediately (no trash)")
	cmd.Flags().BoolVar(&prune, "prune", false, "bypass the trash to reclaim space (policy pruning)")
	cmd.Flags().BoolVar(&apply, "apply", false, "execute the deletion (default is a dry run)")
	return cmd
}

func emitDeletePlan(cmd *cobra.Command, g *globals, plan *trash.Plan) error {
	if g.jsonOut {
		return printJSON(cmd, deletePlanJSON(plan))
	}
	w := out(cmd)
	fmt.Fprintf(w, "Delete plan (mode %s): %d action(s), %d would delete\n", plan.Mode, len(plan.Actions), plan.Pending())
	for _, a := range plan.Actions {
		if a.Skip {
			fmt.Fprintf(w, "  skip  %s (%s)\n", a.Src, a.Reason)
			continue
		}
		if plan.Mode.BypassesTrash() {
			fmt.Fprintf(w, "  rm    %s\n", a.Src)
		} else {
			fmt.Fprintf(w, "  trash %s\n", a.Src)
		}
	}
	fmt.Fprintln(w, "(dry run; pass --apply to execute)")
	return nil
}

func emitDeleteReport(cmd *cobra.Command, g *globals, plan *trash.Plan, rep *trash.Report) error {
	if g.jsonOut {
		return printJSON(cmd, struct {
			Mode           string          `json:"mode"`
			Trashed        int             `json:"trashed"`
			Deleted        int             `json:"deleted"`
			Skipped        int             `json:"skipped"`
			Errored        int             `json:"errored"`
			ReclaimedBytes int64           `json:"reclaimedBytes"`
			Failures       []trash.Failure `json:"failures,omitempty"`
		}{string(plan.Mode), rep.Trashed, rep.Deleted, rep.Skipped, rep.Errored, rep.ReclaimedBytes, rep.Failures})
	}
	w := out(cmd)
	fmt.Fprintf(w, "Deleted (mode %s): trashed %d, removed %d, skipped %d, errored %d, reclaimed %d bytes\n",
		plan.Mode, rep.Trashed, rep.Deleted, rep.Skipped, rep.Errored, rep.ReclaimedBytes)
	for _, f := range rep.Failures {
		fmt.Fprintf(w, "  FAIL %s: %s\n", f.Src, f.Err)
	}
	return nil
}

func deletePlanJSON(plan *trash.Plan) any {
	type actionJSON struct {
		ItemPID string `json:"itemPid"`
		FilePID string `json:"filePid"`
		Src     string `json:"src"`
		Skip    bool   `json:"skip"`
		Reason  string `json:"reason,omitempty"`
	}
	actions := make([]actionJSON, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		actions = append(actions, actionJSON{
			ItemPID: string(a.ItemPID), FilePID: string(a.FilePID),
			Src: a.Src, Skip: a.Skip, Reason: a.Reason,
		})
	}
	return struct {
		Mode    string `json:"mode"`
		Pending int    `json:"pending"`
		Actions any    `json:"actions"`
	}{string(plan.Mode), plan.Pending(), actions}
}
