package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/colespringer/waxbin/waxerr"
)

// Handler runs one proxied method. It receives the raw params frame and returns a
// value to marshal into the response data, or an error to serialize as the typed
// wire error. A nil return value serializes to an empty (ok, no data) response.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Maintainer is the server's hook into a Library's maintenance-mode hand-off. A
// begin closes the Library and releases the write lock so a foreground CLI can
// take it; an end reopens the Library and resyncs. It is optional: a server built
// without one refuses the maintenance methods.
type Maintainer interface {
	BeginMaintenance(ctx context.Context) error
	EndMaintenance(ctx context.Context) error
}

// Server dispatches proxied methods over a unix-socket listener. It is safe for
// concurrent connections; each connection is served on its own goroutine and the
// handlers must themselves be safe for concurrent use (the waxbin Library is).
type Server struct {
	handlers map[string]Handler
	maint    Maintainer
	log      *slog.Logger

	mu        sync.Mutex // guards maintConn
	maintConn net.Conn   // the connection currently holding maintenance, or nil

	connMu       sync.Mutex            // guards conns + shuttingDown
	conns        map[net.Conn]struct{} // active connections, closed on shutdown
	shuttingDown bool
}

// NewServer builds a server from a map of method names to handlers and an optional
// Maintainer. The map is used as given, not copied; callers build it once per Serve.
func NewServer(handlers map[string]Handler, maint Maintainer, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{handlers: handlers, maint: maint, log: log}
}

// Serve accepts connections on ln until ctx is canceled or ln is closed, then
// returns nil. A genuine accept failure is returned. Closing ln (or canceling
// ctx) is the shutdown signal.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// Unblock Accept on cancellation by closing the listener, and force-close any
	// active connections so a blocked read returns and shutdown does not wait on a
	// still-connected client. The resulting accept error is treated as a clean
	// shutdown.
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		s.connMu.Lock()
		s.shuttingDown = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.connMu.Unlock()
	}()
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			wg.Wait()
			return waxerr.Wrap(waxerr.CodeIO, "proxy.Serve", err)
		}
		wg.Go(func() {
			s.serveConn(ctx, conn)
		})
	}
}

// serveConn reads request frames from one connection and writes a response for
// each until the connection closes or a frame fails to decode. When the
// connection drops while it holds maintenance mode (a crashed CLI), the deferred
// release reopens the Library so the server never stays paused.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	if !s.trackConn(conn) {
		_ = conn.Close()
		return
	}
	defer s.untrackConn(conn)
	defer conn.Close()
	defer s.releaseMaintenanceIfHeld(ctx, conn)

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return // EOF, a closed connection, or an unrecoverable framing error.
		}
		resp := s.dispatch(ctx, conn, req)
		if err := enc.Encode(&resp); err != nil {
			return
		}
	}
}

// dispatch routes one request. Maintenance control is handled inline (it mutates
// the Library the handlers close over); every other method is rejected while the
// server is paused for maintenance.
func (s *Server) dispatch(ctx context.Context, conn net.Conn, req request) response {
	if req.V != ProtocolVersion {
		return errResponse(waxerr.New(waxerr.CodeInvalid, "proxy.dispatch", "unsupported protocol version"))
	}
	switch req.Method {
	case MethodPing:
		return okResponse(nil)
	case MethodMaintenanceBegin:
		return s.beginMaintenance(ctx, conn)
	case MethodMaintenanceEnd:
		return s.endMaintenance(ctx, conn)
	}

	if s.isPaused() {
		return errResponse(waxerr.New(waxerr.CodeConflict, "proxy.dispatch",
			"server is in maintenance mode; another client holds the lock"))
	}
	h, ok := s.handlers[req.Method]
	if !ok {
		return errResponse(waxerr.New(waxerr.CodeInvalid, "proxy.dispatch", "unknown method: "+req.Method))
	}
	data, err := h(ctx, req.Params)
	if err != nil {
		return errResponse(err)
	}
	return okResponse(data)
}

func (s *Server) beginMaintenance(ctx context.Context, conn net.Conn) response {
	if s.maint == nil {
		return errResponse(waxerr.New(waxerr.CodeUnsupported, "proxy.maintenance", "maintenance mode is not available"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintConn != nil {
		return errResponse(waxerr.New(waxerr.CodeConflict, "proxy.maintenance", "a maintenance session is already active"))
	}
	if err := s.maint.BeginMaintenance(ctx); err != nil {
		return errResponse(err)
	}
	s.maintConn = conn
	return okResponse(nil)
}

func (s *Server) endMaintenance(ctx context.Context, conn net.Conn) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintConn != conn {
		return errResponse(waxerr.New(waxerr.CodeInvalid, "proxy.maintenance", "no maintenance session on this connection"))
	}
	// Clear the binding first: whether the reopen succeeds or fails, this
	// connection no longer owns maintenance, so the deferred drop handler must not
	// reopen a second time.
	s.maintConn = nil
	if err := s.maint.EndMaintenance(ctx); err != nil {
		return errResponse(err)
	}
	return okResponse(nil)
}

// releaseMaintenanceIfHeld reopens the Library if conn ended while holding
// maintenance mode. This is the crash-safety net: a CLI that dies mid-hand-off
// must not leave the server paused forever. On server shutdown (ctx canceled) it
// deliberately does not reopen: the server is going down, and any foreground
// process still holds the lock it was handed.
func (s *Server) releaseMaintenanceIfHeld(ctx context.Context, conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maintConn != conn {
		return
	}
	s.maintConn = nil
	if ctx.Err() != nil {
		return
	}
	if err := s.maint.EndMaintenance(ctx); err != nil {
		s.log.Error("proxy: reopening after a dropped maintenance session", "err", err)
	}
}

// trackConn registers an active connection, returning false when the server is
// already shutting down (so the caller closes the connection immediately).
func (s *Server) trackConn(conn net.Conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.shuttingDown {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrackConn(conn net.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	delete(s.conns, conn)
}

func (s *Server) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maintConn != nil
}

func okResponse(data any) response {
	if data == nil {
		return response{OK: true}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return errResponse(waxerr.Wrap(waxerr.CodeInternal, "proxy.encode", err))
	}
	return response{OK: true, Data: raw}
}

func errResponse(err error) response {
	return response{OK: false, Error: toWireError(err)}
}

// Listen opens an owner-only (0600) unix-domain listener at path, removing a
// stale socket file left by a previous crash. The 0600 mode matters: the endpoint
// drives admin mutations, so a broader mode would let any local user issue
// unauthenticated writes.
func Listen(path string) (net.Listener, error) {
	const op = "proxy.Listen"
	// A leftover socket file from a crashed owner would make net.Listen fail with
	// "address already in use" even though nothing is listening. Removing a plain
	// file (not a live socket) is safe here because write ownership is guarded by
	// the flock, not this file.
	if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, waxerr.Wrap(waxerr.CodeIO, op, err)
	}
	return ln, nil
}
