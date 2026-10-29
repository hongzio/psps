package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/portproxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/tunnel"
	"github.com/hongzio/gero/internal/web"
)

// A forwarded port (port) is a listener the daemon holds on the Mac's
// 127.0.0.1, and each connection it accepts is connected, through the
// isolation's tunnel, to a port on the isolation's own loopback. The
// listener is the daemon's, not the container's: it stays while its
// isolation is down, and tells whoever connects so.

type forwardKey struct{ iso, service, env string }

type forwards struct {
	mu   sync.Mutex
	open map[int]*hostForward // by Mac port
	// errs says why a forward the store asks for is not listening.
	errs map[forwardKey]string
}

type hostForward struct {
	key forwardKey
	fw  proxy.Forward
	l   net.Listener
	srv *http.Server // an http forward's

	mu     sync.Mutex
	conns  map[net.Conn]bool // into the isolation, and a tcp one's from the Mac
	closed bool
}

// track keeps c, the isolation's side of a connection, so closing the
// forward cuts it too.
func (o *hostForward) track(c net.Conn, err error) (net.Conn, error) {
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		c.Close()
		return nil, proxy.Deny("127.0.0.1:%d is not forwarded any more", o.fw.HostPort)
	}
	o.conns[c] = true
	return &trackedConn{Conn: c, o: o}, nil
}

type trackedConn struct {
	net.Conn
	o *hostForward
}

func (c *trackedConn) Close() error {
	c.o.mu.Lock()
	delete(c.o.conns, c.Conn)
	c.o.mu.Unlock()
	return c.Conn.Close()
}

func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// hold keeps c, the Mac's side of a tcp connection, for close to cut.
func (o *hostForward) hold(c net.Conn) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	o.conns[c] = true
	return true
}

func (o *hostForward) release(c net.Conn) {
	o.mu.Lock()
	delete(o.conns, c)
	o.mu.Unlock()
}

func (o *hostForward) close() {
	o.l.Close()
	if o.srv != nil {
		o.srv.Close()
	}
	o.mu.Lock()
	o.closed = true
	for c := range o.conns {
		c.Close()
	}
	o.mu.Unlock()
}

type wantedForward struct {
	key forwardKey
	fw  proxy.Forward
}

// forwardsWanted is every forward the store asks for. Two on one Mac port
// cannot both be: the one listening keeps it, and the other is a clash.
// Called with m.fwd.mu held.
func (m *Manager) forwardsWanted() (want []wantedForward, clash map[forwardKey]string) {
	clash = map[forwardKey]string{}
	isos, err := m.store.Isolations()
	if err != nil {
		m.log.Printf("forwarded ports: %v", err)
		return nil, clash
	}
	var all []wantedForward
	for _, iso := range isos {
		for _, svc := range m.registry().All() {
			f, ok := svc.(proxy.Forwarder)
			if !ok {
				continue
			}
			v, err := m.viewFor(iso.ID, svc)
			if err != nil || !v.Enabled {
				continue
			}
			fws, _ := f.Forwards(v)
			for _, fw := range fws {
				all = append(all, wantedForward{forwardKey{iso.ID, svc.Name(), fw.Env}, fw})
			}
		}
	}
	held := map[int]wantedForward{}
	for _, w := range all {
		if o := m.fwd.open[w.fw.HostPort]; o != nil && o.key == w.key {
			held[w.fw.HostPort] = w
		}
	}
	for _, w := range all {
		o, taken := held[w.fw.HostPort]
		switch {
		case taken && o.key == w.key:
		case taken:
			clash[w.key] = fmt.Sprintf("the Mac's 127.0.0.1:%d is already isolation %s's %s", w.fw.HostPort, m.isoName(o.key.iso), o.key.env)
			continue
		default:
			held[w.fw.HostPort] = w
		}
		want = append(want, w)
	}
	return want, clash
}

