package chproxy

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Ported from isolate's test_chproxy.py (the decisions) and
// test_chproxy_wire.py (what actually leaves the Mac, on a live socket):
// the workspace token is swapped for the real login and never forwarded,
// the readonly clamp is pinned onto the upstream query, refusals happen
// before upstream is dialled, and the caller's SQL rides through untouched.

func TestEachTokenNamesItsEnv(t *testing.T) {
	toks := map[string]string{"dev": "tok-dev", "prod": "tok-prod"}
	envs := []string{"dev", "prod"}
	if got := ResolveEnv(toks, envs, "tok-prod"); got != "prod" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range []string{"nope", ""} {
		if got := ResolveEnv(toks, envs, bad); got != "" {
			t.Errorf("%q resolved to %q", bad, got)
		}
	}
	if got := ResolveEnv(nil, nil, "tok-dev"); got != "" {
		t.Fatalf("no envs resolved to %q", got)
	}
	// An env with no token minted must not match an empty one.
	if got := ResolveEnv(map[string]string{}, []string{"dev"}, ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTheQueryEndpointAndPingRoute(t *testing.T) {
	for _, m := range []string{"GET", "POST"} {
		for path, want := range map[string]string{"/": "/", "/ping": "/ping", "": "/"} {
			if got, d, _ := Route(m, path); d != nil || got != want {
				t.Errorf("%s %q = %q, %v", m, path, got, d)
			}
		}
	}
}

func TestTheBrowserUIsAreRefusedAndNameTheSurface(t *testing.T) {
	for _, path := range []string{"/play", "/dashboard", "/replicas_status"} {
		_, d, status := Route("GET", path)
		if d == nil || status != 404 || !strings.Contains(d.Reason, path) {
			t.Errorf("%s: %v %d", path, d, status)
		}
	}
}

func TestAVerbOtherThanGetOrPostIsRefused(t *testing.T) {
	for _, m := range []string{"PUT", "PATCH", "DELETE"} {
		if _, d, _ := Route(m, "/"); d == nil {
			t.Errorf("%s accepted", m)
		}
	}
}

func TestATraversalPathIsRefused(t *testing.T) {
	for _, path := range []string{"/../etc", "/%2e%2e/etc", "/./ping", "/ping%00"} {
		if _, d, status := Route("GET", path); d == nil || status != 400 {
			t.Errorf("%s: %v %d", path, d, status)
		}
	}
}

func pairs(t *testing.T, q string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(q)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReadOnlyPinsReadonly2AndDropsTheCallers(t *testing.T) {
	// readonly=0 smuggled as a parameter, in any spelling, must not
	// survive next to the clamp.
	for _, q := range []string{"query=SELECT+1", "readonly=0&query=x", "read%6Fnly=0&query=x"} {
		got, err := GuardQuery(q, true)
		if err != nil {
			t.Fatal(err)
		}
		if v := pairs(t, got)["readonly"]; len(v) != 1 || v[0] != "2" {
			t.Errorf("%q -> %q", q, got)
		}
	}
}

func TestAReadWriteEnvIsLeftAlone(t *testing.T) {
	if got, _ := GuardQuery("query=INSERT", false); got != "query=INSERT" {
		t.Fatalf("%q", got)
	}
}

func TestOtherSettingsRideThroughByteForByte(t *testing.T) {
	got, _ := GuardQuery("database=app&max_result_rows=10&query=SELECT%201", true)
	if got != "database=app&max_result_rows=10&query=SELECT%201&readonly=2" {
		t.Fatalf("%q", got)
	}
}

func TestBadEncodingIsRefusedNotGuessed(t *testing.T) {
	if _, err := GuardQuery("readonly%zz=0", true); err == nil {
		t.Fatal("accepted")
	}
}

func TestURLCredentialsAreStrippedAndThePasswordCaptured(t *testing.T) {
	q, presented, err := StripCredentials("user=default&password=TOK&query=SELECT+1")
	if err != nil || presented != "TOK" || q != "query=SELECT+1" {
		t.Fatalf("%q %q %v", q, presented, err)
	}
	// Names are compared decoded, as ClickHouse reads them.
	if q, presented, _ := StripCredentials("pass%77ord=TOK&us%65r=x"); q != "" || presented != "TOK" {
		t.Fatalf("%q %q", q, presented)
	}
}

func TestBasicAuthYieldsItsPasswordHalf(t *testing.T) {
	if got := BasicPassword("Basic " + base64.StdEncoding.EncodeToString([]byte("someone:TOK"))); got != "TOK" {
		t.Fatalf("%q", got)
	}
	for _, bad := range []string{"Bearer TOK", "Basic ???", ""} {
		if got := BasicPassword(bad); got != "" {
			t.Errorf("%q -> %q", bad, got)
		}
	}
}

// --- the wire ---

type seen struct {
	TLS          bool
	Method, Path string
	Header       http.Header
	Body         string
}

type tap struct {
	mu   sync.Mutex
	reqs []seen
	body string
}

func (tp *tap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	tp.mu.Lock()
	tp.reqs = append(tp.reqs, seen{r.TLS != nil, r.Method, r.URL.RequestURI(), r.Header.Clone(), string(b)})
	body := tp.body
	tp.mu.Unlock()
	w.Header().Set("Content-Type", "text/tab-separated-values")
	w.Header().Set("X-ClickHouse-Summary", `{"read_rows":"1"}`)
	io.WriteString(w, body)
}

func (tp *tap) all() []seen {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]seen(nil), tp.reqs...)
}

const token = "WORKSPACE-TOKEN"

type harness struct {
	t    *testing.T
	tap  *tap
	up   *httptest.Server
	base string

	mu   sync.Mutex
	view proxy.View
}

func newHarness(t *testing.T, secure bool) *harness {
	t.Helper()
	tp := &tap{body: "1\n"}
	var up *httptest.Server
	if secure {
		up = httptest.NewTLSServer(tp)
	} else {
		up = httptest.NewServer(tp)
	}
	t.Cleanup(up.Close)
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("clickhouse", "main", map[string]string{"user": "real-user", "password": "real-password"})
	h := &harness{t: t, tap: tp, up: up}
	u, _ := url.Parse(up.URL)
	port, _ := strconv.Atoi(u.Port())
	h.setEnv("main", Env{Host: u.Hostname(), Port: port, Secure: &secure}, token)
	in := Service{Transport: up.Client().Transport}.New(proxy.Host{Name: func() string { return "work" }, View: h.getView, Creds: cs, Log: log.New(io.Discard, "", 0)})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go in.Serve(Port, l)
	h.base = "http://" + l.Addr().String()
	return h
}

func (h *harness) setEnv(name string, e Env, tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.view.Envs == nil {
		h.view = proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
		h.view.Exposure = store.Exposure{IsolationID: "work", Service: "clickhouse", Default: name}
	}
	b, _ := json.Marshal(e)
	h.view.Envs[name] = b
	if !strings.Contains(strings.Join(h.view.Exposure.Envs, ","), name) {
		h.view.Exposure.Envs = append(h.view.Exposure.Envs, name)
	}
	h.view.Tokens[name] = tok
}

func (h *harness) getView() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.view, nil
}

