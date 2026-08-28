package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/colespringer/waxbin/internal/testsock"
	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/waxerr"
)

// startServer runs a proxy server on a temp socket with the given handlers and
// (optional) maintainer, and returns its socket path. The server is torn down at
// test cleanup.
func startServer(t *testing.T, handlers map[string]proxy.Handler, maint proxy.Maintainer) string {
	t.Helper()
	sock := testsock.Path(t)
	ln, err := proxy.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := proxy.NewServer(handlers, maint, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	return sock
}

func dial(t *testing.T, sock string) *proxy.Client {
	t.Helper()
	c, err := proxy.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestRoundTripAndResultPayload checks a typed method carries structured data back,
// including an edit's write-back failures returned as an ok result (not an error).
func TestRoundTripAndResultPayload(t *testing.T) {
	handlers := map[string]proxy.Handler{
		proxy.MethodEditFields: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.EditFieldsParams
			_ = json.Unmarshal(raw, &p)
			if p.ItemPID != "item1" || p.Edits["artist"] != "New" {
				t.Errorf("server got params %+v", p)
			}
			// The scalar edits carry an attribution too: an embedder that fetched the value
			// is the other half of the bug the art fields close.
			if p.Source != "enrichment" || p.Provider != "itunes" {
				t.Errorf("edit attribution = %q/%q, want the caller's", p.Source, p.Provider)
			}
			return proxy.EditFieldsResult{WriteBackFailures: []proxy.WriteBackFailure{
				{FilePID: "f1", Path: "/x.mp3", Reason: "read-only mount"},
			}}, nil
		},
		proxy.MethodCreateUser: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.CreateUserParams
			_ = json.Unmarshal(raw, &p)
			return &model.User{PID: "u9", Name: p.Name}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	ctx := context.Background()

	editAttr := model.Attribution{Source: model.SourceEnrichment, Provider: "itunes"}
	res, err := c.EditFields(ctx, "item1", map[string]string{"artist": "New"}, true, editAttr, model.LockOn, false)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if len(res.WriteBackFailures) != 1 || res.WriteBackFailures[0].Path != "/x.mp3" {
		t.Fatalf("write-back failures = %+v", res.WriteBackFailures)
	}

	u, err := c.CreateUser(ctx, "Ann")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.PID != "u9" || u.Name != "Ann" {
		t.Fatalf("user = %+v", u)
	}
}

// TestEditBatchRoundTrip checks a per-item-map batch carries each entry's own
// fields over the wire and the shared batch-result shape comes back intact,
// per-item write-back failures included.
func TestEditBatchRoundTrip(t *testing.T) {
	handlers := map[string]proxy.Handler{
		proxy.MethodEditBatch: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.EditBatchParams
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Errorf("unmarshal params: %v", err)
			}
			if len(p.Items) != 2 ||
				p.Items[0].ItemPID != "i1" || p.Items[0].Fields["title"] != "Opener" ||
				p.Items[1].ItemPID != "i2" || p.Items[1].Fields["track_no"] != "9" {
				t.Errorf("server got items %+v", p.Items)
			}
			if !p.WriteBack || p.Lock != string(model.LockOn) || p.Force || !p.SkipLocked {
				t.Errorf("server got flags %+v", p)
			}
			return proxy.EditManyFieldsResult{
				Edited:  []string{"i1", "i2"},
				Skipped: []string{"i3"},
				WriteBackFailures: map[string][]proxy.WriteBackFailure{
					"i2": {{FilePID: "f2", Path: "/two.mp3", Reason: "shared file"}},
				},
			}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))

	res, err := c.EditBatch(context.Background(), []proxy.ItemFieldsEdit{
		{ItemPID: "i1", Fields: map[string]string{"title": "Opener"}},
		{ItemPID: "i2", Fields: map[string]string{"track_no": "9"}},
	}, true, model.Attribution{}, model.LockOn, false, true)
	if err != nil {
		t.Fatalf("edit batch: %v", err)
	}
	if len(res.Edited) != 2 || len(res.Skipped) != 1 || res.Skipped[0] != "i3" {
		t.Fatalf("result = %+v", res)
	}
	fails := res.WriteBackFailures["i2"]
	if len(fails) != 1 || fails[0].Path != "/two.mp3" {
		t.Fatalf("write-back failures = %+v", res.WriteBackFailures)
	}
}

