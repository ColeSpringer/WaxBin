package podcast

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/diskfree"
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
	EnsurePodcastLibrary(ctx context.Context, dir string) (*model.Library, error)
	// UpsertFeed writes the show, its episodes, and its validators in one transaction;
	// source.Provider.Enumerate promises providers exactly that.
	UpsertFeed(ctx context.Context, in model.UpsertFeedInput) (*model.UpsertFeedResult, error)
	// MarkPodcastsFetched records syncs the source answered NotModified, leaving the
	// stored validators alone.
	MarkPodcastsFetched(ctx context.Context, pids []model.PID, atNS int64) error
	// MovePodcastFeed moves a show whose sync was answered NotModified at a new address.
	MovePodcastFeed(ctx context.Context, pid model.PID, feedURL string) error
	UpsertShow(ctx context.Context, in model.UpsertShowInput) (model.PID, bool, error)
	UpsertEpisode(ctx context.Context, in model.UpsertEpisodeInput) (*model.UpsertEpisodeResult, error)
	Podcasts(ctx context.Context) ([]*model.Podcast, error)
	PodcastByPID(ctx context.Context, pid model.PID) (*model.Podcast, error)
	PodcastForFeed(ctx context.Context, feedURL, key string) (*model.Podcast, error)
	EpisodesByPodcast(ctx context.Context, pid model.PID, limit int) ([]*model.Episode, error)
	EpisodesByPIDs(ctx context.Context, pids []model.PID) ([]*model.Episode, error)
	EpisodeMeta(ctx context.Context, pid model.PID) (*model.Episode, error)
	EpisodeByPID(ctx context.Context, pid model.PID) (*model.EpisodeDetail, error)
	DownloadedEpisodes(ctx context.Context, pid model.PID) ([]*model.Episode, error)
	AttachEpisodeFile(ctx context.Context, in model.AttachEpisodeFileInput) (model.PID, error)
	DropEpisodeFile(ctx context.Context, pid model.PID) error
	PutTranscript(ctx context.Context, in model.PutTranscriptInput) error
	TranscriptByEpisode(ctx context.Context, pid model.PID) (*model.Transcript, error)
	PutEpisodeChapters(ctx context.Context, episodePID model.PID, chapters []model.Chapter) error
	RemovePodcast(ctx context.Context, pid model.PID) ([]string, error)
	SetPodcastRetention(ctx context.Context, pid model.PID, keep int) error
	SetPodcastAuth(ctx context.Context, pid model.PID, user, pass string) error
	GetSecret(ctx context.Context, key string) (string, error)
	DeleteSecret(ctx context.Context, key string) error
}

// ReasonPasswordMissing is a show whose login has a user name but no stored password, as
// a restore of a backup trimmed of its secrets leaves it; podcast auth sets it again.
const ReasonPasswordMissing waxerr.Reason = "password_missing"

// Config tunes the podcast service: where downloads land, the network policy
// netsafe enforces, and free-space/retention defaults.
type Config struct {
	Dir               string        // download directory (downloads require it)
	UserAgent         string        // HTTP User-Agent for feed/enclosure fetches
	BlockPrivateIPs   bool          // SSRF guard (off by default; on for untrusted feeds)
	Timeout           time.Duration // per-request timeout; bounds a download's silences, not its length
	MaxFeedBytes      int64         // cap on a feed/transcript/OPML body
	MaxEnclosureBytes int64         // cap on an episode download
	ReserveBytes      int64         // free-space headroom kept on the download volume
	DefaultRetention  int           // keep-N applied to a new subscription (0 = keep all)
	// Providers are injected acquisition providers, such as a youtube provider from
	// another module. The built-in netsafe rss provider is always registered; an
	// injected provider registers under its own SourceType.
	Providers []source.Provider
	// Leaser serializes this package's filesystem mutators against each other. Nil
	// runs them inline, which is what a standalone Service and this package's own
	// tests get.
	Leaser Leaser
}

