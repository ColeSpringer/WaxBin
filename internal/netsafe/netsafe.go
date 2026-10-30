// Package netsafe provides the HTTP client used for remote feeds, enclosures,
// transcripts, artwork, and future metadata lookups. It applies the limits WaxBin
// needs around untrusted URLs: http/https only, bounded redirects and response
// bodies, request timeouts, MIME checks, optional blocking of private and loopback
// addresses after DNS resolution, per-host pacing, logins scoped to their origin, and
// safe filenames. Its errors name a URL's scheme and host, never its login, path or
// query, and carry a reason (failures.go).
//
// It uses only net/http, so WaxBin keeps its no-CGO build.
package netsafe

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
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
	// Timeout bounds a buffered request whole, connect, redirects and body read
	// included. A stream gets the same bound until its answer arrives, and after that
	// fails only when Timeout passes with no byte of body, so a download runs for as
	// long as it keeps arriving.
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
	// conditional headers below, which these can override).
	Header map[string]string
	// Credentials, when set, send an HTTP Basic login to the hops at their origin and to
	// no other host a redirect reaches.
	Credentials *Credentials
	// IfNoneMatch / IfModifiedSince make the request a conditional GET so an
	// unchanged feed answers 304 and costs no bytes.
	IfNoneMatch     string
	IfModifiedSince string
	// AcceptMIME, when non-empty, is the allow-list the response media type is
	// validated against. Each entry is a full type ("application/rss+xml") or a
	// type wildcard ("audio/*"); an empty list accepts any type. A response whose
	// declared type is outside the list is rejected (CodeInvalid).
	AcceptMIME []string
	// RefuseMIME lists types refused even where AcceptMIME admits their family, in the
	// same spelling.
	RefuseMIME []string
	// MaxBytes overrides the policy body cap for this request (0 = policy default).
	MaxBytes int64
	// Body, when non-empty, is sent as the request body; Method then defaults to POST.
	// It is for requests whose parameters are too large for the URL, such as an
	// AcoustID lookup carrying a multi-kilobyte fingerprint. ContentType sets its media
	// type (e.g. application/x-www-form-urlencoded).
	Body        []byte
	ContentType string
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
	Permanent    bool   // every redirect to FinalURL was a 301 or 308; false with none
}

// Client is a reusable HTTP client. It is safe for concurrent use.
type Client struct {
	http    *http.Client
	stream  *http.Client // the same transport with no deadline on the whole request
	policy  Policy
	limiter *hostLimiter
}

// New builds a Client from a policy, filling defaults. A request's Credentials go on
// it and on each redirect hop at their origin, and on no other; the redirect policy caps
// the chain and drops the Referer Go would send to each hop.
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
	redirect := func(req *http.Request, via []*http.Request) error {
		// Go names the previous URL, query and all, which can carry a token.
		req.Header.Del("Referer")
		if p.MaxRedirects < 0 || len(via) > p.MaxRedirects {
			return &RedirectLimitError{Hops: len(via)}
		}
		// net/http converts a POST to GET and drops the body on a 301/302/303. For
		// a body-carrying request (an AcoustID lookup) that would send a body-less
		// GET, so refuse the method-downgrading redirect rather than silently
		// mangling it (307/308 keep the method and resend the body via GetBody).
		if prev := via[len(via)-1]; prev.Method == http.MethodPost && req.Method != http.MethodPost {
			return &downgradeError{method: req.Method}
		}
		// Go has copied the first request's headers, a login among them, to any hop
		// on its domain or below; the hop's own origin decides instead.
		applyLogin(req)
		noteHop(req)
		return nil
	}
	c.http = &http.Client{Transport: tr, Timeout: p.Timeout, CheckRedirect: redirect}
	c.stream = &http.Client{Transport: tr, CheckRedirect: redirect}
	return c
}

// downgradeError is a redirect that would turn a POST into a body-less request.
type downgradeError struct{ method string }

func (e *downgradeError) Error() string {
	return "refusing redirect that downgrades POST to " + e.method + " (request body would be dropped)"
}

// Do executes a buffered request and returns the size-capped response body. It is
// for small/medium resources (feeds, transcripts, OPML); use Stream for large
// enclosures.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	const op = "netsafe.Do"
	req, err := c.request(ctx, r, op)
	if err != nil {
		return nil, err
	}
	if err := c.pace(ctx, req, op); err != nil {
		return nil, err
	}
	resp, err := c.send(ctx, c.http, req, r.URL, op)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out := newResponse(resp)
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		return out, nil
	}
	if err := checkStatus(resp.StatusCode, op, cmp.Or(out.FinalURL, r.URL), r.Credentials); err != nil {
		return nil, err
	}
	if err := validateMIME(mediaTypes(resp.Header.Get("Content-Type")), r, op); err != nil {
		return nil, err
	}

	max := r.MaxBytes
	if max == 0 {
		max = c.policy.MaxBytes
	}
	data, err := readCapped(ctx, resp.Body, max)
	if err != nil {
		return nil, err
	}
	out.Body = data
	return out, nil
}

