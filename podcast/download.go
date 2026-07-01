package podcast

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/diskfree"
	"github.com/colespringer/waxbin/internal/fsx"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/internal/pathx"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/source"
	"github.com/colespringer/waxbin/waxerr"
)

// DownloadResult reports a completed episode download.
type DownloadResult struct {
	EpisodePID model.PID
	FilePID    model.PID
	Path       string
	Bytes      int64
	Transcript bool // a transcript was fetched and stored
}

// Download fetches an episode's enclosure into the podcast directory, catalogs it
// as the episode's file (flipping the episode to present), and opportunistically fetches
// its transcript and artwork. It enforces a free-space preflight and the configured
// size cap. Re-downloading an episode replaces the prior file.
func (s *Service) Download(ctx context.Context, episodePID model.PID) (*DownloadResult, error) {
	const op = "podcast.Download"
	if strings.TrimSpace(s.cfg.Dir) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no podcast download directory configured")
	}
	d, err := s.store.EpisodeByPID(ctx, episodePID)
	if err != nil {
		return nil, err
	}
	ep := d.Episode
	if strings.TrimSpace(ep.EnclosureURL) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "episode has no enclosure to download")
	}
	pod, err := s.store.PodcastByPID(ctx, ep.PodcastPID)
	if err != nil {
		return nil, err
	}
	user, pass := s.authFor(ctx, pod)

	libID, err := s.podcastLibrary(ctx)
	if err != nil {
		return nil, err
	}

	folder := filepath.Join(s.cfg.Dir, folderName(pod))
	if err := os.MkdirAll(pathx.Long(folder), 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := s.preflightSpace(folder, ep.EnclosureSize); err != nil {
		return nil, err
	}

	filename := downloadFilename(ep)
	dst := filepath.Join(folder, filename)
	tmp := dst + ".part"

	// Stream to a temp file then rename, so an interrupted download never leaves a
	// truncated file masquerading as a complete episode. The content hash is computed
	// as the bytes stream past, avoiding a second full read of the finished file. The
	// provider is selected by the show's source type, falling back to the built-in
	// HTTP provider for a manual episode's plain enclosure (and erroring for an absent
	// platform provider).
	prov, err := s.fetchProvider(pod.SourceType)
	if err != nil {
		return nil, err
	}
	n, contentHash, err := s.fetchTo(ctx, prov, tmp, source.FetchRequest{
		URL: ep.EnclosureURL, User: user, Pass: pass, MaxBytes: s.cfg.MaxEnclosureBytes,
	})
	if err != nil {
		_ = os.Remove(pathx.Long(tmp))
		return nil, err
	}
	if err := os.Rename(pathx.Long(tmp), pathx.Long(dst)); err != nil {
		_ = os.Remove(pathx.Long(tmp))
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	file, err := s.fileRow(ctx, dst, n, contentHash)
	if err != nil {
		return nil, err
	}
	filePID, err := s.store.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: episodePID,
		LibraryID:  libID,
		File:       file,
		Image:      s.fetchImage(ctx, ep.ImageURL),
	})
	if err != nil {
		// The catalog write failed after bytes landed at dst. Remove the new file only
		// when the catalog cannot still reference it. A first download, or a re-download
		// with a different name, leaves dst orphaned; a same-path re-download leaves dst
		// as the live file still referenced by the existing episode row.
		if ep.DisplayPath != dst {
			_ = os.Remove(pathx.Long(dst))
		}
		return nil, err
	}
	// A re-download whose filename changed leaves the prior file uncataloged after
	// AttachEpisodeFile drops its row. Remove it here so retention and unsubscribe do
	// not miss it.
	if ep.Downloaded && ep.DisplayPath != "" && ep.DisplayPath != dst {
		if err := os.Remove(pathx.Long(ep.DisplayPath)); err != nil && !os.IsNotExist(err) {
			s.log.Warn("removing superseded episode file", "path", ep.DisplayPath, "err", err)
		}
	}

	res := &DownloadResult{EpisodePID: episodePID, FilePID: filePID, Path: dst, Bytes: n}
	if ep.TranscriptURL != "" {
		if s.fetchTranscript(ctx, episodePID, ep.TranscriptURL, ep.TranscriptType) {
			res.Transcript = true
		}
	}
	return res, nil
}