// Leaser serializes filesystem mutations against WaxBin's other mutating passes. A
// nil Leaser runs the work inline.
//
// The seam is injected rather than the package taking a lease itself because the lease
// store belongs to the embedding Library, and because a standalone podcast Service has
// nothing to serialize against.
type Leaser interface {
	// Lease runs fn holding the podcast filesystem lease, returning CodeConflict if
	// another podcast mutator holds it.
	//
	// No implementation records a job row, retention included. The watch loop calls
	// ApplyRetentionAll every tick, so a row there would fill `waxbin jobs` and, worse,
	// advance the change feed once per tick on a catalog where nothing happened. The
	// episodes a retention pass does drop emit their own deltas.
	Lease(ctx context.Context, fn func(context.Context) error) error
	// LeaseWait is Lease with a bounded retry on a busy lease, for the one caller whose
	// work is already paid for by the time it needs the lease: a finished download's
	// commit tail, which would otherwise throw the bytes away.
	LeaseWait(ctx context.Context, fn func(context.Context) error) error
	// LeaseImport runs fn holding both the user-tree and podcast leases, for the one
	// verb that takes a file from an arbitrary source path into the podcast tree. move
	// reports that the file leaves src, which the embedder may refuse, as WaxBin does
	// for a src inside a library flagged read-only.
	LeaseImport(ctx context.Context, src string, move bool, fn func(context.Context) error) error
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
	builtin   source.Provider // the netsafe rss provider, whose parser bounds its feeds
	now       func() time.Time
	freeSpace func(dir string) (uint64, error)

	libMu sync.Mutex
	libID int64 // cached internal podcast-library id (0 = unresolved)
}

// EnsureLibrary finds or creates the internal library downloads land in, at the
// configured download directory, and caches its id; without a directory it is
// CodeInvalid. A read-write waxbin.Open calls it, and so does the reopen after a
// maintenance hand-off, so the library is there before the first download.
func (s *Service) EnsureLibrary(ctx context.Context) (*model.Library, error) {
	s.libMu.Lock()
	defer s.libMu.Unlock()
	return s.ensureLibraryLocked(ctx)
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
	lib, err := s.ensureLibraryLocked(ctx)
	if err != nil {
		return 0, err
	}
	return lib.ID, nil
}

// ensureLibraryLocked is EnsureLibrary under libMu, which the store call is held under so
// a ForgetLibrary waits for it rather than being overtaken by an id from before.
func (s *Service) ensureLibraryLocked(ctx context.Context) (*model.Library, error) {
	if strings.TrimSpace(s.cfg.Dir) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, "podcast.EnsureLibrary", "no podcast download directory configured")
	}
	lib, err := s.store.EnsurePodcastLibrary(ctx, s.cfg.Dir)
	if err != nil {
		return nil, err
	}
	s.libID = lib.ID
	return lib, nil
}

// ForgetLibrary drops the cached podcast-library id, so the next use resolves it
// again. A maintenance hand-off calls it, since a restore or rebuild beneath the
// hand-off can give the library another rowid.
func (s *Service) ForgetLibrary() {
	s.libMu.Lock()
	defer s.libMu.Unlock()
	s.libID = 0
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
	builtin := source.NewHTTP(client, ParseFeed)
	providers := map[model.SourceType]source.Provider{model.SourceRSS: builtin}
	for _, p := range cfg.Providers {
		if p != nil {
			providers[p.SourceType()] = p
		}
	}
	return &Service{store: store, client: client, reader: reader, cfg: cfg, log: log,
		providers: providers, builtin: builtin, now: time.Now, freeSpace: diskfree.Available}
}

// lease runs fn holding the podcast filesystem lease, or inline when no Leaser is
// injected.
func (s *Service) lease(ctx context.Context, fn func(context.Context) error) error {
	if s.cfg.Leaser == nil {
		return fn(ctx)
	}
	return s.cfg.Leaser.Lease(ctx, fn)
}

// leaseWait is lease with a bounded retry, for a download's commit tail.
func (s *Service) leaseWait(ctx context.Context, fn func(context.Context) error) error {
	if s.cfg.Leaser == nil {
		return fn(ctx)
	}
	return s.cfg.Leaser.LeaseWait(ctx, fn)
}

