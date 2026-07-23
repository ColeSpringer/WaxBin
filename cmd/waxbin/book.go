package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newBookCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "book <pid>",
		Short: "Show an audiobook: contributors, series, chapters, and resume",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			d, err := lib.Book(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			// The default user's resume position and the chapter it falls in. An
			// unplayed book reports a zero position (not an error); a real read failure
			// is surfaced rather than swallowed.
			resume, cur, err := lib.BookResume(ctx(cmd), "", d.Item.PID)
			if err != nil {
				return err
			}

			if g.jsonOut {
				return printJSON(cmd, toBookView(d))
			}
			w := out(cmd)
			fmt.Fprintf(w, "pid:          %s\n", d.Item.PID)
			fmt.Fprintf(w, "title:        %s\n", d.Item.Title)
			if d.Subtitle != "" {
				fmt.Fprintf(w, "subtitle:     %s\n", d.Subtitle)
			}
			fmt.Fprintf(w, "author(s):    %s\n", strings.Join(d.Authors, ", "))
			if len(d.Narrators) > 0 {
				fmt.Fprintf(w, "narrator(s):  %s\n", strings.Join(d.Narrators, ", "))
			}
			if len(d.Translators) > 0 {
				fmt.Fprintf(w, "translator(s):%s\n", strings.Join(d.Translators, ", "))
			}
			if d.Series != "" {
				seq := ""
				if d.SeriesSeq != "" {
					seq = " #" + d.SeriesSeq
				}
				fmt.Fprintf(w, "series:       %s%s\n", d.Series, seq)
			}
			if d.Item.Year != 0 {
				fmt.Fprintf(w, "year:         %d\n", d.Item.Year)
			}
			if d.Publisher != "" {
				fmt.Fprintf(w, "publisher:    %s\n", d.Publisher)
			}
			if d.ASIN != "" {
				fmt.Fprintf(w, "asin:         %s\n", d.ASIN)
			}
			if d.ISBN != "" {
				fmt.Fprintf(w, "isbn:         %s\n", d.ISBN)
			}
			if d.Edition != "" {
				fmt.Fprintf(w, "edition:      %s\n", d.Edition)
			}
			if d.Abridged != nil {
				if *d.Abridged {
					fmt.Fprintln(w, "abridged:     yes")
				} else {
					fmt.Fprintln(w, "abridged:     no (unabridged)")
				}
			}
			fmt.Fprintf(w, "duration:     %s (%d parts, %d chapters)\n",
				durationLabel(d.TotalDurationMS), len(d.Files), len(d.Chapters))
			if cur != nil && resume.PositionMS > 0 {
				fmt.Fprintf(w, "resume:       ch %d %q at %s\n", cur.Position+1, cur.Title, durationLabel(resume.PositionMS))
			}
			if len(d.Files) > 1 {
				fmt.Fprintln(w, "parts:")
				for _, p := range d.Files {
					fmt.Fprintf(w, "  [%d] %s  (%s)\n", p.Position, p.DisplayPath, durationLabel(p.DurationMS))
				}
			}
			printChapters(cmd, d.Chapters)
			return nil
		},
	}
}

func newChaptersCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "chapters <pid>",
		Short: "List an audiobook's chapters with book-timeline offsets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			chs, err := lib.Chapters(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, chapterViews(chs))
			}
			if len(chs) == 0 {
				fmt.Fprintln(out(cmd), "(no chapters)")
				return nil
			}
			printChapters(cmd, chs)
			return nil
		},
	}
	cmd.AddCommand(newChaptersSetCmd(g))
	return cmd
}

func newChaptersSetCmd(g *globals) *cobra.Command {
	var (
		filePath string
		clear    bool
		noLock   bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "set <pid> --file <cue>",
		Short: "Set (or clear) a book's user chapters from a .cue file",
		Long: "Sets user-curated chapters on a book from a .cue file, which win on read over the " +
			"scanned chapters and survive a `scan --force`. The cue's INDEX offsets are read as " +
			"book-timeline positions (cumulative across the parts of a multi-file book; for a " +
			"single file the two are the same). --clear removes the user chapters, falling back " +
			"to the scanned ones. Locks the chapters field by default.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var chapters []model.Chapter
			if !clear {
				if filePath == "" {
					return waxerr.New(waxerr.CodeInvalid, "chapters set", "provide --file or --clear")
				}
				b, err := os.ReadFile(filePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "chapters set", err, "reading %s", filePath)
				}
				chapters = meta.ParseCue(string(b))
				if len(chapters) == 0 {
					return waxerr.New(waxerr.CodeInvalid, "chapters set", "no chapters parsed from "+filePath)
				}
				// ParseCue emits file-relative offsets; a whole-book cue means them as
				// timeline positions, which SetChapters takes in StartMS.
				for i := range chapters {
					chapters[i].StartMS = chapters[i].FileStartMS
				}
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			if err := m.SetChapters(ctx(cmd), model.PID(args[0]), chapters, !noLock, force); err != nil {
				return err
			}
			if clear {
				fmt.Fprintf(out(cmd), "cleared user chapters for %s\n", args[0])
			} else {
				fmt.Fprintf(out(cmd), "set %d user chapter(s) for %s\n", len(chapters), args[0])
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&filePath, "file", "", ".cue file to set as the chapters")
	f.BoolVar(&clear, "clear", false, "remove the user chapters instead of setting them")
	f.BoolVar(&noLock, "no-lock", false, "do not lock the chapters field (it defaults to locked)")
	f.BoolVar(&force, "force", false, "override a locked chapters field")
	return cmd
}

func printChapters(cmd *cobra.Command, chs []model.Chapter) {
	if len(chs) == 0 {
		return
	}
	w := out(cmd)
	fmt.Fprintln(w, "chapters:")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, c := range chs {
		title := c.Title
		if title == "" {
			title = fmt.Sprintf("Chapter %d", c.Position+1)
		}
		fmt.Fprintf(tw, "  %3d\t%s\t%s\n", c.Position+1, durationLabel(c.StartMS), title)
	}
	_ = tw.Flush()
}
