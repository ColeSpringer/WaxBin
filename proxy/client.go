package proxy

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/waxerr"
)

// Client is a connection to a proxy Server over a unix socket. It implements the
// proxied mutation and confirmation-read methods. It is safe for sequential use
// on one goroutine; calls are serialized on the single connection, so a Client is
// not meant to be shared across concurrent callers.
type Client struct {
	conn net.Conn
	enc  *json.Encoder
	dec  *json.Decoder
	mu   sync.Mutex // serializes request/response round-trips on the one connection
}

// Dial connects to a proxy server listening on the unix socket at path.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, "proxy.Dial", err)
	}
	return &Client{conn: conn, enc: json.NewEncoder(conn), dec: json.NewDecoder(conn)}, nil
}

// Close closes the underlying connection. If this connection held a maintenance
// session, closing it signals the server to reopen (the crash-safety path), so a
// clean shutdown should call MaintenanceEnd before Close.
func (c *Client) Close() error { return c.conn.Close() }

// call sends one request and decodes one response, mapping a typed wire error
// back to a waxerr the caller can classify. out, when non-nil, receives the
// response data.
//
// The blocking Encode/Decode honor ctx: a watcher sets an immediate I/O deadline on
// the connection when ctx is canceled, so a wedged or slow server (e.g. one mid
// Reopen) does not hang the caller and a user's Ctrl-C returns promptly. On any
// error after cancellation the result is reported as CodeCanceled; on a clean call
// the deadline is cleared so the connection stays usable for the next call.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	if err := ctx.Err(); err != nil {
		return waxerr.FromContext("proxy.call", err, waxerr.CodeCanceled)
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			// Unblock any in-flight Read/Write on this connection. SetDeadline is safe to
			// call concurrently with the I/O it interrupts.
			_ = c.conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	err := c.roundtrip(method, params, out)
	close(stop)
	// Wait for the watcher to finish before touching the deadline, so its
	// SetDeadline (if ctx fired) cannot land after we clear the deadline below and
	// leave a stale one on a reused connection.
	<-watcherDone
	if err != nil {
		if ctx.Err() != nil {
			return waxerr.FromContext("proxy.call", ctx.Err(), waxerr.CodeCanceled)
		}
		return err
	}
	// Clear any deadline the watcher set so the reused connection has none for the
	// next call.
	_ = c.conn.SetDeadline(time.Time{})
	return nil
}

// roundtrip performs the marshal/encode/decode of one call, without ctx handling.
func (c *Client) roundtrip(method string, params, out any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return waxerr.Wrap(waxerr.CodeInternal, "proxy.call", err)
		}
		raw = b
	}
	if err := c.enc.Encode(request{V: ProtocolVersion, Method: method, Params: raw}); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "proxy.call", err)
	}
	var resp response
	if err := c.dec.Decode(&resp); err != nil {
		return waxerr.Wrap(waxerr.CodeIO, "proxy.call", err)
	}
	if !resp.OK {
		return fromWireError(resp.Error)
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return waxerr.Wrap(waxerr.CodeInternal, "proxy.call", err)
		}
	}
	return nil
}

// Ping checks the server is reachable and speaks the protocol.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, MethodPing, nil, nil)
}

