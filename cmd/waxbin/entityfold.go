package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

// parseMergeEntity reads a fold or merge entity type argument.
func parseMergeEntity(op, arg string) (model.MergeEntity, error) {
	et := model.MergeEntity(arg)
	if !et.Valid() {
		return "", waxerr.New(waxerr.CodeInvalid, op,
			"unknown entity type "+arg+" (want artist|release_group|album|genre|series)")
	}
	return et, nil
}

func newEntityFoldsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "folds <artist|release_group|album|genre|series>",
		Short: "List the keys merges folded into an entity",
		Long: "A merge folds each loser's key into the survivor, so a file still spelled the " +
			"loser's way resolves to the survivor when it is read again instead of bringing the " +
			"loser back. This lists them, key first; a key holding separators (an album's or a " +
			"release group's) is printed quoted, the form `entity unfold` takes back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseMergeEntity("entity folds", args[0])
			if err != nil {
				return err
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			folds, err := lib.EntityFolds(ctx(cmd), et)
			if err != nil {
				return err
			}
			if g.jsonOut {
				type foldJSON struct {
					Key        string `json:"key"`
					EntityPID  string `json:"entityPid"`
					EntityName string `json:"entityName"`
					CreatedAt  int64  `json:"createdAt,string"`
				}
				rows := make([]foldJSON, 0, len(folds))
				for _, f := range folds {
					rows = append(rows, foldJSON{f.Key, string(f.EntityPID), f.EntityName, f.CreatedAt})
				}
				return printJSON(cmd, rows)
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tENTITY\tNAME")
			for _, f := range folds {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", quoteKey(f.Key), f.EntityPID, f.EntityName)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%s)\n", plural(len(folds), string(et)+" fold"))
			return nil
		},
	}
}

func newEntityUnfoldCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "unfold <artist|release_group|album|genre|series> <key>...",
		Short: "Forget folds, so files spelled the old way mint their own entity again",
		Long: "Forgets the given keys' folds (as `entity folds` prints them, quoted or not), " +
			"all of them or none. The entity they folded into is untouched; the next scan of a " +
			"file carrying one of the keys mints that key's own entity again.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			et, err := parseMergeEntity("entity unfold", args[0])
			if err != nil {
				return err
			}
			keys := make([]string, 0, len(args)-1)
			for _, a := range args[1:] {
				k, err := unquoteKey(a)
				if err != nil {
					return err
				}
				keys = append(keys, k)
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.UnfoldEntity(ctx(cmd), et, keys); err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					EntityType string   `json:"entityType"`
					Unfolded   []string `json:"unfolded"`
				}{string(et), keys})
			}
			fmt.Fprintf(out(cmd), "unfolded %s\n", plural(len(keys), string(et)+" key"))
			return nil
		},
	}
}

// quoteKey prints a fold key as it stands, or Go-quoted when it holds a character a
// terminal would not show, which an album's or a release group's separators are.
func quoteKey(key string) string {
	if strings.IndexFunc(key, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 || strings.HasPrefix(key, `"`) {
		return strconv.Quote(key)
	}
	return key
}

func quoteKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = quoteKey(k)
	}
	return out
}

// unquoteKey takes a key back as quoteKey printed it.
func unquoteKey(arg string) (string, error) {
	if !strings.HasPrefix(arg, `"`) {
		return arg, nil
	}
	k, err := strconv.Unquote(arg)
	if err != nil {
		return "", waxerr.New(waxerr.CodeInvalid, "entity unfold", "malformed quoted key "+arg)
	}
	return k, nil
}
