// Package netsafe provides the HTTP client used for remote feeds, enclosures,
// transcripts, artwork, and future metadata lookups. It applies the limits WaxBin
// needs around untrusted URLs: http/https only, bounded redirects and response
// bodies, request timeouts, MIME checks, optional blocking of private and loopback
// addresses after DNS resolution, per-host pacing, and safe filenames.
//
// It uses only net/http, so WaxBin keeps its no-CGO build.
package netsafe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/colespringer/waxbin/waxerr"
)

// Defaults applied when a Policy field is left zero.
const (
	defaultTimeout      = 30 * time.Second
	defaultMaxRedirects = 8
	defaultMaxBytes     = 32 << 20 // 32 MiB: generous for a feed/transcript, bounded
)

// Policy configures a Client. Zero fields take package defaults. A negative value
// disables a limit where that makes sense: MaxRedirects<0 forbids redirects, and
// MaxBytes<0 removes the body cap for trusted callers.
type Policy struct {
	// UserAgent is sent on every request; some providers require an identifying UA.
	UserAgent string
	// Timeout bounds the whole request including connect, redirects, and body read.
	Timeout time.Duration
	// MaxRedirects caps the redirect chain (default 8; <0 forbids any redirect).
	MaxRedirects int
	// MaxBytes caps a buffered response body in bytes (default 32 MiB; <0 = no cap).
	// Streaming downloads pass their own cap to Stream.
	MaxBytes int64
	// BlockPrivateIPs refuses to dial loopback/private/link-local/ULA addresses, the
	// SSRF guard. Off by default because self-hosted feeds legitimately live on a
	// LAN; a deployment exposing this to untrusted feed URLs should enable it.
	BlockPrivateIPs bool
	// MinHostInterval is the minimum spacing between requests to the same host, a
	// politeness/rate limit. Zero disables it.
	MinHostInterval time.Duration
}

// Request is one remote fetch. Only URL is required.
type Request struct {
	URL    string
	Method string // default GET
	// Header overrides/additions sent verbatim (the client sets User-Agent and the
	// conditional/auth headers below, which these can override).
	Header map[string]string
	// BasicUser/BasicPass enable HTTP Basic auth for an authenticated/private feed.
	BasicUser string
	BasicPass string
	// IfNoneMatch / IfModifiedSince make the request a conditional GET so an
	// unchanged feed answers 304 and costs no bytes.
	IfNoneMatch     string
	IfModifiedSince string
	// AcceptMIME, when non-empty, is the allow-list the response media type is
	// validated against. Each entry is a full type ("application/rss+xml") or a
	// type wildcard ("audio/*"); an empty list accepts any type. A response whose
	// declared type is outside the list is rejected (CodeInvalid).
	AcceptMIME []string
	// RequireContentType rejects a response that declares no Content-Type at all.
	// Feeds often omit it (parsed leniently), but an enclosure download sets this so
	// an untyped body (e.g. an HTML error page with no header) is not saved as audio.
	RequireContentType bool
	// MaxBytes overrides the policy body cap for this request (0 = policy default).
	MaxBytes int64
}

// Response is the result of a buffered fetch. Body is nil for a 304 (NotModified).
type Response struct {
	Status       int
	Body         []byte
	ContentType  string
	ETag         string
	LastModified string
	NotModified  bool   // server answered 304 to a conditional GET
	FinalURL     string // URL after any redirects
}

// Client is a reusable HTTP client. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	policy  Policy
	limiter *hostLimiter
}

// New builds a Client from a policy, filling defaults.
func New(p Policy) *Client {
	if p.Timeout <= 0 {
		// A non-positive timeout (including a mis-configured negative value) would
		// otherwise disable all HTTP timeouts; fall back to the default.
		p.Timeout = defaultTimeout
	}
	if p.MaxRedirects == 0 {
		p.MaxRedirects = defaultMaxRedirects
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = defaultMaxBytes
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if p.BlockPrivateIPs {
		// Control runs for each connection attempt after the address is resolved to
		// an IP, so a hostname that resolves to a private address (DNS rebinding) is
		// still refused at dial time, on the very IP that would be contacted.
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			return guardDialAddr(address)
		}
	}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: p.Timeout,
	}
	c := &Client{policy: p, limiter: newHostLimiter(p.MinHostInterval)}
	c.http = &http.Client{
		Transport: tr,
		Timeout:   p.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if p.MaxRedirects < 0 || len(via) > p.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}
	return c
}