// fetchTo downloads an item through the provider into a temp file, returning the byte
// count and tagged content hash (computed from the streamed bytes, no second read).
// It creates and closes the file; the caller renames it into place on success.
func (s *Service) fetchTo(ctx context.Context, prov source.Provider, path string, req source.FetchRequest) (int64, string, error) {
	const op = "podcast.Download"
	f, err := os.Create(pathx.Long(path))
	if err != nil {
		return 0, "", waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	res, ferr := prov.Fetch(ctx, req, f)
	if cerr := f.Close(); cerr != nil && ferr == nil {
		ferr = waxerr.Wrap(waxerr.CodeIO, op, cerr)
	}
	if ferr != nil {
		return 0, "", ferr
	}
	// An injected provider could return (nil, nil); guard rather than nil-deref.
	if res == nil {
		return 0, "", waxerr.New(waxerr.CodeInternal, op, "provider returned no fetch result")
	}
	return res.Bytes, res.ContentHash, nil
}

// ImportEpisodeFile places an already-acquired local media file as an episode's
// downloaded file: it moves (or copies) the file into the podcast directory, records
// it as the episode's primary file, and flips the episode to present. It is the
// acquired-episode ingest path, for example a file another provider already fetched,
// distinct from Download, which fetches from a remote URL.
func (s *Service) ImportEpisodeFile(ctx context.Context, episodePID model.PID, srcPath string, keepOriginal bool) (*DownloadResult, error) {
	const op = "podcast.ImportEpisodeFile"
	if strings.TrimSpace(s.cfg.Dir) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "no podcast download directory configured")
	}
	d, err := s.store.EpisodeByPID(ctx, episodePID)
	if err != nil {
		return nil, err
	}
	pod, err := s.store.PodcastByPID(ctx, d.Episode.PodcastPID)
	if err != nil {
		return nil, err
	}
	libID, err := s.podcastLibrary(ctx)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(pathx.Long(srcPath))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	contentHash, err := identity.ContentHash(srcPath)
	if err != nil {
		return nil, err
	}

	folder := filepath.Join(s.cfg.Dir, folderName(pod))
	if err := os.MkdirAll(pathx.Long(folder), 0o755); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	// Cap the basename (like Download's downloadFilename) so a pathological source name
	// stays within the filesystem segment limit rather than failing with ENAMETOOLONG.
	dst := filepath.Join(folder, string(episodePID)+"-"+capFilename(netsafe.SafeFilename(filepath.Base(srcPath), "episode"), 120))
	if err := fsx.MoveOrCopy(srcPath, dst, keepOriginal); err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}

	file, err := s.fileRow(ctx, dst, info.Size(), contentHash)
	if err != nil {
		return nil, err
	}
	filePID, err := s.store.AttachEpisodeFile(ctx, model.AttachEpisodeFileInput{
		EpisodePID: episodePID, LibraryID: libID, File: file, Image: s.fetchImage(ctx, d.Episode.ImageURL),
	})
	if err != nil {
		// The catalog write failed after the file landed at dst. If it was copied, the
		// original at srcPath survives, so remove the orphan. If it was moved, dst is the
		// user's only copy; restore it to srcPath rather than deleting it. If restore
		// fails, leave the file at dst.
		if keepOriginal {
			_ = os.Remove(pathx.Long(dst))
		} else if rerr := fsx.Move(dst, srcPath); rerr != nil {
			s.log.Warn("could not restore acquired episode file after a catalog failure; left in place",
				"dst", dst, "src", srcPath, "err", rerr)
		}
		return nil, err
	}
	return &DownloadResult{EpisodePID: episodePID, FilePID: filePID, Path: dst, Bytes: info.Size()}, nil
}

