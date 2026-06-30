// Package diskfree reports the free space on the filesystem holding a path, for
// import/move preflight checks. It is best-effort: on platforms without a probe,
// Available returns ErrUnsupported and callers skip the check.
package diskfree

import "errors"

// ErrUnsupported is returned by Available where no free-space probe is available.
var ErrUnsupported = errors.New("diskfree: unsupported on this platform")
