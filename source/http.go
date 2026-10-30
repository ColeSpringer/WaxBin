package source

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/colespringer/waxbin/identity"
	"github.com/colespringer/waxbin/internal/netsafe"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Response media types the HTTP provider accepts. Feeds are validated leniently
// (many servers mislabel RSS). An enclosure may come with a media type or none, but
// never as a playlist or streaming manifest, which names media rather than being it;
// a page or a text body is caught by its bytes (sniffWriter).
var (
	feedMIME             = []string{"application/rss+xml", "application/atom+xml", "application/xml", "text/xml", "application/x-rss+xml", "text/html", "application/octet-stream", "text/plain"}
	enclosureMIME        = []string{"audio/*", "video/*", "application/ogg", "application/mp4", "application/mpeg", "application/octet-stream", "binary/octet-stream"}
	enclosureRefusedMIME = []string{"audio/x-mpegurl", "audio/mpegurl", "application/vnd.apple.mpegurl", "application/x-mpegurl",
		"audio/x-scpls", "application/pls+xml", "video/x-ms-asx", "video/x-ms-wvx", "video/x-ms-wax", "audio/x-ms-wax",
		"audio/x-pn-realaudio", "audio/vnd.rn-realaudio", "application/dash+xml", "application/smil+xml"}
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
// finished file needs no second read. A playlist or manifest type is refused, and so is
// a body that is empty or reads as text, a web page or a playlist under any type or
// none, with ReasonMediaType; the body's head is held back until it is judged, so a
// refused body writes nothing.
func (h *HTTPProvider) Fetch(ctx context.Context, req FetchRequest, w io.Writer) (*FetchResult, error) {
	const op = "source.http.Fetch"
	hasher, finalize := identity.StreamHasher()
	sniff := &sniffWriter{w: io.MultiWriter(w, hasher)}
	resp, n, err := h.client.Stream(ctx, netsafe.Request{
		URL: req.URL, AcceptMIME: enclosureMIME, RefuseMIME: enclosureRefusedMIME,
		Credentials: login(cmp.Or(req.AuthOrigin, req.URL), req.User, req.Pass),
	}, sniff, req.MaxBytes)
	if err != nil {
		return nil, err
	}
	// The GET is unconditional (no validators), so a 304 is a misbehaving server/CDN and
	// wrote zero bytes; refuse it rather than pass back a 0-byte "download" that the
	// caller would catalog as complete (mirrors Resolve).
	if resp.NotModified {
		return nil, waxerr.New(waxerr.CodeIO, op, "enclosure returned not-modified to an unconditional request")
	}
	if err := sniff.flush(false); err != nil {
		return nil, err
	}
	return &FetchResult{Bytes: n, ContentHash: finalize(), ContentType: resp.ContentType}, nil
}

// sniffLen is how much of an enclosure sniffWriter judges before it writes any of it.
const sniffLen = 512

// sniffWriter holds an enclosure's first sniffLen bytes until it can judge them, and
// passes everything after them straight to w. A writer sees no end of stream, so the
// caller flushes it when the stream ends, which is where a shorter body is judged and
// written.
type sniffWriter struct {
	w      io.Writer
	head   []byte
	judged bool
}

func (s *sniffWriter) Write(p []byte) (int, error) {
	if s.judged {
		return s.w.Write(p)
	}
	n := min(len(p), sniffLen-len(s.head))
	s.head = append(s.head, p[:n]...)
	if len(s.head) < sniffLen {
		return len(p), nil
	}
	if err := s.flush(true); err != nil {
		return 0, err
	}
	m, err := s.w.Write(p[n:])
	return n + m, err
}

// flush judges the held head and writes it through; cut says more of the body follows.
func (s *sniffWriter) flush(cut bool) error {
	const op = "source.http.Fetch"
	if s.judged {
		return nil
	}
	s.judged = true
	if sniffRefused(s.head, cut) {
		return waxerr.New(waxerr.CodeInvalid, op, "the enclosure is not media: it is empty or reads as text, a web page or a playlist").
			WithReason(ReasonMediaType)
	}
	_, err := s.w.Write(s.head)
	s.head = nil
	return waxerr.Wrap(waxerr.CodeIO, op, err)
}

// sniffRefused reports whether an enclosure's head is not media: empty, a web page or a
// playlist (after any byte order mark and whitespace it opens with "<", "#EXTM3U" or
// "[playlist]"), or text, which the first bytes of no audio container are. A UTF-16 head
// is read as the ASCII it spells. http.DetectContentType is not enough here: it calls any
// head opening FF FE UTF-16 text, which an MPEG-1 Layer I frame with a CRC also opens
// with, and it takes bytes UTF-8 cannot hold for text.
func sniffRefused(head []byte, cut bool) bool {
	if len(head) == 0 {
		return true
	}
	text, whole := head, true
	if ascii, all, ok := fromUTF16(head); ok {
		text, whole = ascii, all
	}
	text = bytes.TrimPrefix(text, []byte("\xef\xbb\xbf"))
	opening := bytes.TrimLeft(text, " \t\r\n\f\v")
	if bytes.HasPrefix(opening, []byte("<")) || hasPrefixFold(opening, "#EXTM3U") || hasPrefixFold(opening, "[playlist]") {
		return true
	}
	return whole && textual(text, cut)
}

// textual reports whether b is text: UTF-8 with no control character but whitespace.
// Audio carries a zero byte, a 0xff sync byte or another byte UTF-8 cannot hold in its
// first bytes. A rune split at the end of a cut head still reads as text.
func textual(b []byte, cut bool) bool {
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && n == 1:
			return cut && !utf8.FullRune(b)
		case r < 0x20 && !strings.ContainsRune("\t\n\v\f\r", r), r == 0x7f:
			return false
		}
		b = b[n:]
	}
	return true
}

// fromUTF16 reads a head that opens with a UTF-16 byte order mark as the ASCII it
// spells, up to the first code unit outside ASCII; all says it spelled the whole head.
func fromUTF16(head []byte) (ascii []byte, all, ok bool) {
	hi, lo := 0, 1
	switch {
	case bytes.HasPrefix(head, []byte{0xff, 0xfe}):
		hi, lo = 1, 0
	case !bytes.HasPrefix(head, []byte{0xfe, 0xff}):
		return nil, false, false
	}
	for units := head[2:]; len(units) >= 2; units = units[2:] {
		if units[hi] != 0 || units[lo] >= 0x80 {
			return ascii, false, true
		}
		ascii = append(ascii, units[lo])
	}
	return ascii, true, true
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// fetch does the conditional GET and parse shared by Resolve and Enumerate. On a 304
// it returns a nil feed and resp.NotModified.
func (h *HTTPProvider) fetch(ctx context.Context, req Request) (*model.Feed, *netsafe.Response, error) {
	resp, err := h.client.Do(ctx, netsafe.Request{
		URL: req.URL, AcceptMIME: feedMIME, Credentials: login(req.URL, req.User, req.Pass),
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

// login scopes a show's login to the origin of the URL given, or is nil when there is
// none.
func login(origin, user, pass string) *netsafe.Credentials {
	if user == "" && pass == "" {
		return nil
	}
	return &netsafe.Credentials{Origin: origin, User: user, Pass: pass}
}
