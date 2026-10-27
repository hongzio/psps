// Package chproxy is the ClickHouse proxy, a port of isolate's chproxy.py:
// the isolation queries without holding the password.
//
//	isolation  curl "$CLICKHOUSE_URL/?query=SELECT+1" -H "X-ClickHouse-Key: $CLICKHOUSE_PASSWORD"
//	             |  (the isolation's tunnel)
//	daemon     this proxy: query endpoint only, readonly clamped, its own
//	             |  rate budget, the real login attached
//	           https://<env host>:8443/, ClickHouse's HTTP interface
//
// CLICKHOUSE_PASSWORD is a per-environment workspace token: HTTP clients
// want a password-shaped value, the proxy needs the caller to prove it is
// the isolation, and, since the HTTP interface has no slot for an
// environment, the token is also how the isolation picks dev or prod. The
// presented user is ignored; the real login lives in the credentials file
// ([clickhouse <env>]), read per request.
//
// Only the HTTP interface is fronted, on purpose. The native protocol
// carries its settings inside every Query packet, so a read-only guarantee
// there would mean protocol surgery pinned to a revision; over HTTP it is
// one URL parameter the server enforces.
//
// Three boundaries, in the order they are checked:
//
//   - The endpoint table. / (the query endpoint) and /ping exist; /play,
//     /dashboard and every predefined handler do not. Encoded characters and
//     dot segments are refused rather than normalised.
//   - Read-only is the server's word, not a SQL parse. For a read-only env
//     the proxy drops any readonly parameter the caller sent and pins
//     readonly=2. ClickHouse then refuses every write and DDL itself, and
//     refuses SETTINGS readonly=0 inside the SQL too: readonly cannot be
//     relaxed from a readonly session.
//   - The login never rides through. Presented credentials in any of the
//     three slots ClickHouse accepts (basic auth, X-ClickHouse-* headers,
//     URL parameters) are stripped, and the real login attached fresh.
//
// The rate budget refuses rather than queues: the upstream is a shared
// analytics server.
package chproxy

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port        = 3025
	DefaultPort = 8443
)

// Env is one ClickHouse environment. Read-only by default and by choice:
// the login's grants are the real boundary, but the clamp means an agent
// cannot write even through an over-granted login.
type Env struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Secure   *bool  `json:"secure,omitempty"`
	ReadOnly *bool  `json:"read_only,omitempty"`
}

func (e Env) secure() bool   { return e.Secure == nil || *e.Secure }
func (e Env) readOnly() bool { return e.ReadOnly == nil || *e.ReadOnly }
func (e Env) port() int {
	if e.Port == 0 {
		return DefaultPort
	}
	return e.Port
}

func (e Env) base() string {
	scheme := "https"
	if !e.secure() {
		scheme = "http"
	}
	return scheme + "://" + net.JoinHostPort(e.Host, strconv.Itoa(e.port()))
}

// Route is the endpoint a request names, or a refusal with its status.
func Route(method, rawPath string) (string, *proxy.Denied, int) {
	if strings.ContainsAny(rawPath, "%\\\x00") {
		return "", proxy.Deny("clickhouse: encoded characters are not accepted in a path"), 400
	}
	for _, seg := range strings.Split(rawPath, "/") {
		if seg == "." || seg == ".." {
			return "", proxy.Deny("clickhouse: dot segments are not accepted in a path"), 400
		}
	}
	if method != "GET" && method != "POST" {
		return "", proxy.Deny("clickhouse: the HTTP interface answers GET and POST only; %s is refused", method), 403
	}
	switch rawPath {
	case "", "/":
		return "/", nil, 0
	case "/ping":
		return "/ping", nil, 0
	}
	return "", proxy.Deny("clickhouse: %s is not forwarded; this proxy serves the query endpoint (/) and /ping only", rawPath), 404
}

// pair is one query parameter as sent, with its decoded name.
type pair struct{ raw, key, value string }

func parseQuery(q string) ([]pair, error) {
	var out []pair
	for _, part := range strings.Split(q, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(k)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: query parameter %q is not valid URL encoding", k)
		}
		value, err := url.QueryUnescape(v)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: query parameter %q is not valid URL encoding", k)
		}
		out = append(out, pair{part, key, value})
	}
	return out, nil
}

// StripCredentials drops the user and password parameters, returning what
// is left and the password the caller presented there. Parameter names are
// compared decoded, as ClickHouse reads them; what is kept goes on byte for
// byte.
func StripCredentials(q string) (string, string, error) {
	pairs, err := parseQuery(q)
	if err != nil {
		return "", "", err
	}
	var kept []string
	presented := ""
	for _, p := range pairs {
		switch p.key {
		case "password":
			presented = p.value
		case "user":
		default:
			kept = append(kept, p.raw)
		}
	}
	return strings.Join(kept, "&"), presented, nil
}

