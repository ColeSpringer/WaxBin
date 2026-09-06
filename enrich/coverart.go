package enrich

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/colespringer/waxbin/art"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// coverArt fetches release-group front covers from the Cover Art Archive. CAA
// answers with a redirect to the image on archive.org, which the netsafe client
// follows. A 404 means the release group has no cover, reported as CodeNotFound.
type coverArt struct {
	client  *netsafe.Client
	baseURL string // e.g. https://coverartarchive.org
	cache   cache
}

// caaGroupFront is what the group fetch records: the release whose front the archive
// served as the group's, read off the redirect, the hash of those bytes, and the
// validator the archive sent for them. Release is empty when the redirect named none,
// which forbids reuse and nothing else.
type caaGroupFront struct {
	Release string `json:"release"`
	Hash    string `json:"hash"`
	ETag    string `json:"etag,omitempty"`
}

func groupFrontKey(rgMBID string) string { return "caa:rg-front:" + rgMBID }

// recordGroupFront writes the record for one group, replacing any earlier one.
func (c *coverArt) recordGroupFront(ctx context.Context, rgMBID string, rec caaGroupFront) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return waxerr.Wrap(waxerr.CodeInvalid, "enrich.coverart", err)
	}
	return c.cache.put(ctx, groupFrontKey(rgMBID), payload)
}

// groupFrontRecord reads one group's record; ok is false when none was ever written.
func (c *coverArt) groupFrontRecord(ctx context.Context, rgMBID string) (rec caaGroupFront, ok bool, err error) {
	payload, found, err := c.cache.get(ctx, groupFrontKey(rgMBID))
	if err != nil || !found {
		return caaGroupFront{}, false, err
	}
	if err := json.Unmarshal(payload, &rec); err != nil {
		return caaGroupFront{}, false, nil
	}
	return rec, true, nil
}

// coverImageMaxBytes caps a fetched cover; album art is large but bounded.
const coverImageMaxBytes = 24 << 20 // 24 MiB

var coverMIME = []string{"image/*", "application/octet-stream"}

// fetched is one front-cover fetch: the bytes, the request URL (the citation), the URL
// the redirects ended at (which names the release), and the archive's validator.
// notModified is set, and the rest empty, when a conditional fetch was answered 304.
type fetched struct {
	data             []byte
	reqURL, finalURL string
	etag             string
	notModified      bool
}

// frontCover fetches one entity's front cover, conditionally when ifNoneMatch is set,
// or reports CodeNotFound when it has none. rung is the archive path segment:
// "release-group" for the group's cover, or "release" for the specific pressing's own.
// The caller decodes and hashes the bytes.
//
// The request URL is the citation, not the archive.org object netsafe followed the
// redirect to: it is stable and names the entity, while the redirect target is an
// implementation detail of where the file sits today. The final URL and the validator
// are handed back for the group record alone.
func (c *coverArt) frontCover(ctx context.Context, rung, mbid, ifNoneMatch string) (fetched, error) {
	if mbid == "" {
		return fetched{}, waxerr.New(waxerr.CodeNotFound, "enrich.coverart", "no mbid")
	}
	reqURL := c.baseURL + "/" + rung + "/" + url.PathEscape(mbid) + "/front"
	resp, err := c.client.Do(ctx, netsafe.Request{
		URL:         reqURL,
		AcceptMIME:  coverMIME,
		MaxBytes:    coverImageMaxBytes,
		IfNoneMatch: ifNoneMatch,
	})
	if err != nil {
		return fetched{}, err
	}
	return fetched{
		data: resp.Body, reqURL: reqURL, finalURL: resp.FinalURL,
		etag: resp.ETag, notModified: resp.NotModified,
	}, nil
}

// caaItemMBID matches the archive's per-release item name, the only shape a release id
// is read from.
var caaItemMBID = regexp.MustCompile(`(?i)(?:^|/)mbid-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:$|[/-])`)