// leaseImport runs fn holding both the user-tree and podcast leases, or inline when no
// Leaser is injected.
func (s *Service) leaseImport(ctx context.Context, src string, move bool, fn func(context.Context) error) error {
	if s.cfg.Leaser == nil {
		return fn(ctx)
	}
	return s.cfg.Leaser.LeaseImport(ctx, src, move, fn)
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

// AddOptions carries a private feed's HTTP Basic login, used for the first fetch and
// stored for later syncs. It goes only to the feed's own origin, its scheme, host and port
// (an http feed's https form included); an enclosure or redirect on any other host, a
// subdomain of the feed's host among them, is fetched without it, and a refusal from such
// a host says where the login goes.
//
// A show already subscribed keeps its stored address and login when it is added again,
// at that address or, by its podcast:guid, at another, so a second subscriber's own
// address or login never becomes the show's. A re-add at the stored address with no
// login asks with the stored one, as the show's sync does, and updates the show; any
// other re-add fetched a view of the feed that is not the show's, so it writes nothing
// (Added.Synced is false). ReplaceAddress is the operator saying the feed moved: the
// show takes the address added at, that address's feed and the login given, the show
// and its login in one write. A move given no login leaves the stored one behind
// (Added.LoginDropped), since that login was for the old address and the add cannot tell
// which show it moves until the feed answers, so a private feed's move gives its login
// again. A feed that moves itself by permanent redirect within its own origin needs
// none of this, since the next sync follows it. A login needs a user name; a password
// alone is refused.
type AddOptions struct {
	User           string
	Pass           string
	ReplaceAddress bool
}

// Added is the show an add leaves, with what the add did to it.
type Added struct {
	*model.Podcast
	// Created is a new subscription. Synced says the show took this add's feed: a new
	// one, a re-add that asked as the show's own sync asks, or a ReplaceAddress move. A
	// re-add that did not sync left the show as stored, its address and login included.
	Created, Synced bool
	// LoginDropped says a ReplaceAddress move given no login left the stored one behind.
	LoginDropped bool
}

// Add subscribes to an RSS feed. It is AddSource with the rss source type.
func (s *Service) Add(ctx context.Context, feedURL string, opts AddOptions) (*Added, error) {
	return s.AddSource(ctx, feedURL, model.SourceRSS, opts)
}

// AddSource subscribes to a feed/channel served by the given source type: it
// enumerates it through the matching provider, derives the stable identity key,
// ingests the image, and upserts the show plus its episodes. A new subscription gets
// the configured default retention. Re-adding an existing source keeps the show's
// address and login, and is a sync only when it asks as the show's sync does, as
// AddOptions says. A youtube source needs an injected provider; without it the call
// returns CodeUnsupported. A provider's failure is a *source.ProviderError, as for
// Sync.
func (s *Service) AddSource(ctx context.Context, url string, sourceType model.SourceType, opts AddOptions) (*Added, error) {
	const op = "podcast.AddSource"
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "empty feed url")
	}
	if err := checkURLs(op, "the feed url", url); err != nil {
		return nil, err
	}
	given := opts.User != "" || opts.Pass != ""
	if opts.User == "" && opts.Pass != "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "a feed login needs a user name")
	}
	prov, err := s.providerFor(sourceType)
	if err != nil {
		return nil, err
	}
	// The show subscribed at this address, when one is, and its login, which a re-add
	// given none asks with, as the show's sync would.
	user, pass := opts.User, opts.Pass
	stored, err := s.store.PodcastForFeed(ctx, url, "")
	if err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
		return nil, err
	}
	var storedUser, storedPass string
	var storedErr error
	if err == nil {
		storedUser, storedPass, storedErr = s.authFor(ctx, stored)
		if !given {
			if storedErr != nil {
				return nil, storedErr
			}
			user, pass = storedUser, storedPass
		}
	} else {
		stored = nil
	}
	enum, err := prov.Enumerate(ctx, source.Request{URL: url, User: user, Pass: pass})
	if err == nil {
		err = checkEnumeration(op, enum, false)
	}
	if err != nil {
		return nil, providerErr(ctx, op, prov.SourceType(), "enumerate", err)
	}
	key := enum.IdentityKey
	if key == "" {
		key = identity.PodcastKey(model.CapBytes(enum.Feed.GUID, MaxGUIDBytes), url)
	}
	if key == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "feed has no usable identity (url or guid)")
	}
	// The show the add lands on: the one at this address, else the one with the feed's
	// identity. Its cover, and whether that is locked, are what stop a 200-show OPML
	// re-import fetching a full channel image per show only for
	// attachEntityArtUnlessLockedTx to discard it.
	prior := stored
	if prior == nil {
		if prior, err = s.store.PodcastForFeed(ctx, url, key); err != nil && !waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, err
		} else if err != nil {
			prior = nil
		}
	}
	w := feedWrite{feedURL: url, key: key, st: prov.SourceType(), enum: enum, bounded: prov == s.builtin,
		creds: loginOf(url, user, pass)}
	if prior != nil {
		w.pid, w.coverSourceURL, w.coverLocked = prior.PID, prior.CoverSourceURL, prior.CoverLocked
	}
	dropped := false
	switch {
	case prior == nil:
		if given {
			w.login = &model.FeedLogin{User: opts.User, Pass: opts.Pass}
		}
	case !opts.ReplaceAddress:
		// Asked any other way than the show's sync asks, the answer is another view of the
		// feed, whose addresses (a subscriber's tokens among them) the show must not take.
		if prior != stored || storedErr != nil || storedUser != user || storedPass != pass {
			return &Added{Podcast: prior}, nil
		}
	case given:
		w.login = &model.FeedLogin{User: opts.User, Pass: opts.Pass}
	case prior.FeedURL != url:
		// The stored login was for the address the show leaves.
		w.login, dropped = &model.FeedLogin{}, prior.AuthUser != ""
	}
	res, err := s.upsert(ctx, w)
	if err != nil {
		return nil, err
	}
	if res.Created && s.cfg.DefaultRetention > 0 {
		if err := s.store.SetPodcastRetention(ctx, res.PodcastPID, s.cfg.DefaultRetention); err != nil {
			return nil, err
		}
	}
	pod, err := s.store.PodcastByPID(ctx, res.PodcastPID)
	if err != nil {
		return nil, err
	}
	return &Added{Podcast: pod, Created: res.Created, Synced: true, LoginDropped: dropped}, nil
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
// Its text is held to a feed's bounds (caps.go), and a link past MaxURLBytes is
// refused.
func (s *Service) AddManual(ctx context.Context, title string, opts ManualOptions) (*model.Podcast, error) {
	const op = "podcast.AddManual"
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "manual show needs a title")
	}
	if err := checkURLs(op, "the link", opts.Link); err != nil {
		return nil, err
	}
	// A manual show has no feed URL, but podcast.feed_url is NOT NULL UNIQUE; use the
	// synthetic identity key as the feed_url too so two manual shows never collide on
	// an empty string. Sync short-circuits a manual show, so it is never dereferenced.
	key := "manual:" + string(model.NewPID())
	pid, _, err := s.store.UpsertShow(ctx, model.UpsertShowInput{
		IdentityKey: key, FeedURL: key, SourceType: model.SourceManual,
		Title: model.CapText(title, MaxTitleChars), Author: model.CapText(opts.Author, MaxNameChars),
		Description: model.CapBytes(opts.Description, MaxDescriptionBytes), Link: opts.Link,
	})
	if err != nil {
		return nil, err
	}
	return s.store.PodcastByPID(ctx, pid)
}