// reforward makes the Mac's listeners what the store asks for: one that is
// no longer wanted, or wanted otherwise, is closed with its connections,
// and a new one opened. It returns why each wanted forward that is not
// listening is not.
func (m *Manager) reforward() map[forwardKey]string {
	m.fwd.mu.Lock()
	defer m.fwd.mu.Unlock()
	if m.fwd.open == nil {
		m.fwd.open = map[int]*hostForward{}
	}
	if m.ctx.Err() != nil {
		return nil
	}
	want, errs := m.forwardsWanted()
	keep := map[int]bool{}
	for _, w := range want {
		if o := m.fwd.open[w.fw.HostPort]; o != nil && o.key == w.key && o.fw == w.fw {
			keep[w.fw.HostPort] = true
		}
	}
	for port, o := range m.fwd.open {
		if !keep[port] {
			o.close()
			delete(m.fwd.open, port)
			m.log.Printf("isolation %s: port %s: the Mac no longer listens on 127.0.0.1:%d", m.isoName(o.key.iso), o.key.env, port)
		}
	}
	for _, w := range want {
		if keep[w.fw.HostPort] {
			continue
		}
		o, err := m.listenForward(w)
		if err != nil {
			errs[w.key] = err.Error()
			continue
		}
		m.fwd.open[w.fw.HostPort] = o
		m.log.Printf("isolation %s: port %s: %s", m.isoName(w.key.iso), w.key.env, portproxy.Describe(w.fw))
	}
	for k, why := range errs {
		if m.fwd.errs[k] != why {
			m.log.Printf("isolation %s: port %s is not forwarded: %s", m.isoName(k.iso), k.env, why)
		}
	}
	m.fwd.errs = errs
	return errs
}

// retryForwards opens again what could not be opened before.
func (m *Manager) retryForwards() {
	m.fwd.mu.Lock()
	failed := len(m.fwd.errs) > 0
	m.fwd.mu.Unlock()
	if failed {
		m.reforward()
	}
}

func (m *Manager) closeForwards() {
	m.fwd.mu.Lock()
	defer m.fwd.mu.Unlock()
	for port, o := range m.fwd.open {
		o.close()
		delete(m.fwd.open, port)
	}
}

// forwardErrors is what reforward reported that it did not before, for
// the forwards which says are this change's.
func forwardErrors(before, after map[forwardKey]string, which func(forwardKey) bool) error {
	var why []string
	for k, e := range after {
		if which(k) && before[k] != e {
			why = append(why, k.env+": "+e)
		}
	}
	if len(why) == 0 {
		return nil
	}
	sort.Strings(why)
	return httpError{409, errors.New(strings.Join(why, "; "))}
}

func (m *Manager) forwardErrs() map[forwardKey]string {
	m.fwd.mu.Lock()
	defer m.fwd.mu.Unlock()
	out := make(map[forwardKey]string, len(m.fwd.errs))
	for k, v := range m.fwd.errs {
		out[k] = v
	}
	return out
}

// listenForward opens a forward's listener. A port something on the Mac
// already serves on, on any address, is refused: listening on 127.0.0.1
// beside a program that holds every address would take that program's
// loopback connections into the isolation.
func (m *Manager) listenForward(w wantedForward) (*hostForward, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(w.fw.HostPort))
	for _, a := range []string{addr, net.JoinHostPort("::1", strconv.Itoa(w.fw.HostPort))} {
		if c, err := net.DialTimeout("tcp", a, 500*time.Millisecond); err == nil {
			c.Close()
			return nil, fmt.Errorf("the Mac's 127.0.0.1:%d is taken: something on the Mac listens there%s; give the env another --host-port",
				w.fw.HostPort, m.ownPort(w.fw.HostPort))
		}
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("the Mac's 127.0.0.1:%d: %v", w.fw.HostPort, err)
	}
	o := &hostForward{key: w.key, fw: w.fw, l: l, conns: map[net.Conn]bool{}}
	dial := func(ctx context.Context) (net.Conn, error) { return o.track(m.dialForward(ctx, w.key.iso, w.fw.Port)) }
	if w.fw.HTTP {
		o.srv = &http.Server{Handler: portproxy.Handler(dial, []string{web.SessionCookie}), ReadHeaderTimeout: 30 * time.Second}
		go o.srv.Serve(l)
	} else {
		go o.serveTCP(dial, m.log.Printf)
	}
	return o, nil
}