// Stream executes a request and copies its body to dst, enforcing a byte cap, for
// large downloads (episode enclosures) that must not be buffered in memory. It
// returns response metadata (Body is nil). maxBytes<=0 uses the policy cap. The
// policy's Timeout bounds its silences rather than its length: no answer within it,
// or no byte of body for that long, fails the stream as a transport failure. Time
// spent waiting out the host's pace or writing to dst is not silence.
func (c *Client) Stream(ctx context.Context, r Request, dst io.Writer, maxBytes int64) (*Response, int64, error) {
	const op = "netsafe.Stream"
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	trail := &hopTrail{}
	req, err := c.request(context.WithValue(sctx, hopKey{}, trail), r, op)
	if err != nil {
		return nil, 0, err
	}
	trail.url = req.URL
	if err := c.pace(ctx, req, op); err != nil {
		return nil, 0, err
	}
	quiet := newWatchdog(c.policy.Timeout, cancel)
	defer quiet.stop()
	resp, err := c.send(sctx, c.stream, req, r.URL, op)
	if err != nil {
		if ctx.Err() == nil && quiet.fired() {
			return nil, 0, quiet.err(op, trail.url.String())
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	quiet.progress()

	out := newResponse(resp)
	final := cmp.Or(out.FinalURL, r.URL)
	if resp.StatusCode == http.StatusNotModified {
		out.NotModified = true
		return out, 0, nil
	}
	if err := checkStatus(resp.StatusCode, op, final, r.Credentials); err != nil {
		return nil, 0, err
	}
	if err := validateMIME(mediaTypes(resp.Header.Get("Content-Type")), r, op); err != nil {
		return nil, 0, err
	}

	if maxBytes <= 0 {
		maxBytes = c.policy.MaxBytes
	}
	// Copy one past the cap so an oversized body is detected rather than silently
	// truncated to a corrupt file.
	n, err := copyBody(ctx, op, dst, resp.Body, maxBytes, quiet)
	quiet.stop()
	switch {
	case err != nil && ctx.Err() == nil && quiet.fired():
		return nil, n, quiet.err(op, final)
	case err != nil:
		return nil, n, err
	case n > maxBytes:
		return nil, n, &waxerr.Error{Code: waxerr.CodeInvalid, Op: op, Err: &TooLargeError{Limit: maxBytes, Host: shownHost(final)},
			Reason: ReasonTooLarge}
	}
	return out, n, nil
}

// watchdog cancels a stream that goes quiet for span: no answer, or no byte of body.
// A nil watchdog does nothing.
type watchdog struct {
	span  time.Duration
	timer *time.Timer
	quiet atomic.Bool
}

func newWatchdog(span time.Duration, cancel context.CancelFunc) *watchdog {
	w := &watchdog{span: span}
	w.timer = time.AfterFunc(span, func() { w.quiet.Store(true); cancel() })
	return w
}

func (w *watchdog) progress() {
	if w != nil {
		w.timer.Reset(w.span)
	}
}

func (w *watchdog) stop() {
	if w != nil {
		w.timer.Stop()
	}
}

func (w *watchdog) fired() bool { return w.quiet.Load() }

func (w *watchdog) err(op, rawURL string) error {
	return waxerr.New(waxerr.CodeIO, op, fmt.Sprintf("nothing arrived from %s for %s", shownHost(rawURL), w.span)).
		WithReason(ReasonTransport)
}

// hopTrail records the URL a stream last asked, its first request or a redirect hop, so
// a stall names the host that went quiet.
type hopTrail struct{ url *url.URL }

type hopKey struct{}

func noteHop(req *http.Request) {
	if t, ok := req.Context().Value(hopKey{}).(*hopTrail); ok {
		t.url = req.URL
	}
}

// request checks r and builds its request, with r's login on it when its URL is at the
// login's origin. The caller owns closing the answer's body.
func (c *Client) request(ctx context.Context, r Request, op string) (*http.Request, error) {
	if strings.TrimSpace(r.URL) == "" {
		return nil, waxerr.New(waxerr.CodeInvalid, op, "empty url")
	}
	if err := checkScheme(r.URL, op); err != nil {
		return nil, err
	}
	method := r.Method
	if method == "" {
		if len(r.Body) > 0 {
			method = http.MethodPost
		} else {
			method = http.MethodGet
		}
	}
	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(withLogin(ctx, r.Credentials), method, r.URL, body)
	if err != nil {
		// The parse error repeats the URL, login and query included.
		return nil, waxerr.New(waxerr.CodeInvalid, op, "invalid url")
	}
	if r.ContentType != "" {
		req.Header.Set("Content-Type", r.ContentType)
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
	applyLogin(req)
	for k, v := range r.Header {
		req.Header.Set(k, v)
	}
	return req, nil
}

// pace waits out req's host's pace.
func (c *Client) pace(ctx context.Context, req *http.Request, op string) error {
	return waxerr.FromContext(op, c.limiter.wait(ctx, paceKey(req.URL)), waxerr.CodeCanceled)
}

// send sends req with hc, rawURL being what the caller asked for.
func (c *Client) send(ctx context.Context, hc *http.Client, req *http.Request, rawURL, op string) (*http.Response, error) {
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
		}
		return nil, unanswered(op, rawURL, err)
	}
	return resp, nil
}

// unanswered classifies a request that got no answer: refused by the address guard, cut
// off at the redirect limit, or lost in transport. Go's *url.Error repeats the URL with
// its query, so only its cause is kept, and any URL that cause's own text names is cut
// to its scheme and host.
func unanswered(op, rawURL string, err error) error {
	at := shownHost(rawURL)
	if ue, ok := err.(*url.Error); ok {
		err = ue.Err
		// The URL a hop failed at, when Go names it whole; a refused redirect names the
		// Location as written, which can be relative.
		at = cmp.Or(shownHost(ue.URL), at)
	}
	reason := ReasonTransport
	var blocked *BlockedAddressError
	var hops *RedirectLimitError
	var downgrade *downgradeError
	switch {
	case errors.As(err, &blocked):
		reason = ReasonBlockedAddress
	case errors.As(err, &hops), errors.As(err, &downgrade):
		reason = ReasonRedirect
	case strings.HasPrefix(err.Error(), "failed to parse Location header"):
		// Go's text repeats the Location twice, and a relative one has no host to cut to.
		err, reason = errors.New("the redirect's Location does not parse"), ReasonRedirect
	case strings.Contains(err.Error(), "://"):
		err = &hostsOnly{err}
	}
	msg := "fetching"
	if at != "" {
		msg += " " + at
	}
	return &waxerr.Error{Code: waxerr.CodeIO, Op: op, Msg: msg, Err: err, Reason: reason}
}

// hostsOnly reads as err with every absolute URL in its text cut to scheme and host.
type hostsOnly struct{ err error }

func (h *hostsOnly) Error() string { return redactURLs(h.err.Error()) }
func (h *hostsOnly) Unwrap() error { return h.err }

// redactURLs cuts every absolute URL in s to its scheme and host.
func redactURLs(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "://")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		start := i
		for start > 0 && isSchemeByte(s[start-1]) {
			start--
		}
		end := i + len("://")
		for end < len(s) && !strings.ContainsRune(" \t\r\n\"'<>`", rune(s[end])) {
			end++
		}
		b.WriteString(s[:start])
		b.WriteString(hostPart(s[start:end]))
		s = s[end:]
	}
}

func isSchemeByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'
}

// hostPart is a URL's scheme and host, read by its text alone when it does not parse.
func hostPart(raw string) string {
	if h := shownHost(raw); h != "" {
		return h
	}
	scheme, rest, _ := strings.Cut(raw, "://")
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	return scheme + "://" + rest
}

// bodyErr classifies a failure reading an answer's body: a cancel when the caller's
// context ended, and the transport's otherwise, the client's own timeout included.
func bodyErr(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return waxerr.FromContext(op, ctx.Err(), waxerr.CodeCanceled)
	}
	return &waxerr.Error{Code: waxerr.CodeIO, Op: op, Err: err, Reason: ReasonTransport}
}

// copyBody copies at most max+1 bytes of body to dst, so an oversized body shows. Each
// read that brings bytes resets quiet, which stands still while dst writes. A failed read
// is the transport's; a writer that refuses keeps its own class and reason, even when
// the read that fed it failed too.
func copyBody(ctx context.Context, op string, dst io.Writer, body io.Reader, max int64, quiet *watchdog) (int64, error) {
	src := &failedRead{r: io.LimitReader(body, max+1), quiet: quiet}
	n, err := io.Copy(&quietWrite{w: dst, quiet: quiet}, src)
	switch {
	case err == nil:
		return n, nil
	case err == src.err:
		return n, bodyErr(ctx, op, err)
	default:
		return n, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
}

// failedRead remembers its reader's failure, so a copy's error can be told apart from its
// destination's.
type failedRead struct {
	r     io.Reader
	err   error
	quiet *watchdog
}

func (f *failedRead) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if n > 0 {
		f.quiet.progress()
	}
	if err != nil && err != io.EOF {
		f.err = err
	}
	return n, err
}