// fileRow builds the catalog file row for a downloaded enclosure, given the content
// hash already computed during streaming. Codec and duration are read from the file
// opportunistically. Podcast metadata comes from the feed, not the file's tags, so a
// read failure is non-fatal.
func (s *Service) fileRow(ctx context.Context, dst string, size int64, contentHash string) (model.File, error) {
	const op = "podcast.Download"
	info, err := os.Stat(pathx.Long(dst))
	if err != nil {
		return model.File{}, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	rel, err := filepath.Rel(s.cfg.Dir, dst)
	if err != nil {
		rel = filepath.Base(dst)
	}
	file := model.File{
		Path:        []byte(dst),
		DisplayPath: dst,
		RelPath:     []byte(rel),
		Kind:        model.FileAudio,
		Size:        size,
		MTimeNS:     info.ModTime().UnixNano(),
		ContentHash: contentHash,
		EssenceHash: contentHash,
		ScanState:   model.ScanIndexed,
	}
	if fm, err := s.reader.Read(ctx, dst); err == nil {
		file.Container = fm.Tags.Container
		file.Codec = fm.Tags.Codec
		file.DurationMS = fm.Tags.DurationMS
		file.Bitrate = fm.Tags.Bitrate
		file.SampleRate = fm.Tags.SampleRate
		file.Channels = fm.Tags.Channels
		if fm.EssenceHash != "" {
			file.EssenceHash = fm.EssenceHash
		}
	}
	return file, nil
}

// fetchTranscript downloads and stores an episode transcript when possible. It
// returns whether a transcript was stored.
func (s *Service) fetchTranscript(ctx context.Context, episodePID model.PID, url, mimeType string) bool {
	resp, err := s.client.Do(ctx, netsafe.Request{URL: url, AcceptMIME: transcriptMIME, MaxBytes: s.cfg.MaxFeedBytes})
	if err != nil {
		s.log.Debug("transcript fetch failed", "url", url, "err", err)
		return false
	}
	format := transcriptFormat(mimeType, url)
	body := transcriptToText(resp.Body, format)
	if strings.TrimSpace(body) == "" {
		return false
	}
	if err := s.store.PutTranscript(ctx, model.PutTranscriptInput{
		EpisodePID: episodePID, Format: format, Body: body, SourceURL: url,
	}); err != nil {
		s.log.Warn("storing transcript", "episode", episodePID, "err", err)
		return false
	}
	return true
}

// RetentionResult reports a retention pass.
type RetentionResult struct {
	PodcastPID     model.PID
	Removed        int
	ReclaimedBytes int64
}

// ApplyRetention enforces a podcast's keep-newest-N policy by deleting the oldest
// downloaded episodes beyond N. It bypasses the trash to reclaim space and keeps
// play_state on the episode item, which returns to remote and can be downloaded
// again. A keep value of 0 keeps everything.
func (s *Service) ApplyRetention(ctx context.Context, podcastPID model.PID) (*RetentionResult, error) {
	pod, err := s.store.PodcastByPID(ctx, podcastPID)
	if err != nil {
		return nil, err
	}
	res := &RetentionResult{PodcastPID: podcastPID}
	if pod.RetentionKeep <= 0 {
		return res, nil // keep all
	}
	downloaded, err := s.store.DownloadedEpisodes(ctx, podcastPID)
	if err != nil {
		return nil, err
	}
	if len(downloaded) <= pod.RetentionKeep {
		return res, nil
	}
	for _, ep := range downloaded[pod.RetentionKeep:] {
		if ctx.Err() != nil {
			return res, waxerr.FromContext("podcast.ApplyRetention", ctx.Err(), waxerr.CodeCanceled)
		}
		size := onDiskSize(ep.DisplayPath)
		// Remove the file from disk first (reclaim), then update the catalog, mirroring
		// the prune delete path; a failed unlink leaves the episode present and retryable.
		if ep.DisplayPath != "" {
			if err := os.Remove(pathx.Long(ep.DisplayPath)); err != nil && !os.IsNotExist(err) {
				s.log.Warn("retention unlink failed", "path", ep.DisplayPath, "err", err)
				continue
			}
		}
		if err := s.store.DropEpisodeFile(ctx, ep.PID); err != nil {
			return res, err
		}
		res.Removed++
		res.ReclaimedBytes += size
	}
	return res, nil
}

// ApplyRetentionAll runs retention for every podcast and sums the results.
func (s *Service) ApplyRetentionAll(ctx context.Context) (*RetentionResult, error) {
	pods, err := s.store.Podcasts(ctx)
	if err != nil {
		return nil, err
	}
	total := &RetentionResult{}
	for _, p := range pods {
		r, err := s.ApplyRetention(ctx, p.PID)
		if err != nil {
			return total, err
		}
		total.Removed += r.Removed
		total.ReclaimedBytes += r.ReclaimedBytes
	}
	return total, nil
}

// preflightSpace refuses a download when the destination volume lacks room for the
// (declared) enclosure size plus the configured reserve. An unknown/negative size
// checks only the reserve; an unsupported probe (non-Unix) skips the check.
func (s *Service) preflightSpace(dir string, enclosureSize int64) error {
	const op = "podcast.Download"
	reserve := s.cfg.ReserveBytes
	if reserve < 0 {
		reserve = 0
	}
	// A hostile feed can declare a huge (or negative) enclosure length; clamp it and
	// use a saturating add so an overflow never wraps negative and skips the check.
	size := enclosureSize
	if size < 0 {
		size = 0
	}
	need := reserve + size
	if need < reserve { // int64 overflow: treat as "definitely too big"
		return waxerr.New(waxerr.CodeIO, op, "declared enclosure size exceeds addressable space")
	}
	if need <= 0 {
		return nil
	}
	avail, err := diskfree.Available(dir)
	if errors.Is(err, diskfree.ErrUnsupported) {
		return nil
	}
	if err != nil {
		return waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if int64(avail) < need {
		return waxerr.New(waxerr.CodeIO, op, "insufficient free space for download")
	}
	return nil
}

// ImportResult is one feed's outcome from an OPML import.
type ImportResult struct {
	FeedURL string
	Title   string
	PID     model.PID
	Err     string // non-empty when the feed could not be added
}

// ImportOPML subscribes to every feed in an OPML document, returning a per-feed
// result. A failed feed is recorded and the import continues.
func (s *Service) ImportOPML(ctx context.Context, data []byte) ([]ImportResult, error) {
	entries, err := ParseOPML(data)
	if err != nil {
		return nil, err
	}
	out := make([]ImportResult, 0, len(entries))
	for _, e := range entries {
		if ctx.Err() != nil {
			return out, waxerr.FromContext("podcast.ImportOPML", ctx.Err(), waxerr.CodeCanceled)
		}
		r := ImportResult{FeedURL: e.FeedURL, Title: e.Title}
		pod, err := s.Add(ctx, e.FeedURL, AddOptions{})
		if err != nil {
			r.Err = err.Error()
			s.log.Warn("OPML import: feed failed", "url", e.FeedURL, "err", err)
		} else {
			r.PID, r.Title = pod.PID, pod.Title
		}
		out = append(out, r)
	}
	return out, nil
}

// ExportOPML writes every subscription as an OPML document.
func (s *Service) ExportOPML(ctx context.Context, w io.Writer) error {
	pods, err := s.store.Podcasts(ctx)
	if err != nil {
		return err
	}
	entries := make([]model.OPMLEntry, 0, len(pods))
	for _, p := range pods {
		// OPML lists re-subscribable RSS feeds. A manual show's synthetic feed_url and a
		// youtube channel URL are not RSS, and netsafe rejects a non-http(s) xmlUrl on
		// re-import, so skip non-rss shows rather than emit a broken round-trip entry.
		if p.SourceType == model.SourceManual || p.SourceType == model.SourceYouTube {
			continue
		}
		entries = append(entries, model.OPMLEntry{Title: p.Title, FeedURL: p.FeedURL})
	}
	return WriteOPML(w, "WaxBin podcast subscriptions", entries)
}

// --- helpers ---------------------------------------------------------------

// folderName derives a filesystem-safe directory name for a podcast, falling back
// to its pid when the title sanitizes to nothing.
func folderName(pod *model.Podcast) string {
	s := sanitizeSegment(pod.Title)
	if s == "" {
		return string(pod.PID)
	}
	return s
}

// downloadFilename derives a safe, collision-free local filename for an episode
// enclosure: the episode pid, then the enclosure URL's sanitized last path segment.
// It caps the basename length and adds an audio extension when the URL has none.
// The pid prefix is unconditional, so two enclosures of one podcast that share a
// basename never map to the same on-disk path.
func downloadFilename(ep *model.Episode) string {
	name := netsafe.SafeFilename(ep.EnclosureURL, "episode")
	if filepath.Ext(name) == "" {
		name += extForType(ep.EnclosureType)
	}
	name = capFilename(name, 120)
	return string(ep.PID) + "-" + name
}

// capFilename bounds a filename's length (in bytes) while preserving its extension
// and not splitting a UTF-8 rune, so a pathological remote name stays within
// filesystem segment limits.
func capFilename(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) >= max {
		ext = "" // absurdly long "extension": drop it
	}
	stem := name[:len(name)-len(ext)]
	keep := max - len(ext)
	for keep > 0 && !utf8.RuneStart(stem[keep]) {
		keep-- // back up to a rune boundary
	}
	return stem[:keep] + ext
}

// extForType maps an enclosure MIME type to a file extension, defaulting to .mp3
// (the dominant podcast format) when unknown.
func extForType(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "audio/mp4", "audio/x-m4a", "audio/m4a", "video/mp4":
		return ".m4a"
	case "audio/ogg", "audio/opus":
		return ".ogg"
	case "audio/aac":
		return ".aac"
	case "audio/wav", "audio/x-wav":
		return ".wav"
	case "audio/flac", "audio/x-flac":
		return ".flac"
	default:
		return ".mp3"
	}
}

