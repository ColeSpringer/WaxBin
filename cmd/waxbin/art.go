package main

import (
	"fmt"
	"os"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newArtCmd(g *globals) *cobra.Command {
	var (
		entType string
		size    int
		outPath string
	)
	cmd := &cobra.Command{
		Use:   "art PID",
		Short: "Resolve an entity's cover art",
		Long: "Resolves cover art for an entity, following the fallback chain " +
			"(track -> album -> release_group -> artist -> genre) to the first level that " +
			"has art. With --size, returns a thumbnail scaled to fit a square box with that " +
			"maximum side. Writes " +
			"the image bytes to --out (or stdout); with --json, reports metadata instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.ArtEntity(entType)
			if !et.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "art",
					fmt.Sprintf("unknown --type %q; valid: track|album|release_group|artist|genre", entType))
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			blob, err := lib.ResolveArt(ctx(cmd), model.EntityRef{Type: et, PID: model.PID(args[0])}, size)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, map[string]any{
					"format": blob.Format, "width": blob.Width, "height": blob.Height,
					"bytes": len(blob.Bytes), "sourceHash": blob.SourceHash, "thumbnail": blob.Thumbnail,
				})
			}
			if outPath != "" {
				if err := os.WriteFile(outPath, blob.Bytes, 0o644); err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "art", err, "writing %s", outPath)
				}
				fmt.Fprintf(out(cmd), "wrote %d bytes (%s %dx%d) to %s\n",
					len(blob.Bytes), blob.Format, blob.Width, blob.Height, outPath)
				return nil
			}
			if _, err := cmd.OutOrStdout().Write(blob.Bytes); err != nil {
				return waxerr.Wrap(waxerr.CodeIO, "art", err)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&entType, "type", "track", "entity type: track|album|release_group|artist|genre")
	f.IntVar(&size, "size", 0, "thumbnail max dimension in px (0 = original)")
	f.StringVar(&outPath, "out", "", "write image bytes to this file instead of stdout")
	return cmd
}
