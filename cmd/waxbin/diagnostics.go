package main

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

// diagnosticFlags are the shared filter flags of the diagnostics subcommands.
type diagnosticFlags struct {
	origin   string
	code     string
	severity string
	library  string
	file     string
	item     string
}

func (df *diagnosticFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&df.origin, "origin", "", "filter by writer: scan|organize|replaygain|edit")
	f.StringVar(&df.code, "code", "", "filter by diagnostic code (e.g. tag_write_unsynced)")
	f.StringVar(&df.severity, "severity", "", "filter by severity: info|warn|error")
	f.StringVar(&df.library, "library", "", "filter to files under this library, by pid or registered root path")
	f.StringVar(&df.file, "file", "", "filter to one file pid")
	f.StringVar(&df.item, "item", "", "filter to every file backing this item pid (all parts of a book)")
}

// resolvedFilter builds the filter, turning --library's pid-or-root-path into a pid so
// this command spells the flag the way query, facet, and search do.
func (df *diagnosticFlags) resolvedFilter(ctx context.Context, lister libraryLister) (model.DiagnosticFilter, error) {
	f := model.DiagnosticFilter{
		Origin:   model.DiagnosticOrigin(df.origin),
		Code:     model.DiagnosticCode(df.code),
		Severity: model.AuditSeverity(df.severity),
		FilePID:  model.PID(df.file),
		ItemPID:  model.PID(df.item),
	}
	if df.library == "" {
		return f, nil
	}
	pids, err := resolveLibraryRefs(ctx, lister, "diagnostics", []string{df.library})
	if err != nil {
		return model.DiagnosticFilter{}, err
	}
	f.LibraryPID = pids[0]
	return f, nil
}

func newDiagnosticsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagnostics",
		Short: "Query the persisted per-file diagnostics",
		Long: "Reads the diagnostics the scan, organize, analyze, and edit write-back passes " +
			"recorded per file (unsupported formats, dropped cue tracks, unsynced tags, and the " +
			"rest), filtered by writer, code, severity, library, file, or item. `list` prints " +
			"the rows; `summary` prints grouped counts, most severe first.",
	}
	cmd.AddCommand(newDiagnosticsListCmd(g), newDiagnosticsSummaryCmd(g))
	return cmd
}

func newDiagnosticsListCmd(g *globals) *cobra.Command {
	var (
		df     diagnosticFlags
		limit  int
		offset int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted diagnostics, filtered and paged",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			filter, err := df.resolvedFilter(ctx(cmd), lib)
			if err != nil {
				return err
			}
			filter.Limit, filter.Offset = limit, offset
			ds, err := lib.FileDiagnostics(ctx(cmd), filter)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, diagnosticViews(ds))
			}
			if len(ds) == 0 {
				fmt.Fprintln(out(cmd), "(no diagnostics)")
				return nil
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SEVERITY\tORIGIN\tCODE\tPATH\tDETAIL")
			for _, d := range ds {
				detail := d.Detail
				if d.TagKey != "" {
					detail = "[" + d.TagKey + "] " + detail
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Severity, d.Origin, d.Code, d.DisplayPath, detail)
			}
			return tw.Flush()
		},
	}
	df.register(cmd)
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows (0 = all)")
	cmd.Flags().IntVar(&offset, "offset", 0, "rows to skip (paging over the stable order)")
	return cmd
}

func newDiagnosticsSummaryCmd(g *globals) *cobra.Command {
	var df diagnosticFlags
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Grouped diagnostic counts by writer, code, and severity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			filter, err := df.resolvedFilter(ctx(cmd), lib)
			if err != nil {
				return err
			}
			counts, err := lib.DiagnosticSummary(ctx(cmd), filter)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, diagnosticCountViews(counts))
			}
			if len(counts) == 0 {
				fmt.Fprintln(out(cmd), "(no diagnostics)")
				return nil
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "SEVERITY\tORIGIN\tCODE\tCOUNT")
			for _, c := range counts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", c.Severity, c.Origin, c.Code, c.Count)
			}
			return tw.Flush()
		},
	}
	df.register(cmd)
	return cmd
}

// diagnosticView is the JSON shape for one persisted diagnostic row.
type diagnosticView struct {
	FilePID  string `json:"filePid"`
	Path     string `json:"path"`
	Origin   string `json:"origin"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	TagKey   string `json:"tagKey,omitempty"`
	Detail   string `json:"detail,omitempty"`
	SeenAt   int64  `json:"seenAt,string"` // unix ns; a string, see playStateView
}

func diagnosticViews(ds []model.FileDiagnostic) []diagnosticView {
	out := make([]diagnosticView, len(ds))
	for i, d := range ds {
		out[i] = diagnosticView{
			FilePID: string(d.FilePID), Path: d.DisplayPath,
			Origin: string(d.Origin), Code: string(d.Code), Severity: string(d.Severity),
			TagKey: d.TagKey, Detail: d.Detail, SeenAt: d.SeenAt,
		}
	}
	return out
}

// diagnosticCountView is the JSON shape for one summary bucket.
type diagnosticCountView struct {
	Origin   string `json:"origin"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

func diagnosticCountViews(counts []model.DiagnosticCount) []diagnosticCountView {
	out := make([]diagnosticCountView, len(counts))
	for i, c := range counts {
		out[i] = diagnosticCountView{
			Origin: string(c.Origin), Code: string(c.Code), Severity: string(c.Severity), Count: c.Count,
		}
	}
	return out
}
