package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newCreditCmd(g *globals) *cobra.Command {
	var (
		role       string
		names      []string
		writeBack  bool
		noLock     bool
		keepLock   bool
		force      bool
		batchPath  string
		dryRun     bool
		assumeYes  bool
		skipLocked bool
	)
	cmd := &cobra.Command{
		Use:   "credit <pid> [--role <role> --name <name> ...]",
		Short: "View or set an item's contributor credits",
		Long: "Without --role, lists an item's contributors across every role. With --role, " +
			"replaces that role's contributors with the given --name values (repeatable; none " +
			"clears the role). A credit records user provenance and, by default, locks the " +
			"credit.<role> field. --write-back also mirrors the credit into the file's on-disk " +
			"tag (a track's music role, or a book's author/narrator across its parts).\n\n" +
			"--batch sets several credits instead: a JSON array of {\"itemPid\": ..., \"role\": " +
			"..., \"names\": [...]} entries (\"-\" reads stdin), applied in one atomic catalog " +
			"transaction. One item may appear under two roles, but not twice under one role. It " +
			"excludes the pid, --role, and --name, and needs --yes to apply (or --dry-run to " +
			"preview). Gathering the entries also lets a rename that moves every reference to " +
			"an artist land on that entity instead of splitting off a new one.\n\n" +
			"Music roles (tracks): artist, composer, lyricist, conductor, performer, remixer, " +
			"producer, engineer, mixer, arranger, writer, djmixer.\n" +
			"Book roles (audiobooks): author, narrator, translator, editor.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := waxbin.CreditEditOptions{
				WriteBack: writeBack, Lock: lockChange(noLock, keepLock), Force: force, SkipLocked: skipLocked,
			}
			if batchPath != "" {
				if len(args) > 0 || role != "" || len(names) > 0 {
					return fmt.Errorf("--batch is exclusive with the pid, --role, and --name")
				}
				return runCreditBatch(cmd, g, batchPath, opts, dryRun, assumeYes)
			}
			if len(args) == 0 {
				return fmt.Errorf("specify an item pid, or --batch to set several credits at once")
			}
			pid := model.PID(args[0])
			if role == "" {
				return listCredits(cmd, g, pid)
			}
			// --dry-run is answered before anything opens a mutator, so previewing one
			// credit never writes. Applying it first and checking the flag after would
			// silently mutate (and, with --write-back, rewrite the file) despite the user
			// asking only to preview, which is the invariant the edit path documents.
			if dryRun {
				printCreditPreview(cmd, pid, model.ContributorRole(role), names)
				return nil
			}
			return setCredits(cmd, g, pid, model.ContributorRole(role), names, opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&role, "role", "", "contributor role to set (omit to list all credits)")
	f.StringArrayVar(&names, "name", nil, "contributor name for the role (repeatable; none clears it)")
	f.BoolVar(&writeBack, "write-back", false, "also write the credit into the file's on-disk tag")
	f.BoolVar(&noLock, "no-lock", false, "unlock the credit (it defaults to locked)")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the credit"))
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
	f.BoolVar(&force, "force", false, "override a locked credit role")
	f.StringVar(&batchPath, "batch", "", "set several credits from a JSON file (\"-\" = stdin)")
	f.BoolVar(&dryRun, "dry-run", false, "preview the edit without applying it")
	f.BoolVar(&assumeYes, "yes", false, "apply a batch without the preview gate")
	f.BoolVar(&skipLocked, "skip-locked", false, "skip a locked credit role and report it instead of failing")
	return cmd
}

