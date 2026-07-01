package enrich

import (
	"context"
	"net/url"

	"github.com/colespringer/waxbin/internal/netsafe"
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
