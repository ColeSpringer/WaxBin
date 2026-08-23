package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/colespringer/waxbin/config"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/port"
	"github.com/colespringer/waxbin/store/sqlite"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newDBCmd(g *globals) *cobra.Command {
	db := &cobra.Command{
		Use:   "db",
		Short: "Database maintenance",
	}
	db.AddCommand(newDBVerifyCmd(g), newDBVacuumCmd(g), newDBThumbsCmd(g), newDBMigrateCmd(g),
		newDBResetCmd(g), newDBReSealSecretsCmd(g))
	return db
}

// newDBResetCmd is the recovery a stale-baseline refusal names. It works at the
// file level first because the catalog it resets is the one that will not open.
func newDBResetCmd(g *globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Move the catalog aside and create an empty one",
		Long: "Renames the catalog (and its WAL/shm sidecars) to a .stale-<ts>.bak copy " +
			"and creates a fresh one in its place. This is the recovery for a catalog built " +
			"from an older schema baseline, which pre-1.0 is edited in place rather than " +
			"migrated. It discards every play position, rating, star, playlist, curation " +
			"edit, podcast subscription and trash entry; only what is on disk comes back, " +
			"and only through `waxbin rebuild`. The previous catalog is renamed rather than " +
			"deleted, so a mis-fire is recoverable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.readOnly {
				return waxerr.New(waxerr.CodeInvalid, "db reset", "cannot reset in --read-only mode")
			}
			cfg, err := g.loadConfig(cmd)
			if err != nil {
				return err
			}
			if !yes {
				return waxerr.New(waxerr.CodeInvalid, "db reset",
					"this discards all catalog state that is not on disk; pass --yes to confirm")
			}

			if err := ensureNoCatalogOwner(cfg.DBPath); err != nil {
				return err
			}

			// Best-effort: a file too broken to census still resets, it just reports less.
			census, _ := port.ReadCensus(ctx(cmd), cfg.DBPath)

			moved, err := moveCatalogAside(cfg.DBPath)
			if err != nil {
				return err
			}

			lib, _, err := g.open(cmd)
			if err != nil {
				// The catalog is already aside, so the failure has to name where it went.
				// Every later command would otherwise just report an uninitialized
				// catalog.
				if moved != "" {
					return waxerr.Wrapf(waxerr.CodeIO, "db reset", err,
						"the previous catalog was saved to %s but the replacement could not be created; "+
							"fix the cause and rename it back", moved)
				}
				return err
			}
			defer lib.Close()

			// The open above re-registers the configured roots; anything else has to be
			// re-added by hand.
			var extraRoots []string
			if census != nil {
				extraRoots = rootsNotConfigured(census.Roots, cfg)
			}

			if g.jsonOut {
				data := map[string]any{"dbPath": cfg.DBPath, "savedTo": moved, "next": "waxbin rebuild"}
				if census != nil {
					data["discardedUnknown"] = census.Partial
					if !census.Partial {
						data["discarded"] = map[string]int{
							"items": census.Items, "playStates": census.PlayStates,
							"playlists": census.Playlists, "podcasts": census.Podcasts,
							"trashEntries": census.TrashEntries,
						}
					}
					data["rootsNotInConfig"] = extraRoots
				}
				return printJSON(cmd, data)
			}

			w := out(cmd)
			if moved == "" {
				fmt.Fprintf(w, "Catalog created at %s (nothing to move aside)\n", cfg.DBPath)
			} else {
				fmt.Fprintf(w, "Catalog reset. Previous catalog saved to %s\n", moved)
			}
			switch {
			case census == nil:
			case census.Partial:
				// Zeros here would read as "the catalog was empty", which is the one thing
				// they do not mean.
				fmt.Fprintln(w, "Discarded: contents unknown (the previous catalog could not be read)")
			default:
				fmt.Fprintf(w, "Discarded: %d items, %d play states, %d playlists, %d podcasts, %d trash entries\n",
					census.Items, census.PlayStates, census.Playlists, census.Podcasts, census.TrashEntries)
			}
			if len(extraRoots) > 0 {
				fmt.Fprintf(w, "Roots not in config (re-add with `waxbin library add`): %s\n",
					strings.Join(extraRoots, ", "))
			}
			// rebuild, not scan: it adopts WAXBIN_ITEM_PID tags, so stamped items keep
			// their original pid instead of minting a fresh one.
			fmt.Fprintln(w, "Run: waxbin rebuild")
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm discarding the catalog")
	return cmd
}

