package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/colespringer/waxbin/model"
	"github.com/colespringer/waxbin/proxy"
	"github.com/colespringer/waxbin/waxerr"
)

// startServer runs a proxy server on a temp socket with the given handlers and
// (optional) maintainer, and returns its socket path. The server is torn down at
// test cleanup.
func startServer(t *testing.T, handlers map[string]proxy.Handler, maint proxy.Maintainer) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
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

	res, err := c.EditFields(ctx, "item1", map[string]string{"artist": "New"}, true, true, false)
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
			if !p.WriteBack || !p.Lock || p.Force || !p.SkipLocked {
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
	}, true, true, false, true)
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
	handlers := map[string]proxy.Handler{
		proxy.MethodSetCredits: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetCreditsParams
			_ = json.Unmarshal(raw, &p)
			if p.ItemPID != "i1" || p.Role != "producer" || len(p.Names) != 2 {
				t.Errorf("credit params = %+v", p)
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
			return nil, nil
		},
		proxy.MethodSetEntityArt: func(_ context.Context, raw json.RawMessage) (any, error) {
			var p proxy.SetEntityArtParams
			_ = json.Unmarshal(raw, &p)
			if p.EntityType != "album" || p.Role != "front" {
				t.Errorf("entity art params = %+v", p)
			}
			return nil, nil
		},
	}
	c := dial(t, startServer(t, handlers, nil))
	ctx := context.Background()

	res, err := c.SetCredits(ctx, "i1", "producer", []string{"A", "B"}, true, true, false)
	if err != nil {
		t.Fatalf("set credits: %v", err)
	}
	if res.Stored != 2 || len(res.WriteBackFailures) != 1 {
		t.Fatalf("credit result = %+v", res)
	}

	ly := &model.Lyrics{Synced: []model.SyncedLine{{TimeMS: 10, Text: "hi"}}}
	if err := c.SetLyrics(ctx, "i1", ly, true, false); err != nil {
		t.Fatalf("set lyrics: %v", err)
	}
	if gotLyrics == nil || len(gotLyrics.Synced) != 1 || gotLyrics.Synced[0].Text != "hi" {
		t.Fatalf("lyrics not carried: %+v", gotLyrics)
	}

	if _, err := c.SetItemArt(ctx, "i1", model.ArtRoleBack, []byte{1, 2, 3, 4}, true, false, false); err != nil {
		t.Fatalf("set item art: %v", err)
	}
	if len(gotArt) != 4 || gotArt[0] != 1 {
		t.Fatalf("art bytes not carried: %v", gotArt)
	}

	if _, err := c.SetEntityArt(ctx, model.ArtAlbum, "a1", model.ArtRoleFront, []byte{9}, false); err != nil {
		t.Fatalf("set entity art: %v", err)
	}
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

// TestProtocolVersionRejected checks a frame with the wrong version is refused.
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
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", line, err)
	}
	if resp.OK || resp.Error.Code != string(waxerr.CodeInvalid) {
		t.Fatalf("resp = %+v, want a CodeInvalid error", resp)
	}
}

// TestSocketPerms checks the control socket is created owner-only.
func TestSocketPerms(t *testing.T) {
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

	// The server should detect the drop and reopen.
	deadline := time.Now().Add(2 * time.Second)
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

// TestDialMissingSocket checks a dial to an absent socket fails cleanly, so a CLI
// can fall back to a direct open rather than hang.
func TestDialMissingSocket(t *testing.T) {
	_, err := proxy.Dial(filepath.Join(t.TempDir(), "nope.sock"))
	if err == nil {
		t.Fatal("dial to a missing socket should fail")
	}
}

// TestCallHonorsContextCancel checks a call against an unresponsive server returns
// promptly when its context is canceled, instead of blocking forever on the read.
// A wedged server must not hang the CLI, and Ctrl-C must work.
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
