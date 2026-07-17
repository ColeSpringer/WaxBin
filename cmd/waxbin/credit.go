package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newCreditCmd(g *globals) *cobra.Command {
	var (
		role      string
		names     []string
		writeBack bool
		noLock    bool
		force     bool
	)
	cmd := &cobra.Command{
		Use:   "credit <pid> [--role <role> --name <name> ...]",
		Short: "View or set an item's contributor credits",
		Long: "Without --role, lists an item's contributors across every role. With --role, " +
			"replaces that role's contributors with the given --name values (repeatable; none " +
			"clears the role). A credit records user provenance and, by default, locks the " +
			"credit.<role> field. --write-back also mirrors a track credit into its file's tag.\n\n" +
			"Music roles (tracks): composer, lyricist, conductor, performer, remixer, producer, " +
			"engineer, mixer, arranger, writer, djmixer.\n" +
			"Book roles (audiobooks): author, narrator, translator, editor.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			if role == "" {
				return listCredits(cmd, g, pid)
			}
			return setCredits(cmd, g, pid, model.ContributorRole(role), names,
				waxbin.CreditEditOptions{WriteBack: writeBack, Lock: !noLock, Force: force})
		},
	}
	f := cmd.Flags()
	f.StringVar(&role, "role", "", "contributor role to set (omit to list all credits)")
	f.StringArrayVar(&names, "name", nil, "contributor name for the role (repeatable; none clears it)")
	f.BoolVar(&writeBack, "write-back", false, "also write the role into the file's on-disk tag (tracks only)")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the credit (it defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked credit role")
	return cmd
}

func listCredits(cmd *cobra.Command, g *globals, pid model.PID) error {
	lib, _, err := g.openRead(cmd)
	if err != nil {
		return err
	}
	defer lib.Close()
	credits, err := lib.Credits(ctx(cmd), pid)
	if err != nil {
		return err
	}
	if g.jsonOut {
		return printJSON(cmd, creditViews(credits))
	}
	if len(credits) == 0 {
		fmt.Fprintln(out(cmd), "(no credits)")
		return nil
	}
	tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tNAME\tARTIST")
	for _, c := range credits {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Role, c.Name, c.ArtistPID)
	}
	return tw.Flush()
}

func setCredits(cmd *cobra.Command, g *globals, pid model.PID, role model.ContributorRole, names []string, opts waxbin.CreditEditOptions) error {
	m, _, err := g.openMutator(cmd)
	if err != nil {
		return err
	}
	defer m.Close()

	stored, err := m.SetCredits(ctx(cmd), pid, role, names, opts)
	var wbErr *waxbin.WriteBackError
	if errors.As(err, &wbErr) {
		for _, f := range wbErr.Failures {
			fmt.Fprintf(errOut(cmd), "warning: on-disk credit write-back skipped for %s: %s\n", f.Path, f.Reason)
		}
	} else if err != nil {
		return err
	}
	// Report the count actually stored (trimmed, resolvable, deduped) rather than the
	// raw --name count, so an unresolvable name that cleared the role reads as "0".
	if stored == 0 {
		fmt.Fprintf(out(cmd), "cleared %s credits for %s\n", role, pid)
	} else {
		fmt.Fprintf(out(cmd), "set %d %s credit(s) for %s\n", stored, role, pid)
	}
	return nil
}

// creditView is the JSON shape for a contributor.
type creditView struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	ArtistPID string `json:"artistPid"`
	Position  int    `json:"position"`
}

func creditViews(cs []model.Contributor) []creditView {
	out := make([]creditView, len(cs))
	for i, c := range cs {
		out[i] = creditView{Role: string(c.Role), Name: c.Name, ArtistPID: string(c.ArtistPID), Position: c.Position}
	}
	return out
}