// TestCurationRoundTrip checks the curation set methods carry their params over the
// wire (including image bytes and a nested lyrics struct) and that a credit
// write-back failure comes back as an ok result, matching edit_fields.
func TestCurationRoundTrip(t *testing.T) {
	var gotArt []byte
	var gotLyrics *model.Lyrics
	var gotLockRoles []string
	handlers := map[string]proxy.Handler{
		proxy.MethodSetCredits: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetCreditsParams
			_ = json.Unmarshal(raw, &p)
			if p.ItemPID != "i1" || p.Role != "producer" || len(p.Names) != 2 {
				t.Errorf("credit params = %+v", p)
			}
			// The second call is the skip-locked probe: the flag rode the request, so
			// answer with the skip the server would report.
			if p.SkipLocked {
				return proxy.SetCreditsResult{Skipped: true}, nil
			}
			return proxy.SetCreditsResult{Stored: 2, WriteBackFailures: []proxy.WriteBackFailure{{Path: "/x.mp3", Reason: "shared"}}}, nil
		},
		proxy.MethodSetLyrics: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetLyricsParams
			_ = json.Unmarshal(raw, &p)
			gotLyrics = p.Lyrics
			return nil, nil
		},
		proxy.MethodSetItemArt: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetItemArtParams
			_ = json.Unmarshal(raw, &p)
			gotArt = p.Data
			if p.Role != "back" {
				t.Errorf("item art role = %q, want back", p.Role)
			}
			if p.Source != "enrichment" || p.Provider != "itunes" || p.SourceURL != "https://itunes.example/c.png" {
				t.Errorf("item art attribution = %q/%q/%q, want the caller's", p.Source, p.Provider, p.SourceURL)
			}
			// The v13 addition. A client holding a Content-Type sends it verbatim; the
			// facade on the far side is what folds it to a token.
			if p.Format != "image/tiff" {
				t.Errorf("item art format = %q, want the caller's image/tiff", p.Format)
			}
			return nil, nil
		},
		proxy.MethodSetEntityArt: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetEntityArtParams
			_ = json.Unmarshal(raw, &p)
			if p.EntityType != "album" || p.Role != "front" || p.Lock != string(model.LockUnchanged) || p.Force {
				t.Errorf("entity art params = %+v", p)
			}
			if p.Source != "" || p.Provider != "" {
				t.Errorf("unstamped entity art carried %q/%q, want neither", p.Source, p.Provider)
			}
			if p.Format != "bmp" {
				t.Errorf("entity art format = %q, want bmp", p.Format)
			}
			return nil, nil
		},
		proxy.MethodSetArtLock: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetArtLockParams
			_ = json.Unmarshal(raw, &p)
			if p.EntityType != "podcast" || p.EntityPID != "pod1" {
				t.Errorf("art lock params = %+v", p)
			}
			gotLockRoles = append(gotLockRoles, p.Role)
			// The front direction must not carry a role at all, so the frame stays the
			// one a pre-role client sent.
			if ok := rawHas(raw, "role"); ok != (p.Role != "") {
				t.Errorf("role key present = %v with role %q, want them to agree", ok, p.Role)
			}
			return nil, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	ctx := context.Background()

	res, err := c.SetCredits(ctx, "i1", "producer", []string{"A", "B"}, true, model.Attribution{}, model.LockOn, false, false)
	if err != nil {
		t.Fatalf("set credits: %v", err)
	}
	if res.Stored != 2 || res.Skipped || len(res.WriteBackFailures) != 1 {
		t.Fatalf("credit result = %+v", res)
	}
	// skipLocked rides the request and the skipped answer rides the result, the two
	// scalar fields the version-15 protocol added.
	skipRes, err := c.SetCredits(ctx, "i1", "producer", []string{"A", "B"}, false, model.Attribution{}, model.LockOn, false, true)
	if err != nil {
		t.Fatalf("skip-locked set credits: %v", err)
	}
	if !skipRes.Skipped || skipRes.Stored != 0 {
		t.Fatalf("skip-locked credit result = %+v, want the skip reported", skipRes)
	}

	ly := &model.Lyrics{Synced: []model.SyncedLine{{TimeMS: 10, Text: "hi"}}}
	if err := c.SetLyrics(ctx, "i1", ly, model.LockOn, false); err != nil {
		t.Fatalf("set lyrics: %v", err)
	}
	if gotLyrics == nil || len(gotLyrics.Synced) != 1 || gotLyrics.Synced[0].Text != "hi" {
		t.Fatalf("lyrics not carried: %+v", gotLyrics)
	}

	artAttr := model.Attribution{
		Source: model.SourceEnrichment, Provider: "itunes", SourceURL: "https://itunes.example/c.png",
	}
	if _, err := c.SetItemArt(ctx, "i1", model.ArtRoleBack, []byte{1, 2, 3, 4}, "image/tiff", artAttr, model.LockOn, false, false); err != nil {
		t.Fatalf("set item art: %v", err)
	}
	if len(gotArt) != 4 || gotArt[0] != 1 {
		t.Fatalf("art bytes not carried: %v", gotArt)
	}

	// Unstamped and lock-unchanged: the wire carries the caller's silence as silence.
	if _, err := c.SetEntityArt(ctx, model.ArtAlbum, "a1", model.ArtRoleFront, []byte{9}, "bmp",
		model.Attribution{}, model.LockUnchanged, false, false); err != nil {
		t.Fatalf("set entity art: %v", err)
	}

	// set_art_lock carries no result payload; the unlock direction is the one that
	// matters, since it is the way out of a cleared and locked cover.
	if err := c.SetArtLock(ctx, model.ArtPodcast, "pod1", model.ArtRoleFront, false); err != nil {
		t.Fatalf("set art lock: %v", err)
	}
	// An auxiliary role names itself; the front cover does not, in either spelling.
	if err := c.SetArtLock(ctx, model.ArtPodcast, "pod1", model.ArtRoleBack, true); err != nil {
		t.Fatalf("set art lock (back): %v", err)
	}
	if err := c.SetArtLock(ctx, model.ArtPodcast, "pod1", "", true); err != nil {
		t.Fatalf("set art lock (empty role): %v", err)
	}
	if want := []string{"", "back", ""}; !slices.Equal(gotLockRoles, want) {
		t.Errorf("roles on the wire = %v, want %v", gotLockRoles, want)
	}
}

