package enrich

import (
	"context"
	"log/slog"
	"net/url"

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
}

// coverImageMaxBytes caps a fetched cover; album art is large but bounded.
const coverImageMaxBytes = 24 << 20 // 24 MiB

var coverMIME = []string{"image/*", "application/octet-stream"}

// frontCover returns the raw bytes of one entity's front cover and the URL they were
// requested from, or CodeNotFound when it has none. rung is the archive path segment:
// "release-group" for the group's cover, or "release" for the specific pressing's own.
// The caller decodes and hashes the bytes.
//
// The returned URL is the archive request, not the archive.org object netsafe followed
// the redirect to. The request URL is stable and names the entity, so it stays a useful
// citation; the redirect target is an implementation detail of where the file sits today.
func (c *coverArt) frontCover(ctx context.Context, rung, mbid string) (data []byte, reqURL string, err error) {
	if mbid == "" {
		return nil, "", waxerr.New(waxerr.CodeNotFound, "enrich.coverart", "no mbid")
	}
	reqURL = c.baseURL + "/" + rung + "/" + url.PathEscape(mbid) + "/front"
	resp, err := c.client.Do(ctx, netsafe.Request{
		URL:        reqURL,
		AcceptMIME: coverMIME,
		MaxBytes:   coverImageMaxBytes,
	})
	if err != nil {
		return nil, "", err
	}
	return resp.Body, reqURL, nil
}

// caaProvider is the Cover Art Archive as a CapCover Provider. It keys on a release
// group's MBID (resolved by the identity spine), fetches the front cover, and decodes
// it to an ArtImage. A missing cover (404) or an undecodable image is a clean
// no-match; a transient fetch error is returned so the Service logs it and continues
// (cover art never aborts a run).
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
	data, srcURL, err := p.caa.frontCover(ctx, rung, req.MBID)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // no cover at this rung
		}
		return nil, err // transient: the Service logs and skips
	}
	// gatherCover stamps Source and Provider on the winner; the URL is the provider's
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
	img := &model.ArtImage{
		Data: data, Hash: info.Hash, Format: info.Format, Width: info.Width, Height: info.Height,
		Attribution: model.Attribution{SourceURL: srcURL},
	}
	return &Candidate{Cover: img}, nil
}
