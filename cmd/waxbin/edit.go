package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
		// Batch selection + safety.
		qf         queryFlags
		rulePath   string
		batchPath  string
		user       string
		dryRun     bool
		assumeYes  bool
		skipLocked bool
	)
	cmd := &cobra.Command{
		Use:   "edit [<pid> ...] --set field=value [--set field=value ...]",
		Short: "Edit item metadata fields (catalog-only by default)",
		Long: "Edit metadata fields on one or more track/book items. Each edit records user " +
			"provenance and, by default, locks the field so enrichment and organize leave it alone. " +
			"The edit is catalog-only unless --write-back also mirrors it into each file's on-disk " +
			"tags (track items only).\n\n" +
			"Targets are either explicit item pids, or items selected with the shared query flags " +
			"(--artist, --genre, --year, ...) or a --rule document. A multi-item or selection edit " +
			"previews the count and needs --yes to apply (or --dry-run to just preview).\n\n" +
			"--batch applies a per-item edit map instead: a JSON array of {\"itemPid\": ..., " +
			"\"fields\": {...}} entries (\"-\" reads stdin), each item getting its own values, all " +
			"applied in one atomic catalog transaction. It excludes pids, --set, and the selection " +
			"flags, and honors the same preview gate.\n\n" +
			"Track fields: title, artist, album_artist, album, composer, comment, genre, year, " +
			"track_no, disc_no, isrc, mbid, compilation.\n" +
			"Book fields: title, author, narrator, series, subtitle, genre, year, asin, isbn, " +
			"publisher, edition, description, mbid.",
		RunE: func(cmd *cobra.Command, args []string) error {
			hasSelection := qf.title != "" || qf.artist != "" || qf.album != "" || qf.genre != "" ||
				qf.kind != "" || qf.source != "" || qf.year != 0 || rulePath != ""
			opts := waxbin.EditOptions{WriteBack: writeBack, Lock: !noLock, Force: force, SkipLocked: skipLocked}

			if batchPath != "" {
				if len(args) > 0 || len(sets) > 0 || hasSelection {
					return fmt.Errorf("--batch is exclusive with pids, --set, and the selection flags")
				}
				return runMapBatchEdit(cmd, g, batchPath, opts, dryRun, assumeYes)
			}

			edits, err := parseSetFlags(sets)
			if err != nil {
				return err
			}
			if len(args) > 0 && hasSelection {
				return fmt.Errorf("give explicit pids or selection filters, not both")
			}
			if len(args) == 0 && !hasSelection {
				return fmt.Errorf("specify item pids or a selection filter (--artist, --genre, --rule, ...)")
			}

			// Resolve the target pids first (explicit, or a selection query), so --dry-run
			// is honored for EVERY case, including a single explicit pid. Applying the edit
			// before checking --dry-run would silently mutate (and, with --write-back,
			// rewrite the file) despite the user asking only to preview.
			targets, err := resolveEditTargets(cmd, g, args, hasSelection, rulePath, qf, user)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				fmt.Fprintln(out(cmd), "no items matched; nothing to edit")
				return nil
			}
			if dryRun {
				fmt.Fprintf(out(cmd), "%d item(s) would be edited:\n", len(targets))
				for _, pid := range targets {
					fmt.Fprintln(out(cmd), "  "+string(pid))
				}
				return nil
			}

			// A single explicit pid keeps the original single-item behavior: it applies
			// immediately and reports provenance, with no preview gate.
			if len(args) == 1 {
				return runSingleEdit(cmd, g, targets[0], edits, opts)
			}
			// A multi-item or selection edit needs an explicit --yes to apply.
			if !assumeYes {
				fmt.Fprintf(out(cmd), "%d item(s) selected; re-run with --yes to apply (or --dry-run to preview)\n", len(targets))
				return nil
			}
			return runBatchEdit(cmd, g, targets, edits, opts)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&sets, "set", nil, "field=value to set (repeatable)")
	f.BoolVar(&writeBack, "write-back", false, "also write the new values into each file's on-disk tags")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the edited fields (they default to locked)")
	f.BoolVar(&force, "force", false, "override a locked field")
	// Selection flags (mirror the query command).
	f.StringVar(&qf.title, "title", "", "select items whose title matches (substring)")
	f.StringVar(&qf.artist, "artist", "", "select items whose artist matches (substring)")
	f.StringVar(&qf.album, "album", "", "select items whose album matches (substring)")
	f.StringVar(&qf.genre, "genre", "", "select items with this genre (exact)")
	f.StringVar(&qf.kind, "kind", "", "select items of this kind: track|book (exact)")
	f.StringVar(&qf.source, "source", "", "select items with this acquisition source (exact)")
	f.IntVar(&qf.year, "year", 0, "select items of this year (exact)")
	f.StringVar(&rulePath, "rule", "", "select items with a JSON rule document")
	f.StringVar(&batchPath, "batch", "", "apply a per-item edit map from a JSON file (\"-\" = stdin)")
	f.StringVar(&user, "user", "", "user pid for per-user selection fields; empty = default user")
	f.BoolVar(&dryRun, "dry-run", false, "preview the selected items without editing")
	f.BoolVar(&assumeYes, "yes", false, "apply a multi-item/selection edit without the preview gate")
	f.BoolVar(&skipLocked, "skip-locked", false, "skip locked items instead of failing the batch")
	return cmd
}

