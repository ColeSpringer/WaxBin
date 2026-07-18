package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

// newTagCmd views or sets an item's custom tags: the non-standard tag frames a file
// carries that WaxBin's typed model does not map, plus tags a user adds. A set records
// user provenance and, by default, locks the "tag.<KEY>" field so a scan does not
// re-derive it from the file.
func newTagCmd(g *globals) *cobra.Command {
	var (
		key    string
		values []string
		noLock bool
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "tag <pid> [--key KEY --value V ...]",
		Short: "View or set an item's custom tags",
		Long: "Without --key, lists an item's custom tags. With --key, replaces that tag's values " +
			"with the given --value entries (repeatable; none clears the tag). The key is normalized " +
			"to canonical uppercase (BPM and bpm are one tag). A tag records user provenance and, by " +
			"default, locks the tag against a scan re-deriving it.\n\n" +
			"A key WaxBin maps through the scalar, credit, or entity edit surface (title, artist, isrc, " +
			"barcode, a contributor role, ...) is reserved and rejected; use that surface instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			if key == "" {
				// A set-side flag with no --key is a mistake (the values would be silently
				// dropped into a list), so reject it rather than falling through to a listing.
				if len(values) > 0 || cmd.Flags().Changed("no-lock") || cmd.Flags().Changed("force") {
					return fmt.Errorf("--key is required to set a tag (with --value/--no-lock/--force)")
				}
				return listTags(cmd, g, pid)
			}
			return setTag(cmd, g, pid, key, values, waxbin.TagEditOptions{Lock: !noLock, Force: force})
		},
	}
	f := cmd.Flags()
	f.StringVar(&key, "key", "", "custom tag key to set (omit to list all tags)")
	f.StringArrayVar(&values, "value", nil, "value for the tag (repeatable; none clears it)")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the tag (it defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked tag")
	return cmd
}

func listTags(cmd *cobra.Command, g *globals, pid model.PID) error {
	lib, _, err := g.openRead(cmd)
	if err != nil {
		return err
	}
	defer lib.Close()
	tags, err := lib.ItemTags(ctx(cmd), pid)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return printJSON(cmd, tagViews(tags))
	}
	if len(tags) == 0 {
		fmt.Fprintln(out(cmd), "(no custom tags)")
		return nil
	}
	tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE")
	for _, t := range tags {
		for _, v := range t.Values {
			fmt.Fprintf(tw, "%s\t%s\n", t.Key, v)
		}
	}
	return tw.Flush()
}

func setTag(cmd *cobra.Command, g *globals, pid model.PID, key string, values []string, opts waxbin.TagEditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()
	// Report the count the store actually stored (after trimming), so a whitespace-only
	// --value reads as a clear rather than a set.
	canonKey, stored, err := m.SetItemTag(ctx(cmd), pid, key, values, opts)
	if err != nil {
		return err
	}
	if stored == 0 {
		fmt.Fprintf(out(cmd), "cleared tag %s on %s\n", canonKey, pid)
	} else {
		fmt.Fprintf(out(cmd), "set tag %s (%d value(s)) on %s\n", canonKey, stored, pid)
	}
	return nil
}

// tagView is the JSON shape for a custom tag.
type tagView struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

func tagViews(tags []model.ItemTag) []tagView {
	out := make([]tagView, len(tags))
	for i, t := range tags {
		out[i] = tagView{Key: t.Key, Values: t.Values}
	}
	return out
}