// ownPort names what of gero's holds a Mac port, if anything does.
func (m *Manager) ownPort(port int) string {
	if m.webURL != "" && strings.HasSuffix(m.webURL, ":"+strconv.Itoa(port)) {
		return " (gero's own web UI)"
	}
	return ""
}

func (o *hostForward) serveTCP(dial func(context.Context) (net.Conn, error), logf func(string, ...any)) {
	for {
		c, err := o.l.Accept()
		if err != nil {
			return
		}
		if !o.hold(c) {
			c.Close()
			return
		}
		go func() {
			defer o.release(c)
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			in, err := dial(ctx)
			cancel()
			if err != nil {
				logf("port %s: %v", o.key.env, err)
				return
			}
			defer in.Close()
			splice(c, in)
		}()
	}
}

// splice copies both ways, half-closing each direction as it ends.
func splice(a, b net.Conn) {
	done := make(chan struct{})
	go func() {
		io.Copy(b, a)
		closeWrite(b)
		close(done)
	}()
	io.Copy(a, b)
	closeWrite(a)
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// forwardTunnel is the tunnel a forward into isolation id goes through,
// or why there is none to go through now.
func (m *Manager) forwardTunnel(ctx context.Context, id string) (*tunnel.Tunnel, error) {
	name := m.isoName(id)
	if t := m.liveTunnel(id); t != nil {
		if v := m.shimVersion(id); v != "" && v < shimproto.ForwardsVersion {
			return nil, proxy.Deny("isolation %s's image predates forwarded ports: `gero iso upgrade %s` replaces its container with one that has them", name, name)
		}
		return t, nil
	}
	if iso, err := m.store.Isolation(id); err == nil {
		if state, err := m.cli.State(ctx, iso.Container); err == nil && state != "running" {
			return nil, proxy.Deny("isolation %s is not running now; it starts with one of its agents", name)
		}
	}
	return nil, proxy.Deny("isolation %s's tunnel is %s", name, orNone(m.tunnelStatus(id)))
}

// dialForward connects to port on isolation id's loopback.
func (m *Manager) dialForward(ctx context.Context, id string, port int) (net.Conn, error) {
	t, err := m.forwardTunnel(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := t.Dial(ctx, port)
	switch {
	case err == nil:
		return c, nil
	case nothingAnswers(err):
		return nil, proxy.Deny("nothing answers on port %d in isolation %s now", port, m.isoName(id))
	}
	return nil, proxy.Deny("isolation %s: %v", m.isoName(id), err)
}

// nothingAnswers is a refusal from the isolation: no server on the port.
func nothingAnswers(err error) bool {
	return strings.Contains(err.Error(), "connect failed")
}

// forwardsOf is a forwarding service's ports in one isolation, each with
// why the Mac is not listening for it, if it is not.
func (m *Manager) forwardsOf(id string, svc proxy.Service, v proxy.View) []api.Forward {
	f, ok := svc.(proxy.Forwarder)
	if !ok || !v.Enabled {
		return nil
	}
	errs := m.forwardErrs()
	fws, _ := f.Forwards(v)
	out := make([]api.Forward, 0, len(fws))
	for _, fw := range fws {
		out = append(out, api.Forward{Forward: fw, URL: portproxy.URL(fw), Error: errs[forwardKey{id, svc.Name(), fw.Env}]})
	}
	return out
}

// listening is the address the Mac listens on for a forward, or why it
// does not.
func (m *Manager) listening(k forwardKey, fw proxy.Forward) (*net.TCPAddr, string) {
	m.fwd.mu.Lock()
	defer m.fwd.mu.Unlock()
	if why := m.fwd.errs[k]; why != "" {
		return nil, why
	}
	o := m.fwd.open[fw.HostPort]
	if o == nil || o.key != k || o.fw != fw {
		return nil, "the daemon is not listening for it"
	}
	return o.l.Addr().(*net.TCPAddr), ""
}
