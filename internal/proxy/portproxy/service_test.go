package portproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

func TestNormalize(t *testing.T) {
	s := Service{}
	for _, c := range []struct {
		env  map[string]any
		want string
	}{
		{map[string]any{}, "--port is required"},
		{map[string]any{"port": 70000}, "is not a port"},
		{map[string]any{"port": 3021}, "gero's own"},
		{map[string]any{"port": 3000, "host_port": -1}, "is not a port"},
	} {
		if _, err := s.Normalize("web", c.env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v", c.env, err)
		}
	}

	// The same Mac port is not stored, so it follows the port.
	env := map[string]any{"port": 3000, "host_port": 3000}
	if w, err := s.Normalize("web", env); err != nil || len(w) != 0 {
		t.Fatal(w, err)
	}
	if _, ok := env["host_port"]; ok || env["mode"] != "http" {
		t.Fatalf("%v", env)
	}
	env = map[string]any{"port": 5432, "host_port": 15432, "mode": "tcp"}
	w, err := s.Normalize("db", env)
	if err != nil || len(w) != 1 || !strings.Contains(w[0], "127.0.0.1:15432 sends it your gero web session") {
		t.Fatal(w, err)
	}
	b, _ := json.Marshal(env)
	var e Env
	json.Unmarshal(b, &e)
	if f := e.Forward("db"); f != (proxy.Forward{Env: "db", Port: 5432, HostPort: 15432}) {
		t.Fatalf("%+v", f)
	}
}

func TestTwoEnvsOnOneMacPortAreRefused(t *testing.T) {
	v := proxy.View{Exposure: store.Exposure{Envs: []string{"web", "api"}}, Envs: map[string]json.RawMessage{
		"web": json.RawMessage(`{"port":3000,"mode":"http"}`),
		"api": json.RawMessage(`{"port":8080,"host_port":3000,"mode":"http"}`),
	}}
	if err := (Service{}).CheckExposure(v); err == nil || !strings.Contains(err.Error(), "both on the Mac's 127.0.0.1:3000") {
		t.Fatal(err)
	}
	fws, _ := Service{}.Forwards(v)
	files, _ := Service{}.Workspace(v)
	if len(fws) != 2 || !strings.Contains(files.Files[0].Content, "port 8080 here is http://api.localhost:3000 on the Mac") {
		t.Fatalf("%+v\n%s", fws, files.Files[0].Content)
	}
}

// inside is a server in the isolation, reached through dial.
func inside(t *testing.T, h http.Handler) func(context.Context) (net.Conn, error) {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
}

func TestGerosCookiesAreKeptFromTheIsolation(t *testing.T) {
	var got http.Header
	var host string
	dial := inside(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, host = r.Header.Clone(), r.Host
		w.Header().Add("Set-Cookie", "gero_session=planted; Path=/")
		w.Header().Add("Set-Cookie", "app=1; Path=/")
		w.Header().Add("Set-Cookie", "wide=1; Domain=localhost; Path=/; HttpOnly")
		io.WriteString(w, "hello")
	}))
	mac := httptest.NewServer(Handler(dial, []string{"gero_session"}))
	defer mac.Close()

	req, _ := http.NewRequest("GET", mac.URL+"/x", nil)
	req.Host = "localhost:5173"
	req.Header.Set("Cookie", "theme=dark; gero_session=the-web-token; app=1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hello" {
		t.Fatalf("body %q", body)
	}
	if c := got.Get("Cookie"); c != "theme=dark; app=1" || strings.Contains(strings.Join(got.Values("Cookie"), ""), "the-web-token") {
		t.Fatalf("the isolation got Cookie %q", got.Values("Cookie"))
	}
	if host != "localhost:5173" {
		t.Fatalf("Host %q", host)
	}
	if sc := resp.Header.Values("Set-Cookie"); len(sc) != 2 || sc[0] != "app=1; Path=/" || sc[1] != "wide=1; Path=/; HttpOnly" {
		t.Fatalf("Set-Cookie %q", sc)
	}

	// Only the held cookie: no Cookie header at all.
	req, _ = http.NewRequest("GET", mac.URL, nil)
	req.Header.Set("Cookie", "gero_session=the-web-token")
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if _, ok := got["Cookie"]; ok {
		t.Fatalf("Cookie %q", got.Values("Cookie"))
	}
}

func TestADialRefusalIsWhatTheBrowserIsTold(t *testing.T) {
	mac := httptest.NewServer(Handler(func(context.Context) (net.Conn, error) {
		return nil, proxy.Deny("isolation dev is not running now")
	}, nil))
	defer mac.Close()
	resp, err := http.Get(mac.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "gero: isolation dev is not running now") {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

// A dev server's live reload is a WebSocket: the upgrade goes through,
// without gero's cookie, and the connection is the server's after it.
func TestAnUpgradeGoesThrough(t *testing.T) {
	var cookie string
	dial := inside(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: test\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo " + line)
		rw.Flush()
	}))
	mac := httptest.NewServer(Handler(dial, []string{"gero_session"}))
	defer mac.Close()
	c, err := net.Dial("tcp", mac.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.WriteString(c, "GET /hmr HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: test\r\nCookie: gero_session=tok; a=b\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("%v %v", resp, err)
	}
	io.WriteString(c, "ping\n")
	if line, _ := br.ReadString('\n'); line != "echo ping\n" {
		t.Fatalf("%q", line)
	}
	if cookie != "a=b" {
		t.Fatalf("the upgrade carried Cookie %q", cookie)
	}
}

// A page that points a name of its own at 127.0.0.1 does not get through.
func TestOnlyALoopbackNameIsServed(t *testing.T) {
	dial := inside(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	mac := httptest.NewServer(Handler(dial, nil))
	defer mac.Close()
	for host, want := range map[string]int{
		"evil.example:80": http.StatusMisdirectedRequest, "127.0.0.1.nip.io": http.StatusMisdirectedRequest,
		"localhost:3000": 200, "app.localhost": 200, "[::1]:3000": 200, "LOCALHOST.": 200,
	} {
		req, _ := http.NewRequest("GET", mac.URL, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %s: %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestAnHTTPPortHasANameOfItsOwn(t *testing.T) {
	for env, want := range map[string]string{"web": "web.localhost", "My_App": "my-app.localhost", "_x_": "x.localhost", "--": "port.localhost"} {
		if got := Hostname(env); got != want {
			t.Errorf("%s: %s, want %s", env, got, want)
		}
	}
	if u := URL(proxy.Forward{Env: "web", Port: 3000, HostPort: 8080, HTTP: true}); u != "http://web.localhost:8080" {
		t.Fatal(u)
	}
	if u := URL(proxy.Forward{Env: "db", Port: 5432, HostPort: 5432}); u != "127.0.0.1:5432" {
		t.Fatal(u)
	}
}
