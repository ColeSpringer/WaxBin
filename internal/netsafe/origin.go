package netsafe

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Credentials are an HTTP Basic login scoped to the origin of Origin, a URL: its scheme,
// its host lowercased as given and its port. They go on every request at that origin,
// the first and each redirect hop that lands there, and on no other. An http origin also
// admits its https form when both are at the default ports, since a feed that moves to
// https only moves the login to a safer channel. Hosts are not IDNA-mapped, so a
// punycode spelling and its Unicode form are two origins. An Origin that is not an http
// or https URL, such as a manual show's key, admits nothing.
type Credentials struct {
	Origin     string
	User, Pass string
}

// origin is what Credentials are scoped to, the port filled in from the scheme.
type origin struct{ scheme, host, port string }

func originOf(raw string) (origin, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return origin{}, false
	}
	return originOfURL(u)
}

func originOfURL(u *url.URL) (origin, bool) {
	o := origin{scheme: strings.ToLower(u.Scheme), host: strings.ToLower(u.Hostname()), port: u.Port()}
	if o.host == "" {
		return origin{}, false
	}
	switch {
	case o.scheme == "http" && o.port == "":
		o.port = "80"
	case o.scheme == "https" && o.port == "":
		o.port = "443"
	case o.scheme != "http" && o.scheme != "https":
		return origin{}, false
	}
	return o, true
}

// admits reports whether a login scoped to o may go to hop.
func (o origin) admits(hop origin) bool {
	if o == hop {
		return true
	}
	return o.scheme == "http" && o.port == "80" && hop == origin{scheme: "https", host: o.host, port: "443"}
}

// scopedLogin is the form Credentials ride in a request's context, which Go carries to
// every redirect hop.
type scopedLogin struct {
	origin     origin
	user, pass string
}

type loginKey struct{}

func withLogin(ctx context.Context, c *Credentials) context.Context {
	if c == nil {
		return ctx
	}
	o, ok := originOf(c.Origin)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, loginKey{}, &scopedLogin{origin: o, user: c.User, pass: c.Pass})
}

// applyLogin puts the login req's context carries on req when req's URL is at its
// origin, and takes any login off req when it is not. The first request and every
// redirect hop pass through it, so each is judged by its own origin.
func applyLogin(req *http.Request) {
	l, ok := req.Context().Value(loginKey{}).(*scopedLogin)
	if !ok {
		return
	}
	if hop, ok := originOfURL(req.URL); ok && l.origin.admits(hop) {
		req.SetBasicAuth(l.user, l.pass)
		return
	}
	req.Header.Del("Authorization")
}

// permanent reports whether resp was reached through redirects that were all 301 or 308.
// An answer with no redirect before it was not moved.
func permanent(resp *http.Response) bool {
	hops := 0
	for req := resp.Request; req != nil && req.Response != nil; req = req.Response.Request {
		if s := req.Response.StatusCode; s != http.StatusMovedPermanently && s != http.StatusPermanentRedirect {
			return false
		}
		hops++
	}
	return hops > 0
}
