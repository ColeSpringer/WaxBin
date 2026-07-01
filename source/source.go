// Package source defines the provider interface used to resolve, enumerate, and
// fetch remote media. Each source type gets its own implementation. WaxBin ships a
// netsafe HTTPProvider for RSS feeds and plain enclosures plus a test Mock; other
// providers, such as youtube, can be injected by an embedding module. Podcast sync
// and retention dispatch through a show's source_type.
//
// Plain HTTP work stays in WaxBin/netsafe. Platforms that need dedicated extraction
// code live behind injected providers; WaxBin never performs that extraction itself.
package source

import (
	"context"
	"io"

	"github.com/colespringer/waxbin/model"
)

// Provider resolves URLs and fetches media for one source type (rss, youtube, ...).
// Implementations are safe for concurrent use.
type Provider interface {
	// SourceType is the source_type this provider serves; a show with that type is
	// dispatched to it.
	SourceType() model.SourceType
	// Resolve inspects a URL and reports the show identity it maps to, without
	// enumerating its items. It is the lightweight identity probe.
	Resolve(ctx context.Context, req Request) (*Resolved, error)
	// Enumerate lists the current items available at a feed/channel URL as a
	// normalized Feed, honoring the conditional-GET validators in req so an unchanged
	// source can answer NotModified.
	Enumerate(ctx context.Context, req Request) (*Enumeration, error)
	// Fetch streams one item's media to w, returning the byte count and the tagged
	// content hash computed from the streamed bytes.
	Fetch(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error)
}

// Request is the input for Resolve and Enumerate over a feed or channel URL.
// User/Pass carry optional basic-auth; ETag/LastModified make Enumerate a
// conditional GET.
type Request struct {
	URL          string
	User         string
	Pass         string
	ETag         string
	LastModified string
}

// Resolved is a show's identity as a provider reads it from a URL.
type Resolved struct {
	IdentityKey string           // stable show identity (rss: PodcastKey; youtube: youtube:channel:id)
	SourceID    string           // provider-native id (channel/playlist), empty for rss
	SourceType  model.SourceType // the provider's source type
	Title       string
}

// Enumeration is a source's current items plus the fresh conditional-GET validators
// and the resolved identity. Feed is nil when NotModified.
type Enumeration struct {
	NotModified  bool
	Feed         *model.Feed
	ETag         string
	LastModified string
	IdentityKey  string
	SourceID     string
}

// FetchRequest names one item's media to download. MaxBytes bounds the stream
// (0 = the provider/client default).
type FetchRequest struct {
	URL      string
	User     string
	Pass     string
	MaxBytes int64
}

// FetchResult reports a completed media fetch.
type FetchResult struct {
	Bytes       int64
	ContentHash string // identity-tagged hash of the streamed bytes
	ContentType string
}
