package main

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/podcast"
	"github.com/colespringer/waxbin/waxerr"
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
		newPodcastEpisodeCmd(g),
		newPodcastSyncCmd(g),
		newPodcastDownloadCmd(g),
		newPodcastTranscriptCmd(g),
		newPodcastAuthCmd(g),
		newPodcastRetentionCmd(g),
		newPodcastRemoveCmd(g),
	)
	return cmd
}

func newPodcastTranscriptCmd(g *globals) *cobra.Command {
	var (
		fetch     bool
		filePath  string
		format    string
		sourceURL string
	)
	cmd := &cobra.Command{
		Use:   "transcript <episode-pid> [--fetch | --file f --format vtt [--source-url u]]",
		Short: "Show, fetch, or store an episode's transcript",
		Long: "Without flags, prints the stored transcript. --fetch downloads the transcript the " +
			"feed declared and stores it (errors are reported, unlike the best-effort fetch during " +
			"download). --file stores a local transcript file; --format names its format " +
			"(srt|vtt|json|text) and --source-url records where it came from. The stored body is " +
			"the reduced searchable text, not the original document.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid := model.PID(args[0])
			// Validate the flag shape before opening anything: --format and
			// --source-url describe a --file body and mean nothing elsewhere, so
			// rejecting them beats silently ignoring them.
			if fetch && filePath != "" {
				return waxerr.New(waxerr.CodeInvalid, "podcast transcript", "--fetch and --file are exclusive")
			}
			if filePath == "" && (format != "" || sourceURL != "") {
				return waxerr.New(waxerr.CodeInvalid, "podcast transcript", "--format and --source-url only apply with --file")
			}
			if filePath != "" && format == "" {
				return waxerr.New(waxerr.CodeInvalid, "podcast transcript", "--file needs --format (srt|vtt|json|text)")
			}
			if fetch || filePath != "" {
				m, _, err := g.openMutator(cmd)
				if err != nil {
					return err
				}
				defer m.Close()
				if fetch {
					if err := m.FetchTranscript(ctx(cmd), pid); err != nil {
						return err
					}
					fmt.Fprintln(out(cmd), "Transcript fetched and stored.")
					return nil
				}
				body, err := os.ReadFile(filePath)
				if err != nil {
					return waxerr.Wrapf(waxerr.CodeIO, "podcast transcript", err, "reading %s", filePath)
				}
				if err := m.PutTranscript(ctx(cmd), model.PutTranscriptInput{
					EpisodePID: pid, Format: format, Body: string(body), SourceURL: sourceURL,
				}); err != nil {
					return err
				}
				fmt.Fprintln(out(cmd), "Transcript stored.")
				return nil
			}
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			tr, err := lib.Podcasts().Transcript(ctx(cmd), pid)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, transcriptView{
					EpisodePID: string(tr.EpisodePID), Format: tr.Format,
					SourceURL: tr.SourceURL, CreatedAt: tr.CreatedAt, Body: tr.Body,
				})
			}
			w := out(cmd)
			fmt.Fprintf(w, "format:  %s\n", tr.Format)
			if tr.SourceURL != "" {
				fmt.Fprintf(w, "source:  %s\n", tr.SourceURL)
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w, tr.Body)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&fetch, "fetch", false, "fetch the feed-declared transcript and store it")
	f.StringVar(&filePath, "file", "", "local transcript file to store")
	f.StringVar(&format, "format", "", "format of --file: srt|vtt|json|text")
	f.StringVar(&sourceURL, "source-url", "", "provenance URL recorded with --file")
	return cmd
}

// transcriptView is the JSON shape for a stored transcript.
type transcriptView struct {
	EpisodePID string `json:"episodePid"`
	Format     string `json:"format"`
	SourceURL  string `json:"sourceUrl,omitempty"`
	// Unix ns as a decimal string (",string"), like every ns timestamp in the CLI
	// JSON contract.
	CreatedAt int64  `json:"createdAt,string"`
	Body      string `json:"body"`
}

func newPodcastEpisodeCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "episode <episode-pid>",
		Short: "Show one episode: credits, soundbites, chapters, transcript state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lib, _, err := g.openRead(cmd)
			if err != nil {
				return err
			}
			defer lib.Close()
			d, err := lib.Podcasts().Episode(ctx(cmd), model.PID(args[0]))
			if err != nil {
				return err
			}
			if g.jsonOut {
				return printJSON(cmd, toEpisodeDetailView(d))
			}
			w := out(cmd)
			e := d.Episode
			fmt.Fprintf(w, "pid:        %s\n", e.PID)
			fmt.Fprintf(w, "title:      %s\n", e.Title)
			fmt.Fprintf(w, "podcast:    %s (%s)\n", e.PodcastTitle, e.PodcastPID)
			fmt.Fprintf(w, "published:  %s\n", pubDateLabel(e.PubDateNS))
			fmt.Fprintf(w, "duration:   %s\n", durationLabel(e.DurationMS))
			fmt.Fprintf(w, "state:      %s\n", episodeStateLabel(e))
			if len(d.Persons) > 0 {
				labels := make([]string, len(d.Persons))
				for i, p := range d.Persons {
					labels[i] = personLabel(p)
				}
				fmt.Fprintf(w, "people:     %s\n", strings.Join(labels, ", "))
			}
			fmt.Fprintf(w, "transcript: %s\n", yesNo(d.HasTranscript))
			if len(d.Soundbites) > 0 {
				fmt.Fprintln(w, "soundbites:")
				for _, b := range d.Soundbites {
					title := b.Title
					if title == "" {
						title = e.Title
					}
					fmt.Fprintf(w, "  %s +%s  %s\n", durationLabel(b.StartMS), durationLabel(b.DurationMS), title)
				}
			}
			printChapters(cmd, d.Chapters)
			return nil
		},
	}
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
			if pod.Medium != "" {
				fmt.Fprintf(w, "medium:     %s\n", pod.Medium)
			}
			if pod.FundingURL != "" {
				msg := ""
				if pod.FundingMessage != "" {
					msg = " (" + pod.FundingMessage + ")"
				}
				fmt.Fprintf(w, "funding:    %s%s\n", pod.FundingURL, msg)
			}
			if len(pod.Persons) > 0 {
				labels := make([]string, len(pod.Persons))
				for i, p := range pod.Persons {
					labels[i] = personLabel(p)
				}
				fmt.Fprintf(w, "people:     %s\n", strings.Join(labels, ", "))
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
	PID            model.PID    `json:"pid"`
	Title          string       `json:"title"`
	Author         string       `json:"author,omitempty"`
	FeedURL        string       `json:"feedUrl"`
	Source         string       `json:"source"`
	Description    string       `json:"description,omitempty"`
	Link           string       `json:"link,omitempty"`
	Language       string       `json:"language,omitempty"`
	Category       string       `json:"category,omitempty"`
	Explicit       bool         `json:"explicit,omitempty"`
	FundingURL     string       `json:"fundingUrl,omitempty"`
	FundingMessage string       `json:"fundingMessage,omitempty"`
	Medium         string       `json:"medium,omitempty"`
	Persons        []personView `json:"persons,omitempty"`
	Episodes       int          `json:"episodes"`
	Downloaded     int          `json:"downloaded"`
	Keep           int          `json:"retentionKeep"`
	AuthUser       string       `json:"authUser,omitempty"`
}

func toPodcastView(p *model.Podcast) podcastView {
	return podcastView{
		PID: p.PID, Title: p.Title, Author: p.Author, FeedURL: p.FeedURL, Source: sourceLabel(p.SourceType),
		Description: p.Description, Link: p.Link, Language: p.Language, Category: p.Category,
		Explicit: p.Explicit, FundingURL: p.FundingURL, FundingMessage: p.FundingMessage,
		Medium: p.Medium, Persons: personViews(p.Persons),
		Episodes: p.EpisodeCount, Downloaded: p.DownloadedCount,
		Keep: p.RetentionKeep, AuthUser: p.AuthUser,
	}
}

// personView is the JSON shape for one <podcast:person> credit.
type personView struct {
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Group string `json:"group,omitempty"`
	Img   string `json:"img,omitempty"`
	Href  string `json:"href,omitempty"`
}

func personViews(ps []model.FeedPerson) []personView {
	if len(ps) == 0 {
		return nil
	}
	out := make([]personView, len(ps))
	for i, p := range ps {
		out[i] = personView{Name: p.Name, Role: p.Role, Group: p.Group, Img: p.Img, Href: p.Href}
	}
	return out
}

// personLabel renders one credit for the human listing ("Jane Host (host)").
func personLabel(p model.FeedPerson) string {
	if p.Role == "" {
		return p.Name
	}
	return p.Name + " (" + p.Role + ")"
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

// episodeDetailView is the JSON shape for one episode's full detail: the list
// fields plus the Podcasting 2.0 extras, chapters, and transcript state.
type episodeDetailView struct {
	episodeView
	HasTranscript bool            `json:"hasTranscript"`
	Persons       []personView    `json:"persons,omitempty"`
	Soundbites    []soundbiteView `json:"soundbites,omitempty"`
	Chapters      []chapterView   `json:"chapters,omitempty"`
}

type soundbiteView struct {
	StartMS    int64  `json:"startMs"`
	DurationMS int64  `json:"durationMs"`
	Title      string `json:"title,omitempty"`
}

func toEpisodeDetailView(d *model.EpisodeDetail) episodeDetailView {
	v := episodeDetailView{
		episodeView:   toEpisodeView(d.Episode),
		HasTranscript: d.HasTranscript,
		Persons:       personViews(d.Persons),
		Chapters:      chapterViews(d.Chapters),
	}
	for _, b := range d.Soundbites {
		v.Soundbites = append(v.Soundbites, soundbiteView{StartMS: b.StartMS, DurationMS: b.DurationMS, Title: b.Title})
	}
	return v
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
