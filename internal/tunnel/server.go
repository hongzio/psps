package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// Allow decides which forwards the server opens, and which addresses the
// daemon may connect to. The daemon is trusted, but a listener and a
// connection are the only things this process creates inside the
// isolation, so what they may be is spelled out rather than implied.
type Allow func(host string, port int) bool

// LoopbackPorts allows 127.0.0.1 on the given ports only.
func LoopbackPorts(lo, hi int) Allow {
	return func(host string, port int) bool {
		return host == "127.0.0.1" && port >= lo && port <= hi
	}
}

// LoopbackExcept allows the isolation's own loopback on any port but
// lo..hi, the tunnel's own listeners: a server an agent runs there,
// reached from the Mac as a forwarded port.
func LoopbackExcept(lo, hi int) Allow {
	return func(host string, port int) bool {
		return (host == "127.0.0.1" || host == "::1" || host == "localhost") && port > 0 && port <= 65535 && (port < lo || port > hi)
	}
}

// Serve runs the isolation side of a tunnel over rw until the daemon goes
// away. It accepts no sessions: it listens where it is asked to, within
// listen, and hands connections back; and it connects the daemon to the
// isolation's own addresses within dial (none when nil). Nothing it does
// reaches past the isolation's loopback.
func Serve(rw io.ReadWriteCloser, listen, dial Allow, logf func(string, ...any)) error {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return err
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	conn, chans, reqs, err := ssh.NewServerConn(asConn(rw), cfg)
	if err != nil {
		return fmt.Errorf("tunnel handshake: %w", err)
	}
	s := &server{conn: conn, allow: listen, dial: dial, logf: logf, listeners: map[string]net.Listener{}}
	go func() {
		for nc := range chans {
			if nc.ChannelType() == "direct-tcpip" && dial != nil {
				go s.direct(nc)
				continue
			}
			nc.Reject(ssh.Prohibited, "this tunnel only forwards out of the isolation")
		}
	}()
	go s.keepalive()
	for req := range reqs {
		s.handle(req)
	}
	s.closeAll()
	return conn.Wait()
}

type server struct {
	conn  *ssh.ServerConn
	allow Allow
	dial  Allow
	logf  func(string, ...any)

	mu        sync.Mutex
	listeners map[string]net.Listener
	closed    bool
}

// RFC 4254 7.1.
type forwardRequest struct {
	Host string
	Port uint32
}

type forwardReply struct {
	Port uint32
}

// RFC 4254 7.2; a direct-tcpip channel's payload has the same shape, the
// address to connect to first.
type forwardedChannel struct {
	Host       string
	Port       uint32
	OriginHost string
	OriginPort uint32
}

func (s *server) handle(req *ssh.Request) {
	switch req.Type {
	case "tcpip-forward":
		var fr forwardRequest
		if err := ssh.Unmarshal(req.Payload, &fr); err != nil {
			req.Reply(false, nil)
			return
		}
		if fr.Port == 0 || fr.Port > 65535 || !s.allow(fr.Host, int(fr.Port)) {
			s.logf("refused forward %s:%d", fr.Host, fr.Port)
			req.Reply(false, nil)
			return
		}
		addr := net.JoinHostPort(fr.Host, strconv.Itoa(int(fr.Port)))
		l, err := listenSettled(addr)
		if err != nil {
			s.logf("forward %s: %v", addr, err)
			req.Reply(false, nil)
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			l.Close()
			req.Reply(false, nil)
			return
		}
		if old, ok := s.listeners[addr]; ok {
			old.Close()
		}
		s.listeners[addr] = l
		s.mu.Unlock()
		req.Reply(true, ssh.Marshal(forwardReply{Port: fr.Port}))
		go s.accept(l, fr)
	case "cancel-tcpip-forward":
		var fr forwardRequest
		if err := ssh.Unmarshal(req.Payload, &fr); err != nil {
			req.Reply(false, nil)
			return
		}
		addr := net.JoinHostPort(fr.Host, strconv.Itoa(int(fr.Port)))
		s.mu.Lock()
		l, ok := s.listeners[addr]
		delete(s.listeners, addr)
		s.mu.Unlock()
		if ok {
			l.Close()
		}
		req.Reply(ok, nil)
	default:
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func (s *server) accept(l net.Listener, fr forwardRequest) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.forward(c, fr)
	}
}

