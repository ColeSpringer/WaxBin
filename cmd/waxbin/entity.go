package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// newEntityCmd is the parent for entity-level curation, lookup, and per-user state:
// editing identifiers and sort-name overrides on a shared artist, release group, or
// album (as opposed to the per-item `edit`), reading an entity's summary by pid, and
// starring or rating a whole entity for a user. Entity edits are recorded in the
// entity_curation table and, by default, locked so an enrichment pass leaves them alone;
// entity stars and ratings live in entity_play_state, the entity-scoped twin of an item's
// star/rating.
func newEntityCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entity",
		Short: "Curate, look up, and star shared entities",
		Long: "Edit identifiers and sort-name overrides on a shared entity rather than one item, " +
			"look one up by pid, or star and rate a whole entity for a user.\n\n" +
			"Editable entity types: artist, release_group, album.\n" +
			"Artist fields: sort, mbid.\n" +
			"Release-group fields: sort, mbid, type (album|ep|single|compilation|audiobook).\n" +
			"Album fields: sort, mbid, barcode, label, catalog_number.\n" +
			"`entity info` reads those three plus genre and series.\n" +
			"Star-able types (star/rate/state/stars): artist, release_group, album, genre.",
	}
	cmd.AddCommand(
		newEntityEditCmd(g), newEntityShowCmd(g), newEntityInfoCmd(g),
		newEntityStarCmd(g), newEntityRateCmd(g), newEntityStateCmd(g), newEntityStarsCmd(g),
	)
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

// parseStarEntity parses the <type> argument of the star/rate/state/stars commands into
// a model.MergeEntity, the star-able entity vocabulary (artist/release_group/album/genre).
// It is stricter than `entity info` (which also reads genre and series through
// read.EntityKind): only the four mergeable, star-able types are accepted here.
func parseStarEntity(arg string) (model.MergeEntity, error) {
	kind := model.MergeEntity(arg)
	if !kind.Valid() {
		return "", waxerr.New(waxerr.CodeInvalid, "cli.entity",
			fmt.Sprintf("unknown entity type %q (want artist, release_group, album, or genre)", arg))
	}
	return kind, nil
}

// parseEntityRating parses the rating argument of `entity rate`: a 0-100 integer to a
// pointer, or "clear" (an action keyword, matched case-insensitively) to nil for a clear.
// Anything else is a validation error.
func parseEntityRating(arg string) (*int, error) {
	if strings.EqualFold(arg, "clear") {
		return nil, nil
	}
	v, err := strconv.Atoi(arg)
	if err != nil || v < 0 || v > 100 {
		return nil, waxerr.New(waxerr.CodeInvalid, "cli.entity",
			fmt.Sprintf("rating %q must be an integer 0-100 or 'clear'", arg))
	}
	return &v, nil
}

