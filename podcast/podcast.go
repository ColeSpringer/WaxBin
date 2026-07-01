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
	"github.com/colespringer/waxbin/waxerr"
)

// Allowed response media types. Feeds are validated leniently (many servers
// mislabel an RSS document); enclosures must look like media, not an HTML error
// page; transcripts are text/data.
var (
	feedMIME       = []string{"application/rss+xml", "application/atom+xml", "application/xml", "text/xml", "application/x-rss+xml", "text/html", "application/octet-stream", "text/plain"}
	enclosureMIME  = []string{"audio/*", "video/*", "application/ogg", "application/mp4", "application/octet-stream", "binary/octet-stream"}
	transcriptMIME = []string{"text/*", "application/json", "application/x-subrip", "application/srt", "application/octet-stream"}
	imageMIME      = []string{"image/*", "application/octet-stream"}
)

// Store is the persistence the podcast service needs (satisfied by store/sqlite).
type Store interface {
	EnsurePodcastLibrary(ctx context.Context, dir string) (int64, error)
	UpsertFeed(ctx context.Context, in model.UpsertFeedInput) (*model.UpsertFeedResult, error)
	Podcasts(ctx context.Context) ([]*model.Podcast, error)
	PodcastByPID(ctx context.Context, pid model.PID) (*model.Podcast, error)
	PodcastByIdentity(ctx context.Context, key string) (*model.Podcast, error)
	EpisodesByPodcast(ctx context.Context, pid model.PID, limit int) ([]*model.Episode, error)
	EpisodeByPID(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error)
	DownloadedEpisodes(ctx context.Context, pid model.PID) ([]*model.Episode, error)
	AttachEpisodeFile(ctx context.Context, in model.AttachEpisodeFileInput) (model.PID, error)
	DropEpisodeFile(ctx context.Context, pid model.PID) error
	PutTranscript(ctx context.Context, in model.PutTranscriptInput) error
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
	store  Store
	client *netsafe.Client
	reader meta.Reader
	cfg    Config
	log    *slog.Logger

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
	return &Service{store: store, client: client, reader: reader, cfg: cfg, log: log}
}

// AddOptions carries optional basic-auth credentials for a private feed, applied to
// the initial fetch and stored for later syncs.
type AddOptions struct {
	User string
	Pass string
}

// Add subscribes to a feed: it fetches and parses it, derives the stable identity
// key, ingests the feed image, and upserts the podcast plus its episodes. A new
// subscription gets the configured default retention. Re-adding an existing feed is
// a sync.
func (s *Service) Add(ctx context.Context, feedURL string, opts AddOptions) (*model.Podcast, error) {
	const op = "podcast.Add"
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "empty feed url")
	}
	feed, resp, err := s.fetchFeed(ctx, feedURL, opts.User, opts.Pass, "", "")
	if err != nil {
		return nil, err
	}
	// Add sends no conditional headers, so a 304 here is a misbehaving server/CDN and
	// leaves feed nil; refuse rather than dereference it.
	if feed == nil {
		return nil, waxerr.New(waxerr.CodeIO, op, "feed returned not-modified to an unconditional request")
	}
	key := identity.PodcastKey(feed.GUID, feedURL)
	if key == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "feed has no usable identity (url or guid)")
	}
	res, err := s.upsert(ctx, feedURL, key, feed, resp, "")
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

// Sync re-fetches one podcast's feed conditionally (ETag/Last-Modified): a 304
// only stamps the fetch time, otherwise new/updated episodes are upserted. It never
// deletes episodes the feed stopped listing.
func (s *Service) Sync(ctx context.Context, podcastPID model.PID) (*model.UpsertFeedResult, error) {
	pod, err := s.store.PodcastByPID(ctx, podcastPID)
	if err != nil {
		return nil, err
	}
	user, pass := s.authFor(ctx, pod)
	feed, resp, err := s.fetchFeed(ctx, pod.FeedURL, user, pass, pod.ETag, pod.LastModified)
	if err != nil {
		return nil, err
	}
	if resp.NotModified {
		// Nothing changed; refresh the validators/fetch time via a re-upsert of the
		// stored metadata would cost a parse, so just report no change.
		return &model.UpsertFeedResult{PodcastPID: podcastPID}, nil
	}
	// Pass the stored image URL so an unchanged feed cover is not re-fetched/re-decoded.
	return s.upsert(ctx, pod.FeedURL, pod.IdentityKey, feed, resp, pod.ImageURL)
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

// upsert ingests the feed image and persists the parsed feed. priorImageURL is the
// image URL already stored for this podcast (empty on first subscribe); the cover is
// re-fetched only when the feed's image URL differs, avoiding a multi-MiB download +
// decode on every sync of a feed that does not answer 304.
func (s *Service) upsert(ctx context.Context, feedURL, key string, feed *model.Feed, resp *netsafe.Response, priorImageURL string) (*model.UpsertFeedResult, error) {
	var img *model.ArtImage
	if feed.ImageURL != "" && feed.ImageURL != priorImageURL {
		img = s.fetchImage(ctx, feed.ImageURL)
	}
	return s.store.UpsertFeed(ctx, model.UpsertFeedInput{
		FeedURL:      feedURL,
		IdentityKey:  key,
		Feed:         *feed,
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
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

// fetchFeed fetches and parses a feed (conditional on etag/lastmod). On a 304 it
// returns a nil feed and resp.NotModified.
func (s *Service) fetchFeed(ctx context.Context, url, user, pass, etag, lastmod string) (*model.Feed, *netsafe.Response, error) {
	resp, err := s.client.Do(ctx, netsafe.Request{
		URL: url, AcceptMIME: feedMIME, BasicUser: user, BasicPass: pass,
		IfNoneMatch: etag, IfModifiedSince: lastmod,
	})
	if err != nil {
		return nil, nil, err
	}
	if resp.NotModified {
		return nil, resp, nil
	}
	feed, err := ParseFeed(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return feed, resp, nil
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