// TestEditEntityRoundTrip checks the entity-edit params reach the handler and the
// survivor of a merging mbid clear rides back on the response, so a proxied CLI names
// the row that absorbed the entity rather than the pid it typed.
func TestEditEntityRoundTrip(t *testing.T) {
	var got proxy.EditEntityParams
	handlers := map[string]proxy.Handler{
		proxy.MethodEditEntity: func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &got)
			return proxy.EditEntityResult{MergedInto: "al-twin"}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))

	res, err := c.EditEntity(context.Background(), model.MergeAlbum, "al-1",
		map[string]string{"mbid": ""}, false, model.Attribution{Source: model.SourceUser}, model.LockOn, false)
	if err != nil {
		t.Fatalf("edit entity: %v", err)
	}
	if got.EntityType != string(model.MergeAlbum) || got.EntityPID != "al-1" || got.Edits["mbid"] != "" {
		t.Errorf("edit_entity params = %+v, want the album's cleared mbid", got)
	}
	if res.MergedInto != "al-twin" {
		t.Errorf("mergedInto = %q, want the handler's survivor", res.MergedInto)
	}

	// An edit that merged nothing leaves the field out of the frame entirely, so an
	// older server that never sets it decodes to the same empty value.
	plain := proxy.EditEntityResult{}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rawHas(b, "mergedInto") {
		t.Errorf("result frame = %s, want no mergedInto key when nothing merged", b)
	}
}

// TestRenameEntityRoundTrip checks the credit half of an artist rename crosses the wire.
// The count is what the protocol carries: the per-item credit edits behind it stay on the
// server, which is where a proxied write-back runs.
func TestRenameEntityRoundTrip(t *testing.T) {
	var got proxy.RenameEntityParams
	handlers := map[string]proxy.Handler{
		proxy.MethodRenameEntity: func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &got)
			return proxy.RenameEntityResult{Outcome: "renamed", Members: 2, Credits: 3}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))

	res, err := c.RenameEntity(context.Background(), model.MergeArtist, "ar-1",
		map[string]string{"name": "Alpha Prime"}, true,
		model.Attribution{Source: model.SourceUser}, model.LockOn, false)
	if err != nil {
		t.Fatalf("rename entity: %v", err)
	}
	if got.EntityType != string(model.MergeArtist) || got.EntityPID != "ar-1" || got.Fields["name"] != "Alpha Prime" {
		t.Errorf("rename_entity params = %+v", got)
	}
	if res.Members != 2 || res.Credits != 3 {
		t.Errorf("result = %+v, want both halves of the count", res)
	}

	// A rename that moved no credit leaves the key out of the frame, so a peer that
	// never sets it decodes to the same zero.
	b, err := json.Marshal(proxy.RenameEntityResult{Outcome: "renamed", Members: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if rawHas(b, "credits") {
		t.Errorf("result frame = %s, want no credits key when none moved", b)
	}
}

// TestDetachRoundTrip checks the detach params reach the handler and the report comes
// back whole, including the write-back failures a partial tag strip reports as a result
// rather than a transport error.
func TestDetachRoundTrip(t *testing.T) {
	var got proxy.DetachParams
	handlers := map[string]proxy.Handler{
		proxy.MethodDetach: func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &got)
			return proxy.DetachResult{
				OldAlbumPID: "al-old", NewAlbumPID: "al-new", NewReleaseGroupPID: "rg-1",
				WriteBackFailures: []proxy.WriteBackFailure{{Path: "/x.mp3", Reason: "shared"}},
			}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))

	res, err := c.Detach(context.Background(), "i1", true)
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if got.ItemPID != "i1" || !got.WriteBack {
		t.Errorf("detach params = %+v, want i1 with write-back", got)
	}
	if res.OldAlbumPID != "al-old" || res.NewAlbumPID != "al-new" || res.NewReleaseGroupPID != "rg-1" {
		t.Errorf("detach result = %+v, want the handler's pids", res)
	}
	if len(res.WriteBackFailures) != 1 {
		t.Errorf("write-back failures = %v, want the one the handler reported", res.WriteBackFailures)
	}
}

