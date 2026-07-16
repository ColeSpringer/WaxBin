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

// frontCover returns the raw bytes of a release group's front cover, or
// CodeNotFound when it has none. The caller decodes and hashes the bytes.
func (c *coverArt) frontCover(ctx context.Context, mbid string) ([]byte, error) {
	if mbid == "" {
		return nil, waxerr.New(waxerr.CodeNotFound, "enrich.coverart", "no mbid")
	}
	resp, err := c.client.Do(ctx, netsafe.Request{
		URL:        c.baseURL + "/release-group/" + url.PathEscape(mbid) + "/front",
		AcceptMIME: coverMIME,
		MaxBytes:   coverImageMaxBytes,
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
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
	if req.Type != TargetReleaseGroup || req.MBID == "" {
		return nil, nil
	}
	data, err := p.caa.frontCover(ctx, req.MBID)
	if err != nil {
		if waxerr.Is(err, waxerr.CodeNotFound) {
			return nil, nil // no cover for this release group
		}
		return nil, err // transient: the Service logs and skips
	}
	img := &model.ArtImage{Data: data, Hash: art.Hash(data)}
	format, w, h, err := art.Probe(data)
	if err != nil {
		p.log.Debug("cover art undecodable", "mbid", req.MBID, "err", err)
		return nil, nil
	}
	img.Format, img.Width, img.Height = format, w, h
	return &Candidate{Cover: img}, nil
}
