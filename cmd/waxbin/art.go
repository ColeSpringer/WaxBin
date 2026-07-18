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
	cmd.AddCommand(newArtSetCmd(g))
	return cmd
}

func newArtSetCmd(g *globals) *cobra.Command {
	var (
		entType   string
		role      string
		filePath  string
		clear     bool
		noLock    bool
		force     bool
		writeBack bool
	)
	cmd := &cobra.Command{
		Use:   "set <pid> --file <image>",
		Short: "Set (or clear) an entity's cover art from an image file",
		Long: "Sets cover art for a track/book item, or for an album/artist/release_group/genre/" +
			"podcast entity (--type). The image bytes come from --file; --clear removes the cover. " +
			"An item cover locks the item's art field by default so a scan does not re-derive it. " +
			"--write-back also embeds the cover into the backing file(s): a track into its file, a " +
			"book into every part, an album across every member track's file.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.ArtEntity(entType)
			if !et.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "art set",
					fmt.Sprintf("unknown --type %q", entType))
			}
			var raw []byte
			if !clear {
				if filePath == "" {
					return waxerr.New(waxerr.CodeInvalid, "art set", "provide --file or --clear")
				}
				b, err := os.ReadFile(filePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "art set", err, "reading %s", filePath)
				}
				raw = b
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()

			pid := model.PID(args[0])
			if et == model.ArtTrack {
				err = m.SetItemArt(ctx(cmd), pid, raw, !noLock, force, writeBack)
			} else {
				err = m.SetEntityArt(ctx(cmd), et, pid, role, raw, writeBack)
			}
			if err := surfaceWriteBack(cmd, err); err != nil {
				return err
			}
			if clear {
				fmt.Fprintf(out(cmd), "cleared %s art for %s\n", et, pid)
			} else {
				fmt.Fprintf(out(cmd), "set %s art for %s (%d bytes)\n", et, pid, len(raw))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&entType, "type", "track", "entity type: track|album|release_group|artist|genre|podcast")
	f.StringVar(&role, "role", "front", "art role for a non-item entity")
	f.StringVar(&filePath, "file", "", "image file to set as the cover")
	f.BoolVar(&clear, "clear", false, "remove the cover instead of setting one")
	f.BoolVar(&noLock, "no-lock", false, "do not lock an item's art field (it defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked art field")
	f.BoolVar(&writeBack, "write-back", false, "also embed the cover into the backing file(s) on disk")
	return cmd
}