// rawHas reports whether a JSON object literally carries a key, which is how the
// front role's absence from the frame is checked rather than inferred from a zero
// value.
func rawHas(raw json.RawMessage, key string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	_, ok := obj[key]
	return ok
}

// TestErrorCodePreserved checks a handler's waxerr Code survives the round-trip so
// the CLI's exit-code mapping is unchanged whether a command ran locally or proxied.
func TestErrorCodePreserved(t *testing.T) {
	handlers := map[string]proxy.Handler{
		proxy.MethodLock: func(context.Context, json.RawMessage) (any, error) {
			return nil, waxerr.New(waxerr.CodeLocked, "store.Lock", "field is locked")
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	err := c.Lock(context.Background(), "item1", []string{"artist"})
	if !waxerr.Is(err, waxerr.CodeLocked) {
		t.Fatalf("err = %v (code %s), want CodeLocked", err, waxerr.CodeOf(err))
	}
}

// TestUnknownMethod checks an unregistered method is a clean CodeInvalid, not a hang.
func TestUnknownMethod(t *testing.T) {
	c := dial(t, startServer(t, map[string]proxy.Handler{}, nil))
	// Users has no handler registered here.
	_, err := c.Users(context.Background())
	if !waxerr.Is(err, waxerr.CodeInvalid) {
		t.Fatalf("err = %v (code %s), want CodeInvalid", err, waxerr.CodeOf(err))
	}
}

// TestProtocolVersionRejected checks a frame with the wrong version is refused, and
// that the refusal names both versions under the stable prefix, which is what lets a
// newer client tell this from a dead socket and tell the operator which side to
// rebuild.
func TestProtocolVersionRejected(t *testing.T) {
	sock := startServer(t, map[string]proxy.Handler{}, nil)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(`{"v":99,"method":"ping"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
			Msg  string `json:"msg"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	if resp.OK || resp.Error.Code != string(waxerr.CodeInvalid) {
		t.Fatalf("resp = %+v, want a CodeInvalid error", resp)
	}
	want := fmt.Sprintf("%s 99 (this server speaks %d)", proxy.VersionMismatchPrefix, proxy.ProtocolVersion)
	if resp.Error.Msg != want {
		t.Fatalf("refusal msg = %q, want %q", resp.Error.Msg, want)
	}
}

// TestSocketPerms checks the control socket is created owner-only.
func TestSocketPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Chmod cannot express 0600 there; the socket's gate is the DACL it
		// inherits from the catalog directory.
		t.Skip("unix permissions")
	}
	sock := startServer(t, map[string]proxy.Handler{}, nil)
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %04o, want 0600", perm)
	}
}

// fakeMaintainer records begin/end calls and models a paused server.
type fakeMaintainer struct {
	mu     sync.Mutex
	begins int
	ends   int
}

func (m *fakeMaintainer) BeginMaintenance(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.begins++
	return nil
}
func (m *fakeMaintainer) EndMaintenance(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ends++
	return nil
}
func (m *fakeMaintainer) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.begins, m.ends
}

// TestMaintenancePausesAndResumes checks that while a maintenance session is held,
// other methods are refused with CodeConflict, and end restores service.
func TestMaintenancePausesAndResumes(t *testing.T) {
	var called atomic.Bool
	handlers := map[string]proxy.Handler{
		proxy.MethodLock: func(context.Context, json.RawMessage) (any, error) {
			called.Store(true)
			return nil, nil
		},
	}
	maint := &fakeMaintainer{}
	c := dial(t, startServer(t, handlers, maint))
	ctx := context.Background()

	if err := c.MaintenanceBegin(ctx); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if b, _ := maint.counts(); b != 1 {
		t.Fatalf("begins = %d, want 1", b)
	}
	// A normal method is refused while paused, and the handler must not run.
	if err := c.Lock(ctx, "item1", []string{"artist"}); !waxerr.Is(err, waxerr.CodeConflict) {
		t.Fatalf("lock during maintenance = %v, want CodeConflict", err)
	}
	if called.Load() {
		t.Fatal("handler ran while server was in maintenance")
	}

	if err := c.MaintenanceEnd(ctx); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, e := maint.counts(); e != 1 {
		t.Fatalf("ends = %d, want 1", e)
	}
	// Service is restored.
	if err := c.Lock(ctx, "item1", []string{"artist"}); err != nil {
		t.Fatalf("lock after maintenance: %v", err)
	}
	if !called.Load() {
		t.Fatal("handler did not run after maintenance ended")
	}
}

