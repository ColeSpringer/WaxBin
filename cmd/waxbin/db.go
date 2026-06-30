package main

import (
	"fmt"

	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newDBCmd(g *globals) *cobra.Command {
	db := &cobra.Command{
		Use:   "db",
		Short: "Database maintenance",
	}
	db.AddCommand(newDBVerifyCmd(g))
	return db
}

func newDBVerifyCmd(g *globals) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check derived data (FTS, rollups, sort keys) against the source rows",
		Long: "Runs the derived-data consistency check: the writer-maintained FTS, " +
			"rollups, and generated sort keys are compared against a fresh recompute from " +
			"the source rows. Reports drift; --fix recomputes the maintained rollups first. " +
			"Exits non-zero when any drift remains.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --fix recomputes rollups and reclaims orphaned art, so it needs the
			// write lock; a plain verify is read-only and runs alongside a writer.
			lib, _, err := g.openLib(cmd, !fix)
			if err != nil {
				return err
			}
			defer lib.Close()

			if fix {
				if err := lib.RefreshRollups(ctx(cmd)); err != nil {
					return err
				}
				if _, _, err := lib.GCArt(ctx(cmd)); err != nil {
					return err
				}
			}

			rep, err := lib.VerifyDerived(ctx(cmd))
			if err != nil {
				return err
			}

			if g.jsonOut {
				if err := printJSON(cmd, toDerivedView(rep)); err != nil {
					return err
				}
			} else {
				w := out(cmd)
				fmt.Fprintf(w, "items missing FTS:        %d\n", rep.ItemsMissingFTS)
				fmt.Fprintf(w, "orphan FTS rows:          %d\n", rep.OrphanFTSRows)
				fmt.Fprintf(w, "artist rollup drift:      %d\n", rep.ArtistRollupDrift)
				fmt.Fprintf(w, "genre rollup drift:       %d\n", rep.GenreRollupDrift)
				fmt.Fprintf(w, "release-group drift:      %d\n", rep.ReleaseGroupRollupDrift)
				fmt.Fprintf(w, "sort-key drift:           %d\n", rep.SortKeyDrift)
				fmt.Fprintf(w, "orphan art sources:       %d\n", rep.OrphanArtSources)
				fmt.Fprintf(w, "orphan thumbnails:        %d\n", rep.OrphanThumbnails)
				fmt.Fprintf(w, "consistent:               %t\n", rep.Consistent())
				// Orphaned art is reclaimable garbage, not corruption, so it does
				// not fail the check; point the operator at --fix to reclaim it.
				if rep.Reclaimable() && !fix {
					fmt.Fprintln(w, "note: orphaned art can be reclaimed with `db verify --fix`")
				}
			}
			if !rep.Consistent() {
				return waxerr.New(waxerr.CodeInvalid, "db verify",
					"derived data is inconsistent; re-run with --fix or re-scan")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "recompute rollups and reclaim orphaned art before verifying (takes the write lock)")
	return cmd
}
