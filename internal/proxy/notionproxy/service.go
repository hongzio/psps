package notionproxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port = 3028
	// A JSON-RPC message this size is not a tool call anyone writes
	// honestly.
	maxBody = 1 << 20
	// initialize and tools/list answers are read whole to be narrowed; this
	// caps what a misbehaving upstream can make the daemon buffer.
	maxRewrite = 8 << 20
)

// Only these reach Notion: the transport's own headers. Cookies, the
// isolation's Authorization and anything else a client sends stay here.
var forwardHeaders = []string{"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id", "User-Agent"}

// Never relayed: a WWW-Authenticate would send the isolation's MCP client
// off to run OAuth discovery against this proxy, and Notion's cookies are
// for Notion's own browser.
var dropResponse = []string{"set-cookie", "www-authenticate"}

type Service struct {
	// Issuer and Transport default to Notion's; tests point them at a tap.
	Issuer    string
	Transport http.RoundTripper
	Now       func() time.Time
}

func (s Service) oauth() oauth {
	o := oauth{issuer: s.Issuer, transport: s.Transport, now: s.Now}
	if o.issuer == "" {
		o.issuer = DefaultIssuer
	}
	if o.transport == nil {
		o.transport = &http.Transport{ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute, IdleConnTimeout: 90 * time.Second}
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

func (Service) Name() string { return "notion" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "Notion's hosted MCP server, through a fixed tool table; logged in as you, from the Mac"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "families", Kind: proxy.List, Flag: "allow-api", Clear: "clear-apis", Metavar: "FAMILY", Choices: familyNames(),
			Help: "a tool family to open (repeat; replaces the list; default content, search; writes: " + strings.Join(WriteFamilies, ", ") + ")"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	if _, set := env["families"]; set {
		env["families"] = proxy.SortedSet(proxy.Strings(env, "families"))
	} else {
		// Explicit on every add and edit, so `gero env ls` shows what opens.
		env["families"] = append([]string(nil), DefaultFamilies...)
	}
	var writes []string
	for _, f := range env["families"].([]string) {
		if isWrite(f) {
			writes = append(writes, f)
		}
	}
	var warn []string
	if len(writes) > 0 {
		warn = append(warn, fmt.Sprintf("env %q opens Notion writes (%s) as the logged-in user", name, strings.Join(writes, ", ")))
	}
	if len(env["families"].([]string)) == 0 {
		warn = append(warn, fmt.Sprintf("env %q opens no family; only the probe tools answer", name))
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
			return nil, fmt.Errorf("notion env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

func serverName(env, def string) string {
	if env == def {
		return "notion"
	}
	return "notion-" + env
}

// Workspace hands every agent an MCP server per env (the default one plain
// `notion`, the others `notion-<env>`, so a second environment is a second
// server whose tools the agent can tell apart) and the variables for any
// other client.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", Port, MCPPath)
	ws := proxy.Workspace{Env: []string{"NOTION_TOKEN=" + v.Tokens[v.Exposure.Default], "NOTION_MCP_URL=" + url}}
	names := append([]string(nil), v.Exposure.Envs...)
	sort.Strings(names)
	for _, n := range names {
		tok := v.Tokens[n]
		if tok == "" {
			continue
		}
		tokEnv := "NOTION_TOKEN_" + proxy.EnvSuffix(n)
		ws.Env = append(ws.Env, tokEnv+"="+tok)
		ws.MCP = append(ws.MCP, proxy.MCPServer{Name: serverName(n, v.Exposure.Default), URL: url, TokenEnv: tokEnv})
	}
	ws.Files = proxy.SkillFiles("notion", skillBody(v, envs))
	return ws, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("notion") }

func (s Service) New(h proxy.Host) proxy.Instance {
	// 180/min matches Notion's published average of three requests a
	// second, which its MCP server spends from too.
	in := &instance{h: h, oauth: s.oauth(), bucket: proxy.NewTokenBucket(180, 15, nil)}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

type instance struct {
	h      proxy.Host
	oauth  oauth
	bucket *proxy.TokenBucket
	srv    *http.Server
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("notion proxy: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("notion", format, a...) }

func sendJSON(w http.ResponseWriter, status int, payload any) {
	b := append(encode(payload), '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	w.Write(b)
}

// refuse answers in JSON-RPC's own error shape, so the MCP client surfaces
// the sentence instead of choking on an unfamiliar body.
func (in *instance) refuse(w http.ResponseWriter, r *http.Request, status int, reason string, id any, closeConn bool) {
	code := -32000
	if status == 404 {
		code = -32601
	}
	if closeConn {
		// The request body, if any, was never read on this path.
		w.Header().Set("Connection", "close")
	}
	sendJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": reason}})
	in.logf("DENY %s %s: %s", r.Method, r.RequestURI, reason)
}

// toolRefusal is a tool result rather than a protocol error: the agent
// reads the refusal as what the tool answered, fix included.
func (in *instance) toolRefusal(w http.ResponseWriter, id any, reason string) {
	sendJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": reason}}, "isError": true}})
	in.logf("DENY tools/call: %s", reason)
}

type admitted struct {
	view proxy.View
	name string
	env  Env
}

// admit is shared by every verb: the workspace token first, then the path,
// then the budget.
func (in *instance) admit(w http.ResponseWriter, r *http.Request) (admitted, bool) {
	v, err := in.h.View()
	if err != nil {
		in.refuse(w, r, 503, err.Error(), nil, true)
		return admitted{}, false
	}
	if !v.Enabled {
		in.refuse(w, r, 503, fmt.Sprintf("notion: the proxy is off for isolation %s; the operator turns it on with `gero iso expose %s notion <env>`", in.h.Name(), in.h.Name()), nil, true)
		return admitted{}, false
	}
	presented := ""
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		presented = strings.TrimSpace(a[7:])
	}
	name := ""
	for _, n := range v.Exposure.Envs {
		want := v.Tokens[n]
		if presented != "" && want != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
			name = n
		}
	}
	if name == "" {
		// 403, never 401: a 401 tells an MCP client to start OAuth
		// discovery, and there is nothing here for it to discover.
		in.refuse(w, r, 403, "notion: this proxy authenticates the isolation by its per-environment workspace token (sent as `Authorization: Bearer`); the real Notion login never enters the isolation", nil, true)
		return admitted{}, false
	}
	if path, _ := proxy.RawTarget(r); strings.TrimRight(path, "/") != MCPPath {
		if path == "" {
			path = "/"
		}
		in.refuse(w, r, 404, fmt.Sprintf("notion: %s is not served; the MCP endpoint is %s", path, MCPPath), nil, true)
		return admitted{}, false
	}
	if ok, retry := in.bucket.Allow(); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(retry.Seconds())+1))
		in.refuse(w, r, 429, "notion: this isolation's own rate budget is exhausted; wait and retry — Notion's limit is shared by everything this login does and is deliberately not being spent by a loop", nil, true)
		return admitted{}, false
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), nil, true)
		return admitted{}, false
	}
	e, ok := envs[name]
	if !ok {
		in.refuse(w, r, 503, fmt.Sprintf("notion: environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s notion ...`", name, in.h.Name()), nil, true)
		return admitted{}, false
	}
	return admitted{v, name, e}, true
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a, ok := in.admit(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case "POST":
		in.post(w, r, a)
	case "DELETE":
		// Ending a session: no body, no family, still the isolation's own.
		in.forward(w, r, a, nil, nil, "", "session end")
	default:
		// The server-to-client stream is optional in MCP and nothing here
		// needs it; a 405 is how the spec says "not offered".
		w.Header().Set("Allow", "POST, DELETE")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(405)
	}
}

func (in *instance) post(w http.ResponseWriter, r *http.Request, a admitted) {
	if r.ContentLength < 0 {
		in.refuse(w, r, 411, "notion: chunked request bodies are not accepted; send a Content-Length", nil, true)
		return
	}
	if r.ContentLength > maxBody {
		in.refuse(w, r, 413, fmt.Sprintf("notion: request body over %d bytes", maxBody), nil, true)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		in.refuse(w, r, 400, "notion: the body could not be read", nil, true)
		return
	}
	msg, err := decode(raw)
	if err != nil {
		in.refuse(w, r, 400, "notion: the body is not JSON", nil, false)
		return
	}
	id := msg["id"]
	act, err := Classify(msg, a.env, a.name)
	if err != nil {
		d := err.(*Denied)
		if msg["method"] == "tools/call" && d.Status == 403 {
			in.toolRefusal(w, id, d.Reason)
		} else {
			in.refuse(w, r, d.Status, d.Reason, id, false)
		}
		return
	}
	if !act.Forward {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(202)
		in.logf("DROP %v", msg["method"])
		return
	}
	label := "response"
	if m, ok := msg["method"].(string); ok {
		label = m
	}
	if label == "tools/call" {
		tool := msg["params"].(map[string]any)["name"].(string)
		label = fmt.Sprintf("tools/call %s [%s]", tool, ToolFamily(tool))
	}
	in.forward(w, r, a, encode(msg), id, act.Rewrite, label)
}

func (in *instance) upstream(ctx context.Context, r *http.Request, token string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	// Rebuilt, never forwarded raw: the one endpoint, no query.
	up, err := http.NewRequestWithContext(ctx, r.Method, in.oauth.issuer+MCPPath, rd)
	if err != nil {
		return nil, err
	}
	for _, k := range forwardHeaders {
		if v := r.Header.Values(k); len(v) > 0 {
			up.Header[http.CanonicalHeaderKey(k)] = v
		}
	}
	up.Header.Set("Authorization", "Bearer "+token)
	if up.Header.Get("Accept") == "" {
		up.Header.Set("Accept", "application/json, text/event-stream")
	}
	return in.oauth.transport.RoundTrip(up)
}

func (in *instance) forward(w http.ResponseWriter, r *http.Request, a admitted, body []byte, id any, rewrite, label string) {
	ctx := r.Context()
	token, err := in.oauth.accessToken(ctx, in.h.Creds, a.name, "")
	if err != nil {
		d := err.(*Denied)
		in.refuse(w, r, d.Status, d.Reason, id, false)
		return
	}
	resp, err := in.upstream(ctx, r, token, body)
	if err == nil && resp.StatusCode == 401 {
		// The access token died before its stated expiry: revoked, or the
		// clock lied. One refresh, one retry.
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if token, err = in.oauth.accessToken(ctx, in.h.Creds, a.name, token); err != nil {
			d := err.(*Denied)
			in.refuse(w, r, d.Status, d.Reason, id, false)
			return
		}
		resp, err = in.upstream(ctx, r, token, body)
	}
	if err != nil {
		in.logf("UPSTREAM-ERROR %s: %v", label, err)
		in.refuse(w, r, 502, "notion: upstream unavailable: "+err.Error(), id, false)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		in.refuse(w, r, 502, fmt.Sprintf("notion: Notion refused the Mac's login for [notion %s] even after a refresh; the operator can log in again with `gero env secret notion %s`", a.name, a.name), id, false)
		return
	}
	in.logf("%s [env=%s] -> %d", label, a.name, resp.StatusCode)
	if rewrite == "" || resp.StatusCode != 200 {
		proxy.Relay(w, resp, dropResponse...)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRewrite+1))
	if len(raw) > maxRewrite {
		in.refuse(w, r, 502, fmt.Sprintf("notion: the %s answer is over %d bytes; not relayed", rewrite, maxRewrite), id, false)
		return
	}
	out := RewriteBody(raw, resp.Header.Get("Content-Type"), id, rewrite, a.env)
	proxy.CopyHeaders(w.Header(), resp.Header, append([]string{"content-length"}, dropResponse...)...)
	w.Header().Set("Content-Length", fmt.Sprint(len(out)))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

// --- the operator's side ---

// StartLogin begins a browser login for env, landing on redirect (a
// loopback address: a CLI listening there gets the code by itself; anyone
// else pastes the address the browser lands on).
func (s Service) StartLogin(ctx context.Context, env, redirect string) (proxy.Login, error) {
	p, err := s.oauth().startLogin(ctx, env, redirect)
	if err != nil {
		return proxy.Login{}, err
	}
	return proxy.Login{URL: p.URL, Redirect: p.Redirect, Expires: p.Expires, State: p}, nil
}

// FinishLogin checks what came back and stores [notion <env>].
func (s Service) FinishLogin(ctx context.Context, c *creds.Store, env string, l proxy.Login, pasted string) error {
	p, ok := l.State.(Pending)
	if !ok {
		return deny(400, "notion: not a Notion login")
	}
	return s.oauth().finishLogin(ctx, c, env, p, pasted)
}

// Refusals: a wrong workspace token, before the login is touched.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Method: "POST", Path: MCPPath, Body: "{}",
		Header: map[string]string{"Authorization": "Bearer " + proxy.WrongToken, "Content-Type": "application/json",
			"Accept": "application/json, text/event-stream"},
		Status: 403, Want: "per-environment workspace token"}}
}