// TestMaintenanceAutoReopenOnDrop checks the crash-safety net: if the connection
// holding maintenance drops without an explicit end, the server reopens on its own.
func TestMaintenanceAutoReopenOnDrop(t *testing.T) {
	maint := &fakeMaintainer{}
	sock := startServer(t, map[string]proxy.Handler{}, maint)

	c, err := proxy.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.MaintenanceBegin(context.Background()); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Simulate a crashed client: close the connection without ending maintenance.
	_ = c.Close()

	// The server should detect the drop and reopen. The bound is loose because a
	// drop the platform never delivers to the waiting read is only seen when that
	// read is reissued, one probe interval later.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ends := maint.counts(); ends == 1 {
			break
		}
		if time.Now().After(deadline) {
			b, e := maint.counts()
			t.Fatalf("server did not auto-reopen after a dropped session (begins=%d ends=%d)", b, e)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lostCloseConn models what Windows' AF_UNIX does to a small share of closes: the
// read already pending when the peer drops never completes, and only a read issued
// after it reports the drop. It hands over one request frame first.
type lostCloseConn struct {
	mu         sync.Mutex
	req        []byte
	noDeadline bool // refuse deadlines, the way a conn that has none would
	pending    bool // the read whose completion the platform swallowed
	deadline   time.Time
	closed     chan struct{}
	once       sync.Once
}

func (c *lostCloseConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.req) > 0 {
		n := copy(p, c.req)
		c.req = c.req[n:]
		c.mu.Unlock()
		return n, nil
	}
	first := !c.pending
	c.pending = true
	deadline := c.deadline
	noDeadline := c.noDeadline
	c.mu.Unlock()
	if noDeadline {
		return 0, io.EOF // A conn with no deadlines to arm is an ordinary one.
	}
	if !first {
		return 0, io.EOF // A reissued read sees the peer is gone.
	}
	if deadline.IsZero() {
		<-c.closed // Nothing but a close ends a read waiting on a swallowed drop.
		return 0, net.ErrClosed
	}
	// The deadline fires; the test does not wait out the real interval.
	return 0, os.ErrDeadlineExceeded
}

func (c *lostCloseConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *lostCloseConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *lostCloseConn) LocalAddr() net.Addr              { return testAddr{} }
func (c *lostCloseConn) RemoteAddr() net.Addr             { return testAddr{} }
func (c *lostCloseConn) SetWriteDeadline(time.Time) error { return nil }
func (c *lostCloseConn) SetDeadline(t time.Time) error    { return c.SetReadDeadline(t) }
func (c *lostCloseConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noDeadline {
		return os.ErrNoDeadline
	}
	c.deadline = t
	return nil
}

// oneConnListener hands the server a single connection, then waits to be closed.
type oneConnListener struct {
	conn   net.Conn
	handed bool
	closed chan struct{}
	once   sync.Once
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	if !l.handed {
		l.handed = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}
func (l *oneConnListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *oneConnListener) Addr() net.Addr { return testAddr{} }

type testAddr struct{}

func (testAddr) Network() string { return "unix" }
func (testAddr) String() string  { return "test" }

// TestMaintenanceReopensOnAnUndeliveredDrop covers the same crash-safety net over a
// connection whose drop is never delivered to the read waiting on it. Waiting on
// that one read leaves the server paused for good, so it has to be reissued.
func TestMaintenanceReopensOnAnUndeliveredDrop(t *testing.T) {
	maint := &fakeMaintainer{}
	conn := &lostCloseConn{
		req:    fmt.Appendf(nil, "{\"v\":%d,\"method\":%q}\n", proxy.ProtocolVersion, proxy.MethodMaintenanceBegin),
		closed: make(chan struct{}),
	}
	ln := &oneConnListener{conn: conn, closed: make(chan struct{})}
	srv := proxy.NewServer(map[string]proxy.Handler{}, maint, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, e := maint.counts()
		if b == 1 && e == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("undelivered drop left the server paused (begins=%d ends=%d)", b, e)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestServesAConnectionThatHasNoDeadlines covers a listener Serve was handed from
// outside this package, whose connections answer os.ErrNoDeadline. The probe cannot
// run on one, so it steps aside rather than taking the connection down with it.
func TestServesAConnectionThatHasNoDeadlines(t *testing.T) {
	maint := &fakeMaintainer{}
	conn := &lostCloseConn{
		req:        fmt.Appendf(nil, "{\"v\":%d,\"method\":%q}\n", proxy.ProtocolVersion, proxy.MethodMaintenanceBegin),
		noDeadline: true,
		closed:     make(chan struct{}),
	}
	ln := &oneConnListener{conn: conn, closed: make(chan struct{})}
	srv := proxy.NewServer(map[string]proxy.Handler{}, maint, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, e := maint.counts()
		if b == 1 && e == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a connection without deadlines was not served (begins=%d ends=%d)", b, e)
		}
		time.Sleep(time.Millisecond)
	}
}

// maxSocketPath mirrors the unexported limit proxy derives from this platform's
// sockaddr_un (103 on darwin, 107 on Linux and Windows). Recomputed rather than
// hardcoded so the at-the-limit case below tests the real boundary everywhere.
const maxSocketPath = len(syscall.RawSockaddrUnix{}.Path) - 1

// TestSocketPathTooLong pins the length guard on both ends: without it the kernel
// answers a bare "invalid argument" naming neither the path nor a limit.
func TestSocketPathTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 200)+".sock")
	listenErr, dialErr := errOf2(proxy.Listen(long)), errOf2(proxy.Dial(long))
	if !waxerr.Is(listenErr, waxerr.CodeInvalid) {
		t.Errorf("Listen on a %d-byte path: want CodeInvalid, got %v", len(long), listenErr)
	}
	if !waxerr.Is(dialErr, waxerr.CodeInvalid) {
		t.Errorf("Dial on a %d-byte path: want CodeInvalid, got %v", len(long), dialErr)
	}
	// The error has to carry the length and the limit; the kernel's carries neither.
	if msg := listenErr.Error(); !strings.Contains(msg, "socket path is") || !strings.Contains(msg, "limit") {
		t.Errorf("error does not explain the failure: %q", msg)
	}

	// A path exactly at the limit binds, so the guard cannot drift into rejecting
	// valid paths. Skipped rather than panicking if the temp dir alone overruns it,
	// which is the deep-TMPDIR case this whole area exists for.
	dir := filepath.Dir(testsock.Path(t))
	pad := maxSocketPath - len(dir) - 1
	if pad < 1 {
		t.Skipf("temp dir %q is %d bytes, leaving no room under the %d-byte limit", dir, len(dir), maxSocketPath)
	}
	atLimit := filepath.Join(dir, strings.Repeat("y", pad))
	if len(atLimit) != maxSocketPath {
		t.Fatalf("built a %d-byte path, want exactly %d", len(atLimit), maxSocketPath)
	}
	ln, err := proxy.Listen(atLimit)
	if err != nil {
		t.Fatalf("Listen on a %d-byte path (exactly the limit): %v", len(atLimit), err)
	}
	ln.Close()
}

