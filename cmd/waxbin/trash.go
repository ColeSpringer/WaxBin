package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newTrashCmd(g *globals) *cobra.Command {
	t := &cobra.Command{
		Use:   "trash",
		Short: "Inspect and manage the deletion trash",
	}
	t.AddCommand(newTrashListCmd(g), newTrashRestoreCmd(g), newTrashEmptyCmd(g), newTrashPurgeCmd(g))
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
	var olderThan string
	cmd := &cobra.Command{
		Use:   "empty [--older-than 30d]",
		Short: "Permanently delete trashed files and reclaim space",
		Long: "Permanently deletes trashed files and their journal rows. --older-than keeps a " +
			"recent undo window: only entries trashed longer ago than the given age are purged. " +
			"Ages take Nd for whole days or a Go duration like 36h.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var window time.Duration
			if olderThan != "" {
				d, err := parseAge(olderThan)
				if err != nil {
					return err
				}
				window = d
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			rep, err := lib.EmptyTrash(ctx(cmd), waxbin.EmptyTrashOptions{OlderThan: window})
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
	cmd.Flags().StringVar(&olderThan, "older-than", "", "purge only entries older than this age (30d, 36h, ...)")
	return cmd
}

func newTrashPurgeCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "purge <trash-pid>...",
		Short: "Permanently delete specific trashed files",
		Long: "Permanently deletes the named trash entries (file and journal row), leaving the " +
			"rest of the trash restorable. Each pid takes its own fs-mutate job lease, so the " +
			"job log shows one entry per pid.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			purged := 0
			var reclaimed int64
			for _, a := range args {
				size, err := lib.PurgeTrash(ctx(cmd), model.PID(a))
				if err != nil {
					// Earlier pids are already irreversibly gone; say so before
					// reporting the failure rather than discarding the progress.
					if purged > 0 {
						fmt.Fprintf(out(cmd), "Purged %d file(s), reclaimed %d bytes before the failure\n",
							purged, reclaimed)
					}
					return err
				}
				purged++
				reclaimed += size
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Purged         int   `json:"purged"`
					ReclaimedBytes int64 `json:"reclaimedBytes"`
				}{purged, reclaimed})
			}
			fmt.Fprintf(out(cmd), "Purged %d file(s), reclaimed %d bytes\n", purged, reclaimed)
			return nil
		},
	}
}

// parseAge parses a retention age: Nd means N whole days (Go's ParseDuration has
// no day unit), anything else goes through time.ParseDuration.
func parseAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, waxerr.New(waxerr.CodeInvalid, "trash empty", "bad age "+s+" (use 30d, 36h, ...)")
	}
	return d, nil
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
