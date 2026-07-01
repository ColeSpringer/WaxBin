package source

import (
	"context"
	"io"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Response media types the HTTP provider accepts. Feeds are validated leniently
// (many servers mislabel RSS); enclosures must look like media, not an HTML error
// page (RequireContentType is set on Fetch).
var (
	feedMIME      = []string{"application/rss+xml", "application/atom+xml", "application/xml", "text/xml", "application/x-rss+xml", "text/html", "application/octet-stream", "text/plain"}
	enclosureMIME = []string{"audio/*", "video/*", "application/ogg", "application/mp4", "application/octet-stream", "binary/octet-stream"}
)

// HTTPProvider is the built-in provider for RSS feeds and plain HTTP enclosures. It
// is the default for source_type rss and the fallback fetcher for a manual episode
// with a direct media URL. The RSS parser is injected so this package does not depend
// on the podcast package, which owns ParseFeed.
type HTTPProvider struct {
	client *netsafe.Client
	parse  func([]byte) (*model.Feed, error)
}

// NewHTTP builds the built-in provider over a netsafe client and a feed parser.
func NewHTTP(client *netsafe.Client, parse func([]byte) (*model.Feed, error)) *HTTPProvider {
	return &HTTPProvider{client: client, parse: parse}
}

// SourceType reports rss: an HTTP feed.
func (h *HTTPProvider) SourceType() model.SourceType { return model.SourceRSS }

// Resolve fetches and parses the feed to derive its stable identity (its
// <podcast:guid> or normalized feed URL) and title, without persisting anything.
func (h *HTTPProvider) Resolve(ctx context.Context, req Request) (*Resolved, error) {
	const op = "source.http.Resolve"
	feed, _, err := h.fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	if feed == nil {
		return nil, waxerr.New(waxerr.CodeIO, op, "feed returned not-modified to an unconditional request")
	}
	key := identity.PodcastKey(feed.GUID, req.URL)
	if key == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "feed has no usable identity (url or guid)")
	}
	return &Resolved{IdentityKey: key, SourceType: model.SourceRSS, Title: feed.Title}, nil
}

// Enumerate conditionally fetches and parses the feed, returning its episodes and
// fresh validators. A 304 yields NotModified with the stored validators echoed back.
func (h *HTTPProvider) Enumerate(ctx context.Context, req Request) (*Enumeration, error) {
	feed, resp, err := h.fetch(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.NotModified {
		return &Enumeration{NotModified: true, ETag: resp.ETag, LastModified: resp.LastModified}, nil
	}
	out := &Enumeration{Feed: feed, ETag: resp.ETag, LastModified: resp.LastModified}
	if feed != nil {
		out.IdentityKey = identity.PodcastKey(feed.GUID, req.URL)
	}
	return out, nil
}

// Fetch streams the enclosure to w, computing the content hash as bytes pass so the
// finished file needs no second read. RequireContentType rejects an untyped body
// (an HTML error page masquerading as audio).
func (h *HTTPProvider) Fetch(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error) {
	hasher, finalize := identity.StreamHasher()
	resp, n, err := h.client.Stream(ctx, netsafe.Request{
		URL: req.URL, AcceptMIME: enclosureMIME, RequireContentType: true,
		BasicUser: req.User, BasicPass: req.Pass,
	}, io.MultiWriter(w, hasher), req.MaxBytes)
	if err != nil {
		return nil, err
	}
	// The GET is unconditional (no validators), so a 304 is a misbehaving server/CDN and
	// wrote zero bytes; refuse it rather than pass back a 0-byte "download" that the
	// caller would catalog as complete (mirrors Resolve).
	if resp.NotModified {
		return nil, waxerr.New(waxerr.CodeIO, "source.http.Fetch",
			"enclosure returned not-modified to an unconditional request")
	}
	return &FetchResult{Bytes: n, ContentHash: finalize(), ContentType: resp.ContentType}, nil
}

// fetch does the conditional GET and parse shared by Resolve and Enumerate. On a 304
// it returns a nil feed and resp.NotModified.
func (h *HTTPProvider) fetch(ctx context.Context, req Request) (*model.Feed, *netsafe.Response, error) {
	resp, err := h.client.Do(ctx, netsafe.Request{
		URL: req.URL, AcceptMIME: feedMIME, BasicUser: req.User, BasicPass: req.Pass,
		IfNoneMatch: req.ETag, IfModifiedSince: req.LastModified,
	})
	if err != nil {
		return nil, nil, err
	}
	if resp.NotModified {
		return nil, resp, nil
	}
	feed, err := h.parse(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return feed, resp, nil
}
