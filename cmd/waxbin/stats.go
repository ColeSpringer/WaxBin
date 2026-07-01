package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin/read"
	"github.com/spf13/cobra"
)

func newStatsCmd(g *globals) *cobra.Command {
	var user string
	var topN int
	var year int
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Summarize the library (totals, top genres/artists, play stats)",
		Long: "Summarizes the library totals, top genres/artists, and per-user play stats. " +
			"With --year N, prints a listening year-in-review instead: minutes played, top " +
			"artists/genres/tracks that calendar year (UTC), and catalog additions.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			uPID, err := resolveUser(cmd, lib, user)
			if err != nil {
				return err
			}
			if year > 0 {
				yr, err := lib.YearInReview(ctx(cmd), uPID, year, topN)
				if err != nil {
					return err
				}
				if g.jsonOut {
					return printJSON(cmd, toYearReviewView(yr))
				}
				return printYearReview(cmd, yr)
			}
			st, err := lib.Stats(ctx(cmd), uPID, topN)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toStatsView(st))
			}
			return printStats(cmd, st)
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "user name for play stats (default user when omitted)")
	cmd.Flags().IntVar(&topN, "top", 10, "size of the top-genres/artists/most-played lists")
	cmd.Flags().IntVar(&year, "year", 0, "print a listening year-in-review for this calendar year")
	return cmd
}

func printYearReview(cmd *cobra.Command, yr *read.YearReview) error {
	w := out(cmd)
	fmt.Fprintf(w, "%d in review (%s):\n", yr.Year, yr.User)
	fmt.Fprintf(w, "  minutes played: %d\n", yr.MinutesPlayed)
	fmt.Fprintf(w, "  sessions:       %d\n", yr.Sessions)
	fmt.Fprintf(w, "  tracks played:  %d\n", yr.TracksPlayed)
	fmt.Fprintf(w, "  added to library: %d\n", yr.NewInLibrary)
	printBuckets(w, "top artists", yr.TopArtists)
	printBuckets(w, "top genres", yr.TopGenres)
	if len(yr.TopTracks) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "\ntop tracks:\tPLAYS\tTITLE\tARTIST")
		for _, p := range yr.TopTracks {
			fmt.Fprintf(tw, "\t%d\t%s\t%s\n", p.PlayCount, p.Title, p.Artist)
		}
		_ = tw.Flush()
	}
	return nil
}

func printStats(cmd *cobra.Command, st *read.Stats) error {
	w := out(cmd)
	fmt.Fprintf(w, "items:          %d\n", st.Items)
	fmt.Fprintf(w, "books:          %d\n", st.Books)
	fmt.Fprintf(w, "artists:        %d\n", st.Artists)
	fmt.Fprintf(w, "release groups: %d\n", st.ReleaseGroups)
	fmt.Fprintf(w, "albums:         %d\n", st.Albums)
	fmt.Fprintf(w, "genres:         %d\n", st.Genres)
	fmt.Fprintf(w, "total duration: %s\n", durationLabel(st.TotalDuration))

	printBuckets(w, "top genres", st.TopGenres)
	printBuckets(w, "top artists", st.TopArtists)

	fmt.Fprintf(w, "\nplay stats (%s):\n", st.Play.User)
	fmt.Fprintf(w, "  total plays:  %d\n", st.Play.TotalPlays)
	fmt.Fprintf(w, "  finished:     %d\n", st.Play.Finished)
	fmt.Fprintf(w, "  starred:      %d\n", st.Play.Starred)
	if len(st.Play.MostPlayed) > 0 {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  most played:\tPLAYS\tTITLE\tARTIST")
		for _, p := range st.Play.MostPlayed {
			fmt.Fprintf(tw, "\t%d\t%s\t%s\n", p.PlayCount, p.Title, p.Artist)
		}
		_ = tw.Flush()
	}
	return nil
}

func printBuckets(w interface{ Write([]byte) (int, error) }, title string, buckets []read.Bucket) {
	if len(buckets) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, b := range buckets {
		fmt.Fprintf(w, "  %5d  %s\n", b.Count, b.Display)
	}
}

// durationLabel renders milliseconds as H:MM:SS for the totals line.
func durationLabel(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	return fmt.Sprintf("%d:%02d:%02d", h, m, s)
}
