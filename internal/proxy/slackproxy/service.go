package slackproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port = 3024
	// maxBody caps what a request may make the daemon buffer; the clamps
	// read the body before it goes anywhere. The modeled methods are reads
	// whose parameters are a few hundred bytes.
	maxBody = 1 << 20
	// namesTTL bounds how long a denied channel's name is trusted. A rename
	// inside it leaves a stale exclusion, which Slack's search still honours
	// against the old name.
	namesTTL = time.Hour
)

type Service struct {
	// API and Files default to https://slack.com and
	// https://files.slack.com; tests point them, and Transport, at local
	// servers.
	API, Files string
	Transport  http.RoundTripper
}

func (Service) Name() string { return "slack" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "Slack, read-only: Lists, and per env channel messages, search and files with a channel deny list"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "families", Kind: proxy.List, Flag: "allow-api", Clear: "clear-apis", Metavar: "FAMILY", Choices: familyNames(),
			Help: "a method family this env opens (repeat; replaces the list; default lists)"},
		{Key: "deny_channels", Kind: proxy.List, Flag: "deny-channel", Clear: "clear-denied", Metavar: "ID",
			Help: "a channel id refused for messages and files and excluded from search (repeat; replaces the list); everything not denied is readable"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	delete(env, "channels") // isolate's short-lived allowlist shape
	// Made explicit on every add and edit, so `gero env ls` shows the
	// boundary rather than a default the reader must know.
	families := DefaultFamilies
	if _, ok := env["families"]; ok {
		families = proxy.Strings(env, "families")
	}
	for _, f := range families {
		if _, ok := Families[f]; !ok {
			return nil, fmt.Errorf("%q is not a slack method family (have: %s)", f, strings.Join(familyNames(), ", "))
		}
	}
	env["families"] = proxy.SortedSet(families)
	denied := proxy.Strings(env, "deny_channels")
	var bad []string
	for _, c := range denied {
		if !ChannelID.MatchString(c) {
			bad = append(bad, c)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("--deny-channel takes channel ids (C..., G..., D...), not names: %s", strings.Join(bad, ", "))
	}
	env["deny_channels"] = proxy.SortedSet(denied)
	var wide []string
	for _, f := range ChannelFamilies {
		if contains(env["families"].([]string), f) {
			wide = append(wide, f)
		}
	}
	if len(wide) > 0 && len(denied) == 0 {
		return []string{fmt.Sprintf("env %q opens %s across every channel the token can see; carve exceptions with `gero env edit slack %s --deny-channel <id>`",
			name, strings.Join(wide, " and "), name)}, nil
	}
	return nil, nil
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
			return nil, fmt.Errorf("slack env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// Workspace plants SLACK_TOKEN and SLACK_API_URL pointed at the proxy;
// every exposed env gets a SLACK_TOKEN_<ENV> twin so switching is one
// variable swap.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	ws := proxy.Workspace{Env: []string{
		"SLACK_TOKEN=" + v.Tokens[v.Exposure.Default],
		fmt.Sprintf("SLACK_API_URL=http://127.0.0.1:%d/api", Port),
	}}
	names := append([]string(nil), v.Exposure.Envs...)
	sort.Strings(names)
	for _, n := range names {
		if tok := v.Tokens[n]; tok != "" {
			ws.Env = append(ws.Env, "SLACK_TOKEN_"+proxy.EnvSuffix(n)+"="+tok)
		}
	}
	ws.Files = proxy.SkillFiles("slack", skillBody(v, envs))
	return ws, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("slack") }

