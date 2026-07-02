package podcast

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/meta"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

// Allowed response media types for the auxiliary fetches the service still performs
// through netsafe (feed enumeration and enclosure download moved to the source
// providers). Transcripts are text/data; artwork is an image.
var (
	transcriptMIME = []string{"text/*", "application/json", "application/x-subrip", "application/srt", "application/octet-stream"}
	imageMIME      = []string{"image/*", "application/octet-stream"}
	chaptersMIME   = []string{"application/json+chapters", "application/json", "text/json", "application/octet-stream"}
)

// Store is the persistence the podcast service needs (satisfied by store/sqlite).
type Store interface {
	EnsurePodcastLibrary(ctx context.Context, dir string) (int64, error)
	UpsertFeed(ctx context.Context, in model.UpsertFeedInput) (*model.UpsertFeedResult, error)
	UpsertShow(ctx context.Context, in model.UpsertShowInput) (model.PID, bool, error)
	UpsertEpisode(ctx context.Context, in model.UpsertEpisodeInput) (*model.UpsertEpisodeResult, error)
	Podcasts(ctx context.Context) ([]*model.Podcast, error)
	PodcastByPID(ctx context.Context, pid model.PID) (*model.Podcast, error)
	PodcastByIdentity(ctx context.Context, key string) (*model.Podcast, error)
	EpisodesByPodcast(ctx context.Context, pid model.PID, limit int) ([]*model.Episode, error)
	EpisodeByPID(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error)
	DownloadedEpisodes(ctx context.Context, pid model.PID) ([]*model.Episode, error)
	AttachEpisodeFile(ctx context.Context, in model.AttachEpisodeFileInput) (model.PID, error)
	DropEpisodeFile(ctx context.Context, pid model.PID) error
	PutTranscript(ctx context.Context, in model.PutTranscriptInput) error
	PutEpisodeChapters(ctx context.Context, episodePID model.PID, chapters []model.Chapter) error
	RemovePodcast(ctx context.Context, pid model.PID) ([]string, error)
	SetPodcastRetention(ctx context.Context, pid model.PID, keep int) error
	SetPodcastAuthUser(ctx context.Context, pid model.PID, user string) error
	SetSecret(ctx context.Context, key, value string) error
	GetSecret(ctx context.Context, key string) (string, error)
	DeleteSecret(ctx context.Context, key string) error
}

// Config tunes the podcast service: where downloads land, the network policy
// netsafe enforces, and free-space/retention defaults.
type Config struct {
	Dir               string        // download directory (downloads require it)
	UserAgent         string        // HTTP User-Agent for feed/enclosure fetches
	BlockPrivateIPs   bool          // SSRF guard (off by default; on for untrusted feeds)
	Timeout           time.Duration // per-request timeout
	MaxFeedBytes      int64         // cap on a feed/transcript/OPML body
	MaxEnclosureBytes int64         // cap on an episode download
	ReserveBytes      int64         // free-space headroom kept on the download volume
	DefaultRetention  int           // keep-N applied to a new subscription (0 = keep all)
	// Providers are injected acquisition providers, such as a youtube provider from
	// another module. The built-in netsafe rss provider is always registered; an
	// injected provider registers under its own SourceType.
	Providers []source.Provider
}

const (
	defaultUserAgent     = "WaxBin/1.0 (+https://github.com/colespringer/waxbin)"
	defaultMaxFeedBytes  = 16 << 20 // 16 MiB: very large for a feed/transcript
	defaultMaxEnclosure  = 2 << 30  // 2 GiB: a long episode, still bounded
	episodeImageMaxBytes = 16 << 20 // 16 MiB cap on episode/feed artwork
)

// Service subscribes to feeds, syncs episodes, downloads enclosures, stores
// transcripts/artwork, and applies retention. It is safe for concurrent use.
type Service struct {
	store     Store
	client    *netsafe.Client
	reader    meta.Reader
	cfg       Config
	log       *slog.Logger
	providers map[model.SourceType]source.Provider

	libMu sync.Mutex
	libID int64 // cached internal podcast-library id (0 = unresolved)
}

// podcastLibrary returns the internal podcast-library id, resolving and caching it
// on first use so repeated downloads do not each take a write transaction just to
// confirm the library exists (cfg.Dir is fixed for the service's lifetime).
func (s *Service) podcastLibrary(ctx context.Context) (int64, error) {
	s.libMu.Lock()
	defer s.libMu.Unlock()
	if s.libID != 0 {
		return s.libID, nil
	}
	id, err := s.store.EnsurePodcastLibrary(ctx, s.cfg.Dir)
	if err != nil {
		return 0, err
	}
	s.libID = id
	return id, nil
}

