package main

import (
	"fmt"

	"github.com/colespringer/waxbin"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
	"github.com/spf13/cobra"
)

func newImportCmd(g *globals) *cobra.Command {
	var (
		as, sourceType, sourceURL, sourceID, provider string
		profile, dup, showPID, showTitle, epTitle     string
		book, author, bookPID                         string
		disc, part, parts                             int
		apply, asCopy, noPin, placeholder             bool
	)
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Ingest an acquired or manual media file",
		Long: "Routes a file by --as kind. Tracks and books go through the managed-library " +
			"import planner; episodes go into the podcast library as pinned episodes. The " +
			"selected kind is also the scanner override, so --as book can catalog an audiobook " +
			"whose tags do not identify it as one; that kind is then locked (`waxbin provenance` " +
			"shows it, `unlock <pid> kind` clears it) so later scans keep it. WaxBin records an " +
			"acquisition row with the supplied source metadata.\n\n" +
			"--book with --author, or --book-pid, files a book's part into that book at --disc and " +
			"--part whatever its tags say, and later scans keep it there (`book part --clear` " +
			"withdraws that); --book-pid adds a part to a standing book whose title has moved " +
			"since its key was made.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := model.Kind(as)
			if kind != model.KindTrack && kind != model.KindBook && kind != model.KindEpisode {
				return waxerr.New(waxerr.CodeInvalid, "import", "invalid --as (use track|book|episode)")
			}
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()

			meta := waxbin.AcquiredMeta{
				SourceType: model.SourceType(sourceType), SourceURL: sourceURL, SourceID: sourceID,
				Provider: provider, Profile: profile, Copy: asCopy, DupPolicy: model.DupPolicy(dup),
				ShowPID: model.PID(showPID), ShowTitle: showTitle, Title: epTitle,
			}
			switch {
			case bookPID != "" && (book != "" || author != ""):
				return waxerr.New(waxerr.CodeInvalid, "import", "--book-pid names the book; --book and --author go without it")
			case book != "" || bookPID != "":
				meta.Book = &waxbin.AcquiredBook{PID: model.PID(bookPID), Title: book, Author: author, Parts: parts}
			case author != "" || parts != 0:
				return waxerr.New(waxerr.CodeInvalid, "import", "--author and --parts go with --book or --book-pid")
			}
			if kind == model.KindEpisode && noPin {
				no := false
				meta.Pinned = &no
			}
			res, err := lib.ImportAcquired(ctx(cmd), waxbin.AcquiredFile{Path: args[0], PlaceholderCover: placeholder, Disc: disc, Part: part}, kind, meta)
			if err != nil {
				return err
			}

			// An episode is ingested immediately; a track/book returns a reviewable plan.
			if res.Kind == model.KindEpisode {
				if g.jsonOut {
					return printJSON(cmd, struct {
						Kind    string `json:"kind"`
						Episode string `json:"episode"`
						File    string `json:"file,omitempty"`
						Path    string `json:"path,omitempty"`
					}{"episode", string(res.EpisodePID), string(res.FilePID), res.Path})
				}
				fmt.Fprintf(out(cmd), "Ingested episode %s\n", res.EpisodePID)
				if res.Path != "" {
					fmt.Fprintf(out(cmd), "Attached file: %s\n", res.Path)
				}
				return nil
			}
			if !apply {
				return emitImportPlan(cmd, g, res.Plan)
			}
			rep, err := lib.ApplyImport(ctx(cmd), res.Plan)
			if err != nil {
				return err
			}
			return emitImportReport(cmd, g, args[0], rep)
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "media kind: track|book|episode (required)")
	f.StringVar(&sourceType, "source-type", "", "acquisition source: rss|youtube|manual "+
		"(unset makes no claim: a new row records manual, an existing one keeps its type)")
	f.StringVar(&sourceURL, "source-url", "", "origin URL for provenance; for episodes, the enclosure URL")
	f.StringVar(&sourceID, "source-id", "", "provider-native source id recorded as provenance")
	f.StringVar(&provider, "provider", "", "provider name recorded as provenance")
	f.StringVar(&profile, "profile", "", "organization profile (track/book)")
	f.StringVar(&dup, "dup", "skip",
		"duplicate policy (track/book): skip leaves audio the catalog holds; allow imports it as a copy that joins the item already holding it")
	f.BoolVar(&asCopy, "copy", false, "copy the file instead of moving it")
	f.BoolVar(&apply, "apply", false, "execute a track/book import (default is a review)")
	f.StringVar(&showPID, "show", "", "target show pid for an episode (default: a new manual show)")
	f.StringVar(&showTitle, "show-title", "", "manual show title for a new-show episode")
	f.StringVar(&epTitle, "title", "", "episode title (default the file base name)")
	f.BoolVar(&noPin, "no-pin", false, "do not pin an acquired episode (retention may reclaim it)")
	f.StringVar(&book, "book", "", "the title of the book the file is a part of, whatever its tags say (book)")
	f.StringVar(&author, "author", "", "the author of the book --book names")
	f.StringVar(&bookPID, "book-pid", "", "the standing book the file is a part of (book)")
	f.IntVar(&disc, "disc", 0, "the disc the part is on in its book")
	f.IntVar(&part, "part", 0, "the part's number on its disc (0 = the number its own tags give it)")
	f.IntVar(&parts, "parts", 0, "how many parts the book has")
	f.BoolVar(&placeholder, "placeholder-cover", false,
		"the picture in the file's tags stands in for a cover, as a video thumbnail does (track/book; see `art placeholder`)")
	_ = cmd.MarkFlagRequired("as")
	return cmd
}
