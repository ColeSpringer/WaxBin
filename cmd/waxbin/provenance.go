package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newLockCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "lock <pid> <field>...",
		Short: "Lock item fields against enrichment and organize writes",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			if err := m.Lock(ctx(cmd), pid, args[1:]...); err != nil {
				return err
			}
			return reportProvenance(cmd, g, m, pid)
		},
	}
}

func newUnlockCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "unlock <pid> <field>...",
		Short: "Clear locks on item fields",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			pid := model.PID(args[0])
			if err := m.Unlock(ctx(cmd), pid, args[1:]...); err != nil {
				return err
			}
			return reportProvenance(cmd, g, m, pid)
		},
	}
}

func newProvenanceCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "provenance <pid>",
		Short: "Show an item's field provenance (source + lock state)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			return reportProvenance(cmd, g, lib, model.PID(args[0]))
		},
	}
}

type provRow struct {
	Field    string `json:"field"`
	Source   string `json:"source"`
	Locked   bool   `json:"locked"`
	Value    string `json:"value,omitempty"`
	Provider string `json:"provider,omitempty"`
	Updated  int64  `json:"updatedAt"`
}

// reportProvenance prints an item's provenance rows. An item with no curated or
// locked fields reports that every field is plain tag-sourced. It reads through a
// provenanceReader, so it works with a directly-opened Library or a proxied
// mutator alike.
func reportProvenance(cmd *cobra.Command, g *globals, lib provenanceReader, pid model.PID) error {
	rows, err := lib.Provenance(ctx(cmd), pid)
	if err != nil {
		return err
	}
	if g.jsonOut {
		out := make([]provRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, provRow{r.Field, string(r.Source), r.Locked, r.Value, r.Provider, r.UpdatedAt})
		}
		return printJSON(cmd, struct {
			PID  model.PID `json:"pid"`
			Rows []provRow `json:"provenance"`
		}{pid, out})
	}
	w := out(cmd)
	if len(rows) == 0 {
		fmt.Fprintf(w, "%s: all fields tag-sourced, unlocked\n", pid)
		return nil
	}
	for _, r := range rows {
		lock := ""
		if r.Locked {
			lock = " [locked]"
		}
		if r.Value != "" {
			fmt.Fprintf(w, "%-12s %s%s = %q\n", r.Field, r.Source, lock, r.Value)
		} else {
			fmt.Fprintf(w, "%-12s %s%s\n", r.Field, r.Source, lock)
		}
	}
	return nil
}