func (h *harness) do(method, path string, header map[string]string, body string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if body == "" {
		req.Body = nil
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func key(tok string) map[string]string { return map[string]string{"X-ClickHouse-Key": tok} }

func (h *harness) upstreamQuery() url.Values {
	all := h.tap.all()
	u, _ := url.Parse(all[len(all)-1].Path)
	return u.Query()
}

func TestTheWrongTokenIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t, true)
	code, body, _ := h.do("GET", "/?query=SELECT+1", key("not-the-token"), "")
	if code != 401 || !strings.Contains(body, "workspace token") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q %d", code, body, len(h.tap.all()))
	}
}

func TestTheTokenIsSwappedForTheRealLogin(t *testing.T) {
	h := newHarness(t, true)
	code, body, hdr := h.do("GET", "/?query=SELECT+1", key(token), "")
	if code != 200 || body != "1\n" || hdr.Get("X-ClickHouse-Summary") == "" {
		t.Fatalf("%d %q %v", code, body, hdr)
	}
	s := h.tap.all()[0]
	if !s.TLS || s.Header.Get("X-ClickHouse-User") != "real-user" || s.Header.Get("X-ClickHouse-Key") != "real-password" {
		t.Fatalf("upstream saw %+v", s)
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			if strings.Contains(v, token) {
				t.Fatalf("the workspace token went upstream in %s", k)
			}
		}
	}
}

func TestReadonly2IsPinnedAndASmuggledReadonlyDropped(t *testing.T) {
	h := newHarness(t, true)
	h.do("GET", "/?query=SELECT+1&readonly=0", key(token), "")
	q := h.upstreamQuery()
	if q.Get("readonly") != "2" || len(q["readonly"]) != 1 || q.Get("query") != "SELECT 1" {
		t.Fatalf("%v", q)
	}
}

func TestAReadWriteEnvIsNotClamped(t *testing.T) {
	h := newHarness(t, true)
	u, _ := url.Parse(h.up.URL)
	port, _ := strconv.Atoi(u.Port())
	rw, sec := false, true
	h.setEnv("main", Env{Host: u.Hostname(), Port: port, Secure: &sec, ReadOnly: &rw}, token)
	h.do("GET", "/?query=SELECT+1", key(token), "")
	if _, ok := h.upstreamQuery()["readonly"]; ok {
		t.Fatal("clamped")
	}
}

