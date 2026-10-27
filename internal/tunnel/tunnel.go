// Package tunnel carries connections from inside an isolation out to the
// daemon's credential proxies, over the exec channel and nothing else.
//
// The isolation can accept connections but cannot originate any toward the
// host — the fence exists to make sure of that — so every byte of every
// connection has to leave through a channel the host opened. One channel
// carrying many connections is a multiplexer, and SSH is a multiplexer that
// has been attacked for thirty years; a hand-rolled framing protocol in this
// position fails quietly, mixing streams or leaking connections. This is
// isolate's reasoning, kept. What changes is the implementation: Go's SSH
// package on both ends, so the image needs no sshd and there are no keys to
// manage.
//
//	isolation  gero-shim tunnel   SSH server on its stdio; listens on
//	           127.0.0.1:<port> when the daemon asks, and hands every
//	           accepted connection back as a forwarded-tcpip channel
//	              |  container exec -i (the same channel the shim client uses)
//	host       daemon             SSH client; each forwarded connection goes
//	                              straight to an in-process proxy
//
// Authentication is the exec channel itself: only someone who can run
// `container exec` on this Mac reaches the server, and the client is
// talking to a process it just started through a pipe. So the server takes
// the "none" method and a host key made up for the session, and the client
// accepts any host key. There is no network leg to secure.
package tunnel

import (
	"io"
	"net"
	"time"
)

// pipeConn presents a byte stream (exec stdio) as the net.Conn the ssh
// package wants. Deadlines are not supported; keepalives detect a dead peer
// instead.
type pipeConn struct {
	io.Reader
	io.Writer
	close func() error
}

func (c pipeConn) Close() error                     { return c.close() }
func (pipeConn) LocalAddr() net.Addr                { return pipeAddr{} }
func (pipeConn) RemoteAddr() net.Addr               { return pipeAddr{} }
func (pipeConn) SetDeadline(t time.Time) error      { return nil }
func (pipeConn) SetReadDeadline(t time.Time) error  { return nil }
func (pipeConn) SetWriteDeadline(t time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "exec" }

func asConn(rw io.ReadWriteCloser) net.Conn {
	if c, ok := rw.(net.Conn); ok {
		return c
	}
	return pipeConn{Reader: rw, Writer: rw, close: rw.Close}
}

// keepalive is how often each end proves the other is still there. An exec
// client killed on the host can leave its process running in the container
// with its stdin never closing; without this, a tunnel nobody uses would
// hold its ports forever.
const (
	keepaliveEvery = 15 * time.Second
	keepaliveMiss  = 3
)