// AddEpisode adds or updates a single episode under an existing show, bypassing feed
// sync. Pinned keeps the episode out of retention pruning. It returns the episode pid
// and whether it was newly created. The episode's text is held to a feed's bounds
// (caps.go), and a URL past MaxURLBytes or a GUID past MaxGUIDBytes is refused.
func (s *Service) AddEpisode(ctx context.Context, showPID model.PID, ep model.FeedEpisode, pinned bool) (*model.UpsertEpisodeResult, error) {
	const op = "podcast.AddEpisode"
	if strings.TrimSpace(ep.Title) == "" && strings.TrimSpace(ep.GUID) == "" && strings.TrimSpace(ep.EnclosureURL) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "episode needs a title, guid, or enclosure url")
	}
	if err := checkURLs(op, "the enclosure url", ep.EnclosureURL, "the link", ep.Link, "the image url", ep.ImageURL,
		"the transcript url", ep.TranscriptURL, "the chapters url", ep.ChaptersURL); err != nil {
		return nil, err
	}
	if len(ep.GUID) > MaxGUIDBytes {
		return nil, waxerr.New(waxerr.CodeInvalid, op, fmt.Sprintf("the guid is longer than %d bytes", MaxGUIDBytes))
	}
	ep = cloneEpisode(ep)
	capEpisode(&ep)
	return s.store.UpsertEpisode(ctx, model.UpsertEpisodeInput{PodcastPID: showPID, Episode: ep, Pinned: pinned})
}

