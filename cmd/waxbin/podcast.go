package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/spf13/cobra"
)

func newPodcastCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "podcast", Short: "Subscribe to and manage podcast feeds"}
	cmd.AddCommand(
		newPodcastAddCmd(g),
		newPodcastAddManualCmd(g),
		newPodcastAddEpisodeCmd(g),
		newPodcastListCmd(g),
		newPodcastShowCmd(g),
		newPodcastSyncCmd(g),
		newPodcastDownloadCmd(g),
		newPodcastAuthCmd(g),
		newPodcastRetentionCmd(g),
		newPodcastRemoveCmd(g),
	)
	return cmd
}

func newPodcastAddManualCmd(g *globals) *cobra.Command {
	var author, description, link string
	cmd := &cobra.Command{
		Use:   "add-manual <title>",
		Short: "Create a manual (curated) show with no feed to sync",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pod, err := lib.Podcasts().AddManual(ctx(cmd), args[0], podcast.ManualOptions{
				Author: author, Description: description, Link: link,
			})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toPodcastView(pod))
			}
			fmt.Fprintf(out(cmd), "Created manual show: %s (%s)\n", pod.Title, pod.PID)
			return nil
		},
	}
	cmd.Flags().StringVar(&author, "author", "", "show author")
	cmd.Flags().StringVar(&description, "description", "", "show description")
	cmd.Flags().StringVar(&link, "link", "", "show website")
	return cmd
}

func newPodcastAddEpisodeCmd(g *globals) *cobra.Command {
	var title, url string
	var noPin bool
	cmd := &cobra.Command{
		Use:   "add-episode <show-pid>",
		Short: "Add a single episode to a show (pinned by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			res, err := lib.Podcasts().AddEpisode(ctx(cmd), model.PID(args[0]),
				model.FeedEpisode{Title: title, EnclosureURL: url}, !noPin)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Episode string `json:"episode"`
					Created bool   `json:"created"`
				}{string(res.EpisodePID), res.Created})
			}
			verb := "Updated"
			if res.Created {
				verb = "Added"
			}
			fmt.Fprintf(out(cmd), "%s episode %s\n", verb, res.EpisodePID)
			return nil
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "episode title")
	cmd.Flags().StringVar(&url, "url", "", "episode enclosure (media) URL to download later")
	cmd.Flags().BoolVar(&noPin, "no-pin", false, "do not pin the episode (retention may reclaim it)")
	return cmd
}

func newPodcastAddCmd(g *globals) *cobra.Command {
	var user, pass string
	cmd := &cobra.Command{
		Use:   "add <feed-url>",
		Short: "Subscribe to a podcast feed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pod, err := lib.Podcasts().Add(ctx(cmd), args[0], podcast.AddOptions{User: user, Pass: pass})
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toPodcastView(pod))
			}
			fmt.Fprintf(out(cmd), "Subscribed: %s (%s), %d episodes\n", pod.Title, pod.PID, pod.EpisodeCount)
			return nil
		},
	}
	cmd.Flags().StringVar(&user, "user", "", "basic-auth username for a private feed")
	cmd.Flags().StringVar(&pass, "pass", "", "basic-auth password for a private feed")
	return cmd
}

func newPodcastListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List subscribed podcasts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pods, err := lib.Podcasts().List(ctx(cmd))
			if err != nil {
				return err
			}
			if g.jsonOut {
				views := make([]podcastView, len(pods))
				for i, p := range pods {
					views[i] = toPodcastView(p)
				}
				return printJSON(cmd, views)
			}
			tw := tabwriter.NewWriter(out(cmd), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "PID\tTITLE\tSOURCE\tEPISODES\tDOWNLOADED\tKEEP")
			for _, p := range pods {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
					p.PID, p.Title, sourceLabel(p.SourceType), p.EpisodeCount, p.DownloadedCount, keepLabel(p.RetentionKeep))
			}
			return tw.Flush()
		},
	}
}

