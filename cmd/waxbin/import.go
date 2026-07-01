package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newImportCmd(g *globals) *cobra.Command {
	var (
		as, sourceType, sourceURL, sourceID, provider string
		profile, dup, showPID, showTitle, epTitle     string
		apply, asCopy, noPin                          bool
	)
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Ingest an acquired or manual media file",
		Long: "Routes a file by --as kind. Tracks and books go through the managed-library " +
			"import planner; episodes go into the podcast library as pinned episodes. The " +
			"selected kind is also the scanner override, so --as book can catalog an audiobook " +
			"whose tags do not identify it as one. WaxBin records an acquisition row with the " +
			"supplied source metadata.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := model.Kind(as)
			if kind != model.KindTrack && kind != model.KindBook && kind != model.KindEpisode {
				return waxerr.New(waxerr.CodeInvalid, "import", "invalid --as (use track|book|episode)")
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			meta := waxbin.AcquiredMeta{
				SourceType: model.SourceType(sourceType), SourceURL: sourceURL, SourceID: sourceID,
				Provider: provider, Profile: profile, Copy: asCopy, DupPolicy: model.DupPolicy(dup),
				ShowPID: model.PID(showPID), ShowTitle: showTitle, Title: epTitle,
			}
			if kind == model.KindEpisode && noPin {
				no := false
				meta.Pinned = &no
			}
			res, err := lib.ImportAcquired(ctx(cmd), waxbin.AcquiredFile{Path: args[0]}, kind, meta)
			if err != nil {
				return err
			}

			// An episode is ingested immediately; a track/book returns a reviewable plan.
			if res.Kind == model.KindEpisode {
				if g.jsonOut {
					return printJSON(cmd, struct {
						Kind    string `json:"kind"`
						Episode string `json:"episode"`
						File    string `json:"file,omitempty"`
						Path    string `json:"path,omitempty"`
					}{"episode", string(res.EpisodePID), string(res.FilePID), res.Path})
				}
				fmt.Fprintf(out(cmd), "Ingested episode %s\n", res.EpisodePID)
				if res.Path != "" {
					fmt.Fprintf(out(cmd), "Attached file: %s\n", res.Path)
				}
				return nil
			}
			if !apply {
				return emitImportPlan(cmd, g, res.Plan)
			}
			rep, err := lib.ApplyImport(ctx(cmd), res.Plan)
			if err != nil {
				return err
			}
			return emitImportReport(cmd, g, args[0], rep)
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "media kind: track|book|episode (required)")
	f.StringVar(&sourceType, "source-type", "", "acquisition source: rss|youtube|manual (default manual)")
	f.StringVar(&sourceURL, "source-url", "", "origin URL for provenance; for episodes, the enclosure URL")
	f.StringVar(&sourceID, "source-id", "", "provider-native source id recorded as provenance")
	f.StringVar(&provider, "provider", "", "provider name recorded as provenance")
	f.StringVar(&profile, "profile", "", "organization profile (track/book)")
	f.StringVar(&dup, "dup", "skip", "duplicate policy: skip|allow (track/book)")
	f.BoolVar(&asCopy, "copy", false, "copy the file instead of moving it")
	f.BoolVar(&apply, "apply", false, "execute a track/book import (default is a review)")
	f.StringVar(&showPID, "show", "", "target show pid for an episode (default: a new manual show)")
	f.StringVar(&showTitle, "show-title", "", "manual show title for a new-show episode")
	f.StringVar(&epTitle, "title", "", "episode title (default the file base name)")
	f.BoolVar(&noPin, "no-pin", false, "do not pin an acquired episode (retention may reclaim it)")
	_ = cmd.MarkFlagRequired("as")
	return cmd
}
