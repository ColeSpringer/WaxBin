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
	db.AddCommand(newDBVerifyCmd(g), newDBVacuumCmd(g), newDBMigrateCmd(g))
	return db
}

func newDBVacuumCmd(g *globals) *cobra.Command {
	var (
		integrity bool
		prune     int
	)
	cmd := &cobra.Command{
		Use:   "vacuum",
		Short: "Reclaim space and compact the database",
		Long: "Garbage-collects orphaned art, compacts the database file (VACUUM), and " +
			"optionally runs SQLite's integrity check and trims the change_log. Takes the " +
			"write lock.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			var pruned int
			if prune > 0 {
				if pruned, err = lib.PruneChangeLog(ctx(cmd), prune); err != nil {
					return err
				}
			}
			rep, err := lib.Vacuum(ctx(cmd))
			if err != nil {
				return err
			}
			var problems []string
			if integrity {
				if problems, err = lib.IntegrityCheck(ctx(cmd)); err != nil {
					return err
				}
			}
			ok := !integrity || (len(problems) == 1 && problems[0] == "ok")

			if g.jsonOut {
				// Only report integrity fields when the check actually ran, so a
				// consumer can tell "verified healthy" from "never checked".
				data := map[string]any{
					"artSourcesReclaimed": rep.ArtSourcesReclaimed,
					"thumbnailsReclaimed": rep.ThumbnailsReclaimed,
					"orphansDeleted":      rep.OrphansDeleted,
					"orphansPending":      rep.OrphansPending,
					"changeLogPruned":     pruned,
					"integrityChecked":    integrity,
				}
				if integrity {
					data["integrityOK"] = ok
					data["integrityProblems"] = problems
				}
				if err := printJSON(cmd, data); err != nil {
					return err
				}
			} else {
				w := out(cmd)
				fmt.Fprintf(w, "reclaimed art sources: %d\n", rep.ArtSourcesReclaimed)
				fmt.Fprintf(w, "reclaimed thumbnails:  %d\n", rep.ThumbnailsReclaimed)
				fmt.Fprintf(w, "orphan entities gc'd:  %d\n", rep.OrphansDeleted)
				if rep.OrphansPending > 0 {
					fmt.Fprintf(w, "orphans pending:       %d (within grace window)\n", rep.OrphansPending)
				}
				if prune > 0 {
					fmt.Fprintf(w, "change_log pruned:     %d\n", pruned)
				}
				fmt.Fprintln(w, "database compacted")
				if integrity {
					if ok {
						fmt.Fprintln(w, "integrity check:       ok")
					} else {
						fmt.Fprintf(w, "integrity check:       %d problem(s)\n", len(problems))
						for _, p := range problems {
							fmt.Fprintf(w, "  %s\n", p)
						}
					}
				}
			}
			if !ok {
				return waxerr.New(waxerr.CodeInternal, "db vacuum", "integrity check failed")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&integrity, "integrity", false, "also run SQLite's integrity check")
	cmd.Flags().IntVar(&prune, "prune-changelog", 0, "trim the change_log to its newest N rows (0 = keep all)")
	return cmd
}

func newDBMigrateCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Upgrade the catalog schema to this build",
		Long: "Opens the catalog read-write, which applies any pending migrations (backing " +
			"up the database first), and reports the resulting schema version.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A read-write open migrates as a side effect; report the resulting version.
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			rep, err := lib.Doctor(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, map[string]any{
					"schemaVersion": rep.SchemaVersion,
					"buildVersion":  rep.BuildSchemaVersion,
				})
			}
			fmt.Fprintf(out(cmd), "catalog schema is at v%d (build supports v%d)\n",
				rep.SchemaVersion, rep.BuildSchemaVersion)
			return nil
		},
	}
	return cmd
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
				// Sweep entities that have stayed childless past the grace window, then
				// reclaim any art their removal orphaned.
				if _, err := lib.GCOrphans(ctx(cmd)); err != nil {
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
				fmt.Fprintf(w, "book-duration drift:      %d\n", rep.BookDurationDrift)
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
