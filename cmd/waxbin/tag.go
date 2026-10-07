package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/read"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// newTagCmd views or sets an item's custom tags: the non-standard tag frames a file
// carries that WaxBin's typed model does not map, plus tags a user adds. A set records
// user provenance and, by default, locks the "tag.<KEY>" field so a scan does not
// re-derive it from the file.
func newTagCmd(g *globals) *cobra.Command {
	var (
		key       string
		values    []string
		noLock    bool
		keepLock  bool
		force     bool
		writeBack bool
	)
	cmd := &cobra.Command{
		Use:   "tag <pid> [--key KEY --value V ...]",
		Short: "View or set an item's custom tags",
		Long: "Without --key, lists an item's custom tags. With --key, replaces that tag's values " +
			"with the given --value entries (repeatable; none clears the tag). The key is normalized " +
			"to canonical uppercase (KEY and key are one tag). A tag records user provenance and, by " +
			"default, locks the tag against a scan re-deriving it.\n\n" +
			"A key WaxBin maps through the scalar, credit, or entity edit surface (title, artist, isrc, " +
			"barcode, a contributor role, ...) is reserved and rejected; use that surface instead.\n\n" +
			"--write-back also writes the tag into the item's file(s), every part of a book included. " +
			"With it, a spelling the tag library writes onto another field (YEAR, TRACK, ALBUM_ARTIST, ...) " +
			"is refused, and so is a file whose own format would do that (TPE2 in an MP3).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			if key == "" {
				// A set-side flag with no --key is a mistake (the values would be silently
				// dropped into a list), so reject it rather than falling through to a listing.
				if len(values) > 0 || cmd.Flags().Changed("no-lock") || cmd.Flags().Changed("keep-lock") ||
					cmd.Flags().Changed("force") || cmd.Flags().Changed("write-back") {
					return waxerr.New(waxerr.CodeInvalid, "tag", "--key is required to set a tag (with --value/--no-lock/--keep-lock/--force/--write-back)")
				}
				return listTags(cmd, g, pid)
			}
			return setTag(cmd, g, pid, key, values,
				waxbin.TagEditOptions{Lock: lockChange(noLock, keepLock), Force: force, WriteBack: writeBack})
		},
	}
	f := cmd.Flags()
	f.StringVar(&key, "key", "", "custom tag key to set (omit to list all tags)")
	f.StringArrayVar(&values, "value", nil, "value for the tag (repeatable; none clears it)")
	f.BoolVar(&noLock, "no-lock", false, "unlock the tag (it defaults to locked)")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the tag")+
		"; clearing a tag forgets it entirely, lock included, whichever lock flag is given")
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
	f.BoolVar(&force, "force", false, "override a locked tag")
	f.BoolVar(&writeBack, "write-back", false, "also write the tag into the item's file(s) on disk")
	// `tag keys` is a catalog-wide read subcommand. Routing is unambiguous because an item
	// pid is a ULID and can never be the literal "keys", so `tag <ulid>` still hits the
	// parent's set/list RunE while `tag keys` hits the subcommand.
	cmd.AddCommand(newTagKeysCmd(g))
	return cmd
}

// newTagKeysCmd lists the catalog's custom-tag keys and per-key item counts: the browse
// dimensions available to `facet --group-by tag.<KEY>` and `query --tag KEY=VALUE`.
func newTagKeysCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "keys",
		Short: "List custom-tag keys and how many items carry each",
		Long: "Lists every custom-tag key in the catalog with the number of distinct items " +
			"carrying it (most-used first). These are the browse dimensions available to " +
			"`facet --group-by tag.<KEY>` and `query --tag KEY=VALUE`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A catalog-wide read: open read-only so it takes no maintenance lock and does not
			// auto-proxy through a running server, unlike the mutating `tag <pid>` set path.
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			keys, err := lib.TagKeys(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, tagKeyViews(keys))
			}
			if len(keys) == 0 {
				fmt.Fprintln(out(cmd), "(no custom tags)")
				return nil
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tITEMS")
			for _, k := range keys {
				fmt.Fprintf(tw, "%s\t%d\n", k.Key, k.Count)
			}
			return tw.Flush()
		},
	}
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
	if err := surfaceWriteBack(cmd, err); err != nil {
		return err
	}
	text := fmt.Sprintf("set tag %s (%d value(s)) on %s\n", canonKey, stored, pid)
	if stored == 0 {
		text = fmt.Sprintf("cleared tag %s on %s\n", canonKey, pid)
	}
	return reply(cmd, g, struct {
		ItemPID model.PID `json:"itemPid"`
		Key     string    `json:"key"`
		Values  int       `json:"values"`
	}{pid, canonKey, stored}, text)
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

// tagKeyView is the JSON shape for a custom-tag key and its item count.
type tagKeyView struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func tagKeyViews(keys []read.TagKeyCount) []tagKeyView {
	out := make([]tagKeyView, len(keys))
	for i, k := range keys {
		out[i] = tagKeyView{Key: k.Key, Count: k.Count}
	}
	return out
}
