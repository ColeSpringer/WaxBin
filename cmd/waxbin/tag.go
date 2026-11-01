package main

import (
	"errors"
	"fmt"
	"strings"
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
		groups    tagGroups
		noLock    bool
		keepLock  bool
		force     bool
		writeBack bool
	)
	cmd := &cobra.Command{
		Use:   "tag <pid> [--key KEY --value V ...]... [--clear KEY]...",
		Short: "View or set an item's custom tags",
		Long: "Without --key or --clear, lists an item's custom tags. Each --key replaces that tag's " +
			"values with the --value entries that follow it (repeatable; none clears the tag), and " +
			"each --clear clears a tag, all in one write. The key is normalized to canonical " +
			"uppercase (KEY and key are one tag). A tag records user provenance and, by default, " +
			"locks the tag against a scan re-deriving it.\n\n" +
			"A key WaxBin maps through the scalar, credit, or entity edit surface (title, artist, isrc, " +
			"barcode, a contributor role, ...) is reserved and rejected; use that surface instead.\n\n" +
			"--write-back also writes the tags into the item's file(s), every part of a book included, " +
			"each file rewritten once for all the keys it takes. With it, a spelling the tag library writes onto another field " +
			"(YEAR, TRACK, ALBUM_ARTIST, ...) is refused, and so is a key whose file's own format would " +
			"do that (TPE2 in an MP3), which that file refuses alone while the other keys are written.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			if len(groups.edits) == 0 {
				// A set-side flag with no --key is a mistake, so reject it rather than
				// falling through to a listing.
				if cmd.Flags().Changed("no-lock") || cmd.Flags().Changed("keep-lock") ||
					cmd.Flags().Changed("force") || cmd.Flags().Changed("write-back") {
					return waxerr.New(waxerr.CodeInvalid, "tag", "--key is required to set a tag (with --value/--no-lock/--keep-lock/--force/--write-back)")
				}
				return listTags(cmd, g, pid)
			}
			return setTags(cmd, g, pid, groups.edits,
				waxbin.TagEditOptions{Lock: lockChange(noLock, keepLock), Force: force, WriteBack: writeBack})
		},
	}
	f := cmd.Flags()
	f.Var(tagKeyFlag{&groups}, "key", "custom tag key to set, with the --value entries after it (repeatable; omit to list all tags)")
	f.Var(tagValueFlag{&groups}, "value", "value for the --key before it (repeatable; none clears the tag)")
	f.Var(tagClearFlag{&groups}, "clear", "custom tag key to clear (repeatable)")
	f.BoolVar(&noLock, "no-lock", false, "unlock the tags (they default to locked)")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the tags")+
		"; clearing a tag forgets it entirely, lock included, whichever lock flag is given")
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
	f.BoolVar(&force, "force", false, "override a locked tag")
	f.BoolVar(&writeBack, "write-back", false, "also write the tags into the item's file(s) on disk")
	// `tag keys` is a catalog-wide read subcommand. Routing is unambiguous because an item
	// pid is a ULID and can never be the literal "keys", so `tag <ulid>` still hits the
	// parent's set/list RunE while `tag keys` hits the subcommand.
	cmd.AddCommand(newTagKeysCmd(g))
	return cmd
}

// tagGroups gathers --key, --value and --clear in command-line order, which pflag keeps
// by calling each flag's Set as it reads it: a --value joins the --key before it.
type tagGroups struct {
	edits  []model.TagEdit
	clears []bool
}

type tagKeyFlag struct{ g *tagGroups }

func (f tagKeyFlag) String() string { return "" }
func (f tagKeyFlag) Type() string   { return "string" }
func (f tagKeyFlag) Set(key string) error {
	f.g.edits = append(f.g.edits, model.TagEdit{Key: key})
	f.g.clears = append(f.g.clears, false)
	return nil
}

type tagClearFlag struct{ g *tagGroups }

func (f tagClearFlag) String() string { return "" }
func (f tagClearFlag) Type() string   { return "string" }
func (f tagClearFlag) Set(key string) error {
	f.g.edits = append(f.g.edits, model.TagEdit{Key: key})
	f.g.clears = append(f.g.clears, true)
	return nil
}

type tagValueFlag struct{ g *tagGroups }

func (f tagValueFlag) String() string { return "" }
func (f tagValueFlag) Type() string   { return "string" }
func (f tagValueFlag) Set(v string) error {
	n := len(f.g.edits)
	switch {
	case n == 0:
		return errors.New("a --value belongs to the --key before it")
	case f.g.clears[n-1]:
		return errors.New("--clear " + f.g.edits[n-1].Key + " takes no --value")
	}
	f.g.edits[n-1].Values = append(f.g.edits[n-1].Values, v)
	return nil
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

func setTags(cmd *cobra.Command, g *globals, pid model.PID, edits []model.TagEdit, opts waxbin.TagEditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()
	// Report the counts the store actually stored (after trimming), so a whitespace-only
	// --value reads as a clear rather than a set.
	res, err := m.SetItemTags(ctx(cmd), pid, edits, opts)
	if err := surfaceWriteBack(cmd, err); err != nil {
		return err
	}
	var text strings.Builder
	views := make([]storedTagView, len(res.Tags))
	for i, t := range res.Tags {
		views[i] = storedTagView{Key: t.Key, Values: t.Stored}
		if t.Stored == 0 {
			fmt.Fprintf(&text, "cleared tag %s on %s\n", t.Key, pid)
		} else {
			fmt.Fprintf(&text, "set tag %s (%d value(s)) on %s\n", t.Key, t.Stored, pid)
		}
	}
	return reply(cmd, g, struct {
		ItemPID model.PID       `json:"itemPid"`
		Tags    []storedTagView `json:"tags"`
		Written int             `json:"written"`
	}{pid, views, res.Written}, text.String())
}

// storedTagView is the JSON shape for one key a tag set stored: its canonical key and how
// many values it holds (0 for a clear).
type storedTagView struct {
	Key    string `json:"key"`
	Values int    `json:"values"`
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