// sanitizeSegment makes a single path segment safe across platforms: it drops path
// separators and control characters, collapses whitespace, trims leading/trailing
// dots and spaces, and caps the length. It is deliberately conservative.
func sanitizeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case r == '/' || r == '\\' || r == ':' || r == 0 || r == '<' || r == '>' ||
			r == '|' || r == '?' || r == '*' || r == '"':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.Trim(out, ". ")
	if len(out) > 80 {
		// Back up to a UTF-8 rune boundary so the cap never splits a multibyte rune into
		// an invalid directory name.
		keep := 80
		for keep > 0 && !utf8.RuneStart(out[keep]) {
			keep--
		}
		out = strings.TrimRight(out[:keep], " ")
	}
	// Escape a Windows reserved device name (CON, NUL, COM1, ...) so a podcast whose
	// title sanitizes to one can still be a directory cross-platform.
	if isReservedName(out) {
		out += "_"
	}
	return out
}

// reservedNames are the Windows device names that cannot be a path segment.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isReservedName reports whether s (case-insensitively, ignoring any extension) is a
// Windows reserved device name.
func isReservedName(s string) bool {
	base := strings.ToLower(s)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return reservedNames[base]
}

// onDiskSize returns a file's size in bytes, or 0 when it cannot be stat'd.
func onDiskSize(path string) int64 {
	info, err := os.Stat(pathx.Long(path))
	if err != nil {
		return 0
	}
	return info.Size()
}