// Do executes a buffered request and returns the size-capped response body. It is
// for small/medium resources (feeds, transcripts, OPML); use Stream for large
// enclosures.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	const op = "netsafe.Do"
	resp, body, err := c.do(ctx, r, op)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := newResponse(resp)
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		return out, nil
	}
	if err := checkStatus(resp.StatusCode, op, r.URL); err != nil {
		return nil, err
	}
	if err := validateMIME(out.ContentType, r.AcceptMIME, r.RequireContentType, op); err != nil {
		return nil, err
	}

	max := r.MaxBytes
	if max == 0 {
		max = c.policy.MaxBytes
	}
	data, err := readCapped(body, max)
	if err != nil {
		return nil, err
	}
	out.Body = data
	return out, nil
}

// Stream executes a request and copies its body to dst, enforcing a byte cap, for
// large downloads (episode enclosures) that must not be buffered in memory. It
// returns response metadata (Body is nil). maxBytes<=0 uses the policy cap.
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, maxBytes int64) (*Response, int64, error) {
	const op = "netsafe.Stream"
	resp, body, err := c.do(ctx, r, op)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	out := newResponse(resp)
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		return out, 0, nil
	}
	if err := checkStatus(resp.StatusCode, op, r.URL); err != nil {
		return nil, 0, err
	}
	if err := validateMIME(out.ContentType, r.AcceptMIME, r.RequireContentType, op); err != nil {
		return nil, 0, err
	}

	if maxBytes <= 0 {
		maxBytes = c.policy.MaxBytes
	}
	// Copy one past the cap so an oversized body is detected rather than silently
	// truncated to a corrupt file.
	n, err := io.Copy(dst, io.LimitReader(body, maxBytes+1))
	if err != nil {
		return nil, n, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if n > maxBytes {
		return nil, n, waxerr.New(waxerr.CodeInvalid, op,
			fmt.Sprintf("response from %s exceeds %d-byte limit", r.URL, maxBytes))
	}
	return out, n, nil
}

// do builds and sends the request, applying the rate limit, headers, auth, and
// conditional-GET headers. The caller owns closing resp.Body.
func (c *Client) do(ctx context.Context, r Request, op string) (*http.Response, io.Reader, error) {
	if strings.TrimSpace(r.URL) == "" {
		return nil, nil, waxerr.New(waxerr.CodeInvalid, op, "empty url")
	}
	if err := checkScheme(r.URL, op); err != nil {
		return nil, nil, err
	}
	if err := c.limiter.wait(ctx, r.URL); err != nil {
		return nil, nil, waxerr.FromContext(op, err, waxerr.CodeCanceled)
	}

	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL, nil)
	if err != nil {
		return nil, nil, waxerr.Wrap(waxerr.CodeInvalid, op, err)
	}
	if c.policy.UserAgent != "" {
		req.Header.Set("User-Agent", c.policy.UserAgent)
	}
	if r.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", r.IfNoneMatch)
	}
	if r.IfModifiedSince != "" {
		req.Header.Set("If-Modified-Since", r.IfModifiedSince)
	}
	if r.BasicUser != "" || r.BasicPass != "" {
		req.SetBasicAuth(r.BasicUser, r.BasicPass)
	}
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A redirect/SSRF/timeout rejection surfaces here; classify it as an I/O error
		// (or canceled when the context ended).
		if ctx.Err() != nil {
			return nil, nil, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
		}
		return nil, nil, waxerr.Wrapf(waxerr.CodeIO, op, err, "fetching %s", r.URL)
	}
	return resp, resp.Body, nil
}

func newResponse(resp *http.Response) *Response {
	ct := resp.Header.Get("Content-Type")
	media := ct
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		media = mt
	}
	final := ""
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return &Response{
		Status:       resp.StatusCode,
		ContentType:  media,
		ETag:         strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified: strings.TrimSpace(resp.Header.Get("Last-Modified")),
		FinalURL:     final,
	}
}

// checkStatus rejects a non-2xx response.
func checkStatus(status int, op, url string) error {
	if status >= 200 && status < 300 {
		return nil
	}
	code := waxerr.CodeIO
	if status == http.StatusNotFound {
		code = waxerr.CodeNotFound
	}
	return waxerr.New(code, op, fmt.Sprintf("%s returned HTTP %d", url, status))
}