// EditFields proxies a catalog field edit. A committed edit whose write-back
// partially failed returns a non-nil result with the failed files; the transport
// error stays nil, matching the local semantics where the catalog edit stands.
func (c *Client) EditFields(ctx context.Context, itemPID model.PID, edits map[string]string, writeBack, lock, force bool) (*EditFieldsResult, error) {
	var res EditFieldsResult
	err := c.call(ctx, MethodEditFields, EditFieldsParams{
		ItemPID: string(itemPID), Edits: edits, WriteBack: writeBack, Lock: lock, Force: force,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// EditManyFields proxies a multi-item catalog field edit. The catalog batch is
// atomic; per-item write-back failures come back in the result rather than as a
// transport error, matching the local semantics.
func (c *Client) EditManyFields(ctx context.Context, itemPIDs []model.PID, edits map[string]string, writeBack, lock, force, skipLocked bool) (*EditManyFieldsResult, error) {
	pids := make([]string, len(itemPIDs))
	for i, p := range itemPIDs {
		pids[i] = string(p)
	}
	var res EditManyFieldsResult
	err := c.call(ctx, MethodEditManyFields, EditManyFieldsParams{
		ItemPIDs: pids, Edits: edits, WriteBack: writeBack, Lock: lock, Force: force, SkipLocked: skipLocked,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// SetCredits proxies a credit edit. The result carries the stored contributor count
// and, for a committed edit whose music write-back partially failed, the failed files
// (the transport error stays nil, matching edit_fields).
func (c *Client) SetCredits(ctx context.Context, itemPID model.PID, role string, names []string, writeBack, lock, force bool) (*SetCreditsResult, error) {
	var res SetCreditsResult
	err := c.call(ctx, MethodSetCredits, SetCreditsParams{
		ItemPID: string(itemPID), Role: role, Names: names, WriteBack: writeBack, Lock: lock, Force: force,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// SetLyrics proxies a lyrics edit.
func (c *Client) SetLyrics(ctx context.Context, itemPID model.PID, ly *model.Lyrics, lock, force bool) error {
	return c.call(ctx, MethodSetLyrics, SetLyricsParams{
		ItemPID: string(itemPID), Lyrics: ly, Lock: lock, Force: force,
	}, nil)
}

// SetChapters proxies a chapters edit.
func (c *Client) SetChapters(ctx context.Context, itemPID model.PID, chapters []model.Chapter, lock, force bool) error {
	return c.call(ctx, MethodSetChapters, SetChaptersParams{
		ItemPID: string(itemPID), Chapters: chapters, Lock: lock, Force: force,
	}, nil)
}

// SetItemArt proxies an item cover edit. A committed edit whose on-disk embed partially
// failed returns the failed files in the result; the transport error stays nil, matching
// edit_fields.
func (c *Client) SetItemArt(ctx context.Context, itemPID model.PID, data []byte, lock, force, writeBack bool) (*SetItemArtResult, error) {
	var res SetItemArtResult
	err := c.call(ctx, MethodSetItemArt, SetItemArtParams{
		ItemPID: string(itemPID), Data: data, Lock: lock, Force: force, WriteBack: writeBack,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// SetEntityArt proxies a durable entity cover edit. An album cover fan-out whose embed
// partially failed returns the failed files in the result (transport error stays nil).
func (c *Client) SetEntityArt(ctx context.Context, entityType model.ArtEntity, entityPID model.PID, role string, data []byte, writeBack bool) (*SetEntityArtResult, error) {
	var res SetEntityArtResult
	err := c.call(ctx, MethodSetEntityArt, SetEntityArtParams{
		EntityType: string(entityType), EntityPID: string(entityPID), Role: role, Data: data, WriteBack: writeBack,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// SetTag proxies a custom-tag edit, returning the canonical key stored and the number
// of values stored after trimming (0 = the tag was cleared).
func (c *Client) SetTag(ctx context.Context, itemPID model.PID, key string, values []string, lock, force bool) (string, int, error) {
	var res SetTagResult
	err := c.call(ctx, MethodSetTag, SetTagParams{
		ItemPID: string(itemPID), Key: key, Values: values, Lock: lock, Force: force,
	}, &res)
	if err != nil {
		return "", 0, err
	}
	return res.Key, res.Stored, nil
}

// EditEntity proxies a curation edit to one shared entity (artist/release_group/
// album). A committed edit whose member-file fan-out partially failed returns the failed
// files in the result; the transport error stays nil, matching edit_fields.
func (c *Client) EditEntity(ctx context.Context, entityType model.MergeEntity, entityPID model.PID, edits map[string]string, writeBack, lock, force bool) (*EditEntityResult, error) {
	var res EditEntityResult
	err := c.call(ctx, MethodEditEntity, EditEntityParams{
		EntityType: string(entityType), EntityPID: string(entityPID), Edits: edits, Lock: lock, Force: force, WriteBack: writeBack,
	}, &res)
	if err != nil {
		return nil, err
	}
	return &res, nil
}

// Lock proxies locking item fields.
func (c *Client) Lock(ctx context.Context, itemPID model.PID, fields []string) error {
	return c.call(ctx, MethodLock, FieldsParams{ItemPID: string(itemPID), Fields: fields}, nil)
}

// Unlock proxies clearing locks on item fields.
func (c *Client) Unlock(ctx context.Context, itemPID model.PID, fields []string) error {
	return c.call(ctx, MethodUnlock, FieldsParams{ItemPID: string(itemPID), Fields: fields}, nil)
}

// CreateUser proxies creating a playback user.
func (c *Client) CreateUser(ctx context.Context, name string) (*model.User, error) {
	var u model.User
	if err := c.call(ctx, MethodCreateUser, CreateUserParams{Name: name}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// Users proxies listing playback users.
func (c *Client) Users(ctx context.Context) ([]*model.User, error) {
	var users []*model.User
	if err := c.call(ctx, MethodUsers, nil, &users); err != nil {
		return nil, err
	}
	return users, nil
}

// Merge proxies collapsing loser entities onto a survivor.
func (c *Client) Merge(ctx context.Context, entityType model.MergeEntity, survivor model.PID, losers []model.PID) ([]*model.MergeReport, error) {
	ls := make([]string, len(losers))
	for i, l := range losers {
		ls[i] = string(l)
	}
	var reports []*model.MergeReport
	err := c.call(ctx, MethodMerge, MergeParams{
		EntityType: string(entityType), Survivor: string(survivor), Losers: ls,
	}, &reports)
	if err != nil {
		return nil, err
	}
	return reports, nil
}

// SetRating proxies setting or clearing a user's rating for an item.
func (c *Client) SetRating(ctx context.Context, userPID, itemPID model.PID, rating *int) error {
	return c.call(ctx, MethodSetRating, RatingParams{
		UserPID: string(userPID), ItemPID: string(itemPID), Rating: rating,
	}, nil)
}

// SetStar proxies starring or unstarring an item for a user.
func (c *Client) SetStar(ctx context.Context, userPID, itemPID model.PID, starred bool) error {
	return c.call(ctx, MethodSetStar, StarParams{
		UserPID: string(userPID), ItemPID: string(itemPID), Starred: starred,
	}, nil)
}

// MarkPlayed proxies marking an item played (and optionally finished) for a user.
func (c *Client) MarkPlayed(ctx context.Context, userPID, itemPID model.PID, finished bool) error {
	return c.call(ctx, MethodMarkPlayed, PlayedParams{
		UserPID: string(userPID), ItemPID: string(itemPID), Finished: finished,
	}, nil)
}

// SetProgress proxies persisting a user's resume position for an item.
func (c *Client) SetProgress(ctx context.Context, userPID, itemPID model.PID, positionMS int64) error {
	return c.call(ctx, MethodSetProgress, ProgressParams{
		UserPID: string(userPID), ItemPID: string(itemPID), PositionMS: positionMS,
	}, nil)
}

// PlayState proxies reading a user's play state for an item.
func (c *Client) PlayState(ctx context.Context, userPID, itemPID model.PID) (*model.PlayState, error) {
	var st model.PlayState
	if err := c.call(ctx, MethodPlayState, StateParams{UserPID: string(userPID), ItemPID: string(itemPID)}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Provenance proxies reading an item's field provenance.
func (c *Client) Provenance(ctx context.Context, itemPID model.PID) ([]model.FieldProvenance, error) {
	var rows []model.FieldProvenance
	if err := c.call(ctx, MethodProvenance, ItemParams{ItemPID: string(itemPID)}, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// PlaylistAdd proxies appending items to a static playlist.
func (c *Client) PlaylistAdd(ctx context.Context, playlistPID model.PID, itemPIDs []model.PID) error {
	ids := make([]string, len(itemPIDs))
	for i, p := range itemPIDs {
		ids[i] = string(p)
	}
	return c.call(ctx, MethodPlaylistAdd, PlaylistAddParams{PlaylistPID: string(playlistPID), ItemPIDs: ids}, nil)
}

// PlaylistRemove proxies removing every occurrence of an item from a playlist.
func (c *Client) PlaylistRemove(ctx context.Context, playlistPID, itemPID model.PID) error {
	return c.call(ctx, MethodPlaylistRemove, PlaylistRemoveParams{PlaylistPID: string(playlistPID), ItemPID: string(itemPID)}, nil)
}

// PlaylistRemoveAt proxies removing the single playlist entry at a position.
func (c *Client) PlaylistRemoveAt(ctx context.Context, playlistPID model.PID, position int) error {
	return c.call(ctx, MethodPlaylistRemoveAt, PlaylistRemoveAtParams{PlaylistPID: string(playlistPID), Position: position}, nil)
}

// PlaylistSetRule proxies replacing a smart playlist's rule in place. rule is a
// marshaled query rule document (query.MarshalRule); the server parses and
// validates it.
func (c *Client) PlaylistSetRule(ctx context.Context, playlistPID model.PID, rule []byte) error {
	return c.call(ctx, MethodPlaylistSetRule, PlaylistSetRuleParams{PlaylistPID: string(playlistPID), Rule: rule}, nil)
}

// RunScan submits a scan to the server and returns the started job's PID. The
// server runs the job in its own process (staying available); the caller tails the
// job through a read-only catalog handle.
func (c *Client) RunScan(ctx context.Context, params ScanParams) (model.PID, error) {
	return c.runJob(ctx, MethodRunScan, params)
}

// RunAnalyze submits the analyze pass to the server and returns the job PID.
func (c *Client) RunAnalyze(ctx context.Context, params AnalyzeParams) (model.PID, error) {
	return c.runJob(ctx, MethodRunAnalyze, params)
}

// RunEnrich submits the enrichment pass to the server and returns the job PID.
func (c *Client) RunEnrich(ctx context.Context, params EnrichParams) (model.PID, error) {
	return c.runJob(ctx, MethodRunEnrich, params)
}

// RunOrganize submits an organize pass to the server and returns the job PID. rule
// is a marshaled query rule document selecting the items to organize.
func (c *Client) RunOrganize(ctx context.Context, rule []byte, profile string) (model.PID, error) {
	return c.runJob(ctx, MethodRunOrganize, OrganizeParams{Rule: rule, Profile: profile})
}

// runJob is the shared submit path for the run_* methods: it returns the started
// job's PID.
func (c *Client) runJob(ctx context.Context, method string, params any) (model.PID, error) {
	var res JobStartResult
	if err := c.call(ctx, method, params, &res); err != nil {
		return "", err
	}
	return model.PID(res.JobPID), nil
}

// MaintenanceBegin asks the server to close its Library and release the write
// lock so this client can take it. The client must keep the connection open for
// the whole hand-off: closing it (or calling MaintenanceEnd) signals the server
// to reopen.
func (c *Client) MaintenanceBegin(ctx context.Context) error {
	return c.call(ctx, MethodMaintenanceBegin, nil, nil)
}

// MaintenanceEnd asks the server to reopen its Library after the client has
// released the write lock.
func (c *Client) MaintenanceEnd(ctx context.Context) error {
	return c.call(ctx, MethodMaintenanceEnd, nil, nil)
}
