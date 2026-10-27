package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
)

// Tunnel is the daemon's end: connections accepted inside the isolation
// arrive on the listeners Listen returns.
type Tunnel struct {
	client *ssh.Client
	done   chan struct{}
	err    error
}

// Open handshakes over rw, which is the stdio of `gero-shim tunnel` in the
// isolation.
func Open(rw io.ReadWriteCloser) (*Tunnel, error) {
	cfg := &ssh.ClientConfig{
		User: "gero",
		// The peer is a process this daemon just started through a pipe; see
		// the package comment.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	conn, chans, reqs, err := ssh.NewClientConn(asConn(rw), "isolation", cfg)
	if err != nil {
		rw.Close()
		return nil, fmt.Errorf("tunnel handshake: %w", err)
	}
	t := &Tunnel{client: ssh.NewClient(conn, chans, reqs), done: make(chan struct{})}
	go func() {
		t.err = t.client.Wait()
		close(t.done)
	}()
	go t.keepalive()
	return t, nil
}

// Listen asks the isolation to listen on 127.0.0.1:port and returns what it
// accepts.
func (t *Tunnel) Listen(port int) (net.Listener, error) {
	l, err := t.client.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("tunnel: listen on %d in the isolation: %w", port, err)
	}
	return l, nil
}

func (t *Tunnel) Close() error { return t.client.Close() }

// Done is closed when the tunnel ends.
func (t *Tunnel) Done() <-chan struct{} { return t.done }

func (t *Tunnel) Err() error {
	select {
	case <-t.done:
		if t.err == nil || errors.Is(t.err, io.EOF) {
			return errors.New("tunnel closed")
		}
		return t.err
	default:
		return nil
	}
}

func (t *Tunnel) keepalive() {
	tick := time.NewTicker(keepaliveEvery)
	defer tick.Stop()
	missed := 0
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
		}
		res := make(chan error, 1)
		go func() {
			_, _, err := t.client.SendRequest("keepalive@gero", true, nil)
			res <- err
		}()
		select {
		case err := <-res:
			if err != nil {
				missed++
			} else {
				missed = 0
			}
		case <-time.After(keepaliveEvery):
			missed++
		case <-t.done:
			return
		}
		if missed >= keepaliveMiss {
			t.client.Close()
			return
		}
	}
}
