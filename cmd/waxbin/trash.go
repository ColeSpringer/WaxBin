package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newTrashCmd(g *globals) *cobra.Command {
	t := &cobra.Command{
		Use:   "trash",
		Short: "Inspect and manage the deletion trash",
	}
	t.AddCommand(newTrashListCmd(g), newTrashRestoreCmd(g), newTrashEmptyCmd(g))
	return t
}

func newTrashListCmd(g *globals) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List trashed files (restorable undo journal)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			entries, err := lib.Trash(ctx(cmd), all, 0)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, trashEntriesJSON(entries))
			}
			w := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TRASH-PID\tREASON\tITEM\tORIGINAL PATH\tRESTORED")
			for _, e := range entries {
				restored := "no"
				if e.RestoredAt != 0 {
					restored = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.PID, e.Reason, e.ItemPID, e.OrigDisplay, restored)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(entries) == 0 {
				fmt.Fprintln(out(cmd), "(trash is empty)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include already-restored entries")
	return cmd
}

func newTrashRestoreCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "restore <trash-pid>...",
		Short: "Restore trashed files to their original locations",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			restored := 0
			for _, a := range args {
				if err := lib.RestoreTrash(ctx(cmd), model.PID(a)); err != nil {
					return err
				}
				restored++
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Restored int `json:"restored"`
				}{restored})
			}
			fmt.Fprintf(out(cmd), "Restored %d file(s)\n", restored)
			return nil
		},
	}
}

func newTrashEmptyCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete every trashed file and reclaim space",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			rep, err := lib.EmptyTrash(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, rep)
			}
			fmt.Fprintf(out(cmd), "Emptied trash: purged %d, errored %d, reclaimed %d bytes\n",
				rep.Purged, rep.Errored, rep.ReclaimedBytes)
			return nil
		},
	}
}

func trashEntriesJSON(entries []model.TrashEntry) any {
	type entryJSON struct {
		PID        string `json:"pid"`
		ItemPID    string `json:"itemPid"`
		Orig       string `json:"origPath"`
		Reason     string `json:"reason"`
		Size       int64  `json:"size"`
		TrashedAt  int64  `json:"trashedAt,string"`            // unix ns; a string, see playStateView
		RestoredAt int64  `json:"restoredAt,string,omitempty"` // unix ns; 0 (never) omitted
	}
	out := make([]entryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, entryJSON{
			PID: string(e.PID), ItemPID: string(e.ItemPID), Orig: e.OrigDisplay,
			Reason: e.Reason, Size: e.Size, TrashedAt: e.TrashedAt, RestoredAt: e.RestoredAt,
		})
	}
	return out
}