func newPodcastShowCmd(g *globals) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "show <pid>",
		Short: "Show a podcast and its recent episodes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid := model.PID(args[0])
			pod, err := lib.Podcasts().Get(ctx(cmd), pid)
			if err != nil {
				return err
			}
			eps, err := lib.Podcasts().Episodes(ctx(cmd), pid, limit)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Podcast  podcastView   `json:"podcast"`
					Episodes []episodeView `json:"episodes"`
				}{toPodcastView(pod), toEpisodeViews(eps)})
			}
			w := out(cmd)
			fmt.Fprintf(w, "pid:        %s\n", pod.PID)
			fmt.Fprintf(w, "title:      %s\n", pod.Title)
			if pod.Author != "" {
				fmt.Fprintf(w, "author:     %s\n", pod.Author)
			}
			fmt.Fprintf(w, "source:     %s\n", sourceLabel(pod.SourceType))
			fmt.Fprintf(w, "feed:       %s\n", pod.FeedURL)
			if pod.Link != "" {
				fmt.Fprintf(w, "website:    %s\n", pod.Link)
			}
			fmt.Fprintf(w, "episodes:   %d (%d downloaded)\n", pod.EpisodeCount, pod.DownloadedCount)
			fmt.Fprintf(w, "retention:  %s\n", keepLabel(pod.RetentionKeep))
			if pod.AuthUser != "" {
				fmt.Fprintf(w, "auth user:  %s\n", pod.AuthUser)
			}
			if pod.LastFetchedAt != 0 {
				fmt.Fprintf(w, "last sync:  %s\n", time.Unix(0, pod.LastFetchedAt).Format(time.RFC3339))
			}
			fmt.Fprintln(w, "episodes:")
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "  PID\tPUBLISHED\tDUR\tSTATE\tTITLE")
			for _, e := range eps {
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
					e.PID, pubDateLabel(e.PubDateNS), durationLabel(e.DurationMS), episodeStateLabel(e), e.Title)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "max episodes to list (0 = all)")
	return cmd
}

func newPodcastSyncCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "sync [pid]",
		Short: "Re-fetch one feed (or all) and add new episodes",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if len(args) == 1 {
				res, err := lib.Podcasts().Sync(ctx(cmd), model.PID(args[0]))
				if err != nil {
					return err
				}
				if g.jsonOut {
					return printJSON(cmd, res)
				}
				fmt.Fprintf(out(cmd), "Synced: %d new, %d updated\n", res.EpisodesAdded, res.EpisodesUpdated)
				return nil
			}
			results, err := lib.Podcasts().SyncAll(ctx(cmd))
			if err != nil {
				return err
			}
			added, updated := 0, 0
			for _, r := range results {
				added += r.EpisodesAdded
				updated += r.EpisodesUpdated
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Feeds   int `json:"feeds"`
					Added   int `json:"added"`
					Updated int `json:"updated"`
				}{len(results), added, updated})
			}
			fmt.Fprintf(out(cmd), "Synced %d feeds: %d new, %d updated\n", len(results), added, updated)
			return nil
		},
	}
}

func newPodcastDownloadCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "download <episode-pid>",
		Short: "Download an episode's audio (and transcript when available)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			res, err := lib.Podcasts().Download(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, struct {
					Episode    model.PID `json:"episode"`
					File       model.PID `json:"file"`
					Path       string    `json:"path"`
					Bytes      int64     `json:"bytes"`
					Transcript bool      `json:"transcript"`
				}{res.EpisodePID, res.FilePID, res.Path, res.Bytes, res.Transcript})
			}
			fmt.Fprintf(out(cmd), "Downloaded %d bytes to %s\n", res.Bytes, res.Path)
			if res.Transcript {
				fmt.Fprintln(out(cmd), "Transcript stored.")
			}
			return nil
		},
	}
}

func newPodcastAuthCmd(g *globals) *cobra.Command {
	var pass string
	cmd := &cobra.Command{
		Use:   "auth <pid> <user>",
		Short: "Set basic-auth credentials for a private feed",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Podcasts().SetAuth(ctx(cmd), model.PID(args[0]), args[1], pass); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "Credentials saved.")
			return nil
		},
	}
	cmd.Flags().StringVar(&pass, "pass", "", "basic-auth password")
	return cmd
}