// GuardQuery is the query string as it goes upstream. For a read-only env
// any caller readonly is dropped and readonly=2 pinned: 2 rather than 1
// because clients legitimately tune session settings, which 1 would refuse,
// while the one setting 2 still locks is readonly itself.
func GuardQuery(q string, readOnly bool) (string, error) {
	if !readOnly {
		return q, nil
	}
	pairs, err := parseQuery(q)
	if err != nil {
		return "", err
	}
	var kept []string
	for _, p := range pairs {
		if p.key != "readonly" {
			kept = append(kept, p.raw)
		}
	}
	return strings.Join(append(kept, "readonly=2"), "&"), nil
}

// BasicPassword is the password half of a Basic authorization header.
func BasicPassword(h string) string {
	if len(h) < 6 || !strings.EqualFold(h[:6], "basic ") {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(h[6:])
	if err != nil {
		return ""
	}
	_, pw, _ := strings.Cut(string(b), ":")
	return pw
}

// ResolveEnv is which exposed env the presented token names. Every
// candidate is compared, so a mismatch costs what a match does.
func ResolveEnv(tokens map[string]string, envs []string, presented string) string {
	found := ""
	for _, name := range envs {
		want := tokens[name]
		if presented != "" && want != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
			found = name
		}
	}
	return found
}

type Service struct {
	// Transport defaults to a TLS-verifying one; tests point it at a tap.
	Transport http.RoundTripper
}

func (Service) Name() string { return "clickhouse" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "ClickHouse over HTTP; the real login attached, read-only clamped by the server"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "host", Kind: proxy.String, Flag: "host", Metavar: "HOST", Help: "the ClickHouse server (required)"},
		{Key: "port", Kind: proxy.Int, Flag: "port", Metavar: "PORT", Help: "its HTTP(S) port (default 8443)"},
		{Key: "secure", Kind: proxy.Bool, Flag: "tls", Neg: "no-tls", Help: "speak HTTPS to the server (default on)"},
		{Key: "read_only", Kind: proxy.Bool, Flag: "read-only", Neg: "read-write",
			Help: "pin readonly=2 on every request (default on)"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	if proxy.StringOr(env, "host", "") == "" {
		return nil, fmt.Errorf("--host is required; it names the ClickHouse server %q opens", name)
	}
	port := proxy.IntOr(env, "port", DefaultPort)
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("--port %d is not a TCP port", port)
	}
	env["port"] = port
	env["secure"] = proxy.BoolOr(env, "secure", true)
	env["read_only"] = proxy.BoolOr(env, "read_only", true)
	var warn []string
	if !env["read_only"].(bool) {
		warn = append(warn, fmt.Sprintf("env %q forwards writes: the proxy's readonly clamp is off, so the stored login's own grants are the only boundary", name))
	}
	if !env["secure"].(bool) {
		warn = append(warn, fmt.Sprintf("env %q speaks plain HTTP to the server: the real login crosses the network unencrypted", name))
	}
	return warn, nil
}

// TokenKeys: one per env, because the token is the env selector.
func (Service) TokenKeys(x store.Exposure) []string { return x.Envs }

