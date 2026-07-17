package main

import (
	"fmt"
	"os"
	"time"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
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
	cmd.AddCommand(newLyricsSetCmd(g))
	return cmd
}

func newLyricsSetCmd(g *globals) *cobra.Command {
	var (
		filePath string
		clear    bool
		noLock   bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "set <pid> --file <lrc>",
		Short: "Set (or clear) an item's lyrics from an .lrc file",
		Long: "Sets user-curated lyrics on a track from an .lrc file (timed lines are parsed; " +
			"plain lines become unsynchronized text), or --clear removes them. A set locks the " +
			"item's lyrics field by default so a scan does not overwrite it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var ly *model.Lyrics
			if !clear {
				if filePath == "" {
					return waxerr.New(waxerr.CodeInvalid, "lyrics set", "provide --file or --clear")
				}
				b, err := os.ReadFile(filePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "lyrics set", err, "reading %s", filePath)
				}
				synced, _ := meta.ParseLRC(string(b))
				ly = &model.Lyrics{Synced: synced}
				if len(synced) == 0 {
					ly.Unsynced = string(b)
				}
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.SetLyrics(ctx(cmd), model.PID(args[0]), ly, !noLock, force); err != nil {
				return err
			}
			if clear {
				fmt.Fprintf(out(cmd), "cleared lyrics for %s\n", args[0])
			} else {
				fmt.Fprintf(out(cmd), "set lyrics for %s\n", args[0])
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&filePath, "file", "", ".lrc file to set as the lyrics")
	f.BoolVar(&clear, "clear", false, "remove the lyrics instead of setting them")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the lyrics field (it defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked lyrics field")
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