// New builds a podcast service. The netsafe client is constructed from cfg's
// network policy.
func New(store Store, reader meta.Reader, cfg Config, log *slog.Logger) *Service {
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if cfg.MaxFeedBytes <= 0 {
		cfg.MaxFeedBytes = defaultMaxFeedBytes
	}
	if cfg.MaxEnclosureBytes <= 0 {
		cfg.MaxEnclosureBytes = defaultMaxEnclosure
	}
	client := netsafe.New(netsafe.Policy{
		UserAgent:       cfg.UserAgent,
		Timeout:         cfg.Timeout,
		MaxBytes:        cfg.MaxFeedBytes,
		BlockPrivateIPs: cfg.BlockPrivateIPs,
	})
	// The built-in netsafe HTTP provider serves rss and, as the fetch fallback, any
	// plain enclosure (a manual episode's direct URL). Injected providers register
	// under their own source type; a later duplicate (same type) overrides an
	// earlier registration, so an embedder can replace the built-in if needed.
	providers := map[model.SourceType]source.Provider{
		model.SourceRSS: source.NewHTTP(client, ParseFeed),
	}
	for _, p := range cfg.Providers {
		if p != nil {
			providers[p.SourceType()] = p
		}
	}
	return &Service{store: store, client: client, reader: reader, cfg: cfg, log: log, providers: providers}
}

// providerFor returns the provider that syncs a show of source type st, defaulting
// empty to rss. It returns CodeUnsupported when none is registered, such as a youtube
// show in a build without a youtube provider.
func (s *Service) providerFor(st model.SourceType) (source.Provider, error) {
	if st == "" {
		st = model.SourceRSS
	}
	p, ok := s.providers[st]
	if !ok {
		return nil, waxerr.New(waxerr.CodeUnsupported, "podcast",
			"no acquisition provider registered for source type "+string(st))
	}
	return p, nil
}

// fetchProvider returns the provider that downloads a show's episodes. A manual or
// unspecified show uses plain enclosure URLs, so it falls back to the built-in HTTP
// provider. Any other unregistered source, such as youtube without a provider,
// returns CodeUnsupported rather than handing a platform URL to HTTP.
func (s *Service) fetchProvider(st model.SourceType) (source.Provider, error) {
	if p, ok := s.providers[st]; ok {
		return p, nil
	}
	if st == model.SourceManual || st == "" {
		return s.providers[model.SourceRSS], nil
	}
	return nil, waxerr.New(waxerr.CodeUnsupported, "podcast",
		"no acquisition provider registered for source type "+string(st))
}

// AddOptions carries optional basic-auth credentials for a private feed, applied to
// the initial fetch and stored for later syncs.
type AddOptions struct {
	User string
	Pass string
}

// Add subscribes to an RSS feed. It is AddSource with the rss source type.
func (s *Service) Add(ctx context.Context, feedURL string, opts AddOptions) (*model.Podcast, error) {
	return s.AddSource(ctx, feedURL, model.SourceRSS, opts)
}

// AddSource subscribes to a feed/channel served by the given source type: it
// enumerates it through the matching provider, derives the stable identity key,
// ingests the image, and upserts the show plus its episodes. A new subscription gets
// the configured default retention. Re-adding an existing source is a sync. A
// youtube source needs an injected provider; without it the call returns
// CodeUnsupported.
func (s *Service) AddSource(ctx context.Context, url string, sourceType model.SourceType, opts AddOptions) (*model.Podcast, error) {
	const op = "podcast.AddSource"
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "empty feed url")
	}
	prov, err := s.providerFor(sourceType)
	if err != nil {
		return nil, err
	}
	enum, err := prov.Enumerate(ctx, source.Request{URL: url, User: opts.User, Pass: opts.Pass})
	if err != nil {
		return nil, err
	}
	// Add sends no conditional headers, so a NotModified here is a misbehaving
	// server/CDN and leaves the feed nil; refuse rather than dereference it.
	if enum.NotModified || enum.Feed == nil {
		return nil, waxerr.New(waxerr.CodeIO, op, "feed returned not-modified to an unconditional request")
	}
	key := enum.IdentityKey
	if key == "" {
		key = identity.PodcastKey(enum.Feed.GUID, url)
	}
	if key == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "feed has no usable identity (url or guid)")
	}
	res, err := s.upsert(ctx, url, key, prov.SourceType(), enum, "")
	if err != nil {
		return nil, err
	}
	if opts.User != "" || opts.Pass != "" {
		if err := s.setAuth(ctx, res.PodcastPID, opts.User, opts.Pass); err != nil {
			return nil, err
		}
	}
	if res.Created && s.cfg.DefaultRetention > 0 {
		if err := s.store.SetPodcastRetention(ctx, res.PodcastPID, s.cfg.DefaultRetention); err != nil {
			return nil, err
		}
	}
	return s.store.PodcastByPID(ctx, res.PodcastPID)
}