func (s Service) bases() (*url.URL, *url.URL, http.RoundTripper) {
	api, _ := url.Parse(or(s.API, "https://slack.com"))
	files, _ := url.Parse(or(s.Files, "https://files.slack.com"))
	rt := s.Transport
	if rt == nil {
		rt = &http.Transport{
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return api, files, rt
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (s Service) New(h proxy.Host) proxy.Instance {
	api, files, rt := s.bases()
	// 50/min matches Slack's Tier 3, where the Lists reads live: the budget
	// runs out at the pace the upstream limit would. One per isolation, as
	// isolate keeps one per workspace.
	in := &instance{h: h, api: api, files: files, transport: rt,
		bucket: proxy.NewTokenBucket(50, 10, nil)}
	in.names = &ChannelNames{API: api, Transport: rt, TTL: namesTTL}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

type instance struct {
	h          proxy.Host
	api, files *url.URL
	transport  http.RoundTripper
	bucket     *proxy.TokenBucket
	names      Names
	srv        *http.Server
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("slack proxy: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("slack", format, a...) }

// refuse answers in Slack's own error shape, so SDKs surface the sentence
// instead of choking on an unfamiliar body.
func (in *instance) refuse(w http.ResponseWriter, r *http.Request, status int, reason string, retry time.Duration) {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": reason})
	b = append(b, '\n')
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	}
	// The request body may be unread; reusing the connection would parse
	// it as the next request.
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	w.Write(b)
	in.logf("DENY %s %s: %s", r.Method, r.RequestURI, reason)
}

func (in *instance) refuseD(w http.ResponseWriter, r *http.Request, d *Denied) {
	in.refuse(w, r, d.Status, d.Reason, 0)
}

func bearer(h string) string {
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v, err := in.h.View()
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	if !v.Enabled {
		in.refuse(w, r, 503, fmt.Sprintf("slack: the slack proxy is off for isolation %s; the operator turns it on with `gero iso expose %s slack <env>`", in.h.Isolation, in.h.Isolation), 0)
		return
	}
	name := ResolveEnv(v.Tokens, v.Exposure.Envs, bearer(r.Header.Get("Authorization")))
	if name == "" {
		in.refuse(w, r, 403, "slack: this proxy authenticates the isolation by its per-environment workspace token (sent as `Authorization: Bearer`); the real Slack token never enters the isolation", 0)
		return
	}
	if ok, retry := in.bucket.Allow(); !ok {
		in.refuse(w, r, 429, "slack: this isolation's own rate budget is exhausted; wait and retry — Slack's per-method limits are shared with every other integration and are deliberately not being spent by a loop", retry)
		return
	}
	rawPath, rawQuery := proxy.RawTarget(r)
	method, d := Route(r.Method, rawPath)
	if d != nil {
		in.refuseD(w, r, d)
		return
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	e, ok := envs[name]
	if !ok {
		in.refuse(w, r, 503, fmt.Sprintf("slack: environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s slack ...`", name, in.h.Isolation), 0)
		return
	}
	if d := Allowed(e, name, method); d != nil {
		in.refuseD(w, r, d)
		return
	}
	if r.ContentLength < 0 {
		in.refuse(w, r, 411, "slack: chunked request bodies are not accepted; send a Content-Length", 0)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		in.refuse(w, r, 400, "slack: the request body could not be read", 0)
		return
	}
	if len(body) > maxBody {
		in.refuse(w, r, 413, fmt.Sprintf("slack: request bodies are capped at %d bytes", maxBody), 0)
		return
	}
	ctype := r.Header.Get("Content-Type")
	if d := GuardChannel(e, name, method, rawQuery, body, ctype); d != nil {
		in.refuseD(w, r, d)
		return
	}
	secret, err := in.h.Creds.Read("slack", name)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	token := secret["token"]
	query, d := StripCredentials(rawQuery)
	if d != nil {
		in.refuseD(w, r, d)
		return
	}
	if method == Download {
		id, _ := FileIDOf(strings.TrimRight(rawPath, "/"))
		in.download(w, r, e, name, token, id)
		return
	}
	if contains(Families["search"], method) {
		if query, body, d = GuardSearch(e, name, query, body, ctype, token, in.names); d != nil {
			in.refuseD(w, r, d)
			return
		}
	}
	// Rebuilt from the approved pieces, never forwarded raw.
	target := in.api.JoinPath("api", method).String()
	if query != "" {
		target += "?" + query
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		in.refuse(w, r, 502, "slack: upstream request: "+err.Error(), 0)
		return
	}
	up.ContentLength = int64(len(body))
	if len(body) == 0 {
		up.Body = http.NoBody
	}
	proxy.CopyHeaders(up.Header, r.Header, "authorization", "host", "content-length", "expect")
	up.Header.Set("Authorization", "Bearer "+token)
	resp, err := in.transport.RoundTrip(up)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, method, err)
		in.refuse(w, r, 502, "slack: upstream unavailable: "+err.Error(), 0)
		return
	}
	defer resp.Body.Close()
	if method == "files.info" && len(e.DenyChannels) > 0 {
		// Clamped on the answer: the request names a file, and only Slack
		// knows where that file is shared.
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			in.refuse(w, r, 502, "slack: files.info answer cut short", 0)
			return
		}
		if resp.StatusCode == 200 {
			file, d := InfoAnswer(raw)
			if d == nil {
				d = GuardFile(e, name, file)
			}
			if d != nil && d.Status != 404 {
				in.refuseD(w, r, d)
				return
			}
		}
		relayBytes(w, resp, raw)
	} else {
		proxy.Relay(w, resp)
	}
	in.logf("%s /api/%s [env=%s] -> %d", r.Method, method, name, resp.StatusCode)
}

// relayBytes relays an answer a clamp already read.
func relayBytes(w http.ResponseWriter, resp *http.Response, raw []byte) {
	proxy.CopyHeaders(w.Header(), resp.Header, "content-length")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(resp.StatusCode)
	w.Write(raw)
}

func (in *instance) get(ctx context.Context, base *url.URL, pathAndQuery, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", base.Scheme+"://"+base.Host+pathAndQuery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return in.transport.RoundTrip(req)
}

// download is files.info, the deny clamp, then the bytes from the files
// host. A redirect or an HTML page in place of the file is Slack saying the
// token may not read it: relayed as a sentence, never followed and never
// handed over as if it were the file.
func (in *instance) download(w http.ResponseWriter, r *http.Request, e Env, envName, token, id string) {
	resp, err := in.get(r.Context(), in.api, "/api/files.info?file="+url.QueryEscape(id), token)
	if err != nil {
		in.refuse(w, r, 502, "slack: upstream unavailable: "+err.Error(), 0)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		in.refuse(w, r, 502, "slack: files.info answer cut short", 0)
		return
	}
	file, d := InfoAnswer(raw)
	if d == nil {
		d = GuardFile(e, envName, file)
	}
	var path string
	if d == nil {
		path, d = DownloadTarget(file, in.files)
	}
	if d != nil {
		in.refuseD(w, r, d)
		return
	}
	resp, err = in.get(r.Context(), in.files, path, token)
	if err != nil {
		in.logf("UPSTREAM-ERROR GET %s: %v", in.files.Host, err)
		in.refuse(w, r, 502, fmt.Sprintf("slack: %s unavailable: %v", in.files.Host, err), 0)
		return
	}
	defer resp.Body.Close()
	mime, _ := file["mimetype"].(string)
	reason := ""
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		loc := resp.Header.Get("Location")
		if loc == "" {
			loc = "?"
		}
		reason = fmt.Sprintf("slack: %s redirected the download of %s (to %q) instead of serving it — the token cannot read this file; check it carries files:read and can see the conversation it was shared in", in.files.Host, id, loc)
	case resp.StatusCode != 200:
		reason = fmt.Sprintf("slack: %s answered HTTP %d for %s", in.files.Host, resp.StatusCode, id)
	case mediaType(resp.Header.Get("Content-Type")) == "text/html" && mime != "text/html":
		reason = fmt.Sprintf("slack: %s served an HTML page in place of %s — a login page, meaning the token cannot read this file; check it carries files:read", in.files.Host, id)
	}
	if reason != "" {
		in.refuse(w, r, 502, reason, 0)
		return
	}
	proxy.Relay(w, resp)
	in.logf("GET /files/%s [%s env=%s] -> %d", id, Download, envName, resp.StatusCode)
}

// ChannelNames maps channel id to name, asked of Slack with the env's own
// token. Search qualifiers speak names while the deny list stores ids, so
// the search clamp has to cross that bridge.
type ChannelNames struct {
	API       *url.URL
	Transport http.RoundTripper
	TTL       time.Duration

	mu    sync.Mutex
	names map[string]cachedName // by env + id
}

type cachedName struct {
	name string
	at   time.Time
}

func (c *ChannelNames) Get(env, token, id string) (string, *Denied) {
	key := env + "\x00" + id
	c.mu.Lock()
	if n, ok := c.names[key]; ok && (c.TTL == 0 || time.Since(n.at) < c.TTL) {
		c.mu.Unlock()
		return n.name, nil
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", c.API.Scheme+"://"+c.API.Host+"/api/conversations.info?channel="+url.QueryEscape(id), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Transport.RoundTrip(req)
	if err != nil {
		return "", deny(502, "slack: could not resolve denied channel %s to a name (%v); search stays refused rather than leaking it", id, err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var answer struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	json.Unmarshal(raw, &answer)
	if !answer.OK || answer.Channel.Name == "" {
		e := answer.Error
		if e == "" {
			e = "no name"
		}
		return "", deny(502, "slack: denied channel %s did not resolve to a channel name (Slack answered %q); search stays refused rather than leaking it", id, e)
	}
	c.mu.Lock()
	if c.names == nil {
		c.names = map[string]cachedName{}
	}
	c.names[key] = cachedName{answer.Channel.Name, time.Now()}
	c.mu.Unlock()
	return answer.Channel.Name, nil
}

// Refusals: a wrong workspace token, before the real one is read.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Method: "GET", Path: "/api/auth.test",
		Header: map[string]string{"Authorization": "Bearer " + proxy.WrongToken}, Status: 403, Want: "per-environment workspace token"}}
}

// Check asks Slack, now, who the env's token is and whether it carries the
// scope each open family needs.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	secret, err := c.Read("slack", name)
	if err != nil {
		return "", err
	}
	api, _, rt := s.bases()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", api.Scheme+"://"+api.Host+"/api/auth.test", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+secret["token"])
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return "", fmt.Errorf("could not reach %s: %w", api.Host, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var answer struct {
		OK                     bool
		Error, Team, User, URL string
	}
	if json.Unmarshal(raw, &answer) != nil {
		return "", fmt.Errorf("slack: auth.test did not answer JSON (HTTP %d)", resp.StatusCode)
	}
	if !answer.OK {
		return "", fmt.Errorf("the token in [slack %s] is not valid: Slack answered %q to auth.test", name, answer.Error)
	}
	var scopes []string
	for _, sc := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			scopes = append(scopes, sc)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "token      %s as %s (team %s)\n", or(answer.URL, "slack.com"), or(answer.User, "?"), or(answer.Team, "?"))
	families := FamiliesFor(e)
	sort.Strings(families)
	var lacking []string
	for _, f := range families {
		scope := RequiredScopes[f]
		if contains(scopes, scope) {
			fmt.Fprintf(&b, "%-10s open (%s granted)\n", f, scope)
			continue
		}
		have := strings.Join(scopes, ", ")
		if have == "" {
			have = "none reported"
		}
		fmt.Fprintf(&b, "%-10s the token lacks %s; Slack will refuse %s (scopes: %s)\n", f, scope, strings.Join(Families[f], ", "), have)
		lacking = append(lacking, f)
	}
	if len(families) == 0 {
		b.WriteString("(no families allowed; only auth.test answers)\n")
	}
	if ReadsChannels(families) {
		denied := strings.Join(e.DenyChannels, ", ")
		if denied == "" {
			denied = "(no channels — everything the token sees answers)"
		}
		fmt.Fprintf(&b, "denied     %s\n", denied)
	}
	if len(lacking) > 0 {
		return b.String(), fmt.Errorf("the token in [slack %s] lacks the scope for: %s", name, strings.Join(lacking, ", "))
	}
	return b.String(), nil
}
