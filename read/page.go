package read

import (
	"encoding/base64"
	"strings"

	"github.com/colespringer/waxbin/model"
)

// Cursor is an opaque keyset pagination token. It encodes the (sort_key, pid) of
// the last row on a page, so the next page resumes strictly after it. Unlike an
// offset, a cursor is stable under concurrent inserts/deletes: no row is skipped
// or repeated mid-scan. The empty Cursor means "from the beginning".
type Cursor string

const cursorSep = "\x1f"

// EncodeCursor builds the opaque token for a row's sort key and pid.
func EncodeCursor(sortKey string, pid model.PID) Cursor {
	return Cursor(base64.RawURLEncoding.EncodeToString([]byte(sortKey + cursorSep + string(pid))))
}

// Decode splits a cursor back into its sort key and pid. ok is false for a
// malformed token, which a caller should treat as a bad request rather than
// silently restarting pagination. It splits on the LAST separator: a pid is a
// ULID and never contains the separator byte, so this stays correct even if a
// (pathological) sort key contains one. Splitting on the first would not.
func (c Cursor) Decode() (sortKey string, pid model.PID, ok bool) {
	if c == "" {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(string(c))
	if err != nil {
		return "", "", false
	}
	str := string(raw)
	i := strings.LastIndex(str, cursorSep)
	if i < 0 {
		return "", "", false
	}
	return str[:i], model.PID(str[i+len(cursorSep):]), true
}

// Page is one keyset-paginated window of item views. Next is the cursor to pass
// for the following page; it is empty exactly when HasMore is false.
type Page struct {
	Items   []*model.ItemView
	Next    Cursor
	HasMore bool
}
