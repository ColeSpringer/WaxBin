package main

import (
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/colespringer/waxbin"
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
	var (
		limit  int
		cursor string
	)
	cmd := &cobra.Command{
		Use:   "folds <artist|release_group|album|genre|series>",
		Short: "List the keys merges folded into an entity",
		Long: "A merge folds each loser's key into the survivor, so a file still spelled the " +
			"loser's way resolves to the survivor when it is read again instead of bringing the " +
			"loser back; a locked rename folds the key it left the same way. This lists them a " +
			"page at a time in key order, with the spelling each key was read from (and an " +
			"album's folder) beside the entity it names now. A key holding separators (an " +
			"album's or a release group's) is printed quoted, the form `entity unfold` takes back.",
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
			after, err := unquoteKey(cursor)
			if err != nil {
				return err
			}
			page, err := lib.EntityFolds(ctx(cmd), et, after, limit)
			if err != nil {
				return err
			}
			if g.jsonOut {
				type foldJSON struct {
					Key        string `json:"key"`
					Name       string `json:"name"`
					Folder     string `json:"folder,omitempty"`
					LibraryPID string `json:"libraryPid,omitempty"`
					EntityPID  string `json:"entityPid"`
					EntityName string `json:"entityName"`
					CreatedAt  int64  `json:"createdAt,string"`
				}
				rows := make([]foldJSON, 0, len(page.Folds))
				for _, f := range page.Folds {
					rows = append(rows, foldJSON{f.Key, f.Name, f.Folder, string(f.LibraryPID), string(f.EntityPID), f.EntityName, f.CreatedAt})
				}
				return printJSON(cmd, struct {
					Folds      []foldJSON `json:"folds"`
					NextCursor string     `json:"nextCursor,omitempty"`
					HasMore    bool       `json:"hasMore"`
				}{rows, page.Next, page.HasMore})
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tREAD FROM\tFOLDER\tENTITY\tNAME")
			for _, f := range page.Folds {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", quoteKey(f.Key), f.Name, f.Folder, f.EntityPID, f.EntityName)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out(cmd), "(%s)\n", plural(len(page.Folds), string(et)+" fold"))
			if page.HasMore {
				fmt.Fprintf(out(cmd), "(more: entity folds %s --cursor %s)\n", et, quoteKey(page.Next))
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "list at most this many folds (0 = the default page of 100)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "keyset cursor from a prior page's nextCursor, quoted or not")
	return cmd
}

func newEntityUnfoldCmd(g *globals) *cobra.Command {
	var reResolve bool
	cmd := &cobra.Command{
		Use:   "unfold <artist|release_group|album|genre|series> <key>...",
		Short: "Forget folds, so files spelled the old way mint their own entity again",
		Long: "Forgets the given keys' folds (as `entity folds` prints them, quoted or not), " +
			"all of them or none. The entity they folded into is untouched; the next scan of a " +
			"file carrying one of the keys mints that key's own entity again, which an unchanged " +
			"file never prompts. --re-resolve reads again, as a job, the files of the items a fold " +
			"kept (an artist's or genre's whose own spelling folds to the key, every member of an " +
			"album, release group or series), filing each under the name it states, and names the " +
			"items it read and those whose file was offline, missing or unreadable.",
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
			if reResolve {
				rep, err := unfoldReResolving(cmd, g, et, keys)
				if err != nil {
					return err
				}
				return emitUnfold(cmd, g, et, keys, rep)
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.UnfoldEntity(ctx(cmd), et, keys); err != nil {
				return err
			}
			return emitUnfold(cmd, g, et, keys, nil)
		},
	}
	cmd.Flags().BoolVar(&reResolve, "re-resolve", false, "read again the files the folds kept, filing each under the name it states")
	return cmd
}

// unfoldReResolving runs an unfold that reads files again: in a server holding the write
// lock as a job the command follows, or here.
func unfoldReResolving(cmd *cobra.Command, g *globals, et model.MergeEntity, keys []string) (*waxbin.UnfoldReport, error) {
	px, err := g.jobServer(cmd)
	if err != nil {
		return nil, err
	}
	if px != nil {
		defer px.Close()
		jobPID, err := px.RunUnfold(ctx(cmd), et, keys, true)
		if err != nil {
			return nil, err
		}
		job, err := g.tailJob(cmd, jobPID)
		if err != nil {
			return nil, err
		}
		var rep waxbin.UnfoldReport
		if err := unmarshalJobResult(job, &rep); err != nil {
			return nil, err
		}
		return &rep, nil
	}
	lib, _, err := g.open(cmd)
	if err != nil {
		return nil, err
	}
	defer lib.Close()
	return lib.Unfold(ctx(cmd), et, keys, waxbin.UnfoldOptions{ReResolve: true})
}

// emitUnfold prints what an unfold forgot, and with a report what it read again.
func emitUnfold(cmd *cobra.Command, g *globals, et model.MergeEntity, keys []string, rep *waxbin.UnfoldReport) error {
	if g.jsonOut {
		// A re-resolving unfold always prints both lists, a plain one neither.
		view := struct {
			EntityType string       `json:"entityType"`
			Unfolded   []string     `json:"unfolded"`
			Reread     *[]model.PID `json:"reread,omitempty"`
			Skipped    *[]model.PID `json:"skipped,omitempty"`
		}{EntityType: string(et), Unfolded: keys}
		if rep != nil {
			reread, skipped := nonNil(rep.Reread), nonNil(rep.Skipped)
			view.Reread, view.Skipped = &reread, &skipped
		}
		return printJSON(cmd, view)
	}
	fmt.Fprintf(out(cmd), "unfolded %s\n", plural(len(keys), string(et)+" key"))
	if rep != nil {
		fmt.Fprintf(out(cmd), "read again %s\n", plural(len(rep.Reread), "item"))
		for _, pid := range rep.Skipped {
			fmt.Fprintf(out(cmd), "  skip %s (its file is offline, missing or unreadable)\n", pid)
		}
	}
	return nil
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