// readCapped reads up to max bytes (max<0 = unbounded), erroring if the body would
// exceed the cap rather than truncating it.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, waxerr.Wrap(waxerr.CodeIO, "netsafe.read", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "netsafe.read", err)
	}
	if int64(len(data)) > max {
		return nil, waxerr.New(waxerr.CodeInvalid, "netsafe.read",
			fmt.Sprintf("response exceeds %d-byte limit", max))
	}
	return data, nil
}

// validateMIME checks the response media type against an allow-list. An empty
// allow-list accepts anything; an empty/absent Content-Type is accepted (many
// feeds omit it) so validation never rejects on a missing header alone.
func validateMIME(media string, allow []string, requireCT bool, op string) error {
	if media == "" {
		if requireCT {
			return waxerr.New(waxerr.CodeInvalid, op, "response declared no content type")
		}
		return nil
	}
	if len(allow) == 0 {
		return nil
	}
	media = strings.ToLower(strings.TrimSpace(media))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasSuffix(a, "/*") {
			if strings.HasPrefix(media, strings.TrimSuffix(a, "*")) {
				return nil
			}
			continue
		}
		if media == a {
			return nil
		}
	}
	return waxerr.New(waxerr.CodeInvalid, op, "unexpected response content type: "+media)
}

// checkScheme allows only http(s); it blocks file://, ftp://, gopher://, and the
// like, which an SSRF or a malicious feed could otherwise use.
func checkScheme(rawURL, op string) error {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "url has no scheme: "+rawURL)
	}
	switch strings.ToLower(rawURL[:i]) {
	case "http", "https":
		return nil
	default:
		return waxerr.New(waxerr.CodeInvalid, op, "unsupported url scheme: "+rawURL[:i])
	}
}

// guardDialAddr rejects a dial to a private/loopback/link-local/unspecified
// address. It is the Control hook, so it sees the resolved IP that would actually
// be contacted.
func guardDialAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("netsafe: cannot parse dial address " + address)
	}
	if isBlockedIP(ip) {
		return errors.New("netsafe: refusing to connect to non-public address " + ip.String())
	}
	return nil
}

// isBlockedIP reports whether ip is in a range the SSRF guard refuses: loopback,
// RFC1918 private + ULA (IsPrivate), link-local uni/multicast, unspecified, and
// IPv4-mapped forms of any of those.
func isBlockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsInterfaceLocalMulticast()
}

// SafeFilename derives a safe local filename from a remote URL or
// Content-Disposition value. It keeps only the final path segment, strips
// directory separators and control characters, rejects "." traversal, and falls
// back to fallback when nothing usable remains. The result never contains a path
// separator, so a remote source can never write outside its intended directory.
func SafeFilename(remote, fallback string) string {
	s := remote
	// Drop a query/fragment so "ep.mp3?token=..." yields "ep.mp3".
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	// For a URL, keep only the path (drop scheme://host) so a host like "h" is never
	// mistaken for a filename.
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			s = rest[j:]
		} else {
			s = "" // no path component -> fallback
		}
	}
	// Take the last path element of either a URL path or a Windows-style path.
	s = path.Base(strings.ReplaceAll(s, "\\", "/"))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < 0x20 || r == 0x7f:
			// drop control characters
		case r == '/' || r == '\\' || r == 0:
			// never a separator
		case strings.ContainsRune(`:*?"<>|`, r):
			// drop characters reserved on Windows, so a name derived from an arbitrary
			// remote URL is safe to create cross-platform, not just on the host OS.
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".") // no "", ".", ".." and no leading/trailing dots
	if out == "" {
		return fallback
	}
	return out
}

// hostLimiter spaces successive requests to one host by at least minInterval. A
// zero interval makes wait a no-op.
type hostLimiter struct {
	min  time.Duration
	mu   sync.Mutex
	next map[string]time.Time
}

func newHostLimiter(min time.Duration) *hostLimiter {
	return &hostLimiter{min: min, next: map[string]time.Time{}}
}

// wait blocks until this host's next slot, honoring context cancellation. It
// reserves the slot before sleeping so concurrent callers to the same host queue
// rather than all firing at once.
func (l *hostLimiter) wait(ctx context.Context, rawURL string) error {
	if l == nil || l.min <= 0 {
		return ctx.Err()
	}
	host := hostOf(rawURL)

	l.mu.Lock()
	now := time.Now()
	start := l.next[host]
	if start.Before(now) {
		start = now
	}
	l.next[host] = start.Add(l.min)
	l.mu.Unlock()

	delay := time.Until(start)
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}