func (s *server) forward(c net.Conn, fr forwardRequest) {
	defer c.Close()
	origin := c.RemoteAddr().(*net.TCPAddr)
	ch, reqs, err := s.conn.OpenChannel("forwarded-tcpip", ssh.Marshal(forwardedChannel{
		Host: fr.Host, Port: fr.Port, OriginHost: origin.IP.String(), OriginPort: uint32(origin.Port),
	}))
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	splice(c, ch)
}

// direct connects the daemon to a port on the isolation's loopback, as a
// process there would. "localhost" is tried as 127.0.0.1 and then ::1: a
// server told to listen on localhost takes either.
func (s *server) direct(nc ssh.NewChannel) {
	var d forwardedChannel
	if err := ssh.Unmarshal(nc.ExtraData(), &d); err != nil {
		nc.Reject(ssh.ConnectionFailed, "malformed request")
		return
	}
	if d.Port == 0 || d.Port > 65535 || !s.dial(d.Host, int(d.Port)) {
		s.logf("refused a connection to %s:%d", d.Host, d.Port)
		nc.Reject(ssh.Prohibited, fmt.Sprintf("%s:%d is not the isolation's to connect to", d.Host, d.Port))
		return
	}
	hosts := []string{d.Host}
	if d.Host == "localhost" {
		hosts = []string{"127.0.0.1", "::1"}
	}
	var c net.Conn
	var first error
	for _, h := range hosts {
		var err error
		if c, err = net.DialTimeout("tcp", net.JoinHostPort(h, strconv.Itoa(int(d.Port))), dialTimeout); err == nil {
			break
		}
		if first == nil {
			first = err
		}
	}
	if c == nil {
		nc.Reject(ssh.ConnectionFailed, first.Error())
		return
	}
	defer c.Close()
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	splice(c, ch)
}

var dialTimeout = 5 * time.Second

// splice copies both ways and half-closes each direction as it ends, so a
// client that finishes sending (git's request body) still reads the whole
// response.
func splice(c net.Conn, ch ssh.Channel) {
	done := make(chan struct{})
	go func() {
		io.Copy(ch, c)
		ch.CloseWrite()
		close(done)
	}()
	io.Copy(c, ch)
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseWrite()
	}
	<-done
	ch.Close()
}

func (s *server) keepalive() {
	t := time.NewTicker(keepaliveEvery)
	defer t.Stop()
	missed := 0
	for range t.C {
		res := make(chan error, 1)
		go func() {
			_, _, err := s.conn.SendRequest("keepalive@gero", true, nil)
			res <- err
		}()
		select {
		case err := <-res:
			if err != nil && !errors.Is(err, io.EOF) {
				missed++
			} else if err != nil {
				return
			} else {
				missed = 0
			}
		case <-time.After(keepaliveEvery):
			missed++
		}
		if missed >= keepaliveMiss {
			s.logf("the daemon stopped answering; closing the tunnel")
			s.conn.Close()
			return
		}
	}
}

func (s *server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for addr, l := range s.listeners {
		l.Close()
		delete(s.listeners, addr)
	}
}

// listenSettled listens on addr, waiting out a holder that is on its way
// out: a tunnel being replaced closes its listeners as it exits, which can
// be a moment after the new one is asked for the same ports.
func listenSettled(addr string) (net.Listener, error) {
	deadline := time.Now().Add(settleFor)
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			return l, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

var settleFor = 3 * time.Second