// Sync enumerates one show conditionally (ETag/Last-Modified) through its
// provider: a NotModified only records the fetch time, otherwise new/updated episodes
// are upserted and the fresh validators land with them. It never deletes episodes the
// source stopped listing. A manual show has nothing to sync; a youtube show needs an
// injected provider. rss and youtube shows use the same sync path. A feed answered
// through permanent redirects to an address at its own origin, or that origin's https
// form, is stored at that address (source.Enumeration.MovedTo); a login the catalog
// cannot read fails the sync before the source is asked.
//
// The show's cover is refreshed from the feed only when the feed's image URL differs
// from where the cover it holds came from, and never while that cover is locked. Both
// facts ride along on the PodcastByPID this already does, so neither costs a query.
//
// A failure of the provider itself comes back as a *source.ProviderError carrying the
// reason (source.Reason*) and HTTP status its cause names, and any other error is the
// catalog's own. The type does not cross the proxy, which rebuilds a plain waxerr from
// the class, reason and text; no sync, add, or download verb is proxied.
func (s *Service) Sync(ctx context.Context, podcastPID model.PID) (*model.UpsertFeedResult, error) {
	res, unchanged, err := s.sync(ctx, podcastPID)
	if err != nil {
		return nil, err
	}
	if unchanged {
		if err := s.store.MarkPodcastsFetched(ctx, []model.PID{podcastPID}, s.now().UnixNano()); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// sync is Sync without the fetch-time mark, which the caller records: Sync right
// away, SyncAll once for every show that answered NotModified. unchanged reports that
// answer.
func (s *Service) sync(ctx context.Context, podcastPID model.PID) (res *model.UpsertFeedResult, unchanged bool, err error) {
	const op = "podcast.Sync"
	pod, err := s.store.PodcastByPID(ctx, podcastPID)
	if err != nil {
		return nil, false, err
	}
	st := pod.SourceType
	if st == "" {
		st = model.SourceRSS
	}
	if st == model.SourceManual {
		// A manual show is curated episode by episode; there is no feed to enumerate.
		return &model.UpsertFeedResult{PodcastPID: podcastPID}, false, nil
	}
	prov, err := s.providerFor(st)
	if err != nil {
		return nil, false, err
	}
	user, pass, err := s.authFor(ctx, pod)
	if err != nil {
		return nil, false, err
	}
	enum, err := prov.Enumerate(ctx, source.Request{
		URL: pod.FeedURL, User: user, Pass: pass, ETag: pod.ETag, LastModified: pod.LastModified,
	})
	if err == nil {
		err = checkEnumeration(op, enum, pod.ETag != "" || pod.LastModified != "")
	}
	if err != nil {
		return nil, false, providerErr(ctx, op, prov.SourceType(), "enumerate", err)
	}
	// A feed whose own origin says it moved for good is stored at its new address. A move
	// to another origin, a downgrade to http among them, is left to the source to answer
	// again at every sync, since the show's login would not follow it there.
	feedURL := pod.FeedURL
	if moved := enum.MovedTo; moved != "" && len(moved) <= MaxURLBytes && netsafe.WithinOrigin(pod.FeedURL, moved) {
		feedURL = moved
	}
	if enum.NotModified {
		// The stored validators stay (a 304 echoes them at best); the fetch time moves, so
		// the show still reads as checked, and so does the address of a feed that moved.
		if feedURL != pod.FeedURL {
			if err := s.store.MovePodcastFeed(ctx, podcastPID, feedURL); err != nil {
				return nil, false, err
			}
		}
		return &model.UpsertFeedResult{PodcastPID: podcastPID}, true, nil
	}
	// Pass what we know about the cover the show already holds, so an unchanged one is
	// not re-fetched/re-decoded and a locked one is not fetched at all.
	res, err = s.upsert(ctx, feedWrite{pid: pod.PID, feedURL: feedURL, key: pod.IdentityKey, st: st, enum: enum,
		bounded: prov == s.builtin, coverSourceURL: pod.CoverSourceURL, coverLocked: pod.CoverLocked,
		creds: loginOf(pod.FeedURL, user, pass)})
	return res, false, err
}

// providerErr marks err as the failure of st's provider at provOp; a show's documents
// (transcripts, chapters) are the built-in rss provider's, whose client fetches them. A
// call whose context ended is the caller's cancellation whatever the provider made of
// it, so it is not marked.
func providerErr(ctx context.Context, op string, st model.SourceType, provOp string, err error) error {
	if ctx.Err() != nil {
		return waxerr.Classify(waxerr.CodeCanceled, op, err)
	}
	return source.NewProviderError(st, provOp, err)
}

// checkEnumeration refuses the answers the provider contract rules out: no
// enumeration at all, one with neither a feed nor NotModified, and a NotModified to a
// request that carried no validators, which is a misbehaving server or CDN rather
// than an unchanged source.
func checkEnumeration(op string, enum *source.Enumeration, conditional bool) error {
	switch {
	case enum == nil, !enum.NotModified && enum.Feed == nil:
		return waxerr.New(waxerr.CodeIO, op, "provider returned neither a feed nor not-modified")
	case enum.NotModified && !conditional:
		return waxerr.New(waxerr.CodeIO, op, "feed returned not-modified to an unconditional request")
	}
	return nil
}

// SyncAllResult reports a SyncAll pass: the result of each show that synced, and the
// error of each that did not, as Sync would have returned it.
type SyncAllResult struct {
	Results  map[model.PID]*model.UpsertFeedResult
	Failures map[model.PID]error
}

// SyncAll syncs every subscribed podcast. A show that fails is reported in Failures
// and the batch moves on, so one dead feed does not stop the rest; the error return is
// the batch's own failure (a cancellation, or the final fetch-time write), and comes
// with what the batch had done by then. The shows that answered NotModified are marked
// fetched in one write at the end rather than one write transaction per unchanged
// feed.
func (s *Service) SyncAll(ctx context.Context) (*SyncAllResult, error) {
	const op = "podcast.SyncAll"
	pods, err := s.store.Podcasts(ctx)
	if err != nil {
		return nil, err
	}
	out := &SyncAllResult{
		Results:  make(map[model.PID]*model.UpsertFeedResult, len(pods)),
		Failures: map[model.PID]error{},
	}
	var unchanged []model.PID
	for _, p := range pods {
		if ctx.Err() != nil {
			return out, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
		}
		res, notModified, err := s.sync(ctx, p.PID)
		if err != nil {
			if ctx.Err() != nil {
				return out, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
			}
			out.Failures[p.PID] = err
			continue
		}
		out.Results[p.PID] = res
		if notModified {
			unchanged = append(unchanged, p.PID)
		}
	}
	if err := s.store.MarkPodcastsFetched(ctx, unchanged, s.now().UnixNano()); err != nil {
		return out, err
	}
	return out, nil
}

// feedWrite is one enumeration's write: the show it lands on (pid, empty for one not yet
// subscribed), the address, identity and source type it is stored under, whether its
// feed is already bounded (the built-in provider's parser bounds what it parses), the
// login it stores with the show (nil keeps the stored one), and the cover and
// credentials the image fetch goes by.
type feedWrite struct {
	pid            model.PID
	feedURL, key   string
	st             model.SourceType
	enum           *source.Enumeration
	bounded        bool
	login          *model.FeedLogin
	coverSourceURL string
	coverLocked    bool
	creds          *netsafe.Credentials
}

// upsert ingests the show image and persists an enumeration under a source type.
//
// coverSourceURL is where the cover the show currently holds was fetched from
// (art_map.source_url, empty when it holds none), and the download is skipped when the
// feed still advertises that same URL. This avoids a multi-MiB fetch + decode on every
// sync of a source that does not answer NotModified, and it compares against the cover
// we actually have rather than against podcast.image_url, which the feed advances
// whether or not the picture was ever attached. A cleared cover leaves no art_map row,
// so this reads empty and the next sync refills it; image_url would still match and
// leave the show coverless forever.
//
// coverLocked short-circuits the whole thing: a locked cover is one the user chose, so
// there is nothing to fetch. Checking it here rather than at the store is what keeps
// the compare honest, since attachEntityArtUnlessLockedTx would otherwise discard the
// bytes after paying for them, every sync, for as long as the lock stands.
//
// Two consequences worth knowing. A cover a user set with --no-lock carries no
// source_url, so the next sync fetches once and the feed re-points it; that is what an
// unlocked cover means, and it settles after that one fetch. And a fetch that fails
// attaches nothing, so the compare still differs and the next sync retries. Retrying is
// right for an outage but not free for a permanently broken image URL, which re-pulls
// up to episodeImageMaxBytes on every sync that actually re-enumerates. Feeds that
// answer NotModified never get that far.
func (s *Service) upsert(ctx context.Context, w feedWrite) (*model.UpsertFeedResult, error) {
	feed := *w.enum.Feed
	if !w.bounded {
		feed = cloneFeed(feed)
		capFeed(&feed)
	}
	if feed.EpisodesDropped > 0 {
		s.log.Warn("feed lists more episodes than a show keeps; the rest are left out",
			"host", netsafe.ShownHost(w.feedURL), "kept", len(feed.Episodes), "dropped", feed.EpisodesDropped)
	}
	var img *model.ArtImage
	if !w.coverLocked && feed.ImageURL != "" && feed.ImageURL != w.coverSourceURL {
		img = s.fetchImage(ctx, feed.ImageURL, w.creds)
	}
	return s.store.UpsertFeed(ctx, model.UpsertFeedInput{
		Login:        w.login,
		PodcastPID:   w.pid,
		FeedURL:      w.feedURL,
		IdentityKey:  w.key,
		SourceType:   w.st,
		Feed:         feed,
		ETag:         w.enum.ETag,
		LastModified: w.enum.LastModified,
		FetchedAtNS:  s.now().UnixNano(),
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

// EpisodesByPIDs returns the episodes named by pids in request order, each once,
// leaving out a pid that is no episode.
func (s *Service) EpisodesByPIDs(ctx context.Context, pids []model.PID) ([]*model.Episode, error) {
	return s.store.EpisodesByPIDs(ctx, pids)
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

// SetAuth stores basic-auth credentials for a private feed, scoped as AddOptions says.
func (s *Service) SetAuth(ctx context.Context, pid model.PID, user, pass string) error {
	if _, err := s.store.PodcastByPID(ctx, pid); err != nil {
		return err
	}
	return s.setAuth(ctx, pid, user, pass)
}

func (s *Service) setAuth(ctx context.Context, pid model.PID, user, pass string) error {
	return s.store.SetPodcastAuth(ctx, pid, user, pass)
}

// authFor returns a show's basic-auth login, empty when it has no user. A login always
// has its secret row, so one that is missing is a password lost (ReasonPasswordMissing),
// as a restore of a backup trimmed of its secrets leaves it, and one the catalog cannot
// read (a sealed one with no cipher configured) is the catalog's failure; either way
// nothing is sent rather than half a login.
func (s *Service) authFor(ctx context.Context, pod *model.Podcast) (user, pass string, err error) {
	if pod.AuthUser == "" {
		return "", "", nil
	}
	pw, err := s.store.GetSecret(ctx, model.PodcastSecretKey(pod.PID))
	switch {
	case waxerr.Is(err, waxerr.CodeNotFound):
		return "", "", waxerr.New(waxerr.CodeInvalid, "podcast",
			"the stored password of "+string(pod.PID)+" is missing; set it again with podcast auth").WithReason(ReasonPasswordMissing)
	case err != nil:
		return "", "", waxerr.Wrapf(waxerr.CodeIO, "podcast", err, "reading the login of %s", pod.PID)
	}
	return pod.AuthUser, pw, nil
}

// loginOf scopes a show's login to the origin of its feed URL, or is nil when it has
// none.
func loginOf(feedURL, user, pass string) *netsafe.Credentials {
	if user == "" && pass == "" {
		return nil
	}
	return &netsafe.Credentials{Origin: feedURL, User: user, Pass: pass}
}

// Remove unsubscribes from a podcast and deletes its episodes and downloaded files,
// reclaiming the disk space. It also drops the stored basic-auth password so a
// private feed's credential does not outlive the subscription.
//
// It unlinks the files itself rather than calling Unfetch, so it holds the podcast
// filesystem lease around the whole thing.
func (s *Service) Remove(ctx context.Context, pid model.PID) error {
	return s.lease(ctx, func(ctx context.Context) error {
		files, err := s.store.RemovePodcast(ctx, pid)
		if err != nil {
			return err
		}
		// Drop the auth secret (idempotent when the feed had none).
		if err := s.store.DeleteSecret(ctx, model.PodcastSecretKey(pid)); err != nil {
			s.log.Warn("dropping podcast auth secret on unsubscribe", "podcast", pid, "err", err)
		}
		for _, p := range files {
			if err := os.Remove(pathx.Long(p)); err != nil && !os.IsNotExist(err) {
				s.log.Warn("removing episode file on unsubscribe", "path", p, "err", err)
			}
		}
		s.pruneFolders(files...)
		return nil
	})
}

// fetchImage downloads and decodes an image for the art store, best effort: a
// failure logs and returns nil so a missing/oversized image never blocks a sync. It
// is the one constructor behind the show image, the episode image, and the manual
// import, so stamping "feed" and the fetched URL here covers all three. login is the
// show's, sent only at its origin.
func (s *Service) fetchImage(ctx context.Context, url string, login *netsafe.Credentials) *model.ArtImage {
	if strings.TrimSpace(url) == "" {
		return nil
	}
	resp, err := s.client.Do(ctx, netsafe.Request{URL: url, AcceptMIME: imageMIME, MaxBytes: episodeImageMaxBytes,
		Credentials: login})
	if err != nil {
		s.log.Debug("podcast image fetch failed", "host", netsafe.ShownHost(url), "err", err)
		return nil
	}
	// Describe recognizes an AVIF/HEIC channel image on its magic alone, the same second
	// chance probeArtImage gives a user-supplied one. Without it a feed publishing an
	// exotic-but-valid image attaches nothing, and since the sync compares against the
	// cover it holds rather than the URL the feed advertises, nothing would ever advance
	// and every later sync would re-download it.
	//
	// The response's own Content-Type is deliberately not a fallback here, where it is
	// one for a hand-set cover. A remote server naming its bytes image/anything would get
	// them attached, and upsert then skips the fetch while the feed advertises the same
	// URL, so one bad answer pins a broken cover until someone intervenes. Discarding it
	// costs a re-download on the next sync and heals itself.
	info := art.Describe(resp.Body)
	if info.Format == "" {
		// Re-probe for the reason, the way the cover-art provider does: a feed serving an
		// error page and one serving a truncated image should not log identically.
		_, _, _, perr := art.Probe(resp.Body)
		s.log.Debug("podcast image undecodable", "host", netsafe.ShownHost(url), "bytes", len(resp.Body), "err", perr)
		return nil
	}
	return &model.ArtImage{
		Data: resp.Body, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: model.Attribution{Source: model.SourceFeed, SourceURL: url},
	}
}
