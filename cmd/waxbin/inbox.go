package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/inbox"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/scan"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
	"io/fs"
	"path/filepath"
)

func newInboxCmd(g *globals) *cobra.Command {
	in := &cobra.Command{
		Use:   "inbox",
		Short: "Stage and import files from watched inbox folders",
	}
	in.AddCommand(newInboxListCmd(g), newInboxImportCmd(g), newInboxHistoryCmd(g))
	return in
}

func newInboxListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured inbox folders and their pending audio file counts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			folders := lib.InboxFolders()
			type folderView struct {
				Path    string `json:"path"`
				Pending int    `json:"pendingAudioFiles"`
			}
			views := make([]folderView, 0, len(folders))
			for _, f := range folders {
				views = append(views, folderView{Path: f, Pending: countAudio(f)})
			}
			if g.jsonOut {
				return printJSON(cmd, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(out(cmd), "No inbox folders configured (set \"inbox\" in the config).")
				return nil
			}
			w := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "PENDING\tFOLDER")
			for _, v := range views {
				fmt.Fprintf(w, "%d\t%s\n", v.Pending, v.Path)
			}
			return w.Flush()
		},
	}
}

func newInboxImportCmd(g *globals) *cobra.Command {
	var (
		apply   bool
		asCopy  bool
		dup     string
		profile string
	)
	cmd := &cobra.Command{
		Use:   "import [folder]",
		Short: "Import a staging folder into the managed library (review unless --apply)",
		Long: "Plans an import of [folder] (or every configured inbox folder) into the " +
			"managed library: which files import where, which are catalog duplicates, and " +
			"which are quarantined. Pass --apply to execute. Files are moved unless --copy.",
		RunE: func(cmd *cobra.Command, args []string) error {
			policy := model.DupPolicy(dup)
			if !policy.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "inbox.import", "invalid --dup policy (use skip|allow)")
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			sources := args
			if len(sources) == 0 {
				sources = lib.InboxFolders()
			}
			if len(sources) == 0 {
				return waxerr.New(waxerr.CodeInvalid, "inbox.import",
					"no folder given and no inbox folders configured")
			}
			for _, src := range sources {
				plan, err := lib.PlanImport(ctx(cmd), waxbin.ImportRequest{
					Source: src, Profile: profile, DupPolicy: policy, Copy: asCopy,
				})
				if err != nil {
					return err
				}
				if !apply {
					if err := emitImportPlan(cmd, g, plan); err != nil {
						return err
					}
					continue
				}
				rep, err := lib.ApplyImport(ctx(cmd), plan)
				if err != nil {
					return err
				}
				if err := emitImportReport(cmd, g, src, rep); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "execute the import (default is a review)")
	cmd.Flags().BoolVar(&asCopy, "copy", false, "copy files (keep inbox originals) instead of moving")
	cmd.Flags().StringVar(&dup, "dup", "skip", "duplicate policy: skip|allow")
	cmd.Flags().StringVar(&profile, "profile", "", "organization profile (default: the library's configured profile)")
	return cmd
}

func newInboxHistoryCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "history",
		Short: "List recorded import batches",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			batches, err := lib.ImportBatches(ctx(cmd), limit)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, batchesJSON(batches))
			}
			w := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "BATCH-PID\tSTATE\tIMPORTED\tDUP\tQUARANTINE\tERR\tSOURCE")
			for _, b := range batches {
				fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%s\n",
					b.PID, b.State, b.Imported, b.Duplicates, b.Quarantined, b.Errored, b.Source)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "max batches to show (0 = all)")
	return cmd
}

func emitImportPlan(cmd *cobra.Command, g *globals, plan *inbox.Plan) error {
	if g.jsonOut {
		return printJSON(cmd, importPlanJSON(plan))
	}
	w := out(cmd)
	fmt.Fprintf(w, "Import plan for %s: %d importable, %d total bytes\n", plan.Source, plan.Importable(), plan.TotalBytes)
	for _, a := range plan.Actions {
		switch a.Outcome {
		case inbox.OutcomeImport:
			fmt.Fprintf(w, "  import     %s\n             -> %s\n", a.Src, a.Dst)
		case inbox.OutcomeDuplicate:
			fmt.Fprintf(w, "  duplicate  %s\n", a.Src)
		case inbox.OutcomeQuarantine:
			fmt.Fprintf(w, "  quarantine %s (%s)\n", a.Src, a.Reason)
		}
	}
	fmt.Fprintln(w, "(review; pass --apply to import)")
	return nil
}

func emitImportReport(cmd *cobra.Command, g *globals, src string, rep *inbox.Report) error {
	if g.jsonOut {
		return printJSON(cmd, struct {
			BatchPID    string          `json:"batchPid"`
			Source      string          `json:"source"`
			Imported    int             `json:"imported"`
			Duplicates  int             `json:"duplicates"`
			Quarantined int             `json:"quarantined"`
			Errored     int             `json:"errored"`
			Sidecars    int             `json:"sidecars"`
			Bytes       int64           `json:"bytes"`
			Failures    []inbox.Failure `json:"failures,omitempty"`
		}{string(rep.BatchPID), src, rep.Imported, rep.Duplicates, rep.Quarantined, rep.Errored, rep.Sidecars, rep.Bytes, rep.Failures})
	}
	fmt.Fprintf(out(cmd), "Imported %s: %d imported, %d duplicate, %d quarantined, %d errored, %d sidecars (%d bytes)\n",
		src, rep.Imported, rep.Duplicates, rep.Quarantined, rep.Errored, rep.Sidecars, rep.Bytes)
	for _, f := range rep.Failures {
		fmt.Fprintf(out(cmd), "  FAIL %s: %s\n", f.Src, f.Err)
	}
	return nil
}

func importPlanJSON(plan *inbox.Plan) any {
	type actionJSON struct {
		Src     string `json:"src"`
		Dst     string `json:"dst,omitempty"`
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}
	actions := make([]actionJSON, 0, len(plan.Actions))
	for _, a := range plan.Actions {
		actions = append(actions, actionJSON{Src: a.Src, Dst: a.Dst, Outcome: string(a.Outcome), Reason: a.Reason})
	}
	return struct {
		Source     string `json:"source"`
		Importable int    `json:"importable"`
		TotalBytes int64  `json:"totalBytes"`
		Actions    any    `json:"actions"`
	}{plan.Source, plan.Importable(), plan.TotalBytes, actions}
}

func batchesJSON(batches []*model.ImportBatch) any {
	type batchJSON struct {
		PID         string `json:"pid"`
		Source      string `json:"source"`
		State       string `json:"state"`
		Imported    int    `json:"imported"`
		Duplicates  int    `json:"duplicates"`
		Quarantined int    `json:"quarantined"`
		Errored     int    `json:"errored"`
		Bytes       int64  `json:"bytes"`
	}
	out := make([]batchJSON, 0, len(batches))
	for _, b := range batches {
		out = append(out, batchJSON{
			PID: string(b.PID), Source: b.Source, State: string(b.State),
			Imported: b.Imported, Duplicates: b.Duplicates, Quarantined: b.Quarantined,
			Errored: b.Errored, Bytes: b.Bytes,
		})
	}
	return out
}

// countAudio counts recognized audio files under a folder, for the inbox listing.
func countAudio(folder string) int {
	n := 0
	_ = filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type().IsRegular() && scan.IsAudio(path) {
			n++
		}
		return nil
	})
	return n
}