func TestBasicAuthAndURLCredentialsAuthenticateToo(t *testing.T) {
	h := newHarness(t, true)
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+token))
	if code, _, _ := h.do("GET", "/?query=SELECT+1", map[string]string{"Authorization": basic}, ""); code != 200 {
		t.Fatalf("basic: %d", code)
	}
	if code, _, _ := h.do("GET", "/?query=SELECT+1&user=whoever&password="+token, nil, ""); code != 200 {
		t.Fatalf("url: %d", code)
	}
	for _, s := range h.tap.all() {
		u, _ := url.Parse(s.Path)
		if s.Header.Get("Authorization") != "" || u.Query().Has("password") || u.Query().Has("user") {
			t.Fatalf("a credential slot rode through: %+v", s)
		}
	}
}

func TestAPostBodyRidesThroughUntouched(t *testing.T) {
	h := newHarness(t, true)
	sql := "SELECT count() FROM system.tables FORMAT PrettyCompact"
	if code, _, _ := h.do("POST", "/", key(token), sql); code != 200 {
		t.Fatalf("%d", code)
	}
	if got := h.tap.all()[0].Body; got != sql {
		t.Fatalf("%q", got)
	}
}

func TestAChunkedBodyIsRefused(t *testing.T) {
	h := newHarness(t, true)
	req, _ := http.NewRequest("POST", h.base+"/", io.NopCloser(strings.NewReader("SELECT 1")))
	req.ContentLength = -1
	req.Header.Set("X-ClickHouse-Key", token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 411 || len(h.tap.all()) != 0 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestAnUnmodeledPathNeverReachesUpstream(t *testing.T) {
	h := newHarness(t, true)
	code, body, _ := h.do("GET", "/play", key(token), "")
	if code != 404 || !strings.Contains(body, "/ping") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAMissingSecretIsARefusalNotACrash(t *testing.T) {
	h := newHarness(t, true)
	h.setEnv("ghost", Env{Host: "ch"}, "GHOST-TOKEN")
	code, body, _ := h.do("GET", "/?query=SELECT+1", key("GHOST-TOKEN"), "")
	if code != 503 || !strings.Contains(body, "[clickhouse ghost]") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := newHarness(t, true)
	var last int
	var hdr http.Header
	for range 61 {
		last, _, hdr = h.do("GET", "/?query=SELECT+1", key(token), "")
	}
	if last != 429 || hdr.Get("Retry-After") == "" || len(h.tap.all()) != 60 {
		t.Fatalf("%d %v %d", last, hdr, len(h.tap.all()))
	}
}

func TestAPlaintextEnvDialsWithoutTLS(t *testing.T) {
	h := newHarness(t, false)
	if code, _, _ := h.do("GET", "/?query=SELECT+1", key(token), ""); code != 200 || h.tap.all()[0].TLS {
		t.Fatalf("%d %+v", code, h.tap.all())
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t, true)
	h.mu.Lock()
	h.view.Enabled = false
	h.mu.Unlock()
	if code, body, _ := h.do("GET", "/?query=SELECT+1", key(token), ""); code != 503 || !strings.Contains(body, "gero iso expose work clickhouse") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestTheProbeCarriesTheRealLoginAndTheClamp(t *testing.T) {
	h := newHarness(t, true)
	h.tap.body = "24.8.1\n"
	h.mu.Lock()
	cfg := h.view.Envs["main"]
	h.mu.Unlock()
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("clickhouse", "main", map[string]string{"user": "real-user", "password": "real-password"})
	out, err := Service{Transport: h.up.Client().Transport}.Check(t.Context(), "main", cfg, cs)
	if err != nil || !strings.Contains(out, "ClickHouse 24.8.1") {
		t.Fatalf("%q %v", out, err)
	}
	s := h.tap.all()[0]
	u, _ := url.Parse(s.Path)
	if s.Header.Get("X-ClickHouse-User") != "real-user" || u.Query().Get("readonly") != "2" {
		t.Fatalf("%+v", s)
	}
}

func TestAHostlessEnvNamesTheEdit(t *testing.T) {
	_, err := Service{}.Check(t.Context(), "main", json.RawMessage(`{}`), nil)
	if err == nil || !strings.Contains(err.Error(), "gero env edit clickhouse main") {
		t.Fatalf("%v", err)
	}
}

func TestTheWorkspaceGetsAPasswordPerEnv(t *testing.T) {
	v := proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{"main": []byte(`{"host":"ch","port":8443}`), "us-east": []byte(`{"host":"ch2"}`)},
		Tokens:   map[string]string{"main": "t1", "us-east": "t2"},
		Exposure: store.Exposure{Envs: []string{"main", "us-east"}, Default: "main"}}
	ws, err := Service{}.Workspace(v)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(ws.Env, "\n")
	for _, want := range []string{"CLICKHOUSE_PASSWORD=t1", "CLICKHOUSE_PASSWORD_MAIN=t1", "CLICKHOUSE_PASSWORD_US_EAST=t2", "CLICKHOUSE_URL=http://127.0.0.1:3025"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	if s := ws.Files[0].Content; !strings.Contains(s, "- `main` (default) — ch:8443, read-only") || strings.Contains(s, "%!") {
		t.Fatalf("skill:\n%s", s)
	}
}
