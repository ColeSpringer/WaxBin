package main

import (
	"fmt"
	"strings"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/audit"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newAuditCmd(g *globals) *cobra.Command {
	var (
		integrity bool
		checks    []string
		sample    int
	)
	// The valid-names list is generated from the model so it never drifts from the
	// checks the command actually accepts.
	names := make([]string, len(model.AuditChecks()))
	for i, c := range model.AuditChecks() {
		names[i] = string(c)
	}
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Report catalog quality and integrity problems",
		Long: "Runs quality checks over the catalog: duplicate/split entities, inconsistent " +
			"metadata, missing art/ReplayGain, unportable filenames, orphaned sidecars, " +
			"case-insensitive path conflicts, invalid feeds, derived-data drift, and the " +
			"diagnostics recorded during scanning and tag write-back. " +
			"Corrupt-audio reporting comes in two halves. The free half reads signals the " +
			"scan already derived, and covers MP3, AAC, AIFF, MP4, and WAV only. It is a " +
			"true positive when it fires and proves nothing when it does not, so a quiet " +
			"run is not a clean bill of health; FLAC, Opus, Vorbis, and Matroska need the " +
			"decode probe. --integrity adds that probe plus an on-disk bitrot " +
			"(content-hash) pass, both of which re-read every audio file. " +
			"--check <name> (repeatable) restricts the run; valid " +
			"names: " + strings.Join(names, ", ") + ". Exits non-zero when any error-severity " +
			"finding is reported.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			only := make([]model.AuditCheck, 0, len(checks))
			for _, c := range checks {
				ac := model.AuditCheck(c)
				if !ac.Valid() {
					// Without this, a typo'd check name matches nothing and the audit
					// silently "passes" with no issues found.
					return waxerr.New(waxerr.CodeInvalid, "audit", "unknown check: "+c)
				}
				only = append(only, ac)
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			rep, err := lib.Audit(ctx(cmd), waxbin.AuditOptions{
				Only: only, Integrity: integrity, Sample: sample,
			})
			if err != nil {
				return err
			}

			if g.jsonOut {
				if err := printJSON(cmd, toAuditView(rep)); err != nil {
					return err
				}
			} else {
				printAuditText(cmd, rep)
			}
			if rep.Errors() > 0 {
				return waxerr.New(waxerr.CodeInvalid, "audit",
					fmt.Sprintf("%d error-severity finding(s)", rep.Errors()))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&integrity, "integrity", false, "also re-read every audio file for bitrot and corruption (slow)")
	cmd.Flags().StringArrayVar(&checks, "check", nil, "restrict to a specific check (repeatable)")
	cmd.Flags().IntVar(&sample, "sample", 0, "cap the sample size per check (0 = default)")
	return cmd
}

func printAuditText(cmd *cobra.Command, rep *audit.Report) {
	w := out(cmd)
	if len(rep.Findings) == 0 {
		fmt.Fprintln(w, "no issues found")
		return
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "[%s] %s: %s\n", f.Severity, f.Check, f.Message)
	}
	fmt.Fprintf(w, "\n%d finding(s): %d error, %d warn\n",
		len(rep.Findings), rep.Errors(), rep.Warnings())
	if rep.FilesChecked > 0 {
		fmt.Fprintf(w, "integrity: %d files checked\n", rep.FilesChecked)
	}
}

type auditFindingView struct {
	Check     string   `json:"check"`
	Severity  string   `json:"severity"`
	Message   string   `json:"message"`
	Entities  []string `json:"entities,omitempty"`
	Path      string   `json:"path,omitempty"`
	MergeType string   `json:"mergeType,omitempty"`
}

type auditView struct {
	Findings     []auditFindingView `json:"findings"`
	Errors       int                `json:"errors"`
	Warnings     int                `json:"warnings"`
	FilesChecked int                `json:"filesChecked"`
}

func toAuditView(rep *audit.Report) auditView {
	fs := make([]auditFindingView, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		ents := make([]string, 0, len(f.Entities))
		for _, e := range f.Entities {
			ents = append(ents, string(e))
		}
		fs = append(fs, auditFindingView{
			Check:     string(f.Check),
			Severity:  string(f.Severity),
			Message:   f.Message,
			Entities:  ents,
			Path:      f.Path,
			MergeType: string(f.MergeType),
		})
	}
	return auditView{Findings: fs, Errors: rep.Errors(), Warnings: rep.Warnings(), FilesChecked: rep.FilesChecked}
}