// runSingleEdit applies a one-item edit and reports its provenance, matching the
// original single-pid behavior.
func runSingleEdit(cmd *cobra.Command, g *globals, pid model.PID, edits map[string]string, opts waxbin.EditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := surfaceWriteBack(cmd, m.EditFields(ctx(cmd), pid, edits, opts)); err != nil {
		return err
	}
	return reportProvenance(cmd, g, m, pid)
}

// resolveEditTargets returns the pids to edit: the explicit args, or the items a
// selection query matches.
func resolveEditTargets(cmd *cobra.Command, g *globals, args []string, hasSelection bool, rulePath string, qf queryFlags, user string) ([]model.PID, error) {
	if !hasSelection {
		out := make([]model.PID, len(args))
		for i, a := range args {
			out[i] = model.PID(a)
		}
		return out, nil
	}
	q, err := buildQuery(cmd, rulePath, qf)
	if err != nil {
		return nil, err
	}
	lib, _, err := g.openRead(cmd)
	if err != nil {
		return nil, err
	}
	defer lib.Close()
	items, err := lib.Query(ctx(cmd), q, model.PID(user))
	if err != nil {
		return nil, err
	}
	pids := make([]model.PID, len(items))
	for i, v := range items {
		pids[i] = v.PID
	}
	return pids, nil
}

// runBatchEdit applies the atomic multi-item edit and prints a summary.
func runBatchEdit(cmd *cobra.Command, g *globals, targets []model.PID, edits map[string]string, opts waxbin.EditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()

	res, err := m.EditManyFields(ctx(cmd), targets, edits, opts)
	// The catalog batch is atomic and, on a non-nil error, already committed (the error
	// is a post-commit write-back failure such as a canceled context). Surface which
	// items were edited before returning it, so the operator does not wrongly retry.
	// The result is nil only when the whole call failed (or over a proxy that cannot
	// convey a partial result).
	if res != nil {
		printBatchEditResult(cmd, res)
	}
	return err
}

// loadBatchEdits reads a per-item edit map: a JSON array of {"itemPid", "fields"}
// entries, from a file or stdin ("-"). The shape matches the proxy's edit_batch
// wire entries, so one document drives the CLI and an embedder's client alike.
// Beyond the document shape, the loader also rejects a repeated itemPid, since
// the engine refuses that unconditionally and a dry-run listing both entries as
// applicable would mislead. Everything needing the catalog (field validity per
// kind, whether the items exist) stays the engine's job, so a preview describes
// the batch, not a guaranteed outcome.
func loadBatchEdits(cmd *cobra.Command, path string) ([]model.ItemFieldEdit, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(cmd.InOrStdin())
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	var entries []struct {
		ItemPID string            `json:"itemPid"`
		Fields  map[string]string `json:"fields"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse batch document: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("batch document has no entries")
	}
	out := make([]model.ItemFieldEdit, len(entries))
	seen := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.ItemPID == "" {
			return nil, fmt.Errorf("batch entry %d has no itemPid", i)
		}
		if len(e.Fields) == 0 {
			return nil, fmt.Errorf("batch entry %d (%s) has no fields", i, e.ItemPID)
		}
		if first, dup := seen[e.ItemPID]; dup {
			return nil, fmt.Errorf("batch entries %d and %d both name item %s; give each item one map", first, i, e.ItemPID)
		}
		seen[e.ItemPID] = i
		out[i] = model.ItemFieldEdit{ItemPID: model.PID(e.ItemPID), Fields: e.Fields}
	}
	return out, nil
}

// runMapBatchEdit loads a --batch document and applies it through the atomic
// per-item-map edit, honoring the same dry-run preview and --yes gate as a
// selection edit. The preview describes the document (the loader has already
// rejected a malformed one), not a promise the engine will accept every entry.
func runMapBatchEdit(cmd *cobra.Command, g *globals, batchPath string, opts waxbin.EditOptions, dryRun, assumeYes bool) error {
	edits, err := loadBatchEdits(cmd, batchPath)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out(cmd), "%d item(s) in the batch:\n", len(edits))
		for _, e := range edits {
			fmt.Fprintf(out(cmd), "  %s (%d field(s))\n", e.ItemPID, len(e.Fields))
		}
		return nil
	}
	if !assumeYes {
		fmt.Fprintf(out(cmd), "%d item(s) in the batch; re-run with --yes to apply (or --dry-run to preview)\n", len(edits))
		return nil
	}

	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()
	res, err := m.EditItemsFields(ctx(cmd), edits, opts)
	// Same contract as runBatchEdit: the catalog batch already committed when res is
	// non-nil beside an error, so report what was edited before returning it.
	if res != nil {
		printBatchEditResult(cmd, res)
	}
	return err
}

// printBatchEditResult reports an applied batch edit: the edited count, any
// skipped locked items, and the per-item write-back warnings. Warnings follow
// the result's Edited order rather than map order, so two runs of the same
// batch print identically.
func printBatchEditResult(cmd *cobra.Command, res *waxbin.BatchEditResult) {
	w := out(cmd)
	fmt.Fprintf(w, "edited %d item(s)\n", len(res.Edited))
	if len(res.Skipped) > 0 {
		fmt.Fprintf(w, "skipped %d locked item(s)\n", len(res.Skipped))
		for _, pid := range res.Skipped {
			fmt.Fprintln(w, "  "+string(pid))
		}
	}
	for _, pid := range res.Edited {
		wbErr, ok := res.WriteBackErrors[pid]
		if !ok {
			continue
		}
		for _, f := range wbErr.Failures {
			fmt.Fprintf(errOut(cmd), "warning: on-disk tag write-back skipped for %s (%s): %s\n", pid, f.Path, f.Reason)
		}
	}
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