func newPodcastRetentionCmd(g *globals) *cobra.Command {
	var keep int
	var apply bool
	cmd := &cobra.Command{
		Use:   "retention <pid>",
		Short: "Set or apply a podcast's keep-newest-N retention policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			pid := model.PID(args[0])
			if cmd.Flags().Changed("keep") {
				if err := lib.Podcasts().SetRetention(ctx(cmd), pid, keep); err != nil {
					return err
				}
				fmt.Fprintf(out(cmd), "Retention set: %s\n", keepLabel(keep))
			}
			if apply {
				res, err := lib.Podcasts().ApplyRetention(ctx(cmd), pid)
				if err != nil {
					return err
				}
				if g.jsonOut {
					return printJSON(cmd, res)
				}
				fmt.Fprintf(out(cmd), "Removed %d episode files, reclaimed %d bytes\n", res.Removed, res.ReclaimedBytes)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&keep, "keep", 0, "keep the newest N downloaded episodes (0 = keep all)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the policy now, deleting older downloaded files")
	return cmd
}

func newPodcastRemoveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <pid>",
		Short: "Unsubscribe and delete a podcast's episodes and downloads",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			if err := lib.Podcasts().Remove(ctx(cmd), model.PID(args[0])); err != nil {
				return err
			}
			fmt.Fprintln(out(cmd), "Unsubscribed.")
			return nil
		},
	}
}

// --- JSON views ------------------------------------------------------------

type podcastView struct {
	PID         model.PID `json:"pid"`
	Title       string    `json:"title"`
	Author      string    `json:"author,omitempty"`
	FeedURL     string    `json:"feedUrl"`
	Source      string    `json:"source"`
	Description string    `json:"description,omitempty"`
	Link        string    `json:"link,omitempty"`
	Language    string    `json:"language,omitempty"`
	Category    string    `json:"category,omitempty"`
	Explicit    bool      `json:"explicit,omitempty"`
	Episodes    int       `json:"episodes"`
	Downloaded  int       `json:"downloaded"`
	Keep        int       `json:"retentionKeep"`
	AuthUser    string    `json:"authUser,omitempty"`
}

func toPodcastView(p *model.Podcast) podcastView {
	return podcastView{
		PID: p.PID, Title: p.Title, Author: p.Author, FeedURL: p.FeedURL, Source: sourceLabel(p.SourceType),
		Description: p.Description, Link: p.Link, Language: p.Language, Category: p.Category,
		Explicit: p.Explicit, Episodes: p.EpisodeCount, Downloaded: p.DownloadedCount,
		Keep: p.RetentionKeep, AuthUser: p.AuthUser,
	}
}

// sourceLabel renders a show's source type, defaulting an unset value to rss for
// catalogs written before source_type was stored.
func sourceLabel(st model.SourceType) string {
	if st == "" {
		return string(model.SourceRSS)
	}
	return string(st)
}

type episodeView struct {
	PID          model.PID `json:"pid"`
	Title        string    `json:"title"`
	Podcast      string    `json:"podcast,omitempty"`
	GUID         string    `json:"guid,omitempty"`
	Published    string    `json:"published,omitempty"`
	Season       int       `json:"season,omitempty"`
	EpisodeNo    int       `json:"episode,omitempty"`
	Type         string    `json:"type,omitempty"`
	DurationMS   int64     `json:"durationMs,omitempty"`
	State        string    `json:"state"`
	Downloaded   bool      `json:"downloaded"`
	EnclosureURL string    `json:"enclosureUrl,omitempty"`
}

func toEpisodeView(e *model.Episode) episodeView {
	return episodeView{
		PID: e.PID, Title: e.Title, Podcast: e.PodcastTitle, GUID: e.GUID,
		Published: pubDateLabel(e.PubDateNS), Season: e.Season, EpisodeNo: e.EpisodeNo,
		Type: string(e.EpisodeType), DurationMS: e.DurationMS, State: string(e.State),
		Downloaded: e.Downloaded, EnclosureURL: e.EnclosureURL,
	}
}

func toEpisodeViews(eps []*model.Episode) []episodeView {
	out := make([]episodeView, len(eps))
	for i, e := range eps {
		out[i] = toEpisodeView(e)
	}
	return out
}

func keepLabel(keep int) string {
	if keep <= 0 {
		return "all"
	}
	return fmt.Sprintf("newest %d", keep)
}

func pubDateLabel(ns int64) string {
	if ns == 0 {
		return "-"
	}
	return time.Unix(0, ns).UTC().Format("2006-01-02")
}

func episodeStateLabel(e *model.Episode) string {
	if e.Downloaded {
		return "downloaded"
	}
	return string(e.State)
}