// loadBatchCredits reads a batch credit document: a JSON array of {"itemPid", "role",
// "names"} entries, from a file or stdin ("-"). The shape matches the proxy's
// set_credits_batch wire entries, so one document drives the CLI and an embedder's
// client alike. Beyond the document shape it rejects a repeated (item, role) pair,
// since the engine refuses that unconditionally and a dry-run listing both entries as
// applicable would mislead. An empty names list is legitimate: it clears the role.
// Everything needing the catalog (whether the role applies to the item's kind, whether
// the items exist) stays the engine's job.
func loadBatchCredits(cmd *cobra.Command, path string) ([]model.ItemCreditEdit, error) {
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
		ItemPID string   `json:"itemPid"`
		Role    string   `json:"role"`
		Names   []string `json:"names"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse batch document: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("batch document has no entries")
	}
	type pair struct{ pid, role string }
	out := make([]model.ItemCreditEdit, len(entries))
	seen := make(map[pair]int, len(entries))
	for i, e := range entries {
		if e.ItemPID == "" {
			return nil, fmt.Errorf("batch entry %d has no itemPid", i)
		}
		if e.Role == "" {
			return nil, fmt.Errorf("batch entry %d (%s) has no role", i, e.ItemPID)
		}
		if first, dup := seen[pair{e.ItemPID, e.Role}]; dup {
			return nil, fmt.Errorf("batch entries %d and %d both set the %s credit of %s; give each pair one entry",
				first, i, e.Role, e.ItemPID)
		}
		seen[pair{e.ItemPID, e.Role}] = i
		out[i] = model.ItemCreditEdit{
			ItemPID: model.PID(e.ItemPID), Role: model.ContributorRole(e.Role), Names: e.Names,
		}
	}
	return out, nil
}

// runCreditBatch loads a --batch document and applies it through the atomic batch
// credit surface, honoring the same dry-run preview and --yes gate as edit --batch. The
// preview describes the document, not a promise the engine will accept every entry.
func runCreditBatch(cmd *cobra.Command, g *globals, batchPath string, opts waxbin.CreditEditOptions, dryRun, assumeYes bool) error {
	edits, err := loadBatchCredits(cmd, batchPath)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(out(cmd), "%d credit(s) in the batch:\n", len(edits))
		for _, e := range edits {
			fmt.Fprintf(out(cmd), "  %s %s (%d name(s))\n", e.ItemPID, e.Role, len(e.Names))
		}
		return nil
	}
	if !assumeYes {
		fmt.Fprintf(out(cmd), "%d credit(s) in the batch; re-run with --yes to apply (or --dry-run to preview)\n", len(edits))
		return nil
	}

	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()
	res, err := m.SetCreditsBatch(ctx(cmd), edits, opts)
	// Same contract as edit --batch: the catalog batch already committed when res is
	// non-nil beside an error, so report what was edited before returning it.
	if res != nil {
		printCreditBatchResult(cmd, res)
	}
	return err
}

// printCreditPreview describes what a single credit edit would do without applying it.
// It is the --dry-run answer for the single-item path, printed before any mutator
// opens, and mirrors the wording of the applied report below.
func printCreditPreview(cmd *cobra.Command, pid model.PID, role model.ContributorRole, names []string) {
	if len(names) == 0 {
		fmt.Fprintf(out(cmd), "would clear the %s credits of %s\n", role, pid)
		return
	}
	fmt.Fprintf(out(cmd), "would set the %s credits of %s to %s\n", role, pid, strings.Join(names, "; "))
}

// printCreditBatchResult reports an applied credit batch. The batch's unit is the
// (item, role) entry, so the summary counts edits and the distinct items they landed
// on, and a skipped line names its role. Warnings follow the result's Edited order
// rather than map order, deduped by item: an item edited under two roles appears twice
// in Edited, but its roles were mirrored into its files in one pass, so its failures
// are reported once.
func printCreditBatchResult(cmd *cobra.Command, res *waxbin.CreditBatchResult) {
	w := out(cmd)
	distinct := map[model.PID]bool{}
	for _, e := range res.Edited {
		distinct[e.ItemPID] = true
	}
	fmt.Fprintf(w, "applied %d credit edit(s) across %d item(s)\n", len(res.Edited), len(distinct))
	if len(res.Skipped) > 0 {
		fmt.Fprintf(w, "skipped %d locked credit(s)\n", len(res.Skipped))
		for _, e := range res.Skipped {
			fmt.Fprintf(w, "  %s %s\n", e.ItemPID, e.Role)
		}
	}
	warned := map[model.PID]bool{}
	for _, e := range res.Edited {
		if warned[e.ItemPID] {
			continue
		}
		warned[e.ItemPID] = true
		wbErr, ok := res.WriteBackErrors[e.ItemPID]
		if !ok {
			continue
		}
		for _, f := range wbErr.Failures {
			fmt.Fprintf(errOut(cmd), "warning: on-disk tag write-back skipped for %s (%s): %s\n", e.ItemPID, f.Path, f.Reason)
		}
	}
}

func listCredits(cmd *cobra.Command, g *globals, pid model.PID) error {
	lib, _, err := g.openRead(cmd)
	if err != nil {
		return err
	}
	defer lib.Close()
	credits, err := lib.Credits(ctx(cmd), pid)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return printJSON(cmd, creditViews(credits))
	}
	if len(credits) == 0 {
		fmt.Fprintln(out(cmd), "(no credits)")
		return nil
	}
	tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tNAME\tARTIST")
	for _, c := range credits {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Role, c.Name, c.ArtistPID)
	}
	return tw.Flush()
}

func setCredits(cmd *cobra.Command, g *globals, pid model.PID, role model.ContributorRole, names []string, opts waxbin.CreditEditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()

	stored, skipped, err := m.SetCredits(ctx(cmd), pid, role, names, opts)
	// The catalog edit stands even when the on-disk write-back partially failed, so
	// surface those as warnings and keep reporting the stored count.
	if err := surfaceWriteBack(cmd, err); err != nil {
		return err
	}
	if skipped {
		fmt.Fprintf(out(cmd), "skipped the locked %s credit of %s\n", role, pid)
		return nil
	}
	// Report the count actually stored (trimmed, resolvable, deduped) rather than the
	// raw --name count, so an unresolvable name that cleared the role reads as "0".
	if stored == 0 {
		fmt.Fprintf(out(cmd), "cleared %s credits for %s\n", role, pid)
	} else {
		fmt.Fprintf(out(cmd), "set %d %s credit(s) for %s\n", stored, role, pid)
	}
	return nil
}

// creditView is the JSON shape for a contributor.
type creditView struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	ArtistPID string `json:"artistPid"`
	Position  int    `json:"position"`
}

func creditViews(cs []model.Contributor) []creditView {
	out := make([]creditView, len(cs))
	for i, c := range cs {
		out[i] = creditView{Role: string(c.Role), Name: c.Name, ArtistPID: string(c.ArtistPID), Position: c.Position}
	}
	return out
}
