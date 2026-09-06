package main

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newDBEnrichCacheCmd(g *globals) *cobra.Command {
	var olderThan, maxBytes string
	cmd := &cobra.Command{
		Use:   "enrich-cache",
		Short: "Report and prune the enrichment response cache",
		Long: "Reports what the enrichment response cache holds, by request kind: " +
			"MusicBrainz lookups and searches, edition browses, and the Cover Art Archive's " +
			"group-cover records. A cached answer is read only when its target is asked " +
			"again (a forced run, a retry of an expired miss, a scoped run), so a matched " +
			"entity's lookup sits unread until then.\n\n" +
			"Pruning costs one request the next time that target is re-asked, never a " +
			"catalog value; edition browses are the costly ones to rebuild, a page per " +
			"second per group. The archive's group records (caa:) are exempt: each says " +
			"which release's bytes a group holds and carries the validator a forced " +
			"re-fetch spends, so losing one costs a full download per group, and the report " +
			"marks them.\n\n" +
			"--older-than and --max-bytes prune rather than only report, and combine the way " +
			"`db thumbs` combines them; the byte budget bounds what can be pruned. It frees " +
			"pages inside the catalog file; `waxbin db vacuum` returns that space to the " +
			"filesystem.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			const op = "db enrich-cache"
			var policy waxbin.EnrichmentCachePrunePolicy
			pruning := olderThan != "" || maxBytes != ""
			if olderThan != "" {
				age, err := parseAge(op, olderThan)
				if err != nil {
					return err
				}
				policy.OlderThan = &age
			}
			if maxBytes != "" {
				budget, err := parseBytes(op, maxBytes)
				if err != nil {
					return err
				}
				policy.MaxBytes = &budget
			}

			lib, _, err := g.openLib(cmd, !pruning)
			if err != nil {
				return err
			}
			defer lib.Close()

			var removed int
			var freed int64
			if pruning {
				if removed, freed, err = lib.PruneEnrichmentCache(ctx(cmd), policy); err != nil {
					return err
				}
			}
			rep, err := lib.EnrichmentCacheStats(ctx(cmd))
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toEnrichCacheView(rep, pruning, removed, freed))
			}
			w := out(cmd)
			if pruning {
				// Not plural(): "entry" does not take a bare s.
				noun := "entries"
				if removed == 1 {
					noun = "entry"
				}
				fmt.Fprintf(w, "pruned %d %s, freed %s\n", removed, noun, byteLabel(freed))
				if removed > 0 {
					fmt.Fprintln(w, "run `waxbin db vacuum` to return the freed pages to the filesystem")
				}
				fmt.Fprintln(w)
			}
			printEnrichCacheReport(w, rep, time.Now())
			return nil
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "prune entries fetched at least this long ago (90d, 36h, ...)")
	cmd.Flags().StringVar(&maxBytes, "max-bytes", "", "prune oldest first until the prunable entries fit this budget (100MB, 512MiB, 0 empties them)")
	return cmd
}

// printEnrichCacheReport writes the cache census. now is passed in rather than read here
// so the age line is reproducible in a test.
func printEnrichCacheReport(w io.Writer, rep *model.EnrichmentCacheReport, now time.Time) {
	fmt.Fprintf(w, "cache:    %s, %s", plural(rep.Rows, "row"), byteLabel(rep.Bytes))
	if rep.ExemptRows > 0 {
		fmt.Fprintf(w, " (%s, %s exempt from prune)", plural(rep.ExemptRows, "row"), byteLabel(rep.ExemptBytes))
	}
	fmt.Fprintln(w)
	// Both ends, and UTC, for the reasons printThumbReport gives.
	if rep.OldestAt > 0 {
		oldest, newest := time.Unix(0, rep.OldestAt).UTC(), time.Unix(0, rep.NewestAt).UTC()
		fmt.Fprintf(w, "fetched:  %s to %s (oldest %s)\n",
			oldest.Format("2006-01-02"), newest.Format("2006-01-02"), agoLabel(now, oldest))
	}
	if len(rep.Kinds) == 0 {
		return
	}
	// The kind is a name, so it reads left-aligned while the counts beside it read
	// right-aligned; one tabwriter cannot do both, so the widths are measured here.
	kw, rw, bw := len("KIND"), len("ROWS"), len("BYTES")
	bytes := make([]string, len(rep.Kinds))
	rows := make([]string, len(rep.Kinds))
	for i, k := range rep.Kinds {
		bytes[i], rows[i] = byteLabel(k.Bytes), strconv.Itoa(k.Rows)
		kw, rw, bw = max(kw, len(k.Kind)), max(rw, len(rows[i])), max(bw, len(bytes[i]))
	}
	fmt.Fprintf(w, "\n  %-*s  %*s  %*s\n", kw, "KIND", rw, "ROWS", bw, "BYTES")
	for i, k := range rep.Kinds {
		mark := ""
		if k.Exempt {
			mark = "  exempt"
		}
		fmt.Fprintf(w, "  %-*s  %*s  %*s%s\n", kw, k.Kind, rw, rows[i], bw, bytes[i], mark)
	}
}

type enrichCacheKindView struct {
	Kind   string `json:"kind"`
	Rows   int    `json:"rows"`
	Bytes  int64  `json:"bytes"`
	Exempt bool   `json:"exempt,omitempty"`
}

type enrichCachePrunedView struct {
	Rows  int   `json:"rows"`
	Bytes int64 `json:"bytes"`
}

type enrichCacheView struct {
	Rows        int                    `json:"rows"`
	Bytes       int64                  `json:"bytes"`
	OldestAt    int64                  `json:"oldestAt,string,omitempty"` // unix ns; a string, see playStateView. 0 (empty cache) omitted
	NewestAt    int64                  `json:"newestAt,string,omitempty"`
	ExemptRows  int                    `json:"exemptRows,omitempty"`
	ExemptBytes int64                  `json:"exemptBytes,omitempty"`
	Kinds       []enrichCacheKindView  `json:"kinds"`
	Pruned      *enrichCachePrunedView `json:"pruned,omitempty"` // absent when no prune was asked for; present with zeroes when one matched nothing
}

func toEnrichCacheView(rep *model.EnrichmentCacheReport, pruned bool, removed int, freed int64) enrichCacheView {
	v := enrichCacheView{
		Rows: rep.Rows, Bytes: rep.Bytes, OldestAt: rep.OldestAt, NewestAt: rep.NewestAt,
		ExemptRows: rep.ExemptRows, ExemptBytes: rep.ExemptBytes,
		Kinds: make([]enrichCacheKindView, 0, len(rep.Kinds)),
	}
	for _, k := range rep.Kinds {
		v.Kinds = append(v.Kinds, enrichCacheKindView{Kind: k.Kind, Rows: k.Rows, Bytes: k.Bytes, Exempt: k.Exempt})
	}
	if pruned {
		v.Pruned = &enrichCachePrunedView{Rows: removed, Bytes: freed}
	}
	return v
}
