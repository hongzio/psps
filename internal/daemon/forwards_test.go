package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/portproxy"
	"github.com/hongzio/gero/internal/store"
)

// The fake tunnel runs in this process, so the isolation's loopback is
// this machine's: a server the test starts on 127.0.0.1 is one an agent
// runs in the isolation.

func portEnv(t *testing.T) (*testEnv, *Manager) {
	t.Helper()
	e := newTestEnv(t)
	m, stop := e.daemon()
	t.Cleanup(stop)
	m.SetProxies(proxy.NewRegistry(portproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	return e, m
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// serveInside starts a server in the isolation and returns its port.
func serveInside(t *testing.T, h http.Handler) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return l.Addr().(*net.TCPAddr).Port, func() { srv.Close() }
}

func (e *testEnv) tunnelUp(m *Manager) {
	e.t.Helper()
	if _, err := m.ensureIsolation(context.Background(), testIsoID); err != nil {
		e.t.Fatal(err)
	}
	e.waitFor("the tunnel", func() bool { return m.liveTunnel(testIsoID) != nil })
}

func get(t *testing.T, url, cookie string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAPortOfTheIsolationsIsReachedFromTheMac(t *testing.T) {
	e, m := portEnv(t)
	ctx := context.Background()
	var cookie string
	port, stopServer := serveInside(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		io.WriteString(w, "from the isolation")
	}))
	host := freeTCPPort(t)
	if _, err := m.PutEnv(ctx, "port", "web", proxy.Patch{Set: map[string]any{"port": port, "host_port": host}}, true); err != nil {
		t.Fatal(err)
	}
	x, err := m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{"web"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Forwards) != 1 || x.Forwards[0].HostPort != host || x.Forwards[0].Port != port || !x.Forwards[0].HTTP || x.Forwards[0].Error != "" ||
		x.Forwards[0].URL != fmt.Sprintf("http://web.localhost:%d", host) {
		t.Fatalf("%+v", x.Forwards)
	}
	if e.creates() != 0 {
		t.Fatal("a forward touched the container")
	}
	e.tunnelUp(m)
	url := fmt.Sprintf("http://127.0.0.1:%d/", host)
	if code, body := get(t, url, "gero_session=the-web-token; app=1"); code != 200 || body != "from the isolation" {
		t.Fatalf("%d %q", code, body)
	}
	if cookie != "app=1" {
		t.Fatalf("the isolation got Cookie %q", cookie)
	}

	// The skill gives the Mac's address.
	req, _ := e.fc.lastSync()
	var skill string
	for _, f := range req.Files {
		if strings.HasSuffix(f.Path, "gero-port/SKILL.md") {
			skill = f.Content
		}
	}
	if !strings.Contains(skill, fmt.Sprintf("port %d here is http://web.localhost:%d on the Mac", port, host)) {
		t.Fatalf("skill:\n%s", skill)
	}

	v, err := m.Verify(ctx, "dev")
	if err != nil {
		t.Fatal(err)
	}
	check := fmt.Sprintf("the Mac's 127.0.0.1:%d reaches %d", host, port)
	if cs := checkNamed(v, check); len(cs) != 1 || !cs[0].OK || cs[0].Got != "listening; a server answers" {
		t.Fatalf("verify: %+v", cs)
	}

	// No server: the browser is told so, and verify does not fail it.
	stopServer()
	if code, body := get(t, url, ""); code != http.StatusBadGateway || !strings.Contains(body, fmt.Sprintf("nothing answers on port %d in isolation dev now", port)) {
		t.Fatalf("%d %q", code, body)
	}
	v, _ = m.Verify(ctx, "dev")
	if cs := checkNamed(v, check); len(cs) != 1 || !cs[0].OK || !strings.Contains(cs[0].Got, "nothing answers") {
		t.Fatalf("verify: %+v", cs)
	}

	// Off: the Mac stops listening.
	if err := m.Unexpose(ctx, "dev", "port"); err != nil {
		t.Fatal(err)
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host)); err == nil {
		c.Close()
		t.Fatal("still listening after unexpose")
	}
}

func TestATCPPortPassesBytes(t *testing.T) {
	e, m := portEnv(t)
	ctx := context.Background()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("got:"), b...))
			}()
		}
	}()
	host := freeTCPPort(t)
	env, err := m.PutEnv(ctx, "port", "db", proxy.Patch{Set: map[string]any{"port": l.Addr().(*net.TCPAddr).Port, "host_port": host, "mode": "tcp"}}, true)
	if err != nil || len(env.Warnings) != 1 {
		t.Fatal(env.Warnings, err)
	}
	if _, err := m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{"db"}}); err != nil {
		t.Fatal(err)
	}
	e.tunnelUp(m)
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("SELECT 1"))
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	c.Close()
	if string(b) != "got:SELECT 1" {
		t.Fatalf("%q", b)
	}

	// Off: a connection still open is cut, not left to the isolation.
	open, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	open.Write([]byte("BEGIN"))
	time.Sleep(100 * time.Millisecond)
	if err := m.Unexpose(ctx, "dev", "port"); err != nil {
		t.Fatal(err)
	}
	open.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(open); err != nil {
		t.Fatalf("the connection outlived the forward: %v", err)
	}
}

