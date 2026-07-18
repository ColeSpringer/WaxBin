package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

// newEntityCmd is the parent for entity-level curation: editing identifiers and
// sort-name overrides on a shared artist, release group, or album (as opposed to the
// per-item `edit`). Entity edits are recorded in the entity_curation table and, by
// default, locked so an enrichment pass leaves them alone.
func newEntityCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entity",
		Short: "Curate shared entities (artist, release group, album)",
		Long: "Edit identifiers and sort-name overrides on a shared entity rather than one item.\n\n" +
			"Entity types: artist, release_group, album.\n" +
			"Artist fields: sort, mbid.\n" +
			"Release-group fields: sort, mbid, type (album|ep|single|compilation|audiobook).\n" +
			"Album fields: sort, mbid, barcode, label, catalog_number.",
	}
	cmd.AddCommand(newEntityEditCmd(g), newEntityShowCmd(g))
	return cmd
}

func newEntityEditCmd(g *globals) *cobra.Command {
	var (
		sets      []string
		noLock    bool
		force     bool
		writeBack bool
	)
	cmd := &cobra.Command{
		Use:   "edit <type> <pid> --set field=value [--set field=value ...]",
		Short: "Edit fields on a shared entity",
		Long: "Edit identifiers and sort-name overrides on a shared entity. --write-back also " +
			"fans the values that round-trip through a scan across the entity's member files' " +
			"on-disk tags: an album's BARCODE, LABEL, CATALOGNUMBER, and ALBUMSORT, and an " +
			"artist's ARTISTSORT. A release-group field, a type, and an entity MBID stay " +
			"catalog-only.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.MergeEntity(args[0])
			if !model.EntityEditable(et) {
				return fmt.Errorf("unknown or non-editable entity type %q (want artist, release_group, or album)", args[0])
			}
			pid := model.PID(args[1])
			edits, err := parseSetFlags(sets)
			if err != nil {
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			err = m.EditEntity(ctx(cmd), et, pid, edits, waxbin.EntityEditOptions{WriteBack: writeBack, Lock: !noLock, Force: force})
			if err := surfaceWriteBack(cmd, err); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "edited %d field(s) on %s %s\n", len(edits), et, pid)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&sets, "set", nil, "field=value to set (repeatable; empty value clears)")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the edited fields (they default to locked)")
	f.BoolVar(&force, "force", false, "override a locked entity field")
	f.BoolVar(&writeBack, "write-back", false, "also fan the values across the entity's member files' on-disk tags")
	return cmd
}

func newEntityShowCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <type> <pid>",
		Short: "Show an entity's curated fields",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.MergeEntity(args[0])
			pid := model.PID(args[1])
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			rows, err := lib.EntityCuration(ctx(cmd), et, pid)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, entityCurationViews(rows))
			}
			if len(rows) == 0 {
				fmt.Fprintln(out(cmd), "(no curated fields)")
				return nil
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "FIELD\tSOURCE\tLOCKED\tVALUE")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%t\t%s\n", r.Field, r.Source, r.Locked, r.Value)
			}
			return tw.Flush()
		},
	}
	return cmd
}

// entityCurationView is the JSON shape for an entity_curation row.
type entityCurationView struct {
	Field  string `json:"field"`
	Source string `json:"source"`
	Locked bool   `json:"locked"`
	Value  string `json:"value"`
}

func entityCurationViews(rows []model.EntityCuration) []entityCurationView {
	out := make([]entityCurationView, len(rows))
	for i, r := range rows {
		out[i] = entityCurationView{Field: r.Field, Source: string(r.Source), Locked: r.Locked, Value: r.Value}
	}
	return out
}
