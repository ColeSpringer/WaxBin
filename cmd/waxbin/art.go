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
		role    string
		size    int
		outPath string
	)
	cmd := &cobra.Command{
		Use:   "art PID",
		Short: "Resolve an entity's artwork",
		Long: "Resolves artwork for an entity. The front cover (the default --role) follows the " +
			"fallback chain (track -> album -> release_group -> artist -> genre) to the first level " +
			"that has one; any other role (back|disc|booklet|background) resolves at the entity's " +
			"own level only. With --size, returns a thumbnail scaled to fit a square box with that " +
			"maximum side. Writes " +
			"the image bytes to --out (or stdout); with --json, reports metadata instead, " +
			"including the chain level that answered and whether an album's cover was derived " +
			"from a member track.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.ArtEntity(entType)
			if !et.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "art",
					fmt.Sprintf("unknown --type %q; valid: track|album|release_group|artist|genre", entType))
			}
			r, ok := model.ParseArtRole(role)
			if !ok {
				return waxerr.New(waxerr.CodeInvalid, "art",
					fmt.Sprintf("unknown --role %q; valid: front|back|disc|booklet|background", role))
			}

			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			blob, err := lib.ResolveArt(ctx(cmd), model.EntityRef{Type: et, PID: model.PID(args[0])}, r, size)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, map[string]any{
					"format": blob.Format, "width": blob.Width, "height": blob.Height,
					"bytes": len(blob.Bytes), "sourceHash": blob.SourceHash, "thumbnail": blob.Thumbnail,
					"level": string(blob.Level), "derived": blob.Derived,
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
	f.StringVar(&role, "role", "front", "art role: front|back|disc|booklet|background")
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
		Short: "Set (or clear) an entity's artwork from an image file",
		Long: "Sets artwork for a track/book item, or for an album/artist/release_group/genre/" +
			"podcast entity (--type), in one role (--role; front is the default, and " +
			"back|disc|booklet|background hold a release's auxiliary images). The image bytes come " +
			"from --file; --clear removes only the named role. An item front cover locks the " +
			"item's art field by default so a scan does not re-derive it; the other roles have no " +
			"scan producer and take no lock. " +
			"--write-back also embeds a front cover into the backing file(s): a track into its " +
			"file, a book into every part, an album across every member track's file. Only the " +
			"front cover has an embedded representation, so --write-back with another role is " +
			"refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et := model.ArtEntity(entType)
			if !et.Valid() {
				return waxerr.New(waxerr.CodeInvalid, "art set",
					fmt.Sprintf("unknown --type %q", entType))
			}
			r, ok := model.ParseArtRole(role)
			if !ok {
				return waxerr.New(waxerr.CodeInvalid, "art set",
					fmt.Sprintf("unknown --role %q; valid: front|back|disc|booklet|background", role))
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
				err = m.SetItemArt(ctx(cmd), pid, r, raw, !noLock, force, writeBack)
			} else {
				err = m.SetEntityArt(ctx(cmd), et, pid, r, raw, writeBack)
			}
			if err := surfaceWriteBack(cmd, err); err != nil {
				return err
			}
			if clear {
				fmt.Fprintf(out(cmd), "cleared %s %s art for %s\n", et, r, pid)
			} else {
				fmt.Fprintf(out(cmd), "set %s %s art for %s (%d bytes)\n", et, r, pid, len(raw))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&entType, "type", "track", "entity type: track|album|release_group|artist|genre|podcast")
	f.StringVar(&role, "role", "front", "art role: front|back|disc|booklet|background")
	f.StringVar(&filePath, "file", "", "image file to set as this role's artwork")
	f.BoolVar(&clear, "clear", false, "remove this role's artwork instead of setting it")
	f.BoolVar(&noLock, "no-lock", false, "do not lock an item's art field (a front cover defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked art field")
	f.BoolVar(&writeBack, "write-back", false, "also embed a front cover into the backing file(s) on disk")
	return cmd
}
