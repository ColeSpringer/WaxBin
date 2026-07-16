package main

import (
	"fmt"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newUserCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage playback users",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List playback users",
			RunE: func(cmd *cobra.Command, _ []string) error {
				lib, _, err := g.openRead(cmd)
				if err != nil {
					return err
				}
				defer lib.Close()
				users, err := lib.Users(ctx(cmd))
				if err != nil {
					return err
				}
				if g.jsonOut {
					return printJSON(cmd, userViews(users))
				}
				w := out(cmd)
				for _, u := range users {
					def := ""
					if u.IsDefault {
						def = " (default)"
					}
					fmt.Fprintf(w, "%s  %s%s\n", u.PID, u.Name, def)
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "add <name>",
			Short: "Add a playback user",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				m, _, err := g.openMutator(cmd)
				if err != nil {
					return err
				}
				defer m.Close()
				u, err := m.CreateUser(ctx(cmd), args[0])
				if err != nil {
					return err
				}
				if g.jsonOut {
					return printJSON(cmd, userViews([]*model.User{u}))
				}
				fmt.Fprintf(out(cmd), "created user %s (%s)\n", u.Name, u.PID)
				return nil
			},
		},
	)
	return cmd
}

func newStateCmd(g *globals) *cobra.Command {
	var (
		user     string
		rating   int
		star     bool
		unstar   bool
		played   bool
		finished bool
		position int64
	)
	set := &cobra.Command{
		Use:   "set <item-pid>",
		Short: "Set a user's playback state for an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			item := model.PID(args[0])
			uPID, err := resolveUser(cmd, m, user)
			if err != nil {
				return err
			}
			flags := cmd.Flags()

			if flags.Changed("rating") {
				var r *int
				if rating >= 0 { // a negative rating clears it
					r = &rating
				}
				if err := m.SetRating(ctx(cmd), uPID, item, r); err != nil {
					return err
				}
			}
			if star {
				if err := m.SetStar(ctx(cmd), uPID, item, true); err != nil {
					return err
				}
			}
			if unstar {
				if err := m.SetStar(ctx(cmd), uPID, item, false); err != nil {
					return err
				}
			}
			if played || finished {
				if err := m.MarkPlayed(ctx(cmd), uPID, item, finished); err != nil {
					return err
				}
			}
			if flags.Changed("position") {
				if err := m.Checkpoint(ctx(cmd), uPID, item, position); err != nil {
					return err
				}
			}

			st, err := m.PlayState(ctx(cmd), uPID, item)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toPlayStateView(st))
			}
			return printPlayState(cmd, st)
		},
	}
	pf := set.Flags()
	pf.StringVar(&user, "user", "", "user name (default user when omitted)")
	pf.IntVar(&rating, "rating", 0, "rating 0-100 (negative clears)")
	pf.BoolVar(&star, "star", false, "star the item")
	pf.BoolVar(&unstar, "unstar", false, "unstar the item")
	pf.BoolVar(&played, "played", false, "mark played (increments play count)")
	pf.BoolVar(&finished, "finished", false, "mark finished (implies played)")
	pf.Int64Var(&position, "position", 0, "set resume position in milliseconds")

	show := &cobra.Command{
		Use:   "show <item-pid>",
		Short: "Show a user's playback state for an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			uPID, err := resolveUser(cmd, lib, user)
			if err != nil {
				return err
			}
			st, err := lib.Playback().State(ctx(cmd), uPID, model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toPlayStateView(st))
			}
			return printPlayState(cmd, st)
		},
	}
	show.Flags().StringVar(&user, "user", "", "user name (default user when omitted)")

	cmd := &cobra.Command{Use: "state", Short: "Inspect or set playback state"}
	cmd.AddCommand(set, show)
	return cmd
}

// resolveUser maps a user name to its pid, returning "" (the store's default-user
// sentinel) when no name is given. It reads users through a userLister, so it works
// with a directly-opened Library or a proxied mutator alike.
func resolveUser(cmd *cobra.Command, lib userLister, name string) (model.PID, error) {
	if name == "" {
		return "", nil
	}
	users, err := lib.Users(ctx(cmd))
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Name == name {
			return u.PID, nil
		}
	}
	return "", waxerr.New(waxerr.CodeNotFound, "cli.state", "no such user: "+name)
}

func printPlayState(cmd *cobra.Command, st *model.PlayState) error {
	w := out(cmd)
	fmt.Fprintf(w, "item:      %s\n", st.ItemPID)
	fmt.Fprintf(w, "position:  %d ms\n", st.PositionMS)
	fmt.Fprintf(w, "played:    %t (count %d)\n", st.Played, st.PlayCount)
	fmt.Fprintf(w, "finished:  %t\n", st.Finished)
	if st.HasRating {
		fmt.Fprintf(w, "rating:    %d/100\n", st.Rating)
	}
	fmt.Fprintf(w, "starred:   %t\n", st.Starred)
	return nil
}