// releaseMBIDFromURL reads the release id off the URL a front-cover fetch ended at. The
// archive serves images out of per-release items, so the redirect lands on a path with
// an "mbid-<uuid>" segment; only that shape counts, and only when the fetch was
// redirected at all, since the request path itself carries the group's id and must
// never be read as a release. "" otherwise.
func releaseMBIDFromURL(reqURL, finalURL string) string {
	if finalURL == "" || finalURL == reqURL {
		return ""
	}
	m := caaItemMBID.FindStringSubmatch(finalURL)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// caaProvider is the Cover Art Archive as a CapCover Provider. It keys on a release
// group's MBID (resolved by the identity spine), fetches the front cover, and decodes
// it to an ArtImage. A missing cover (404) or an undecodable image is a clean
// no-match; a transient fetch error is returned so the Service logs it and continues
// (cover art never aborts a run).
//
// The group fetch records which release's bytes it took; a release ask whose album is
// that release, under a group still holding those bytes, is answered from the record
// instead of downloading the same picture again. A group re-fetch is conditional on the
// recorded validator when the catalog still holds the recorded bytes, so a forced run
// costs a request and no download for a cover the archive has not changed.
type caaProvider struct {
	caa *coverArt
	log *slog.Logger
}

func (p *caaProvider) Name() string             { return providerCoverArt }
func (p *caaProvider) Capabilities() Capability { return CapCover }

func (p *caaProvider) Enrich(ctx context.Context, req Request) (*Candidate, error) {
	var rung string
	switch req.Type {
	case TargetReleaseGroup:
		rung = "release-group"
	case TargetRelease:
		rung = "release"
	default:
		return nil, nil
	}
	if req.MBID == "" {
		return nil, nil
	}
	if req.Type == TargetRelease && req.ReleaseGroupMBID != "" && req.GroupFrontHash != "" && req.Wants(CapCover) {
		rec, ok, err := p.caa.groupFrontRecord(ctx, req.ReleaseGroupMBID)
		switch {
		case err != nil:
			p.log.Debug("cover art group record unreadable; fetching the release front", "group", req.ReleaseGroupMBID, "err", err)
		case ok && rec.Release != "" && rec.Release == strings.ToLower(req.MBID) && rec.Hash == req.GroupFrontHash:
			return &Candidate{FrontIsGroupFront: true}, nil
		}
	}
	// A forced walk re-fetches every group front. The recorded validator turns that into
	// a conditional request for a cover the archive has not changed, and it is sent only
	// when the catalog still holds the recorded bytes, so a cleared or hand-set front is
	// fetched plainly.
	var cond string
	if req.Type == TargetReleaseGroup && req.GroupFrontHash != "" {
		if rec, ok, err := p.caa.groupFrontRecord(ctx, req.MBID); err == nil && ok && rec.Hash == req.GroupFrontHash {
			cond = rec.ETag
		}
	}
	f, err := p.caa.frontCover(ctx, rung, req.MBID, cond)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // no cover at this rung
		}
		return nil, err // transient: the Service logs and skips
	}
	if f.notModified {
		// The archive still serves the bytes the catalog holds: nothing to fetch, nothing
		// to record, and a nil answer leaves the group's front where it is.
		p.log.Debug("cover art unchanged", "group", req.MBID)
		return nil, nil
	}
	data, srcURL := f.data, f.reqURL
	// gatherArt stamps Source and Provider on the winner; the URL is the provider's
	// to report, since only it knows where it fetched.
	// An ISOBMFF cover (AVIF/HEIC) has no pure-Go decoder, so it describes with a
	// sniffed format and no dimensions while still being a perfectly good image to
	// store. Only bytes nothing recognizes at all are discarded; the archive's own
	// Content-Type is not a second chance here, for the reason podcast.fetchImage gives.
	info := art.Describe(data)
	if info.Format == "" {
		// Re-probe for the reason: Describe reports only that nothing recognized the
		// bytes, and an HTML error page and a truncated JPEG are worth telling apart.
		_, _, _, perr := art.Probe(data)
		p.log.Debug("cover art undecodable", "mbid", req.MBID, "bytes", len(data), "err", perr)
		return nil, nil
	}
	if req.Type == TargetReleaseGroup {
		// The record is what lets the album rung reuse these bytes rather than download
		// them again. A write failure costs that reuse, not the cover.
		rec := caaGroupFront{Release: releaseMBIDFromURL(f.reqURL, f.finalURL), Hash: info.Hash, ETag: f.etag}
		if rec.Release == "" {
			// The reuse reads the release off the archive's redirect, so a fetch that named
			// none disables it for this group. Logged because the symptom otherwise looks
			// exactly like a catalog with nothing to reuse.
			p.log.Debug("cover art group fetch named no release; the album rung cannot reuse it",
				"group", req.MBID, "final", f.finalURL)
		}
		if err := p.caa.recordGroupFront(ctx, req.MBID, rec); err != nil {
			p.log.Debug("cover art group record unwritable", "group", req.MBID, "err", err)
		}
	}
	img := &model.ArtImage{
		Data: data, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: model.Attribution{SourceURL: srcURL},
	}
	return &Candidate{Cover: img}, nil
}
