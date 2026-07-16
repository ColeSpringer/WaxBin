package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newEditCmd(g *globals) *cobra.Command {
	var (
		sets      []string
		writeBack bool
		noLock    bool
		force     bool
	)
	cmd := &cobra.Command{
		Use:   "edit <pid> --set field=value [--set field=value ...]",
		Short: "Edit an item's metadata fields (catalog-only by default)",
		Long: "Edit metadata fields on a track or book item. Each edit records user provenance " +
			"and, by default, locks the field so enrichment and organize leave it alone. The edit " +
			"is catalog-only unless --write-back also mirrors it into the file's on-disk tags " +
			"(track items only).\n\n" +
			"Track fields: title, artist, album_artist, album, composer, comment, genre, year, " +
			"track_no, disc_no.\n" +
			"Book fields: title, author, narrator, series, subtitle, genre, year.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			edits, err := parseSetFlags(sets)
			if err != nil {
				return err
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			pid := model.PID(args[0])
			opts := waxbin.EditOptions{WriteBack: writeBack, Lock: !noLock, Force: force}
			err = lib.EditFields(ctx(cmd), pid, edits, opts)

			// A write-back failure is its own outcome. The catalog edit committed and only
			// the on-disk tags did not follow, so surface it as a warning rather than a
			// failed edit and still report the updated provenance below.
			var wbErr *waxbin.WriteBackError
			if errors.As(err, &wbErr) {
				for _, f := range wbErr.Failures {
					fmt.Fprintf(errOut(cmd), "warning: on-disk tag write-back skipped for %s: %s\n", f.Path, f.Reason)
				}
			} else if err != nil {
				return err
			}
			return reportProvenance(cmd, g, lib, pid)
		},
	}
	cmd.Flags().StringArrayVar(&sets, "set", nil, "field=value to set (repeatable)")
	cmd.Flags().BoolVar(&writeBack, "write-back", false, "also write the new values into the file's on-disk tags")
	cmd.Flags().BoolVar(&noLock, "no-lock", false, "do not lock the edited fields (they default to locked)")
	cmd.Flags().BoolVar(&force, "force", false, "override a locked field")
	return cmd
}

// parseSetFlags parses repeated field=value flags into an edit map. It needs at least
// one, rejects an empty field name, and rejects a field given twice. A value may be
// empty, which clears the field, and may itself contain '='.
func parseSetFlags(sets []string) (map[string]string, error) {
	if len(sets) == 0 {
		return nil, fmt.Errorf("at least one --set field=value is required")
	}
	edits := make(map[string]string, len(sets))
	for _, s := range sets {
		field, value, ok := strings.Cut(s, "=")
		// Trim both sides so a space added for readability around the '=', as in
		// "title = My Song", stays out of the field name and the stored value.
		field = strings.TrimSpace(field)
		value = strings.TrimSpace(value)
		if !ok || field == "" {
			return nil, fmt.Errorf("invalid --set %q: want field=value", s)
		}
		if _, dup := edits[field]; dup {
			return nil, fmt.Errorf("field %q set more than once", field)
		}
		edits[field] = value
	}
	return edits, nil
}