// A Mac port that is someone else's is refused, not shared: a program
// that holds every address would lose its loopback connections to the
// isolation.
func TestAMacPortThatIsTakenIsRefused(t *testing.T) {
	_, m := portEnv(t)
	ctx := context.Background()
	for _, addr := range []string{"127.0.0.1:0", ":0"} {
		held, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		host := held.Addr().(*net.TCPAddr).Port
		name := fmt.Sprintf("web%d", host)
		if _, err := m.PutEnv(ctx, "port", name, proxy.Patch{Set: map[string]any{"port": 3000, "host_port": host}}, true); err != nil {
			t.Fatal(err)
		}
		_, err = m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{name}})
		var he httpError
		if err == nil || !errors.As(err, &he) || he.code != 409 || !strings.Contains(err.Error(), "is taken: something on the Mac listens there") {
			t.Fatalf("%s: %v", addr, err)
		}
		if _, err := m.store.Exposure(testIsoID, "port"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: the exposure was kept: %v", addr, err)
		}
		held.Close()
	}
}

// An edit that would put two ports on one Mac port is put back.
func TestAnEditOntoATakenMacPortIsPutBack(t *testing.T) {
	_, m := portEnv(t)
	ctx := context.Background()
	web, api2 := freeTCPPort(t), freeTCPPort(t)
	m.PutEnv(ctx, "port", "web", proxy.Patch{Set: map[string]any{"port": 3000, "host_port": web}}, true)
	m.PutEnv(ctx, "port", "api", proxy.Patch{Set: map[string]any{"port": 8080, "host_port": api2}}, true)
	if _, err := m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{"web", "api"}}); err != nil {
		t.Fatal(err)
	}
	_, err := m.PutEnv(ctx, "port", "api", proxy.Patch{Set: map[string]any{"host_port": web}}, false)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("127.0.0.1:%d is already isolation dev's web", web)) {
		t.Fatal(err)
	}
	cur, _ := m.store.Env("port", "api")
	if !strings.Contains(string(cur.Config), fmt.Sprintf(`"host_port":%d`, api2)) {
		t.Fatalf("not put back: %s", cur.Config)
	}
	xs, _ := m.Exposures("dev")
	for _, x := range xs {
		for _, f := range x.Forwards {
			if f.Error != "" {
				t.Fatalf("%+v", f)
			}
		}
	}
}

// The Mac's listeners are the daemon's: they go with it and come back
// with the next one, before any container is up.
func TestForwardsComeBackWithTheDaemon(t *testing.T) {
	e := newTestEnv(t)
	reg := proxy.NewRegistry(portproxy.Service{})
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	m, stop := e.daemon()
	m.SetProxies(reg, cs)
	ctx := context.Background()
	host := freeTCPPort(t)
	m.PutEnv(ctx, "port", "web", proxy.Patch{Set: map[string]any{"port": 3000, "host_port": host}}, true)
	if _, err := m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{"web"}}); err != nil {
		t.Fatal(err)
	}
	stop()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", host))
	if err != nil {
		t.Fatalf("the port outlived the daemon: %v", err)
	}
	l.Close()

	e.fc.reboot(testCtr, false)
	m2, stop2 := e.daemon()
	defer stop2()
	m2.SetProxies(reg, cs)
	if code, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/", host), ""); code != http.StatusBadGateway || !strings.Contains(body, "isolation dev is not running now") {
		t.Fatalf("%d %q", code, body)
	}
}

// An isolation whose shim predates forwards is told to upgrade.
func TestAnOldImageIsToldToUpgrade(t *testing.T) {
	e, m := portEnv(t)
	ctx := context.Background()
	host := freeTCPPort(t)
	m.PutEnv(ctx, "port", "web", proxy.Patch{Set: map[string]any{"port": 3000, "host_port": host}}, true)
	m.Expose(ctx, "dev", "port", api.Expose{Envs: []string{"web"}})
	e.tunnelUp(m)
	m.mu.Lock()
	m.versions[testIsoID] = "2"
	m.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if code, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/", host), ""); code == http.StatusBadGateway && strings.Contains(body, "`gero iso upgrade dev`") {
			return
		}
	}
	t.Fatal("not told to upgrade")
}
