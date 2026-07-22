// Package proxy is WaxBin's local control channel. It defines a versioned
// JSON-over-unix-socket protocol that lets a waxbin CLI redirect its mutations
// through a server (waxbin serve, or an embedding app such as WaxDeck) that
// already holds the catalog write lock, rather than failing with CodeConflict.
//
// The wire format is newline-delimited JSON frames. A request carries a protocol
// version, a method name, and opaque params. A response carries an ok flag with
// either a data payload or a typed error. Error codes map to and from waxerr.Code
// in both directions, so a proxied failure keeps its class (CodeLocked,
// CodeNotFound, and so on) and the CLI's exit-code mapping is the same whether a
// command ran locally or through the socket.
//
// The package depends only on model and waxerr, not on the waxbin facade. The
// server therefore takes its Library through the Maintainer interface and a
// handler map wired by the caller (waxbin.Serve), which avoids an import cycle and
// lets an embedder mount the handler on its own listener.
package proxy

import (
	"encoding/json"
	"errors"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// ProtocolVersion is the wire protocol version. A request carrying a different
// version is rejected, so a newer client cannot silently misdrive an older
// server.
const ProtocolVersion = 1

// Method names for the proxied operations: the fast request/response catalog
// mutations, the reads a mutating command needs for its confirmation output, the
// two maintenance-mode control methods, and the run_* submitters for long jobs. A
// long job (scan/analyze/enrich/organize) is submitted with run_*, runs in the
// server's own process so the server is not paused, and is then followed by the
// client through the read-only job row. Maintenance mode is a separate escape
// hatch, for the few operations that have no server method such as rebuild and
// restore.
const (
	MethodPing             = "ping"
	MethodEditFields       = "edit_fields"
	MethodEditManyFields   = "edit_many_fields"
	MethodSetCredits       = "set_credits"
	MethodSetLyrics        = "set_lyrics"
	MethodSetChapters      = "set_chapters"
	MethodSetItemArt       = "set_item_art"
	MethodSetEntityArt     = "set_entity_art"
	MethodEditEntity       = "edit_entity"
	MethodSetTag           = "set_tag"
	MethodLock             = "lock"
	MethodUnlock           = "unlock"
	MethodCreateUser       = "create_user"
	MethodUsers            = "users"
	MethodMerge            = "merge"
	MethodSetRating        = "set_rating"
	MethodSetStar          = "set_star"
	MethodMarkPlayed       = "mark_played"
	MethodSetProgress      = "set_progress"
	MethodPlayState        = "play_state"
	MethodProvenance       = "provenance"
	MethodPlaylistAdd      = "playlist_add"
	MethodPlaylistRemove   = "playlist_remove"
	MethodPlaylistRemoveAt = "playlist_remove_at"
	MethodPlaylistSetRule  = "playlist_set_rule"
	MethodMaintenanceBegin = "maintenance_begin"
	MethodMaintenanceEnd   = "maintenance_end"

	// Server-run long jobs. The server starts the job in its own process (staying
	// available) and returns the job PID; the client tails the read-only job row.
	MethodRunScan     = "run_scan"
	MethodRunAnalyze  = "run_analyze"
	MethodRunEnrich   = "run_enrich"
	MethodRunOrganize = "run_organize"
)

// request is one wire frame from client to server.
type request struct {
	V      int             `json:"v"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// response is one wire frame from server to client. Exactly one of Data or Error
// is meaningful, selected by OK.
type response struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error *wireError      `json:"error,omitempty"`
}

// wireError is a serialized waxerr.Error: the stable code plus the operation and
// message, so the client can rebuild an error that carries the same class.
type wireError struct {
	Code string `json:"code"`
	Op   string `json:"op,omitempty"`
	Msg  string `json:"msg"`
}

// toWireError serializes err for transport, preserving its waxerr class. A nil
// error yields nil.
func toWireError(err error) *wireError {
	if err == nil {
		return nil
	}
	we := &wireError{Code: string(waxerr.CodeOf(err)), Msg: err.Error()}
	var e *waxerr.Error
	if errors.As(err, &e) {
		we.Op = e.Op
		if e.Msg != "" {
			we.Msg = e.Msg
		}
	}
	return we
}

// fromWireError rebuilds a *waxerr.Error carrying the wire code, so waxerr.Is /
// CodeOf and the CLI exit-code mapping behave the same as a local failure. A nil
// wireError yields nil.
func fromWireError(we *wireError) error {
	if we == nil {
		return nil
	}
	op := we.Op
	if op == "" {
		op = "proxy.remote"
	}
	return waxerr.New(waxerr.Code(we.Code), op, we.Msg)
}

// --- request/response payload DTOs ---

// EditFieldsParams is the edit_fields request payload.
type EditFieldsParams struct {
	ItemPID   string            `json:"itemPid"`
	Edits     map[string]string `json:"edits"`
	WriteBack bool              `json:"writeBack"`
	Lock      bool              `json:"lock"`
	Force     bool              `json:"force"`
}

// WriteBackFailure names one backing file whose on-disk tag write-back did not
// apply during a proxied edit. It mirrors the facade's write-back failure so the
// CLI can rebuild the typed error the local path produces.
type WriteBackFailure struct {
	FilePID string `json:"filePid,omitempty"`
	Path    string `json:"path,omitempty"`
	Reason  string `json:"reason"`
}

// EditFieldsResult is the edit_fields response payload. A committed catalog edit
// whose write-back partially failed returns the failures here rather than as a
// transport error, matching the local semantics where the edit still stands.
type EditFieldsResult struct {
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// EditManyFieldsParams is the edit_many_fields request payload.
type EditManyFieldsParams struct {
	ItemPIDs   []string          `json:"itemPids"`
	Edits      map[string]string `json:"edits"`
	WriteBack  bool              `json:"writeBack"`
	Lock       bool              `json:"lock"`
	Force      bool              `json:"force"`
	SkipLocked bool              `json:"skipLocked"`
}

// EditManyFieldsResult is the edit_many_fields response payload. The catalog batch
// is atomic; per-item write-back failures are reported here (keyed by item pid), not
// as a transport error, matching the local semantics.
type EditManyFieldsResult struct {
	Edited            []string                      `json:"edited,omitempty"`
	Skipped           []string                      `json:"skipped,omitempty"`
	WriteBackFailures map[string][]WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// SetCreditsParams is the set_credits request payload.
type SetCreditsParams struct {
	ItemPID   string   `json:"itemPid"`
	Role      string   `json:"role"`
	Names     []string `json:"names,omitempty"`
	WriteBack bool     `json:"writeBack"`
	Lock      bool     `json:"lock"`
	Force     bool     `json:"force"`
}

// SetCreditsResult is the set_credits response payload: the number of contributors
// actually stored (after trim/dedup) and any music write-back failures.
type SetCreditsResult struct {
	Stored            int                `json:"stored"`
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// SetLyricsParams is the set_lyrics request payload. A nil Lyrics clears the row.
type SetLyricsParams struct {
	ItemPID string        `json:"itemPid"`
	Lyrics  *model.Lyrics `json:"lyrics,omitempty"`
	Lock    bool          `json:"lock"`
	Force   bool          `json:"force"`
}

// SetChaptersParams is the set_chapters request payload. An empty list clears the
// user chapters.
type SetChaptersParams struct {
	ItemPID  string          `json:"itemPid"`
	Chapters []model.Chapter `json:"chapters,omitempty"`
	Lock     bool            `json:"lock"`
	Force    bool            `json:"force"`
}

// SetItemArtParams is the set_item_art request payload. Empty Data clears the cover.
// The image bytes travel base64-encoded in the JSON frame.
type SetItemArtParams struct {
	ItemPID   string `json:"itemPid"`
	Data      []byte `json:"data,omitempty"`
	Lock      bool   `json:"lock"`
	Force     bool   `json:"force"`
	WriteBack bool   `json:"writeBack"`
}

// SetItemArtResult is the set_item_art response payload. A committed cover edit whose
// on-disk embed partially failed returns the failed files here rather than as a
// transport error, matching edit_fields.
type SetItemArtResult struct {
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// SetEntityArtParams is the set_entity_art request payload (album/artist/... covers).
type SetEntityArtParams struct {
	EntityType string `json:"entityType"`
	EntityPID  string `json:"entityPid"`
	Role       string `json:"role"`
	Data       []byte `json:"data,omitempty"`
	WriteBack  bool   `json:"writeBack"`
}

// SetEntityArtResult is the set_entity_art response payload: the member files an album
// cover fan-out could not embed into (empty for a non-album cover or a clean fan-out).
type SetEntityArtResult struct {
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// SetTagParams is the set_tag request payload: a custom tag's ordered values on an
// item. Empty Values clears the tag.
type SetTagParams struct {
	ItemPID string   `json:"itemPid"`
	Key     string   `json:"key"`
	Values  []string `json:"values,omitempty"`
	Lock    bool     `json:"lock"`
	Force   bool     `json:"force"`
}

// SetTagResult is the set_tag response payload: the canonical key actually stored (the
// normalized uppercase form) and the number of values stored after trimming (0 = the
// tag was cleared).
type SetTagResult struct {
	Key    string `json:"key"`
	Stored int    `json:"stored"`
}

// EditEntityParams is the edit_entity request payload: curation edits to one shared
// entity (artist/release_group/album). With WriteBack the fanned identifiers/sort are
// also mirrored across the entity's member files.
type EditEntityParams struct {
	EntityType string            `json:"entityType"`
	EntityPID  string            `json:"entityPid"`
	Edits      map[string]string `json:"edits"`
	Lock       bool              `json:"lock"`
	Force      bool              `json:"force"`
	WriteBack  bool              `json:"writeBack"`
}

// EditEntityResult is the edit_entity response payload. A committed entity edit whose
// member-file fan-out partially failed returns the failed files here rather than as a
// transport error, matching edit_fields.
type EditEntityResult struct {
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// FieldsParams is the lock / unlock request payload.
type FieldsParams struct {
	ItemPID string   `json:"itemPid"`
	Fields  []string `json:"fields"`
}

// CreateUserParams is the create_user request payload.
type CreateUserParams struct {
	Name string `json:"name"`
}

// MergeParams is the merge request payload.
type MergeParams struct {
	EntityType string   `json:"entityType"`
	Survivor   string   `json:"survivor"`
	Losers     []string `json:"losers"`
}

// RatingParams is the set_rating request payload. Rating is nil to clear the
// rating.
type RatingParams struct {
	UserPID string `json:"userPid"`
	ItemPID string `json:"itemPid"`
	Rating  *int   `json:"rating"`
}

// StarParams is the set_star request payload.
type StarParams struct {
	UserPID string `json:"userPid"`
	ItemPID string `json:"itemPid"`
	Starred bool   `json:"starred"`
}

// PlayedParams is the mark_played request payload.
type PlayedParams struct {
	UserPID  string `json:"userPid"`
	ItemPID  string `json:"itemPid"`
	Finished bool   `json:"finished"`
}

// ProgressParams is the set_progress request payload.
type ProgressParams struct {
	UserPID    string `json:"userPid"`
	ItemPID    string `json:"itemPid"`
	PositionMS int64  `json:"positionMs"`
}

// StateParams is the play_state request payload.
type StateParams struct {
	UserPID string `json:"userPid"`
	ItemPID string `json:"itemPid"`
}

// ItemParams is the provenance request payload (an item pid alone).
type ItemParams struct {
	ItemPID string `json:"itemPid"`
}

// PlaylistAddParams is the playlist_add request payload.
type PlaylistAddParams struct {
	PlaylistPID string   `json:"playlistPid"`
	ItemPIDs    []string `json:"itemPids"`
}

// PlaylistRemoveParams is the playlist_remove request payload.
type PlaylistRemoveParams struct {
	PlaylistPID string `json:"playlistPid"`
	ItemPID     string `json:"itemPid"`
}

// PlaylistRemoveAtParams is the playlist_remove_at request payload.
type PlaylistRemoveAtParams struct {
	PlaylistPID string `json:"playlistPid"`
	Position    int    `json:"position"`
}

// PlaylistSetRuleParams is the playlist_set_rule request payload. Rule is a
// marshaled query rule document (the versioned envelope), opaque to this
// package; the server parses it with query.ParseRule, so validation lives on
// the server side like run_organize's rule.
type PlaylistSetRuleParams struct {
	PlaylistPID string          `json:"playlistPid"`
	Rule        json.RawMessage `json:"rule"`
}

// ScanParams is the run_scan request payload.
type ScanParams struct {
	LibraryPID       string `json:"libraryPid,omitempty"`
	SubPath          string `json:"subPath,omitempty"`
	Force            bool   `json:"force,omitempty"`
	AdoptStampedPIDs bool   `json:"adoptStampedPids,omitempty"`
	ForceReconcile   bool   `json:"forceReconcile,omitempty"`
	IgnoreLocks      bool   `json:"ignoreLocks,omitempty"`
}

// AnalyzeParams is the run_analyze request payload.
type AnalyzeParams struct {
	WriteReplayGainTags bool `json:"writeReplayGainTags,omitempty"`
}

// EnrichParams is the run_enrich request payload.
type EnrichParams struct {
	Force bool `json:"force,omitempty"`
	Limit int  `json:"limit,omitempty"`
}

// OrganizeParams is the run_organize request payload. Rule is a marshaled query
// rule document (opaque to this package); Profile overrides the library profile.
type OrganizeParams struct {
	Rule    json.RawMessage `json:"rule,omitempty"`
	Profile string          `json:"profile,omitempty"`
}

// JobStartResult is the response for a run_* method: the PID of the started job,
// which the client tails through the read-only job row.
type JobStartResult struct {
	JobPID string `json:"jobPid"`
}
