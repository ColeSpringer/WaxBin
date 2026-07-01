package source

import (
	"context"
	"io"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Mock is a scriptable Provider for tests and for standing in for an injected
// provider, such as youtube, without any network. Set Type to the source it serves and
// either the *Func hooks for full control or the simple Feed/Payload fields for the
// common case. It never touches the network.
type Mock struct {
	Type model.SourceType

	// Simple mode: Enumerate returns Feed (with IdentityKey), Fetch writes Payload.
	Feed        *model.Feed
	IdentityKey string
	SourceID    string
	Payload     []byte
	ContentType string

	// Hook mode overrides simple mode when set.
	ResolveFunc   func(ctx context.Context, req Request) (*Resolved, error)
	EnumerateFunc func(ctx context.Context, req Request) (*Enumeration, error)
	FetchFunc     func(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error)
}

// SourceType reports the mock's configured type (manual when unset).
func (m *Mock) SourceType() model.SourceType {
	if m.Type == "" {
		return model.SourceManual
	}
	return m.Type
}

// Resolve returns the scripted resolution, or the identity of the simple-mode feed.
func (m *Mock) Resolve(ctx context.Context, req Request) (*Resolved, error) {
	if m.ResolveFunc != nil {
		return m.ResolveFunc(ctx, req)
	}
	return &Resolved{IdentityKey: m.IdentityKey, SourceID: m.SourceID, SourceType: m.SourceType(), Title: m.title()}, nil
}

// Enumerate returns the scripted enumeration, or the simple-mode feed.
func (m *Mock) Enumerate(ctx context.Context, req Request) (*Enumeration, error) {
	if m.EnumerateFunc != nil {
		return m.EnumerateFunc(ctx, req)
	}
	if m.Feed == nil {
		return nil, waxerr.New(waxerr.CodeInvalid, "source.mock.Enumerate", "mock has no feed configured")
	}
	return &Enumeration{Feed: m.Feed, IdentityKey: m.IdentityKey, SourceID: m.SourceID}, nil
}

// Fetch writes the scripted payload to w and returns its tagged content hash.
func (m *Mock) Fetch(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error) {
	if m.FetchFunc != nil {
		return m.FetchFunc(ctx, req, w)
	}
	hasher, finalize := identity.StreamHasher()
	n, err := w.Write(m.Payload)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "source.mock.Fetch", err)
	}
	_, _ = hasher.Write(m.Payload)
	ct := m.ContentType
	if ct == "" {
		ct = "audio/mpeg"
	}
	return &FetchResult{Bytes: int64(n), ContentHash: finalize(), ContentType: ct}, nil
}

func (m *Mock) title() string {
	if m.Feed != nil {
		return m.Feed.Title
	}
	return ""
}
