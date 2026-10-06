package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/spf13/cobra"
)

type profileView struct {
	Name      string `json:"name"`
	TagWrite  bool   `json:"tagWrite"`
	Music     string `json:"music"`
	Audiobook string `json:"audiobook"`
	Podcast   string `json:"podcast"`
	// CompilationFolder is the folder compilations file under, empty for their album
	// artist's.
	CompilationFolder string `json:"compilationFolder"`
}

func newProfilesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "List the organization profiles and their path templates",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.loadConfig(cmd)
			if err != nil {
				return err
			}
			profiles, err := waxbin.ProfilesFor(cfg.Profiles)
			if err != nil {
				return err
			}
			if g.jsonOut {
				views := make([]profileView, len(profiles))
				for i, p := range profiles {
					views[i] = profileView{p.Name, p.TagWrite, p.Music, p.Audiobook, p.Podcast, p.Compilations()}
				}
				return printJSON(cmd, views)
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTAG-WRITE\tCOMPILATIONS\tMUSIC\tAUDIOBOOK\tPODCAST")
			for _, p := range profiles {
				folder := p.Compilations()
				if folder == "" {
					folder = "(album artist)"
				}
				fmt.Fprintf(tw, "%s\t%t\t%s\t%s\t%s\t%s\n", p.Name, p.TagWrite, folder, p.Music, p.Audiobook, p.Podcast)
			}
			return tw.Flush()
		},
	}
}