func decodeEnvs(v proxy.View) (map[string]Env, error) {
	envs := map[string]Env{}
	for _, name := range v.Exposure.Envs {
		if _, ok := v.Envs[name]; !ok {
			continue
		}
		var e Env
		if err := v.Decode(name, &e); err != nil {
			return nil, fmt.Errorf("clickhouse env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// Workspace plants the variable names ClickHouse HTTP drivers and scripts
// already read, pointed at the proxy. CLICKHOUSE_USER is a label; every
// exposed env gets a CLICKHOUSE_PASSWORD_<ENV> twin so switching is one
// variable swap.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	ws := proxy.Workspace{Env: []string{
		"CLICKHOUSE_HOST=127.0.0.1",
		fmt.Sprintf("CLICKHOUSE_PORT=%d", Port),
		"CLICKHOUSE_SECURE=false",
		"CLICKHOUSE_USER=workspace",
		"CLICKHOUSE_PASSWORD=" + v.Tokens[v.Exposure.Default],
		fmt.Sprintf("CLICKHOUSE_URL=http://127.0.0.1:%d", Port),
	}}
	names := append([]string(nil), v.Exposure.Envs...)
	sort.Strings(names)
	for _, n := range names {
		if tok := v.Tokens[n]; tok != "" {
			ws.Env = append(ws.Env, "CLICKHOUSE_PASSWORD_"+proxy.EnvSuffix(n)+"="+tok)
		}
	}
	ws.Files = proxy.SkillFiles("clickhouse", skillBody(v, envs))
	return ws, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("clickhouse") }

func (s Service) New(h proxy.Host) proxy.Instance {
	rt := s.Transport
	if rt == nil {
		rt = &http.Transport{
			Proxy:               nil,
			ForceAttemptHTTP2:   true,
			DisableCompression:  true,
			TLSHandshakeTimeout: 15 * time.Second,
			// Analytics queries can legitimately run for minutes; the
			// ceiling exists so a dead upstream is an error.
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	in := &instance{h: h, transport: rt, buckets: &proxy.Buckets{PerMinute: 240, Burst: 60}}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

type instance struct {
	h         proxy.Host
	transport http.RoundTripper
	buckets   *proxy.Buckets
	srv       *http.Server
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("clickhouse proxy: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("clickhouse", format, a...) }

func (in *instance) refuse(w http.ResponseWriter, r *http.Request, status int, reason string, retry time.Duration) {
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	}
	// The request body, if any, was never read on this path; reusing the
	// connection would parse it as the next request.
	w.Header().Set("Connection", "close")
	// Plain text, the shape ClickHouse's own errors take: drivers surface
	// the body as the exception message.
	proxy.Refuse(w, status, "gero clickhouse proxy: "+reason)
	in.logf("DENY %s %s: %s", r.Method, r.RequestURI, reason)
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawPath, rawQuery := proxy.RawTarget(r)
	query, queryPassword, err := StripCredentials(rawQuery)
	if err != nil {
		in.refuse(w, r, 400, err.Error(), 0)
		return
	}
	v, err := in.h.View()
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	if !v.Enabled {
		in.refuse(w, r, 503, fmt.Sprintf("the clickhouse proxy is off for isolation %s; the operator turns it on with `gero iso expose %s clickhouse <env>`", in.h.Isolation, in.h.Isolation), 0)
		return
	}
	presented := r.Header.Get("X-ClickHouse-Key")
	if presented == "" {
		presented = BasicPassword(r.Header.Get("Authorization"))
	}
	if presented == "" {
		presented = queryPassword
	}
	name := ResolveEnv(v.Tokens, v.Exposure.Envs, presented)
	if name == "" {
		in.refuse(w, r, 401, "this proxy authenticates the isolation by its per-environment workspace token (sent as the password, in any slot ClickHouse accepts); the real login never enters the isolation", 0)
		return
	}
	if ok, retry := in.buckets.Get(name).Allow(); !ok {
		in.refuse(w, r, 429, "this isolation's own rate budget is exhausted; wait and retry — the shared cluster's capacity is deliberately not being spent by a loop", retry)
		return
	}
	endpoint, denied, status := Route(r.Method, rawPath)
	if denied != nil {
		in.refuse(w, r, status, denied.Reason, 0)
		return
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	e, ok := envs[name]
	if !ok {
		in.refuse(w, r, 503, fmt.Sprintf("environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s clickhouse ...`", name, in.h.Isolation), 0)
		return
	}
	login, err := in.h.Creds.Read("clickhouse", name)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	if r.ContentLength < 0 {
		in.refuse(w, r, 411, "clickhouse: chunked request bodies are not accepted; send a Content-Length", 0)
		return
	}
	if query, err = GuardQuery(query, e.readOnly()); err != nil {
		in.refuse(w, r, 400, err.Error(), 0)
		return
	}
	// Rebuilt from the approved pieces, never forwarded raw.
	target := e.base() + endpoint
	if query != "" {
		target += "?" + query
	}
	var body io.Reader
	if r.ContentLength > 0 {
		body = r.Body
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		in.refuse(w, r, 502, "upstream request: "+err.Error(), 0)
		return
	}
	up.ContentLength = r.ContentLength
	proxy.CopyHeaders(up.Header, r.Header, "authorization", "x-clickhouse-user", "x-clickhouse-key", "host", "content-length", "expect")
	up.Header.Set("X-ClickHouse-User", login["user"])
	up.Header.Set("X-ClickHouse-Key", login["password"])
	resp, err := in.transport.RoundTrip(up)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, endpoint, err)
		in.refuse(w, r, 502, "upstream unavailable: "+err.Error(), 0)
		return
	}
	defer resp.Body.Close()
	in.logf("%s %s [env=%s] -> %d", r.Method, endpoint, name, resp.StatusCode)
	proxy.Relay(w, resp)
}

// Refusals: a query with a wrong workspace token, before any login is
// looked up.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Method: "GET", Path: "/?query=SELECT%201",
		Header: map[string]string{"X-ClickHouse-Key": proxy.WrongToken}, Status: 401, Want: "per-environment workspace token"}}
}

// Check asks the server, now and under the same clamp, whether this env's
// login answers a read.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	if e.Host == "" {
		return "", fmt.Errorf("environment %q has no host; run `gero env edit clickhouse %s --host ...`", name, name)
	}
	login, err := c.Read("clickhouse", name)
	if err != nil {
		return "", err
	}
	q, _ := GuardQuery("query="+url.QueryEscape("SELECT version()"), e.readOnly())
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.base()+"/?"+q, nil)
	req.Header.Set("X-ClickHouse-User", login["user"])
	req.Header.Set("X-ClickHouse-Key", login["password"])
	rt := s.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", e.base(), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("the login in [clickhouse %s] was refused by %s (HTTP %d): %.300s", name, e.base(), resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	mode := "read-only (readonly=2 pinned)"
	if !e.readOnly() {
		mode = "read-write"
	}
	return fmt.Sprintf("server     %s, ClickHouse %s\nlogin      %s\nmode       %s\n", e.base(), strings.TrimSpace(string(raw)), login["user"], mode), nil
}
