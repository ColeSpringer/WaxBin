package main

import (
	"fmt"
	"math"
	"strconv"
	"time"

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
		asOf     string
	)
	set := &cobra.Command{
		Use:   "set <item-pid>",
		Short: "Set a user's playback state for an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate the flag combination before opening the catalog and taking the
			// write lock, so a doomed command fails fast.
			flags := cmd.Flags()
			asOfNS, err := parseAsOf(asOf)
			if err != nil {
				return err
			}
			// --as-of records the star/rating change time; with none of those
			// operations present it would be silently ignored (played/finished/position
			// carry no recorded time), so refuse rather than mislead.
			if flags.Changed("as-of") && !star && !unstar && !flags.Changed("rating") {
				return waxerr.New(waxerr.CodeInvalid, "cli.state",
					"--as-of applies only to --rating, --star, or --unstar")
			}

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

			if flags.Changed("rating") {
				var r *int
				if rating >= 0 { // a negative rating clears it
					r = &rating
				}
				if err := m.SetRating(ctx(cmd), uPID, item, r, asOfNS); err != nil {
					return err
				}
			}
			if star {
				if err := m.SetStar(ctx(cmd), uPID, item, true, asOfNS); err != nil {
					return err
				}
			}
			if unstar {
				if err := m.SetStar(ctx(cmd), uPID, item, false, asOfNS); err != nil {
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
	pf.StringVar(&asOf, "as-of", "", "record the star/rating change at this time (unix ns or RFC3339); default is now")
	// star and unstar are contradictory. Rejecting the pair avoids the order-dependent
	// outcome of applying both, which a shared --as-of makes worse: the second flip
	// carries the same recorded time as the first and loses the stale-replay comparison.
	set.MarkFlagsMutuallyExclusive("star", "unstar")

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

// asOfNSFloor is the smallest bare integer parseAsOf accepts as a unix-nanosecond
// stamp. Below it the value is almost certainly a timestamp in the wrong unit (a
// seconds, millisecond, or microsecond time), which as nanoseconds would land an
// instant after the epoch and be silently discarded as a stale replay. 1e17 ns is
// early 1973, so every real recorded-time stamp clears it while every plausible
// wrong-unit value (a modern seconds/ms/micros timestamp) falls below it. RFC3339
// input is exempt: a date typed out carries its own unambiguous intent.
const asOfNSFloor = 100_000_000_000_000_000 // 1e17 ns, ~1973

// parseAsOf parses a --as-of flag into an optional recorded-time stamp (unix
// nanoseconds): the empty string yields nil (stamp at server now), a bare integer
// is taken as unix nanoseconds, and anything else is parsed as RFC3339. A bare
// integer below asOfNSFloor is rejected rather than silently read as an
// epoch-adjacent (stale) time, catching a seconds/milliseconds unit mix-up. It is
// the one shared parser for every --as-of flag so the item and entity mutations read
// the flag identically.
func parseAsOf(s string) (*int64, error) {
	if s == "" {
		return nil, nil
	}
	if ns, err := strconv.ParseInt(s, 10, 64); err == nil {
		if ns < asOfNSFloor {
			return nil, waxerr.New(waxerr.CodeInvalid, "cli.state",
				"--as-of "+s+" is too small to be unix nanoseconds (looks like a seconds or milliseconds time); pass nanoseconds or an RFC3339 time")
		}
		return &ns, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, waxerr.New(waxerr.CodeInvalid, "cli.state", "invalid --as-of (want unix ns or RFC3339): "+s)
	}
	// time.Time.UnixNano is undefined outside roughly 1678..2262 (the int64-ns range),
	// where it wraps silently to a garbage stamp. Reject an out-of-range date instead.
	if lo, hi := time.Unix(0, math.MinInt64), time.Unix(0, math.MaxInt64); t.Before(lo) || t.After(hi) {
		return nil, waxerr.New(waxerr.CodeInvalid, "cli.state",
			"--as-of "+s+" is outside the representable range (about 1678 to 2262)")
	}
	ns := t.UnixNano()
	return &ns, nil
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