// ManualOptions carries the optional metadata of a manually created show.
type ManualOptions struct {
	Author      string
	Description string
	Link        string
}

// AddManual creates a user-curated show with no feed to sync. Episodes are added
// with AddEpisode and can be pinned so retention leaves them alone. Its identity is
// a synthetic manual:<ulid>, so two manual shows with the same title never collide.
func (s *Service) AddManual(ctx context.Context, title string, opts ManualOptions) (*model.Podcast, error) {
	const op = "podcast.AddManual"
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "manual show needs a title")
	}
	// A manual show has no feed URL, but podcast.feed_url is NOT NULL UNIQUE; use the
	// synthetic identity key as the feed_url too so two manual shows never collide on
	// an empty string. Sync short-circuits a manual show, so it is never dereferenced.
	key := "manual:" + string(model.NewPID())
	pid, _, err := s.store.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: key, FeedURL: key, SourceType: model.SourceManual,
		Title: title, Author: opts.Author, Description: opts.Description, Link: opts.Link,
	})
	if err != nil {
		return nil, err
	}
	return s.store.PodcastByPID(ctx, pid)
}

// AddEpisode adds or updates a single episode under an existing show, bypassing feed
// sync. Pinned keeps the episode out of retention pruning. It returns the episode pid
// and whether it was newly created.
func (s *Service) AddEpisode(ctx context.Context, showPID model.PID, ep model.FeedEpisode, pinned bool) (*model.UpsertEpisodeResult, error) {
	if strings.TrimSpace(ep.Title) == "" && strings.TrimSpace(ep.GUID) == "" && strings.TrimSpace(ep.EnclosureURL) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, "podcast.AddEpisode", "episode needs a title, guid, or enclosure url")
	}
	return s.store.UpsertEpisode(ctx, model.UpsertEpisodeInput{PodcastPID: showPID, Episode: ep, Pinned: pinned})
}

// Sync enumerates one show conditionally (ETag/Last-Modified) through its
// provider: a NotModified only reports no change, otherwise new/updated episodes are
// upserted. It never deletes episodes the source stopped listing. A manual show has
// nothing to sync; a youtube show needs an injected provider. rss and youtube shows
// use the same sync path.
func (s *Service) Sync(ctx context.Context, podcastPID model.PID) (*model.UpsertFeedResult, error) {
	pod, err := s.store.PodcastByPID(ctx, podcastPID)
	if err != nil {
		return nil, err
	}
	st := pod.SourceType
	if st == "" {
		st = model.SourceRSS
	}
	if st == model.SourceManual {
		// A manual show is curated episode by episode; there is no feed to enumerate.
		return &model.UpsertFeedResult{PodcastPID: podcastPID}, nil
	}
	prov, err := s.providerFor(st)
	if err != nil {
		return nil, err
	}
	user, pass := s.authFor(ctx, pod)
	enum, err := prov.Enumerate(ctx, source.Request{
		URL: pod.FeedURL, User: user, Pass: pass, ETag: pod.ETag, LastModified: pod.LastModified,
	})
	if err != nil {
		return nil, err
	}
	if enum.NotModified || enum.Feed == nil {
		// Nothing changed; refreshing the validators/fetch time would cost a re-parse,
		// so just report no change.
		return &model.UpsertFeedResult{PodcastPID: podcastPID}, nil
	}
	// Pass the stored image URL so an unchanged cover is not re-fetched/re-decoded.
	return s.upsert(ctx, pod.FeedURL, pod.IdentityKey, st, enum, pod.ImageURL)
}

// SyncAll syncs every subscribed podcast, returning the per-podcast results. A
// failed feed is logged and skipped so one dead feed does not abort the batch.
func (s *Service) SyncAll(ctx context.Context) (map[model.PID]*model.UpsertFeedResult, error) {
	pods, err := s.store.Podcasts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[model.PID]*model.UpsertFeedResult, len(pods))
	for _, p := range pods {
		if ctx.Err() != nil {
			return out, waxerr.FromContext("podcast.SyncAll", ctx.Err(), waxerr.CodeCanceled)
		}
		res, err := s.Sync(ctx, p.PID)
		if err != nil {
			s.log.Warn("podcast sync failed", "podcast", p.Title, "err", err)
			continue
		}
		out[p.PID] = res
	}
	return out, nil
}

