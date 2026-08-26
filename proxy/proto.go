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
// server. Version 2 added SetItemArtParams.Role: a version-1 server would drop
// the unknown field and store a back-cover set as a front-cover overwrite,
// which is exactly the misdrive the check exists to stop. Version 3 added the
// EnrichParams scope fields: a version-2 server would drop them and run a
// full-catalog pass where the client asked for one item or entity. Version 4
// added the StarParams/RatingParams as-of stamp: a version-3 server would drop it
// and stamp the change at server-now, a real misdrive for a replayed offline
// toggle whose recorded time is what orders it against an out-of-band change.
// Version 5 added the PlayStateChangeResult payload on the four star/rating
// methods: a version-4 server returns no data at all, which decodes as
// changed=false, so every proxied write would report itself as a no-op.
// Version 6 added EnrichParams.WriteTags: a version-5 server drops it and runs
// without the on-disk write-back, so a client that asked for durable enrichment
// silently gets values the next rescan clears.
// Version 7 added mark_missing: a version-6 server does not implement it, so a
// client that recorded a vanished file would leave the catalog claiming the bytes
// are still there and keep handing the same doomed work back out.
// Version 8 added set_played.
// Version 9 added unfetch and podcast_remove, the two podcast verbs that had been
// taking the maintenance hand-off (pausing the whole server for a short mutation).
//
// Neither could misdrive a version-8 server the way the field additions above could:
// the version gate rejects every frame, ping included, so a v9 client reads a v8
// server as absent and falls back to the maintenance hand-off, which is the pause this
// removes rather than a wrong answer. The bump is what makes that fallback total
// instead of leaving the client to discover an unknown-method error partway through a
// command, matching how mark_missing (v7) and set_played (v8) were added. Adding a
// method without a bump is also precedent (put_transcript, fetch_transcript,
// add_root), and is the choice when keeping the other proxied methods working across a
// version boundary matters more.
//
// Version 10 added artwork provenance: model.FieldProvenance gained SourceURL, the
// provenance result gained the item's own cover as an "art" row, and
// SetEntityArtParams gained Lock/Force. The params addition alone forces the bump by
// the rule above, and the read addition follows version 6's precedent: the version
// gate rejects every frame, so a WaxDeck built against 9 falls back cleanly instead of
// rendering the art row as an editable scalar field, and it has to rebuild anyway to
// draw the mark.
//
// set_art_lock landed after 10 with no bump, following the add_root precedent: it adds
// no field to any existing struct, ArtRoles (the read that made the lock visible) has
// no wire surface at all, and WaxDeck already has Lock/Force on set_entity_art. A bump
// would cost it a rebuild for a method it has no reason to call, and the version gate
// would meanwhile reject every frame from a client built against 10, including ping.
//
// Version 11 added curation attribution and the three-state lock. The forcing addition
// is Source and Provider on every curation params struct (plus SourceURL on the two art
// ones), by the rule above: a version-10 server would drop them and store a cover or a
// genre the client fetched itself as hand-set, which is the bug this change exists to
// fix. The retyped Lock (a bool became the LockChange string) is not the argument for
// the bump, since the version gate rejects on version before any field decodes. A *bool
// for Lock would have encoded the same three states without a bump, and was rejected for
// splitting the lock vocabulary between the wire and Go.
//
// Version 12 added the playlist lifecycle: playlist_create, playlist_delete,
// playlist_rename and playlist_import_m3u8. By the add_root precedent these could
// have landed without a bump, since they add no field to any existing struct. The
// bump is deliberate: without it a client built against 11 keeps taking the
// maintenance hand-off for exactly these calls, which is the pause they exist to
// remove. Nothing has shipped yet, so the total fallback a bump causes costs a
// rebuild and nothing else.
//
// Version 13 added Format on the two art params structs, for a picture whose bytes no
// decoder here recognizes, and the generated provenance source. Neither is bump-forcing
// under the rule the entries above use, which is misdrive and not refusal: a version-12
// server drops Format and refuses the cover with CodeInvalid, and refuses an unknown
// source value the same way, so both fail loudly rather than storing the wrong thing.
// The bump is taken for the add_root reason instead. It makes the fallback total, so a
// client built against 12 reads a 13 server as absent from the first frame rather than
// discovering a refusal partway through a command, and neither version has shipped, so
// it costs a rebuild and nothing else.
//
// Version 14 added SetArtLockParams.Role, which forces the bump under the rule above: a
// version-13 server drops the field and locks the whole entity, where the client asked
// to lock one auxiliary slot. That is the widest possible misdrive of the call, since
// the entity lock also stops enrichment filling every other role. An omitted Role still
// encodes exactly as it did at 13 and still means the front cover, so the call a
// version-13 client makes is unchanged in meaning; the bump is for the new field alone.
// The same version added detach and EditEntityResult.MergedInto, both riding along
// rather than earning a bump. Neither can misdrive a call. The only peer that could
// take a detach for something else is another version-14 build predating the handler,
// which fails loudly with "unknown method: detach" (server.go) rather than doing the
// wrong thing, and a server built without MergedInto simply omits the field, leaving the
// client with the generic edited-fields line it printed before. That is the house
// precedent as well: set_art_lock landed at an already-committed version 10 and was left
// there, with the bump taken later.
const ProtocolVersion = 14

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
	MethodEditBatch        = "edit_batch"
	MethodSetCredits       = "set_credits"
	MethodSetLyrics        = "set_lyrics"
	MethodSetChapters      = "set_chapters"
	MethodSetItemArt       = "set_item_art"
	MethodSetEntityArt     = "set_entity_art"
	MethodSetArtLock       = "set_art_lock"
	MethodEditEntity       = "edit_entity"
	MethodDetach           = "detach"
	MethodSetTag           = "set_tag"
	MethodLock             = "lock"
	MethodUnlock           = "unlock"
	MethodCreateUser       = "create_user"
	MethodUsers            = "users"
	MethodMerge            = "merge"
	MethodMarkMissing      = "mark_missing"
	MethodSetRating        = "set_rating"
	MethodSetStar          = "set_star"
	MethodSetEntityStar    = "set_entity_star"
	MethodSetEntityRating  = "set_entity_rating"
	MethodMarkPlayed       = "mark_played"
	MethodSetPlayed        = "set_played"
	MethodSetProgress      = "set_progress"
	MethodPlayState        = "play_state"
	MethodProvenance       = "provenance"
	MethodPlaylistCreate   = "playlist_create"
	MethodPlaylistDelete   = "playlist_delete"
	MethodPlaylistRename   = "playlist_rename"
	MethodPlaylistImport   = "playlist_import_m3u8"
	MethodPlaylistAdd      = "playlist_add"
	MethodPlaylistRemove   = "playlist_remove"
	MethodPlaylistRemoveAt = "playlist_remove_at"
	MethodPlaylistSetRule  = "playlist_set_rule"
	MethodPutTranscript    = "put_transcript"
	MethodFetchTranscript  = "fetch_transcript"
	MethodUnfetch          = "unfetch"
	MethodPodcastRemove    = "podcast_remove"
	MethodAddRoot          = "add_root"
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

