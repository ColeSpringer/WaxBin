package main

import (
	"fmt"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/spf13/cobra"
)

func newLyricsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lyrics PID",
		Short: "Show an item's structured lyrics",
		Long: "Prints an item's lyrics: timed (synced) lines when present, otherwise the " +
			"unsynchronized text. Lyrics come from a sibling .lrc sidecar or embedded tags, " +
			"captured at scan time.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			ly, err := lib.Lyrics(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toLyricsView(ly))
			}
			if len(ly.Synced) > 0 {
				for _, l := range ly.Synced {
					fmt.Fprintf(out(cmd), "[%s] %s\n", fmtLyricTime(l.TimeMS), l.Text)
				}
				return nil
			}
			fmt.Fprintln(out(cmd), ly.Unsynced)
			return nil
		},
	}
	return cmd
}

// fmtLyricTime renders a millisecond offset as mm:ss.mmm for synced-lyric display.
func fmtLyricTime(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	m := int(d / time.Minute)
	s := int(d/time.Second) % 60
	frac := ms % 1000
	return fmt.Sprintf("%02d:%02d.%03d", m, s, frac)
}
