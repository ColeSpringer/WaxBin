package netsafe

import (
	"fmt"

	"github.com/colespringer/waxbin/waxerr"
)

// The reasons a fetch fails, on the waxerr.Error each failure returns. A host cannot
// import this package, so source re-exports them and the typed causes below.
const (
	ReasonHTTPStatus     waxerr.Reason = "http_status"
	ReasonTooLarge       waxerr.Reason = "too_large"
	ReasonBlockedAddress waxerr.Reason = "blocked_address"
	ReasonMediaType      waxerr.Reason = "media_type"
	ReasonRedirect       waxerr.Reason = "redirect"  // a redirect the client will not follow
	ReasonTransport      waxerr.Reason = "transport" // no answer: a dial, TLS, timeout or a cut-off body
)

// StatusError is an answer whose status is not 2xx. Host is the scheme and host that
// answered. LoginOrigin is set on a 401 or 403 from a host outside the origin of a login
// the request carried, which was not sent there: it is that origin's scheme and host.
type StatusError struct {
	Status      int
	Host        string
	LoginOrigin string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.Status)
	if e.Host != "" {
		msg = fmt.Sprintf("%s returned HTTP %d", e.Host, e.Status)
	}
	if e.LoginOrigin != "" {
		msg += "; the login goes only to " + e.LoginOrigin + ", so it was not sent there"
	}
	return msg
}

// TooLargeError is a body over the byte cap. Host is the scheme and host asked, when the
// message names one.
type TooLargeError struct {
	Limit int64
	Host  string
}

func (e *TooLargeError) Error() string {
	if e.Host == "" {
		return fmt.Sprintf("response exceeds %d-byte limit", e.Limit)
	}
	return fmt.Sprintf("response from %s exceeds %d-byte limit", e.Host, e.Limit)
}

// BlockedAddressError is a dial the address guard refused.
type BlockedAddressError struct{ Addr string }

func (e *BlockedAddressError) Error() string {
	return "refusing to connect to non-public address " + e.Addr
}

// MediaTypeError is an answer whose declared media type the request refused.
type MediaTypeError struct{ Got string }

func (e *MediaTypeError) Error() string { return "unexpected response content type: " + e.Got }

// RedirectLimitError is a redirect chain cut off at the policy's limit.
type RedirectLimitError struct{ Hops int }

func (e *RedirectLimitError) Error() string { return fmt.Sprintf("stopped after %d redirects", e.Hops) }