// transcriptFormat classifies a transcript by its declared MIME type, falling back
// to the URL extension.
func transcriptFormat(mimeType, url string) string {
	t := strings.ToLower(mimeType)
	switch {
	case strings.Contains(t, "json"):
		return "json"
	case strings.Contains(t, "srt"), strings.Contains(t, "subrip"), strings.HasSuffix(strings.ToLower(url), ".srt"):
		return "srt"
	case strings.Contains(t, "vtt"), strings.HasSuffix(strings.ToLower(url), ".vtt"):
		return "vtt"
	default:
		return "text"
	}
}

// transcriptToText reduces a transcript to searchable plain text: SRT/VTT cue
// numbers and timestamp lines are dropped so the FTS index holds words, not
// timecodes. JSON and unknown formats are stored verbatim (FTS tokenization
// ignores punctuation anyway).
func transcriptToText(data []byte, format string) string {
	if format != "srt" && format != "vtt" {
		return string(data)
	}
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(line)
		if l == "" || l == "WEBVTT" || isTimecodeLine(l) || isAllDigits(l) {
			continue
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// isTimecodeLine reports whether a line is an SRT/VTT timestamp cue ("00:00:01,000
// --> 00:00:04,000").
func isTimecodeLine(l string) bool { return strings.Contains(l, "-->") }

func isAllDigits(l string) bool {
	if l == "" {
		return false
	}
	for _, r := range l {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