// moveCatalogAside renames the catalog and its sidecars to a timestamped .bak set,
// keeping the -wal/-shm suffixes so the saved files stay a readable trio. Returns
// the new main-file path, or "" when there was no catalog to move. The set moves
// together or not at all, rolling back what it already renamed, because the -wal
// can hold commits the main file lacks.
func moveCatalogAside(dbPath string) (string, error) {
	// The stamp is second-granular and os.Rename overwrites silently, so a retry
	// within the same second would overwrite the backup it just made. Take the first
	// free name instead.
	base := fmt.Sprintf("%s.stale-%s", dbPath, time.Now().UTC().Format("20060102T150405Z"))
	dest := base + ".bak"
	for n := 2; ; n++ {
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			break
		}
		dest = fmt.Sprintf("%s-%d.bak", base, n)
	}
	return moveCatalogTo(dbPath, dest)
}

// moveCatalogTo is moveCatalogAside with the destination supplied, so a test can
// arrange a failure partway through the set.
func moveCatalogTo(dbPath, dest string) (string, error) {
	const op = "db reset"
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	var done [][2]string
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src, dst := dbPath+suffix, dest+suffix
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			for _, d := range done {
				_ = os.Rename(d[1], d[0])
			}
			return "", waxerr.Wrap(waxerr.CodeIO, op, err)
		}
		done = append(done, [2]string{src, dst})
	}
	return dest, nil
}

// ensureNoCatalogOwner refuses the reset while another process owns the catalog:
// renaming a live owner's file leaves it writing to a path nothing points at. It
// probes the flock rather than opening, because an open would migrate and
// checkpoint the file this command is about to preserve, and only the flock tells
// a live owner from a lockfile a crash left behind.
func ensureNoCatalogOwner(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	return sqlite.ProbeWriteLock(dbPath)
}

// rootsNotConfigured returns the catalog's roots that configuration does not name,
// the ones a reset really loses.
func rootsNotConfigured(catalogRoots []string, cfg *config.Config) []string {
	// Matched with pathx.SamePath, not by bytes: the store matches a root by case fold
	// where the platform does and keeps the first-registered spelling, so a catalog root
	// and the configured root naming it can differ in case. A byte compare would warn
	// that a configured root is about to be lost.
	var out []string
	for _, r := range catalogRoots {
		configured := false
		for _, c := range cfg.Roots {
			if pathx.SamePath(c.Path, r) {
				configured = true
				break
			}
		}
		if !configured {
			out = append(out, r)
		}
	}
	return out
}

func newDBReSealSecretsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reseal-secrets",
		Short: "Seal any plaintext secrets with the configured cipher",
		Long: "Seals every plaintext secret in the catalog with the configured secret " +
			"cipher, leaving already-sealed values untouched. It is the one-time adoption " +
			"step after a cipher is first configured, and is idempotent. Requires a build " +
			"with a secret cipher wired in; the stock CLI configures none.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			n, err := lib.ReSealSecrets(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, map[string]any{"sealed": n})
			}
			fmt.Fprintf(out(cmd), "sealed %d secret(s)\n", n)
			return nil
		},
	}
	return cmd
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
	var resorted int
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check derived data (FTS, rollups, sort keys, book durations) against the source rows",
		Long: "Runs the derived-data consistency check: the writer-maintained FTS, " +
			"rollups, and generated sort keys are compared against a fresh recompute from " +
			"the source rows. Reports drift; --fix recomputes the maintained rollups and " +
			"book durations and refolds stale sort keys first. Exits non-zero when any " +
			"drift remains.",
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
				// Rewrites the keys model.SortKey no longer generates, which no rescan
				// reaches.
				n, err := lib.RefreshSortKeys(ctx(cmd))
				if err != nil {
					return err
				}
				resorted = n
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
				view := toDerivedView(rep)
				view.SortKeysRewritten = resorted
				if err := printJSON(cmd, view); err != nil {
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
				// A refold moves the keys page cursors resume against.
				if resorted > 0 {
					fmt.Fprintf(w, "sort keys rewritten:      %d (re-page any open cursors)\n", resorted)
				}
				// Orphaned art is reclaimable garbage, not corruption, so it does
				// not fail the check; point the operator at --fix to reclaim it.
				if rep.Reclaimable() && !fix {
					fmt.Fprintln(w, "note: orphaned art can be reclaimed with `db verify --fix`")
				}
			}
			if !rep.Consistent() {
				// Only --fix rewrites a sort key, so do not offer a re-scan when that
				// is all that drifted.
				msg := "derived data is inconsistent; re-run with --fix or re-scan"
				if rep.SortKeyDriftOnly() {
					msg = "sort keys are stale; re-run with --fix (a re-scan cannot rewrite them)"
				}
				return waxerr.New(waxerr.CodeInvalid, "db verify", msg)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "recompute rollups and book durations, refold stale sort keys, and reclaim orphaned art before verifying (takes the write lock)")
	return cmd
}
