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
	"errors"
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
	//
	// The catalog stores the ETag and LastModified an enumeration returns in the same
	// transaction as the show row and every episode of its Feed, and it hands that
	// pair back verbatim in the next Request for the show. Only a committed
	// enumeration writes them: a sync that fails stores neither the pair nor the
	// episodes, and a NotModified answer's pair is ignored in favor of the stored one.
	// A provider may therefore treat the pair as an opaque cursor, and getting it back
	// is the receipt that the enumeration it came from committed. Two syncs of one
	// show can interleave, in which case the pair that comes back is the last to
	// commit rather than the last returned, so a provider must be prepared to hand an
	// item over again; the store treats a repeated item as an update. Enumeration
	// says what a Feed has to carry.
	Enumerate(ctx context.Context, req Request) (*Enumeration, error)
	// Fetch streams one item's media to w, returning the byte count and the tagged
	// content hash computed from the streamed bytes.
	Fetch(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error)
}

// Request is the input for Resolve and Enumerate over a feed or channel URL.
// User/Pass carry optional basic-auth; ETag/LastModified make Enumerate a
// conditional GET. They are the pair the show's last committed enumeration
// returned, and empty when a source is added or re-added (an OPML import re-adds)
// or has never enumerated.
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
//
// Feed is the source's whole current state: its channel fields replace the show's
// stored ones, and episodes it no longer lists are kept. An episode needs a GUID, an
// enclosure URL, or a title to be written; one with none of the three is dropped
// while the validators still commit. A provider that only needs to record new
// validators returns the Feed anyway, since a NotModified answer's are discarded; an
// unchanged episode costs the store one read. IdentityKey is read when a source is
// added, and a sync keeps the show's stored identity; SourceID is not stored, and is
// there for a caller that enumerates a provider directly.
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

// ProviderError marks a failure as the provider's: the source could not be enumerated
// or fetched, or the provider's answer broke the Provider contract. The podcast service
// returns one from every provider call it makes, so a caller can tell a failing feed
// from a failing catalog. Err keeps its waxerr class, which waxerr.CodeOf reads through
// Unwrap. The type lives in-process only; a proxied error carries its class and text.
type ProviderError struct {
	SourceType model.SourceType
	Op         string // "enumerate" or "fetch"
	Err        error
}

func (e *ProviderError) Error() string {
	return string(e.SourceType) + " provider " + e.Op + ": " + e.Err.Error()
}

// Unwrap exposes the provider's own error.
func (e *ProviderError) Unwrap() error { return e.Err }

// IsProviderError reports whether err is, or wraps, a *ProviderError.
func IsProviderError(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe)
}