// EditFieldsParams is the edit_fields request payload. Source and Provider record where
// the values came from, so a client that fetched them is not stored as having typed
// them; an empty Source means a user edit. Lock is a model.LockChange, whose empty value
// leaves the stored lock alone.
type EditFieldsParams struct {
	ItemPID   string            `json:"itemPid"`
	Edits     map[string]string `json:"edits"`
	WriteBack bool              `json:"writeBack"`
	Source    string            `json:"source,omitempty"`
	Provider  string            `json:"provider,omitempty"`
	Lock      string            `json:"lock"`
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

// EditManyFieldsParams is the edit_many_fields request payload. Source, Provider and
// Lock carry the same meaning as on EditFieldsParams.
type EditManyFieldsParams struct {
	ItemPIDs   []string          `json:"itemPids"`
	Edits      map[string]string `json:"edits"`
	WriteBack  bool              `json:"writeBack"`
	Source     string            `json:"source,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Lock       string            `json:"lock"`
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

// ItemFieldsEdit is one entry of an edit_batch request: an item and its own
// field map.
type ItemFieldsEdit struct {
	ItemPID string            `json:"itemPid"`
	Fields  map[string]string `json:"fields"`
}

// EditBatchParams is the edit_batch request payload: a per-item-map batch edit,
// each item carrying its own fields where edit_many_fields shares one map. The
// response reuses EditManyFieldsResult (the same atomic-batch shape). Source, Provider
// and Lock carry the same meaning as on EditFieldsParams.
type EditBatchParams struct {
	Items      []ItemFieldsEdit `json:"items"`
	WriteBack  bool             `json:"writeBack"`
	Source     string           `json:"source,omitempty"`
	Provider   string           `json:"provider,omitempty"`
	Lock       string           `json:"lock"`
	Force      bool             `json:"force"`
	SkipLocked bool             `json:"skipLocked"`
}

// SetCreditsParams is the set_credits request payload. Source, Provider and Lock carry
// the same meaning as on EditFieldsParams.
type SetCreditsParams struct {
	ItemPID   string   `json:"itemPid"`
	Role      string   `json:"role"`
	Names     []string `json:"names,omitempty"`
	WriteBack bool     `json:"writeBack"`
	Source    string   `json:"source,omitempty"`
	Provider  string   `json:"provider,omitempty"`
	Lock      string   `json:"lock"`
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
	Lock    string        `json:"lock"`
	Force   bool          `json:"force"`
}

// SetChaptersParams is the set_chapters request payload. An empty list clears the
// user chapters.
type SetChaptersParams struct {
	ItemPID  string          `json:"itemPid"`
	Chapters []model.Chapter `json:"chapters,omitempty"`
	Lock     string          `json:"lock"`
	Force    bool            `json:"force"`
}

// SetItemArtParams is the set_item_art request payload. Empty Data clears the
// named role; an empty Role means the front cover. The image bytes travel
// base64-encoded in the JSON frame. Source, Provider and SourceURL record where the
// picture came from, so a client that fetched it itself is not stored as having chosen
// it by hand; an empty Source means a user set. Format names the image's format for a
// picture whose bytes cannot say so themselves (a BMP or TIFF cover); it is a fallback
// the decoded format beats, and it takes a short token, a bare extension, or an image
// media type. Lock is a model.LockChange, whose empty value leaves the stored lock
// alone; it governs the curation lock the named role owns, the item's "art" field for
// the front cover and "art.<role>" for an auxiliary one.
type SetItemArtParams struct {
	ItemPID   string `json:"itemPid"`
	Role      string `json:"role,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Source    string `json:"source,omitempty"`
	Provider  string `json:"provider,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
	Format    string `json:"format,omitempty"`
	Lock      string `json:"lock"`
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
// Lock and Force govern the curation lock the named role owns, the entity's "art"
// field for the front cover and "art.<role>" for an auxiliary one, the same way
// SetItemArtParams' do for an item. Source, Provider, SourceURL and Format carry the
// same meaning there too.
type SetEntityArtParams struct {
	EntityType string `json:"entityType"`
	EntityPID  string `json:"entityPid"`
	Role       string `json:"role"`
	Data       []byte `json:"data,omitempty"`
	Source     string `json:"source,omitempty"`
	Provider   string `json:"provider,omitempty"`
	SourceURL  string `json:"sourceUrl,omitempty"`
	Format     string `json:"format,omitempty"`
	Lock       string `json:"lock"`
	Force      bool   `json:"force"`
	WriteBack  bool   `json:"writeBack"`
}

// SetEntityArtResult is the set_entity_art response payload: the member files an album
// cover fan-out could not embed into (empty for a non-album cover or a clean fan-out).
type SetEntityArtResult struct {
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// SetArtLockParams is the set_art_lock request payload: an entity's art lock in one
// role, set or cleared without touching the image. It is the mutation set_entity_art
// cannot express, since that one always writes the slot too. Lock stays a bool here,
// where it is the write itself rather than an instruction accompanying one.
//
// An omitted Role means the front cover, whose lock is the entity's whole "art" field
// and also gates enrichment's fills in the other roles, so a caller written before
// per-role locks keeps its exact meaning and its exact bytes. Spelling "front" out is
// refused instead of aliased: one lock, one home, and an alias would hide that the
// front lock does the second job.
type SetArtLockParams struct {
	EntityType string `json:"entityType"`
	EntityPID  string `json:"entityPid"`
	Role       string `json:"role,omitempty"`
	Lock       bool   `json:"lock"`
}

// SetTagParams is the set_tag request payload: a custom tag's ordered values on an
// item. Empty Values clears the tag. Source, Provider and Lock carry the same meaning as
// on EditFieldsParams.
type SetTagParams struct {
	ItemPID  string   `json:"itemPid"`
	Key      string   `json:"key"`
	Values   []string `json:"values,omitempty"`
	Source   string   `json:"source,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Lock     string   `json:"lock"`
	Force    bool     `json:"force"`
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
// also mirrored across the entity's member files. Source, Provider and Lock carry the
// same meaning as on EditFieldsParams.
type EditEntityParams struct {
	EntityType string            `json:"entityType"`
	EntityPID  string            `json:"entityPid"`
	Edits      map[string]string `json:"edits"`
	Source     string            `json:"source,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	Lock       string            `json:"lock"`
	Force      bool              `json:"force"`
	WriteBack  bool              `json:"writeBack"`
}

// EditEntityResult is the edit_entity response payload. A committed entity edit whose
// member-file fan-out partially failed returns the failed files here rather than as a
// transport error, matching edit_fields. MergedInto names the survivor when clearing an
// mbid re-keyed the entity onto a key a heuristic twin already owned, so the edited pid
// no longer exists; it is empty for every other edit.
type EditEntityResult struct {
	MergedInto        string             `json:"mergedInto,omitempty"`
	WriteBackFailures []WriteBackFailure `json:"writeBackFailures,omitempty"`
}

// DetachParams is the detach request payload: one member of an album identified by a
// release id, moved onto the heuristic chain its own tags imply. With WriteBack the
// member's files also lose the two MusicBrainz release tags, which is what keeps the
// detach across a rescan.
type DetachParams struct {
	ItemPID   string `json:"itemPid"`
	WriteBack bool   `json:"writeBack"`
}

// DetachResult is the detach response payload: the album the member left, the album and
// release group it landed on, and the files a write-back could not rewrite (empty
// without write-back or after a clean one). The two new pids are empty when the member
// grouped on nothing but the release id.
type DetachResult struct {
	OldAlbumPID        string             `json:"oldAlbumPid"`
	NewAlbumPID        string             `json:"newAlbumPid,omitempty"`
	NewReleaseGroupPID string             `json:"newReleaseGroupPid,omitempty"`
	WriteBackFailures  []WriteBackFailure `json:"writeBackFailures,omitempty"`
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
// rating. AsOfNS is the optional recorded-time stamp (see asOfToWire).
type RatingParams struct {
	UserPID string `json:"userPid"`
	ItemPID string `json:"itemPid"`
	Rating  *int   `json:"rating"`
	AsOfNS  int64  `json:"asOfNs,string,omitempty"`
}

// StarParams is the set_star request payload. AsOfNS is the optional recorded-time
// stamp (see asOfToWire).
type StarParams struct {
	UserPID string `json:"userPid"`
	ItemPID string `json:"itemPid"`
	Starred bool   `json:"starred"`
	AsOfNS  int64  `json:"asOfNs,string,omitempty"`
}

// EntityStarParams is the set_entity_star request payload: a per-user star on a catalog
// entity (Kind is a model.MergeEntity: artist|release_group|album|genre). AsOfNS is the
// optional recorded-time stamp, the same encoding as StarParams (see asOfToWire).
type EntityStarParams struct {
	UserPID   string `json:"userPid"`
	Kind      string `json:"kind"`
	EntityPID string `json:"entityPid"`
	Starred   bool   `json:"starred"`
	AsOfNS    int64  `json:"asOfNs,string,omitempty"`
}

// EntityRatingParams is the set_entity_rating request payload. Rating is nil to clear.
// AsOfNS is the optional recorded-time stamp (see asOfToWire).
type EntityRatingParams struct {
	UserPID   string `json:"userPid"`
	Kind      string `json:"kind"`
	EntityPID string `json:"entityPid"`
	Rating    *int   `json:"rating"`
	AsOfNS    int64  `json:"asOfNs,string,omitempty"`
}

// MarkMissingParams is the mark_missing request payload. Force skips the server's
// on-disk verification, for a client whose own view of the filesystem is the
// authoritative one (one in a different container, say, whose mounts differ from
// the server's).
type MarkMissingParams struct {
	ItemPID string `json:"itemPid"`
	Force   bool   `json:"force,omitempty"`
}

// MarkMissingResult is the mark_missing result payload: what the call did, or why
// it did nothing. It carries the outcome rather than a bool because the refusals
// differ in what they tell the caller to do next (see model.MarkMissingOutcome).
type MarkMissingResult struct {
	Outcome string `json:"outcome"`
}

// PlayStateChangeResult is the result payload for the star/rating methods
// (set_rating, set_star, set_entity_star, set_entity_rating): whether the write
// changed anything. A value-identical call or a stale replay reports false,
// matching the store's suppressed change-feed delta. One struct for all four
// because they answer the same question and a consumer switching between the item
// and entity twins should not decode two shapes.
type PlayStateChangeResult struct {
	Changed bool `json:"changed"`
}

// asOfToWire encodes an optional recorded-time stamp for the wire: nil becomes 0,
// which omitempty then drops from the frame. A real value travels as a quoted
// decimal string (the `,string` tag on AsOfNS), matching the RatingChangedNS/
// StarredChangedNS stamp encoding in importer.go: a nanosecond value can exceed
// 2^53, so a bare JSON number is not safe for a JS client, and a plain int64 with
// omitempty (0 = not provided) sidesteps the *int64-with-,string encoding footgun.
func asOfToWire(asOf *int64) int64 {
	if asOf == nil {
		return 0
	}
	return *asOf
}

// AsOf decodes a wire as-of stamp back to the optional recorded time a server hands
// the store: 0 (the omitted/absent value) becomes nil, which stamps at server-now;
// any other value points to itself. It is exported because the server dispatch lives
// outside this package (waxbin.Serve).
func AsOf(ns int64) *int64 {
	if ns == 0 {
		return nil
	}
	return &ns
}

// PlayedParams is the mark_played request payload.
type PlayedParams struct {
	UserPID  string `json:"userPid"`
	ItemPID  string `json:"itemPid"`
	Finished bool   `json:"finished"`
}

// SetPlayedParams is the set_played request payload: played/finished set directly
// rather than incremented. PlayCount is nil to keep the stored count, &0 to reset
// it, &n to set it exactly. AsOfNS is the optional recorded-time stamp (see
// asOfToWire); the result is a PlayStateChangeResult.
type SetPlayedParams struct {
	UserPID   string `json:"userPid"`
	ItemPID   string `json:"itemPid"`
	Played    bool   `json:"played"`
	Finished  bool   `json:"finished"`
	PlayCount *int   `json:"playCount,omitempty"`
	AsOfNS    int64  `json:"asOfNs,string,omitempty"`
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

// PlaylistCreateParams is the playlist_create request payload. A nil Rule creates
// a static playlist and a present one a smart playlist, so the kind is derived
// rather than carried: two fields that have to agree can be sent disagreeing, and
// there is nothing a caller could mean by a static playlist with a rule. Rule is a
// marshaled query rule document (the versioned envelope) like
// PlaylistSetRuleParams.Rule, parsed and validated on the server side.
type PlaylistCreateParams struct {
	Name       string          `json:"name"`
	OwnerPID   string          `json:"ownerPid,omitempty"`
	Visibility string          `json:"visibility,omitempty"`
	Rule       json.RawMessage `json:"rule,omitempty"`
}

// PlaylistCreateResult carries the new playlist's pid, which is the whole point
// of the call: the caller cannot derive it and every follow-up needs it.
type PlaylistCreateResult struct {
	PlaylistPID string `json:"playlistPid"`
}

// PlaylistDeleteParams is the playlist_delete request payload.
type PlaylistDeleteParams struct {
	PlaylistPID string `json:"playlistPid"`
}

// PlaylistRenameParams is the playlist_rename request payload.
type PlaylistRenameParams struct {
	PlaylistPID string `json:"playlistPid"`
	Name        string `json:"name"`
}

// PlaylistImportParams is the playlist_import_m3u8 request payload. The document
// travels whole rather than as a path, since the file is the client's and the
// server may not be able to read it; path matching against the catalog happens on
// the server, which is the half that needs the catalog.
type PlaylistImportParams struct {
	Name       string `json:"name"`
	OwnerPID   string `json:"ownerPid,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	Document   []byte `json:"document"`
}

// PlaylistImportResult is the playlist_import_m3u8 response: the new playlist and
// what the import could not match. The unmatched paths are the reason this is not
// a bare pid, since an import that silently dropped half a file is the failure a
// caller needs to see.
type PlaylistImportResult struct {
	PlaylistPID    string   `json:"playlistPid"`
	Matched        int      `json:"matched"`
	Unmatched      int      `json:"unmatched"`
	UnmatchedPaths []string `json:"unmatchedPaths,omitempty"`
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

// PutTranscriptParams is the put_transcript request payload: a caller-supplied
// transcript body for an episode. Body travels base64-encoded in the JSON frame
// (like SetItemArtParams.Data); the server-side service enforces the format
// whitelist and size cap.
type PutTranscriptParams struct {
	EpisodePID string `json:"episodePid"`
	Format     string `json:"format"`
	Body       []byte `json:"body"`
	SourceURL  string `json:"sourceUrl,omitempty"`
}

// FetchTranscriptParams is the fetch_transcript request payload. The fetch of
// the episode's declared transcript URL runs in the server process.
type FetchTranscriptParams struct {
	EpisodePID string `json:"episodePid"`
}

// UnfetchParams is the unfetch request payload: the episode whose downloaded bytes
// are to be reclaimed, leaving it remote and re-fetchable.
type UnfetchParams struct {
	EpisodePID string `json:"episodePid"`
}

// UnfetchResult is the unfetch response. Unfetched is false when the episode held no
// file, which is a no-op rather than an error, so a client needs the flag to tell "I
// reclaimed these bytes" from "there was nothing to reclaim".
type UnfetchResult struct {
	Unfetched      bool  `json:"unfetched"`
	ReclaimedBytes int64 `json:"reclaimedBytes"`
}

// PodcastRemoveParams is the podcast_remove request payload: the show to unsubscribe
// from, deleting its episodes and their downloaded files.
type PodcastRemoveParams struct {
	PodcastPID string `json:"podcastPid"`
}

// AddRootParams is the add_root request payload: a library root spec to
// register at runtime. The response is the resulting model.Library row. The
// server validates the spec against its own registered set (Library.AddRoot),
// so mode/media/profile vocabulary and defaults live server-side. Path should
// be sent absolute: the server resolves a relative path against its own working
// directory, not the client's.
type AddRootParams struct {
	Path    string `json:"path"`
	Mode    string `json:"mode,omitempty"`
	Media   string `json:"media,omitempty"`
	Profile string `json:"profile,omitempty"`
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

// EnrichParams is the run_enrich request payload. The scoping fields are
// additive and mutually exclusive: ItemPID scopes the pass to one item's
// enrichable targets, EntityType+EntityPID to one entity (artist,
// release_group, or album). The server validates and resolves the scope.
type EnrichParams struct {
	Force      bool   `json:"force,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	ItemPID    string `json:"itemPid,omitempty"`
	EntityType string `json:"entityType,omitempty"`
	EntityPID  string `json:"entityPid,omitempty"`
	// WriteTags asks the server to write what the pass filled back into the files.
	// Additive: an older server drops it and runs without the write-back, which is
	// the same as the default.
	WriteTags bool `json:"writeTags,omitempty"`
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