// quietWrite holds a stream's watchdog still while its destination writes, so a slow
// disk does not read as a quiet source.
type quietWrite struct {
	w     io.Writer
	quiet *watchdog
}

func (q *quietWrite) Write(p []byte) (int, error) {
	q.quiet.stop()
	defer q.quiet.progress()
	return q.w.Write(p)
}

// shownHost is what an error may say of a URL: its scheme and host, never a login, path
// or query, any of which can carry a token. A URL that does not parse, or names no host,
// shows nothing.
func shownHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func newResponse(resp *http.Response) *Response {
	media := ""
	if types := mediaTypes(resp.Header.Get("Content-Type")); len(types) > 0 {
		media = types[0]
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
		Permanent:    permanent(resp),
	}
}

// mediaTypes reads a Content-Type header as the media types it lists, lowercased. One
// header naming several is malformed but seen, and neither a malformed parameter nor a
// second type hides a refused one; a part that names no type is skipped.
func mediaTypes(header string) []string {
	var out []string
	for part := range strings.SplitSeq(header, ",") {
		mt, _, err := mime.ParseMediaType(part)
		if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
			t, _, _ := strings.Cut(part, ";")
			mt = strings.ToLower(strings.TrimSpace(t))
		}
		if strings.Contains(mt, "/") {
			out = append(out, mt)
		}
	}
	return out
}

// checkStatus rejects a non-2xx response from rawURL. A refusal of a request that had a
// login it did not send there says where the login goes.
func checkStatus(status int, op, rawURL string, login *Credentials) error {
	if status >= 200 && status < 300 {
		return nil
	}
	code := waxerr.CodeIO
	if status == http.StatusNotFound {
		code = waxerr.CodeNotFound
	}
	se := &StatusError{Status: status, Host: shownHost(rawURL)}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		o, ok := originOf(cmp.Or(login, &Credentials{}).Origin)
		hop, hok := originOf(rawURL)
		if ok && hok && !o.admits(hop) {
			se.LoginOrigin = shownHost(login.Origin)
		}
	}
	return &waxerr.Error{Code: code, Op: op, Err: se, Reason: ReasonHTTPStatus}
}

// TooLarge reports whether err is a response body over the byte cap. That is the same
// answer every time the resource is fetched, so a caller asking about many resources
// can take it as an answer about that one, where a status or a refused media type can
// come from a service that is not answering at all.
func TooLarge(err error) bool {
	var e *TooLargeError
	return errors.As(err, &e)
}

// readCapped reads up to max bytes (max<0 = unbounded), erroring if the body would
// exceed the cap rather than truncating it.
func readCapped(ctx context.Context, r io.Reader, max int64) ([]byte, error) {
	if max < 0 {
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, bodyErr(ctx, "netsafe.read", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, bodyErr(ctx, "netsafe.read", err)
	}
	if int64(len(data)) > max {
		return nil, &waxerr.Error{Code: waxerr.CodeInvalid, Op: "netsafe.read", Err: &TooLargeError{Limit: max}, Reason: ReasonTooLarge}
	}
	return data, nil
}

// validateMIME checks an answer's media types against the request's lists: any refused
// type refuses it, and the first must be in a non-empty allow-list. An absent
// Content-Type is accepted (many feeds omit it, and an enclosure is judged by its bytes).
func validateMIME(types []string, r Request, op string) error {
	for _, t := range types {
		if matchMIME(t, r.RefuseMIME) {
			return mediaTypeErr(op, t)
		}
	}
	if len(types) > 0 && len(r.AcceptMIME) > 0 && !matchMIME(types[0], r.AcceptMIME) {
		return mediaTypeErr(op, types[0])
	}
	return nil
}

// matchMIME reports whether media is in list, whose entries are full types or type
// wildcards such as "audio/*".
func matchMIME(media string, list []string) bool {
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
		case strings.HasSuffix(a, "/*"):
			if strings.HasPrefix(media, strings.TrimSuffix(a, "*")) {
				return true
			}
		case media == a:
			return true
		}
	}
	return false
}