func newEntityStarCmd(g *globals) *cobra.Command {
	var (
		user   string
		unstar bool
		asOf   string
	)
	cmd := &cobra.Command{
		Use:   "star <type> <pid>",
		Short: "Star (or unstar) a whole entity for a user",
		Long: "Stars an artist, release_group, album, or genre for a user, the entity-scoped twin " +
			"of an item star. --as-of records the star at a given time (unix ns or RFC3339) so a " +
			"replayed offline toggle or a migration import orders by recorded time rather than " +
			"server-now.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseStarEntity(args[0])
			if err != nil {
				return err
			}
			asOfNS, err := parseAsOf(asOf)
			if err != nil {
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			uPID, err := resolveUser(cmd, m, user)
			if err != nil {
				return err
			}
			pid := model.PID(args[1])
			if err := m.SetEntityStar(ctx(cmd), uPID, kind, pid, !unstar, asOfNS); err != nil {
				return err
			}
			verb := "starred"
			if unstar {
				verb = "unstarred"
			}
			fmt.Fprintf(out(cmd), "%s %s %s\n", verb, kind, pid)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&user, "user", "", "user name (default user when omitted)")
	f.BoolVar(&unstar, "unstar", false, "remove the star instead of setting it")
	f.StringVar(&asOf, "as-of", "", "record the change at this time (unix ns or RFC3339); default is now")
	return cmd
}

func newEntityRateCmd(g *globals) *cobra.Command {
	var (
		user string
		asOf string
	)
	cmd := &cobra.Command{
		Use:   "rate <type> <pid> <0-100|clear>",
		Short: "Rate (or clear the rating on) a whole entity for a user",
		Long: "Rates an artist, release_group, album, or genre 0-100 for a user, or clears the " +
			"rating with `clear`. --as-of records the change at a given time (unix ns or RFC3339) " +
			"for recorded-time ordering, like `entity star`.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseStarEntity(args[0])
			if err != nil {
				return err
			}
			asOfNS, err := parseAsOf(asOf)
			if err != nil {
				return err
			}
			rating, err := parseEntityRating(args[2])
			if err != nil {
				return err
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			uPID, err := resolveUser(cmd, m, user)
			if err != nil {
				return err
			}
			pid := model.PID(args[1])
			if err := m.SetEntityRating(ctx(cmd), uPID, kind, pid, rating, asOfNS); err != nil {
				return err
			}
			if rating == nil {
				fmt.Fprintf(out(cmd), "cleared rating on %s %s\n", kind, pid)
			} else {
				fmt.Fprintf(out(cmd), "rated %s %s %d/100\n", kind, pid, *rating)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&user, "user", "", "user name (default user when omitted)")
	f.StringVar(&asOf, "as-of", "", "record the change at this time (unix ns or RFC3339); default is now")
	return cmd
}

func newEntityStateCmd(g *globals) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "state <type> <pid>",
		Short: "Show a user's star/rating state for an entity",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseStarEntity(args[0])
			if err != nil {
				return err
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			uPID, err := resolveUser(cmd, lib, user)
			if err != nil {
				return err
			}
			st, err := lib.EntityPlayState(ctx(cmd), uPID, kind, model.PID(args[1]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toEntityPlayStateView(st))
			}
			w := out(cmd)
			fmt.Fprintf(w, "entity:   %s %s\n", st.Kind, st.EntityPID)
			fmt.Fprintf(w, "starred:  %t\n", st.Starred)
			if st.HasRating {
				fmt.Fprintf(w, "rating:   %d/100\n", st.Rating)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user name (default user when omitted)")
	return cmd
}

func newEntityStarsCmd(g *globals) *cobra.Command {
	var user string
	cmd := &cobra.Command{
		Use:   "stars <type>",
		Short: "List a user's starred entities of a type, most recent first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, err := parseStarEntity(args[0])
			if err != nil {
				return err
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			uPID, err := resolveUser(cmd, lib, user)
			if err != nil {
				return err
			}
			states, err := lib.StarredEntities(ctx(cmd), uPID, kind)
			if err != nil {
				return err
			}
			if g.jsonOut {
				views := make([]entityPlayStateView, len(states))
				for i := range states {
					views[i] = toEntityPlayStateView(&states[i])
				}
				return printJSON(cmd, views)
			}
			w := out(cmd)
			for i := range states {
				fmt.Fprintf(w, "%s\t%s\n", states[i].Kind, states[i].EntityPID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user name (default user when omitted)")
	return cmd
}

// entityPlayStateView is the JSON shape for a user's star/rating on an entity. The change
// stamps are unix-ns epochs encoded as decimal strings (",string"), the same contract as
// playStateView: the values exceed IEEE-754 double precision, so a bare number would be
// corrupted by a loose-JSON consumer. 0 (never changed) is omitted.
type entityPlayStateView struct {
	Kind             string `json:"kind"`
	EntityPID        string `json:"entityPid"`
	Rating           *int   `json:"rating,omitempty"`
	Starred          bool   `json:"starred"`
	StarredAt        int64  `json:"starredAt,string,omitempty"`
	RatingChangedAt  int64  `json:"ratingChangedAt,string,omitempty"`
	StarredChangedAt int64  `json:"starredChangedAt,string,omitempty"`
}

func toEntityPlayStateView(st *model.EntityPlayState) entityPlayStateView {
	v := entityPlayStateView{
		Kind: string(st.Kind), EntityPID: string(st.EntityPID), Starred: st.Starred,
		StarredAt: st.StarredAt, RatingChangedAt: st.RatingChangedAt, StarredChangedAt: st.StarredChangedAt,
	}
	if st.HasRating {
		r := st.Rating
		v.Rating = &r
	}
	return v
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
