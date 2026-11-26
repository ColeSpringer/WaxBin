package main

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newBookCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
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
			if d.IdentityHeld {
				fmt.Fprintln(w, "identity:     held by assignment (title, author and identifiers stay as the catalog holds them)")
			}
			if len(d.Files) > 1 || slices.ContainsFunc(d.Files, func(p model.BookPart) bool { return p.Assigned }) {
				fmt.Fprintln(w, "parts:")
				for _, p := range d.Files {
					assigned := ""
					if p.Assigned {
						assigned = "  assigned"
					}
					fmt.Fprintf(w, "  [%d] %s  from %s (%s)%s\n", p.Position, p.DisplayPath, durationLabel(p.StartMS), durationLabel(p.DurationMS), assigned)
				}
			}
			printChapters(cmd, d.Chapters)
			return nil
		},
	}
	cmd.AddCommand(newBookPartCmd(g))
	return cmd
}

func newBookPartCmd(g *globals) *cobra.Command {
	var (
		book         string
		disc, number int
		clear        bool
	)
	cmd := &cobra.Command{
		Use:   "part <file-pid>...",
		Short: "File parts into a book whatever their tags say, or withdraw that",
		Long: "Files each named file into the book --book names, as its part at --disc and " +
			"--number (the files take that number and the ones after it, in the order named), or " +
			"at the number its own tags give it when no number is given. Later scans keep the " +
			"file there whatever its tags say, and the book keeps its title, author and " +
			"identifiers, until --clear withdraws the assignment, a kind change makes the file a " +
			"track, or the book goes. A one-part book a file leaves folds into the book it joins, and " +
			"the file's copies go with it. --clear withdraws the assignment and files each file as " +
			"later scans will, by its own tags and its folder; a file with no assignment is left as " +
			"it is. `book <pid>` marks the assigned parts.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case (book == "") == !clear:
				return waxerr.New(waxerr.CodeInvalid, "book part", "give --book <pid> or --clear")
			case clear && (disc != 0 || number != 0):
				return waxerr.New(waxerr.CodeInvalid, "book part", "--clear takes no number")
			case disc < 0 || number < 0:
				return waxerr.New(waxerr.CodeInvalid, "book part", "--disc and --number count from 1")
			case disc != 0 && number == 0:
				return waxerr.New(waxerr.CodeInvalid, "book part", "--disc needs --number")
			}
			changes := make([]waxbin.BookPartChange, len(args))
			for i, a := range args {
				changes[i] = waxbin.BookPartChange{FilePID: model.PID(a), BookPID: model.PID(book)}
				if number > 0 {
					changes[i].Disc, changes[i].Part = disc, number+i
				}
			}
			m, _, err := g.openMutator(cmd)
			if err != nil {
				return err
			}
			defer m.Close()
			rep, err := m.AssignBookParts(ctx(cmd), changes, func(job model.PID) (*model.Job, error) { return g.tailJob(cmd, job) })
			if rep != nil && (err == nil || len(rep.Files) > 0) {
				if perr := emitBookParts(cmd, g, rep, clear, err); err == nil {
					err = perr
				}
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&book, "book", "", "the book to file the parts into")
	f.IntVar(&disc, "disc", 0, "the disc the parts are on")
	f.IntVar(&number, "number", 0, "the first part's number on its disc; the files after it follow")
	f.BoolVar(&clear, "clear", false, "withdraw the files' assignments and read them by their own tags")
	return cmd
}

// emitBookParts prints what a book part change did. Under --json a change cut short carries
// its failure as the error member.
func emitBookParts(cmd *cobra.Command, g *globals, rep *waxbin.BookPartsReport, withdrew bool, failure error) error {
	if g.jsonOut {
		return printJSON(cmd, struct {
			*waxbin.BookPartsReport
			Error string `json:"error,omitempty"`
		}{rep, errText(failure)})
	}
	w := out(cmd)
	if withdrew {
		fmt.Fprintf(w, "Withdrew %s\n", plural(len(rep.Withdrawn), "part"))
	} else {
		fmt.Fprintf(w, "Filed %s\n", plural(len(rep.Files), "part"))
	}
	for _, f := range slices.Sorted(maps.Keys(rep.Files)) {
		fmt.Fprintf(w, "  file %s -> %s\n", f, rep.Files[f])
	}
	for _, p := range slices.Sorted(maps.Keys(rep.Absorbed)) {
		fmt.Fprintf(w, "  absorbed  %s -> %s\n", p, rep.Absorbed[p])
	}
	for _, p := range slices.Sorted(maps.Keys(rep.Created)) {
		fmt.Fprintf(w, "  created   %s from %s\n", p, rep.Created[p])
	}
	return nil
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
		keepLock bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "set <pid> --file <cue>",
		Short: "Set (or clear) a book's user chapters from a .cue file",
		Long: "Sets user-curated chapters on a book from a .cue file, which win on read over the " +
			"scanned chapters and survive a `scan --force`. The cue's INDEX offsets are read as " +
			"book-timeline positions, the timeline `book` shows (each part starting where the " +
			"one before it ends; for a single file the two are the same). A chapter belongs to " +
			"the part its start falls in and ends by that part's end, except in the last part. " +
			"When the edit changes how long a part runs, resume positions and bookmarks move " +
			"with their audio. --clear removes the user chapters, falling back to the scanned " +
			"ones. Locks the chapters field by default.",
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
				chapters, err = meta.ParseCue(string(b))
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeInvalid, "chapters set", err, "parsing %s", filePath)
				}
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
			if err := m.SetChapters(ctx(cmd), model.PID(args[0]), chapters, lockChange(noLock, keepLock), force); err != nil {
				return err
			}
			text := fmt.Sprintf("set %d user chapter(s) for %s\n", len(chapters), args[0])
			if clear {
				text = fmt.Sprintf("cleared user chapters for %s\n", args[0])
			}
			return reply(cmd, g, struct {
				ItemPID  string `json:"itemPid"`
				Chapters int    `json:"chapters"`
			}{args[0], len(chapters)}, text)
		},
	}
	f := cmd.Flags()
	f.StringVar(&filePath, "file", "", ".cue file to set as the chapters")
	f.BoolVar(&clear, "clear", false, "remove the user chapters instead of setting them")
	f.BoolVar(&noLock, "no-lock", false, "unlock the chapters field (it defaults to locked)")
	f.BoolVar(&keepLock, "keep-lock", false, keepLockUsage("the chapters field"))
	cmd.MarkFlagsMutuallyExclusive("no-lock", "keep-lock")
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
