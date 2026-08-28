package proxy

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/colespringer/waxbin/waxerr"
)

// failWriteConn fails its first write and behaves for every later one, so a test can
// pick which of the two failures a Client has to tell apart. It stands in for a socket
// because Windows' AF_UNIX gives a sender no backpressure: it swallowed a 256MB write
// while the peer had read 4KB, so no cancel there can interrupt a write at all.
type failWriteConn struct {
	net.Conn        // a pipe end, so the methods this leaves out work rather than panic
	torn     bool   // whether the failed write reports bytes already on the wire
	writes   int    // how many calls reached Write; a refused one never does
	reply    []byte // the response frame the reads after a good write hand back
}

func newFailWriteConn() *failWriteConn {
	end, _ := net.Pipe()
	return &failWriteConn{Conn: end}
}

func (c *failWriteConn) Write(p []byte) (int, error) {
	c.writes++
	if c.writes > 1 {
		return len(p), nil
	}
	if c.torn {
		return len(p) / 2, os.ErrDeadlineExceeded
	}
	return 0, os.ErrDeadlineExceeded
}

func (c *failWriteConn) Read(p []byte) (int, error) {
	if len(c.reply) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.reply)
	c.reply = c.reply[n:]
	return n, nil
}

// TestTornWriteRetiresTheConnection covers the half-sent request, which is what
// TestCanceledPartialWriteRetiresTheConnection reaches over a real socket on the
// platforms whose sockets can be made to block mid write. Nothing in the protocol
// pairs a response with its request, so a frame that ended partway leaves the framing
// unknowable and the calls after it have to be refused rather than appended to the stump.
func TestTornWriteRetiresTheConnection(t *testing.T) {
	conn := newFailWriteConn()
	conn.torn = true
	c := newClient(conn)
	if err := c.Ping(context.Background()); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("torn request = %v (code %s), want CodeIO", err, waxerr.CodeOf(err))
	}
	err := c.Ping(context.Background())
	if !waxerr.Is(err, waxerr.CodeIO) || !strings.Contains(err.Error(), "framing") {
		t.Fatalf("call after a torn request = %v (code %s), want a CodeIO framing refusal",
			err, waxerr.CodeOf(err))
	}
	if conn.writes != 1 {
		t.Fatalf("writes = %d, want the refused call to have stayed off the wire", conn.writes)
	}
}

// TestWriteThatNeverLeftKeepsTheConnection is the other half of that distinction. A
// write the deadline turned away before a byte went out leaves the framing whole, and
// retiring the Client for it would end a healthy connection over an ordinary cancel.
func TestWriteThatNeverLeftKeepsTheConnection(t *testing.T) {
	conn := newFailWriteConn()
	conn.reply = []byte("{\"ok\":true}\n")
	c := newClient(conn)
	if err := c.Ping(context.Background()); !waxerr.Is(err, waxerr.CodeIO) {
		t.Fatalf("failed request = %v (code %s), want CodeIO", err, waxerr.CodeOf(err))
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("call after a write that never left the client: %v", err)
	}
	if conn.writes != 2 {
		t.Fatalf("writes = %d, want the second call to have reached the wire", conn.writes)
	}
}