func mediaTypeErr(op, got string) error {
	return &waxerr.Error{Code: waxerr.CodeInvalid, Op: op, Err: &MediaTypeError{Got: got}, Reason: ReasonMediaType}
}

// checkScheme allows only http(s); it blocks file://, ftp://, gopher://, and the
// like, which an SSRF or a malicious feed could otherwise use.
func checkScheme(rawURL, op string) error {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return waxerr.New(waxerr.CodeInvalid, op, "url has no scheme")
	}
	scheme := rawURL[:i]
	switch {
	case strings.EqualFold(scheme, "http"), strings.EqualFold(scheme, "https"):
		return nil
	case !validScheme(scheme):
		// What comes before a nested "://" is a login, path or query, not a scheme.
		return waxerr.New(waxerr.CodeInvalid, op, "url has no scheme")
	default:
		return waxerr.New(waxerr.CodeInvalid, op, "unsupported url scheme: "+scheme)
	}
}

// validScheme reports whether s is a URL scheme as RFC 3986 spells one: a letter, then
// letters, digits, "+", "-" or ".".
func validScheme(s string) bool {
	for i, r := range s {
		letter := 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'
		if !letter && (i == 0 || !('0' <= r && r <= '9' || r == '+' || r == '-' || r == '.')) {
			return false
		}
	}
	return s != ""
}

// guardDialAddr rejects a dial to a private/loopback/link-local/unspecified
// address. It is the Control hook, so it sees the resolved IP that would actually
// be contacted.
func guardDialAddr(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// An address the guard cannot judge is not one it lets through.
		return &BlockedAddressError{Addr: address}
	}
	if isBlockedIP(ip) {
		return &BlockedAddressError{Addr: ip.String()}
	}
	return nil
}

// blockedPrefixes are the special-use ranges netip's classifiers miss: "this network",
// RFC 6598 CGNAT (which reaches metadata endpoints such as Alibaba's 100.100.100.200,
// and Tailscale hosts), the IETF protocol assignments, documentation and benchmarking
// in both families, the reserved 240/4 with the broadcast address, IPv6 site-local and
// discard, and the local-use NAT64 prefix, refused whole since where it carries an IPv4
// address depends on its length.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// isBlockedIP reports whether addr is in a range the SSRF guard refuses: loopback,
// RFC 1918 private and ULA, link-local, multicast of either family, unspecified, the
// special-use ranges above, or an IPv6 form that carries such an IPv4 address.
func isBlockedIP(addr netip.Addr) bool {
	// A prefix never contains a zoned address, so the zone goes first.
	addr = addr.WithZone("").Unmap()
	if blockedAddr(addr) {
		return true
	}
	for _, v4 := range embeddedIPv4(addr) {
		if blockedAddr(v4) {
			return true
		}
	}
	return false
}

func blockedAddr(a netip.Addr) bool {
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// embeddedIPv4 returns the IPv4 addresses an IPv6 address carries: the last four bytes
// of a NAT64 (64:ff9b::/96), IPv4-compatible or IPv4-translated address, bytes 2 to 5 of
// a 6to4 one, a Teredo server's bytes 4 to 7 and its client's inverted last four, and
// the last four behind an ISATAP interface identifier under any prefix.
func embeddedIPv4(a netip.Addr) []netip.Addr {
	if !a.Is6() {
		return nil
	}
	b := a.As16()
	at := func(i int) netip.Addr { return netip.AddrFrom4([4]byte(b[i : i+4])) }
	var out []netip.Addr
	switch {
	case b[0] == 0 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]):
		out = append(out, at(12))
	case b[0] == 0x20 && b[1] == 0x02:
		out = append(out, at(2))
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0:
		out = append(out, at(4), netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}))
	case allZero(b[:12]), allZero(b[:8]) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0:
		out = append(out, at(12))
	}
	// ISATAP's identifier is 0:5efe or 200:5efe; the low bits of its first byte are the
	// universal and group flags.
	if b[8]&^0x03 == 0 && b[9] == 0 && b[10] == 0x5e && b[11] == 0xfe {
		out = append(out, at(12))
	}
	return out
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
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

// wait blocks until host's next slot, honoring context cancellation. It reserves the
// slot before sleeping so concurrent callers to the same host queue rather than all
// firing at once.
func (l *hostLimiter) wait(ctx context.Context, host string) error {
	if l == nil || l.min <= 0 {
		return ctx.Err()
	}

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

// paceKey is the pacing key for a URL: its host alone, whatever login, port or scheme
// it names, so one host is paced once.
func paceKey(u *url.URL) string { return strings.ToLower(u.Hostname()) }