// errOf2 drops the value from a (T, error) pair so two calls can be made on one line.
func errOf2[T any](_ T, err error) error { return err }

// TestDialMissingSocket checks a dial to an absent socket fails cleanly, so a CLI
// can fall back to a direct open rather than hang.
func TestDialMissingSocket(t *testing.T) {
	// testSocket's directory exists and holds no socket, so the failure under test is
	// the missing socket and not an unbindable path.
	_, err := proxy.Dial(testsock.Path(t))
	if err == nil {
		t.Fatal("dial to a missing socket should fail")
	}
}

// TestCallHonorsContextCancel checks a call against an unresponsive server returns
// promptly when its context is canceled, instead of blocking forever on the read.
// A wedged server must not hang the CLI, and Ctrl-C must work. The bound below leaves
// a probe interval of slack: a watcher deadline landing just as the read probe rearms
// is clobbered, and the cancellation is seen on the following probe instead.
func TestCallHonorsContextCancel(t *testing.T) {
	block := make(chan struct{})
	handlers := map[string]proxy.Handler{
		proxy.MethodLock: func(context.Context, json.RawMessage) (any, error) {
			<-block // never answers during the test
			return nil, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	// Unblock the handler at teardown; registered after startServer/dial so it runs
	// first (LIFO) and lets the server stop cleanly.
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	done := make(chan error, 1)
	go func() { done <- c.Lock(ctx, "x", []string{"a"}) }()
	select {
	case err := <-done:
		if !waxerr.Is(err, waxerr.CodeCanceled) {
			t.Fatalf("err = %v (code %s), want CodeCanceled", err, waxerr.CodeOf(err))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not return after context cancellation (it hung)")
	}
}

// TestProvenanceRoundTripCarriesArtRow checks the art row the store overlays survives
// the wire, source URL included. The provenance result marshals model.FieldProvenance
// directly, so a field added to the model has to reach the client to be usable.
func TestProvenanceRoundTripCarriesArtRow(t *testing.T) {
	handlers := map[string]proxy.Handler{
		proxy.MethodProvenance: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.ItemParams
			_ = json.Unmarshal(raw, &p)
			if p.ItemPID != "i1" {
				t.Errorf("provenance pid = %q, want i1", p.ItemPID)
			}
			return []model.FieldProvenance{
				{ItemPID: "i1", Field: "art", Locked: true, UpdatedAt: 42, Attribution: model.Attribution{
					Source:    model.SourceEnrichment,
					Provider:  "coverartarchive",
					SourceURL: "https://coverartarchive.org/release-group/rg/front",
				}},
				{ItemPID: "i1", Field: "title", Value: "T",
					Attribution: model.Attribution{Source: model.SourceUser}},
			}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	rows, err := c.Provenance(context.Background(), "i1")
	if err != nil {
		t.Fatalf("provenance: %v", err)
	}
	if len(rows) != 2 || rows[0].Field != "art" {
		t.Fatalf("rows = %+v, want the art row first", rows)
	}
	art := rows[0]
	if art.Source != model.SourceEnrichment || art.Provider != "coverartarchive" ||
		art.SourceURL != "https://coverartarchive.org/release-group/rg/front" ||
		!art.Locked || art.UpdatedAt != 42 {
		t.Errorf("art row did not survive the wire: %+v", art)
	}
	if rows[1].SourceURL != "" {
		t.Errorf("scalar row picked up a source URL: %+v", rows[1])
	}
}

// TestAcquisitionRoundTrip checks both acquisition curation verbs reach their handlers
// whole. Every field on the set travels, an empty one included, since an emptied field
// is the claim that separates this from the merge-wise recording path.
func TestAcquisitionRoundTrip(t *testing.T) {
	var setP proxy.SetAcquisitionParams
	var clearP proxy.ClearAcquisitionParams
	var setRaw json.RawMessage
	handlers := map[string]proxy.Handler{
		proxy.MethodSetAcquisition: func(_ context.Context, raw json.RawMessage) (any, error) {
			setRaw = raw
			_ = json.Unmarshal(raw, &setP)
			return proxy.AcquisitionResult{
				WriteBackFailures: []proxy.WriteBackFailure{{Path: "/x.mp3", Reason: "shared"}},
			}, nil
		},
		proxy.MethodClearAcquisition: func(_ context.Context, raw json.RawMessage) (any, error) {
			_ = json.Unmarshal(raw, &clearP)
			return proxy.AcquisitionResult{}, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	ctx := context.Background()

	res, err := c.SetAcquisition(ctx, "i1", model.AcquisitionInput{
		SourceType: model.SourceManual, SourceURL: "https://right.test/y",
		AcquiredAt: 1_700_000_000_000_000_000,
	}, model.LockOn, true, true)
	if err != nil {
		t.Fatalf("set_acquisition: %v", err)
	}
	if setP.ItemPID != "i1" || setP.SourceType != "manual" || setP.SourceURL != "https://right.test/y" {
		t.Errorf("set params = %+v", setP)
	}
	if setP.AcquiredAt != 1_700_000_000_000_000_000 {
		t.Errorf("acquired at = %d, want the ns stamp intact across the wire", setP.AcquiredAt)
	}
	if setP.Lock != string(model.LockOn) || !setP.Force || !setP.WriteBack {
		t.Errorf("set params lost the lock/force/write-back trio: %+v", setP)
	}
	// The emptied fields travel rather than being elided, which is what makes the
	// replace authoritative on the far side.
	for _, key := range []string{"sourceId", "provider", "providerVersion", "options"} {
		if !rawHas(setRaw, key) {
			t.Errorf("set frame elided %q; an emptied field is a claim here", key)
		}
	}
	if len(res.WriteBackFailures) != 1 {
		t.Errorf("write-back failures = %v, want the one the handler reported", res.WriteBackFailures)
	}

	if _, err := c.ClearAcquisition(ctx, "i2", model.LockUnchanged, false, false); err != nil {
		t.Fatalf("clear_acquisition: %v", err)
	}
	if clearP.ItemPID != "i2" || clearP.Lock != "" || clearP.Force || clearP.WriteBack {
		t.Errorf("clear params = %+v, want a bare clear on i2", clearP)
	}
}

// pastReadProbe outlasts the client's read probe interval, so a test that sleeps this
// long forces the reader through a reissue. The interval itself is a second, and it is
// unexported, so this tracks it by hand.
const pastReadProbe = 1200 * time.Millisecond

// startRawServer runs a bare listener speaking the wire protocol by hand, so a test can
// control exactly how, when, and whether a response reaches the socket. handle gets the
// one accepted connection and may close it.
func startRawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	sock := testsock.Path(t)
	ln, err := proxy.Listen(sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handle(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("raw server did not stop")
		}
	})
	return sock
}

// TestSlowResponseOutlastsTheReadProbe checks the client reissues rather than failing a
// call whose server simply took its time. The probe is a liveness check, not a timeout.
func TestSlowResponseOutlastsTheReadProbe(t *testing.T) {
	sock := startRawServer(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		time.Sleep(pastReadProbe)
		_, _ = conn.Write([]byte("{\"ok\":true}\n"))
	})
	if err := dial(t, sock).Ping(context.Background()); err != nil {
		t.Fatalf("ping over a slow response: %v", err)
	}
}

// TestSplitResponseDecodesIntact pins the partial-read branch: a frame whose halves
// straddle a probe boundary comes back whole. Returning the deadline error with bytes
// already in flight would poison the decoder instead, and the timeout would stick.
func TestSplitResponseDecodesIntact(t *testing.T) {
	const frame = `{"ok":true,"data":[{"Field":"title","Value":"Kid A","Locked":true}]}` + "\n"
	half := len(frame) / 2
	sock := startRawServer(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		_, _ = conn.Write([]byte(frame[:half]))
		time.Sleep(pastReadProbe)
		_, _ = conn.Write([]byte(frame[half:]))
	})
	rows, err := dial(t, sock).Provenance(context.Background(), "i1")
	if err != nil {
		t.Fatalf("provenance across a probe boundary: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "title" || rows[0].Value != "Kid A" || !rows[0].Locked {
		t.Fatalf("rows = %+v, want the split frame decoded whole", rows)
	}
}

// TestCanceledCallLeavesTheConnectionUsable covers a Client an embedder keeps after a
// caller gave up on one call. Two things used to retire it: the cancel watcher's
// deadline was cleared only on the success path, so every later write failed instantly,
// and the json.Decoder remembered the read error forever. The late answer to the
// abandoned call is on the wire too, so the call that follows has to discard it rather
// than read it as its own.
func TestCanceledCallLeavesTheConnectionUsable(t *testing.T) {
	sock := startRawServer(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		// Answer the first call after its caller has already given up on it.
		time.Sleep(150 * time.Millisecond)
		_, _ = conn.Write([]byte("{\"ok\":true}\n"))
		if _, err := br.ReadString('\n'); err != nil {
			return
		}
		_, _ = conn.Write([]byte(`{"ok":true,"data":[{"Field":"title"}]}` + "\n"))
	})
	cl := dial(t, sock)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := cl.Ping(ctx); !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("first ping = %v (code %s), want CodeCanceled", err, waxerr.CodeOf(err))
	}
	rows, err := cl.Provenance(context.Background(), "i1")
	if err != nil {
		t.Fatalf("second call on the same client: %v", err)
	}
	if len(rows) != 1 || rows[0].Field != "title" {
		t.Fatalf("rows = %+v, want this call's own answer and not the abandoned one", rows)
	}
}

// TestServerCloseMidCallDoesNotHang covers a server that dies holding a call. Windows'
// AF_UNIX can drop the completion of the read that was already pending, so without the
// reissue this blocks for good instead of reporting the drop.
func TestServerCloseMidCallDoesNotHang(t *testing.T) {
	sock := startRawServer(t, func(conn net.Conn) {
		_, _ = bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
	})
	cl := dial(t, sock)
	done := make(chan error, 1)
	go func() { done <- cl.Ping(context.Background()) }()
	select {
	case err := <-done:
		if !waxerr.Is(err, waxerr.CodeIO) {
			t.Fatalf("err = %v (code %s), want CodeIO", err, waxerr.CodeOf(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a call outliving its server hung instead of failing")
	}
}

// TestCanceledPartialWriteRetiresTheConnection: a request the cancel deadline cut off
// halfway leaves bytes on the wire with no way to say where the frame ended, and nothing
// in the protocol pairs a response with its request. The Client refuses further calls
// with a reason instead of appending a second request onto the stump of the first.
func TestCanceledPartialWriteRetiresTheConnection(t *testing.T) {
	if runtime.GOOS == "windows" {
		// AF_UNIX there gives the sender no backpressure: it took a 256MB write whole
		// while the peer had read 4KB, so the frame this tears on other platforms always
		// goes out intact and the call that follows waits on the answer the stalled
		// server never sends. TestTornWriteRetiresTheConnection covers the refusal
		// itself without a socket.
		t.Skip("no write backpressure to interrupt")
	}
	// The cancel has to land while the write is in flight, so the server reads enough to
	// prove it started and then stops, leaving the rest of an oversized frame to fill the
	// socket buffer and block. A timer alone races the write and, losing, cancels a call
	// that never reached the wire at all.
	started := make(chan struct{})
	sock := startRawServer(t, func(conn net.Conn) {
		if _, err := io.ReadFull(conn, make([]byte, 4096)); err != nil {
			return
		}
		close(started)
		<-t.Context().Done()
	})
	cl := dial(t, sock)

	huge := make([]string, 512)
	for i := range huge {
		huge[i] = strings.Repeat("x", 16*1024)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	if err := cl.Lock(ctx, "i1", huge); !waxerr.Is(err, waxerr.CodeCanceled) {
		t.Fatalf("canceled oversized call = %v (code %s), want CodeCanceled", err, waxerr.CodeOf(err))
	}

	// Bounded, so a Client that wrongly carried on reports a timeout here rather than
	// hanging the package until the test binary's own alarm.
	next, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	err := cl.Ping(next)
	if !waxerr.Is(err, waxerr.CodeIO) || !strings.Contains(err.Error(), "framing") {
		t.Fatalf("call after a torn request = %v (code %s), want a CodeIO framing refusal",
			err, waxerr.CodeOf(err))
	}
}