// Check asks Notion, now, who the env's login is and what it offers. The
// live tool list is the other answer: a tool Notion offers that the table
// does not model is shut from the isolation, and the operator should know
// it exists.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	o := s.oauth()
	token, err := o.accessToken(ctx, c, name, "")
	if err != nil {
		return "", err
	}
	me, tools, err := probe(ctx, o, token)
	if err == errUnauthorized {
		if token, err = o.accessToken(ctx, c, name, token); err != nil {
			return "", err
		}
		if me, tools, err = probe(ctx, o, token); err == errUnauthorized {
			return "", fmt.Errorf("the login in [notion %s] is not accepted by Notion; run `gero env secret notion %s`", name, name)
		}
	}
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "identity   %s\n", IdentitySummary(me))
	fams := FamiliesFor(e)
	sort.Strings(fams)
	for _, f := range fams {
		mark := ""
		if isWrite(f) {
			mark = " (WRITE)"
		}
		fmt.Fprintf(&b, "  %-10s open%s\n", f, mark)
	}
	if len(fams) == 0 {
		b.WriteString("  (no families allowed; only the probe tools answer)\n")
	}
	visible := 0
	var unmodeled []string
	for _, t := range tools {
		if Visible(e, t) {
			visible++
		}
		if ToolFamily(t) == "" {
			unmodeled = append(unmodeled, t)
		}
	}
	fmt.Fprintf(&b, "tools      %d offered by Notion, %d visible here\n", len(tools), visible)
	if len(unmodeled) > 0 {
		fmt.Fprintf(&b, "unmodeled  %s — shut from the isolation until the tool table names them\n", strings.Join(unmodeled, ", "))
	}
	b.WriteString("reach      everything the logged-in Notion user can see — an MCP login has no sharing screen in front of it\n")
	return b.String(), nil
}

// IdentitySummary is one line naming the login, from whatever
// notion-get-self said.
func IdentitySummary(result map[string]any) string {
	var parts []string
	content, _ := result["content"].([]any)
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				parts = append(parts, t)
			}
		}
	}
	text := strings.TrimSpace(strings.Join(parts, " "))
	var data map[string]any
	if json.Unmarshal([]byte(text), &data) == nil && data != nil {
		user, ok := data["user"].(map[string]any)
		if !ok {
			user = data
		}
		ws, _ := data["workspace"].(map[string]any)
		who := str(user["name"])
		if who == "" {
			who = str(user["email"])
		}
		where := str(ws["name"])
		if where == "" {
			where = str(data["workspace_name"])
		}
		if who != "" || where != "" {
			return or(who, "?") + " in workspace " + or(where, "?")
		}
	}
	if text == "" {
		return "no answer"
	}
	line, _, _ := strings.Cut(text, "\n")
	if len(line) > 160 {
		line = line[:160]
	}
	return line
}

func str(v any) string { s, _ := v.(string); return s }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