// upsert ingests the show image and persists an enumeration under a source type.
// priorImageURL is the image URL already stored for this show (empty on first
// subscribe); the cover is re-fetched only when it differs, avoiding a multi-MiB
// download + decode on every sync of a source that does not answer NotModified.
func (s *Service) upsert(ctx context.Context, feedURL, key string, st model.SourceType, enum *source.Enumeration, priorImageURL string) (*model.UpsertFeedResult, error) {
	var img *model.ArtImage
	if enum.Feed.ImageURL != "" && enum.Feed.ImageURL != priorImageURL {
		img = s.fetchImage(ctx, enum.Feed.ImageURL)
	}
	return s.store.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:      feedURL,
		IdentityKey:  key,
		SourceType:   st,
		Feed:         *enum.Feed,
		ETag:         enum.ETag,
		LastModified: enum.LastModified,
		FetchedAtNS:  time.Now().UnixNano(),
		Image:        img,
	})
}

// List returns the subscribed podcasts.
func (s *Service) List(ctx context.Context) ([]*model.Podcast, error) { return s.store.Podcasts(ctx) }

// Get returns one podcast.
func (s *Service) Get(ctx context.Context, pid model.PID) (*model.Podcast, error) {
	return s.store.PodcastByPID(ctx, pid)
}

// Episodes lists a podcast's episodes, newest first (limit 0 = all).
func (s *Service) Episodes(ctx context.Context, pid model.PID, limit int) ([]*model.Episode, error) {
	return s.store.EpisodesByPodcast(ctx, pid, limit)
}

// Episode returns one episode's detail.
func (s *Service) Episode(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error) {
	return s.store.EpisodeByPID(ctx, pid)
}

// SetRetention sets a podcast's keep-newest-N policy (0 keeps all).
func (s *Service) SetRetention(ctx context.Context, pid model.PID, keep int) error {
	if keep < 0 {
		return waxerr.New(waxerr.CodeInvalid, "podcast.SetRetention", "retention count cannot be negative")
	}
	return s.store.SetPodcastRetention(ctx, pid, keep)
}

// SetAuth stores basic-auth credentials for a private feed.
func (s *Service) SetAuth(ctx context.Context, pid model.PID, user, pass string) error {
	if _, err := s.store.PodcastByPID(ctx, pid); err != nil {
		return err
	}
	return s.setAuth(ctx, pid, user, pass)
}

func (s *Service) setAuth(ctx context.Context, pid model.PID, user, pass string) error {
	if err := s.store.SetPodcastAuthUser(ctx, pid, user); err != nil {
		return err
	}
	return s.store.SetSecret(ctx, secretKey(pid), pass)
}

// authFor returns the basic-auth user/pass for a podcast (empty when none).
func (s *Service) authFor(ctx context.Context, pod *model.Podcast) (user, pass string) {
	if pod.AuthUser == "" {
		return "", ""
	}
	pw, err := s.store.GetSecret(ctx, secretKey(pod.PID))
	if err != nil {
		return pod.AuthUser, ""
	}
	return pod.AuthUser, pw
}

func secretKey(pid model.PID) string { return "podcast.auth." + string(pid) }

// Remove unsubscribes from a podcast and deletes its episodes and downloaded files,
// reclaiming the disk space. It also drops the stored basic-auth password so a
// private feed's credential does not outlive the subscription.
func (s *Service) Remove(ctx context.Context, pid model.PID) error {
	files, err := s.store.RemovePodcast(ctx, pid)
	if err != nil {
		return err
	}
	// Drop the auth secret (idempotent when the feed had none).
	if err := s.store.DeleteSecret(ctx, secretKey(pid)); err != nil {
		s.log.Warn("dropping podcast auth secret on unsubscribe", "podcast", pid, "err", err)
	}
	for _, p := range files {
		if err := os.Remove(pathx.Long(p)); err != nil && !os.IsNotExist(err) {
			s.log.Warn("removing episode file on unsubscribe", "path", p, "err", err)
		}
	}
	return nil
}

// fetchImage downloads and decodes an image for the art store, best effort: a
// failure logs and returns nil so a missing/oversized image never blocks a sync.
func (s *Service) fetchImage(ctx context.Context, url string) *model.ArtImage {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	resp, err := s.client.Do(ctx, netsafe.Request{URL: url, AcceptMIME: imageMIME, MaxBytes: episodeImageMaxBytes})
	if err != nil {
		s.log.Debug("podcast image fetch failed", "url", url, "err", err)
		return nil
	}
	img := &model.ArtImage{Data: resp.Body}
	img.Hash = art.Hash(resp.Body)
	format, w, h, err := art.Probe(resp.Body)
	if err != nil {
		s.log.Debug("podcast image undecodable", "url", url, "err", err)
		return nil
	}
	img.Format, img.Width, img.Height = format, w, h
	return img
}
