package main

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/spf13/cobra"
)

// newEntityCmd is the parent for entity-level curation and lookup: editing
// identifiers and sort-name overrides on a shared artist, release group, or album
// (as opposed to the per-item `edit`), and reading an entity's summary by pid.
// Entity edits are recorded in the entity_curation table and, by default, locked
// so an enrichment pass leaves them alone.
func newEntityCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entity",
		Short: "Curate and look up shared entities",
		Long: "Edit identifiers and sort-name overrides on a shared entity rather than one item, " +
			"or look one up by pid.\n\n" +
			"Editable entity types: artist, release_group, album.\n" +
			"Artist fields: sort, mbid.\n" +
			"Release-group fields: sort, mbid, type (album|ep|single|compilation|audiobook).\n" +
			"Album fields: sort, mbid, barcode, label, catalog_number.\n" +
			"`entity info` reads those three plus genre and series.",
	}
	cmd.AddCommand(newEntityEditCmd(g), newEntityShowCmd(g), newEntityInfoCmd(g))
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

// newEntityInfoCmd reads one entity's summary by pid. It is named `info`, not
// `get`: `entity show` already prints the curation rows alone, and get/show as
// two different data contracts under one noun would read ambiguous.
func newEntityInfoCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "info <type> <pid>",
		Short: "Show an entity's summary (identity, links, counts, libraries)",
		Long: "Looks up one shared entity by pid and prints its identity, parent links, " +
			"membership counts, and the libraries its members' files live in.\n\n" +
			"Entity types: artist, release_group, album, genre, series.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := read.EntityKind(args[0])
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			info, err := lib.EntityByPID(ctx(cmd), kind, model.PID(args[1]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toEntityInfoView(info))
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintf(tw, "KIND\t%s\n", info.Kind)
			fmt.Fprintf(tw, "PID\t%s\n", info.PID)
			fmt.Fprintf(tw, "NAME\t%s\n", info.Name)
			fmt.Fprintf(tw, "SORT\t%s\n", info.SortKey)
			if info.MBID != "" {
				fmt.Fprintf(tw, "MBID\t%s\n", info.MBID)
			}
			if info.Type != "" {
				fmt.Fprintf(tw, "TYPE\t%s\n", info.Type)
			}
			if info.Year != 0 {
				fmt.Fprintf(tw, "YEAR\t%d\n", info.Year)
			}
			if info.ArtistPID != "" {
				fmt.Fprintf(tw, "ARTIST\t%s\n", info.ArtistPID)
			}
			if info.ReleaseGroupPID != "" {
				fmt.Fprintf(tw, "RELEASE GROUP\t%s\n", info.ReleaseGroupPID)
			}
			fmt.Fprintf(tw, "ITEMS\t%d\n", info.ItemCount)
			if info.Kind == read.EntityArtist {
				fmt.Fprintf(tw, "RELEASE GROUPS\t%d\n", info.ReleaseGroupCount)
			}
			fmt.Fprintf(tw, "DURATION\t%s\n", durationLabel(info.TotalDurationMS))
			libs := make([]string, len(info.LibraryPIDs))
			for i, p := range info.LibraryPIDs {
				libs[i] = string(p)
			}
			fmt.Fprintf(tw, "LIBRARIES\t%s\n", strings.Join(libs, ", "))
			return tw.Flush()
		},
	}
	return cmd
}

// entityInfoView is the JSON shape for an entity summary.
type entityInfoView struct {
	Kind              string   `json:"kind"`
	PID               string   `json:"pid"`
	Name              string   `json:"name"`
	SortKey           string   `json:"sortKey"`
	MBID              string   `json:"mbid,omitempty"`
	Type              string   `json:"type,omitempty"`
	Year              int      `json:"year,omitempty"`
	ArtistPID         string   `json:"artistPid,omitempty"`
	ReleaseGroupPID   string   `json:"releaseGroupPid,omitempty"`
	ItemCount         int      `json:"itemCount"`
	ReleaseGroupCount int      `json:"releaseGroupCount,omitempty"`
	TotalDurationMS   int64    `json:"totalDurationMs"`
	LibraryPIDs       []string `json:"libraryPids"`
}

func toEntityInfoView(info *read.EntityInfo) entityInfoView {
	libs := make([]string, len(info.LibraryPIDs))
	for i, p := range info.LibraryPIDs {
		libs[i] = string(p)
	}
	return entityInfoView{
		Kind: string(info.Kind), PID: string(info.PID), Name: info.Name, SortKey: info.SortKey,
		MBID: info.MBID, Type: info.Type, Year: info.Year,
		ArtistPID: string(info.ArtistPID), ReleaseGroupPID: string(info.ReleaseGroupPID),
		ItemCount: info.ItemCount, ReleaseGroupCount: info.ReleaseGroupCount,
		TotalDurationMS: info.TotalDurationMS, LibraryPIDs: libs,
	}
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
