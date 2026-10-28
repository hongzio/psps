package ddproxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port = 3023
	// maxScopedBody caps the one body this proxy must hold in memory: a log
	// search it rewrites. Search bodies are a few hundred bytes.
	maxScopedBody = 1 << 20
)

// siteName keeps the upstream host a host: the site is spliced into
// https://api.<site>, and the real key pair goes wherever that points.
var siteName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// Env is one Datadog environment: a site (one org), the endpoint families
// that open, the families among them that may also write, and the scope
// every log search is narrowed to.
type Env struct {
	Site      string   `json:"site"`
	Families  []string `json:"families"`
	Writes    []string `json:"writes"`
	LogsQuery string   `json:"logs_query,omitempty"`
}

func (e Env) site() string {
	if e.Site == "" {
		return DefaultSite
	}
	return e.Site
}

// WritesFor is the families this env may write, which is nothing until
// someone says otherwise: absent and empty mean the same thing here, since
// a write is never something an env grows by default.
func WritesFor(e Env) []string { return append([]string(nil), e.Writes...) }

// ResolveEnv is which exposed env the presented workspace token names.
// Datadog's wire has no slot for an environment, so the credential is the
// selector. Every candidate is compared, so a mismatch costs what a match
// does.
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

// readKeys is both real keys for one env, read per request: rotation
// needs no restart, and there is never a second copy in the daemon.
func readKeys(c *creds.Store, env string) (apiKey, appKey string, err error) {
	if c == nil {
		return "", "", deny(503, "datadog: no credentials store is configured")
	}
	v, err := c.Read("datadog", env)
	if err != nil {
		return "", "", deny(503, "%s", err.Error())
	}
	return v["api_key"], v["app_key"], nil
}

type Service struct {
	// Transport defaults to a TLS-verifying one; tests point it at a tap.
	Transport http.RoundTripper
}

func (Service) Name() string { return "datadog" }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "the Datadog API; an endpoint-family allowlist, writes per family, an enforced log scope"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "site", Kind: proxy.String, Flag: "site", Metavar: "SITE",
			Help: "the Datadog site this env opens (default datadoghq.com)"},
		{Key: "families", Kind: proxy.List, Flag: "allow-api", Clear: "clear-apis", Metavar: "FAMILY", Choices: Families,
			Help: "an endpoint family that opens (repeat; replaces the list)"},
		{Key: "writes", Kind: proxy.List, Flag: "allow-write", Clear: "clear-writes", Metavar: "FAMILY", Choices: Families,
			Help: "a family whose writes open too: monitors, dashboards, events, metrics; its reads must be open (repeat; replaces the list)"},
		{Key: "logs_query", Kind: proxy.String, Flag: "allow-logs-query", Metavar: "QUERY",
			Help: "the scope every log search is narrowed to, e.g. service:api"},
	}
}

func (Service) Settings() []proxy.Field                { return nil }
func (Service) NormalizeSettings(map[string]any) error { return nil }

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	families := proxy.Strings(env, "families")
	for _, f := range families {
		if !contains(Families, f) {
			return nil, fmt.Errorf("%q is not an endpoint family (have: %s)", f, strings.Join(Families, ", "))
		}
	}
	writes := proxy.Strings(env, "writes")
	for _, f := range writes {
		if !contains(Families, f) {
			return nil, fmt.Errorf("%q is not an endpoint family (have: %s)", f, strings.Join(Families, ", "))
		}
		if !contains(Writable, f) {
			return nil, fmt.Errorf("the %q family has no write surface to open (writable: %s); Datadog's writes for it either do not exist or go somewhere this proxy does not forward", f, strings.Join(Writable, ", "))
		}
	}
	site := strings.ToLower(proxy.StringOr(env, "site", DefaultSite))
	if !siteName.MatchString(site) {
		return nil, fmt.Errorf("--site %q is not a host name (datadoghq.com, datadoghq.eu, us5.datadoghq.com, ...)", site)
	}
	env["site"] = site
	env["families"] = proxy.SortedSet(families)
	// A write whose reads are shut is incoherent, and an env must never
	// describe a boundary nobody chose: the write goes, and the operator
	// hears exactly which flag keeps it.
	var warn []string
	kept := []string{}
	for _, f := range proxy.SortedSet(writes) {
		if contains(families, f) {
			kept = append(kept, f)
		} else {
			warn = append(warn, fmt.Sprintf("env %q: the %s write was dropped because %s is not read-open; `gero env edit datadog %s --allow-api ... --allow-api %s --allow-write %s` keeps both", name, f, f, name, f, f))
		}
	}
	env["writes"] = kept
	env["logs_query"] = proxy.StringOr(env, "logs_query", "")
	if len(families) == 0 {
		warn = append(warn, fmt.Sprintf("env %q has an empty allowlist; only /api/v1/validate will answer until --allow-api", name))
	}
	if contains(families, "logs") && env["logs_query"] == "" {
		warn = append(warn, "the logs family is open with no --allow-logs-query scope; log search can read the entire org")
	}
	if len(kept) > 0 {
		warn = append(warn, fmt.Sprintf("env %q opens Datadog writes (%s): the agent can change what these families describe, not just read it", name, strings.Join(kept, ", ")))
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
			return nil, fmt.Errorf("datadog env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// Workspace plants the variable names Datadog's SDKs read: the unsuffixed
// keys carry the default env's token, every exposed env gets a
// DD_API_KEY_<ENV> twin, and switching envs is using a different key value.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	def := v.Tokens[v.Exposure.Default]
	ws := proxy.Workspace{Env: []string{
		"DD_API_KEY=" + def,
		"DD_APP_KEY=" + def,
		"DATADOG_API_KEY=" + def,
		"DATADOG_APP_KEY=" + def,
		fmt.Sprintf("DATADOG_HOST=http://127.0.0.1:%d", Port),
	}}
	names := append([]string(nil), v.Exposure.Envs...)
	sort.Strings(names)
	for _, n := range names {
		if tok := v.Tokens[n]; tok != "" {
			ws.Env = append(ws.Env, "DD_API_KEY_"+proxy.EnvSuffix(n)+"="+tok)
		}
	}
	ws.Files = proxy.SkillFiles("datadog", skillBody(v, envs))
	return ws, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("datadog") }

func (s Service) New(h proxy.Host) proxy.Instance {
	in := &instance{h: h, transport: s.transport(), buckets: &proxy.Buckets{PerMinute: 120, Burst: 40}}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

func (s Service) transport() http.RoundTripper {
	if s.Transport != nil {
		return s.Transport
	}
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}

type instance struct {
	h         proxy.Host
	transport http.RoundTripper
	buckets   *proxy.Buckets
	srv       *http.Server
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("datadog proxy: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("datadog", format, a...) }

// refuse answers in Datadog's own error shape, so SDKs surface the reason
// as the exception message.
func (in *instance) refuse(w http.ResponseWriter, r *http.Request, status int, reason string, retry time.Duration) {
	b, _ := json.Marshal(map[string][]string{"errors": {reason}})
	b = append(b, '\n')
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	// The request body, if any, was never read on this path; reusing the
	// connection would parse it as the next request.
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	w.Write(b)
	in.logf("DENY %s %s: %s", r.Method, r.RequestURI, reason)
}

func (in *instance) refuseErr(w http.ResponseWriter, r *http.Request, err error) {
	status := 403
	if d, ok := err.(*Denied); ok {
		status = d.Status
	}
	in.refuse(w, r, status, err.Error(), 0)
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawPath, rawQuery := proxy.RawTarget(r)
	query, queryToken, err := StripCredentials(rawQuery)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	v, err := in.h.View()
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	if !v.Enabled {
		in.refuse(w, r, 503, fmt.Sprintf("datadog: the proxy is off for isolation %s; the operator turns it on with `gero iso expose %s datadog <env>`", in.h.Name(), in.h.Name()), 0)
		return
	}
	// The caller's proof, from the header SDKs use or the parameter
	// datadogpy uses. Which real keys go out is decided here either way.
	presented := r.Header.Get("DD-API-KEY")
	if presented == "" {
		presented = queryToken
	}
	name := ResolveEnv(v.Tokens, v.Exposure.Envs, presented)
	if name == "" {
		in.refuse(w, r, 403, "datadog: this proxy authenticates the isolation by its per-environment workspace token (sent as DD-API-KEY); the real Datadog keys never enter the isolation", 0)
		return
	}
	if ok, retry := in.buckets.Get(name).Allow(); !ok {
		in.refuse(w, r, 429, "datadog: this isolation's own rate budget is exhausted; wait and retry — the org-wide Datadog limit is deliberately not being spent by a loop", retry)
		return
	}
	m, err := Route(r.Method, rawPath)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	e, ok := envs[name]
	if !ok {
		in.refuse(w, r, 503, fmt.Sprintf("datadog: environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s datadog ...`", name, in.h.Name()), 0)
		return
	}
	// The read family is checked first even for a write: an operator whose
	// env has neither hears about the half they must fix first.
	if m.Family != "" && !contains(e.Families, m.Family) {
		in.refuse(w, r, 403, fmt.Sprintf("datadog: endpoint family %q is not in the allowlist for environment %q. Ask the operator to run:\n  gero env edit datadog %s --allow-api %s", m.Family, name, name, m.Family), 0)
		return
	}
	// Naming the family, not just the request: opening writes is a
	// per-family decision, so the refusal must not read as "writes are off".
	if m.Write && !contains(WritesFor(e), m.Family) {
		path, _, _ := strings.Cut(rawPath, "?")
		in.refuse(w, r, 403, fmt.Sprintf("datadog: %s %s is a write to the %q family, which environment %q reads but does not write. Ask the operator to run:\n  gero env edit datadog %s --allow-write %s", r.Method, path, m.Family, name, name, m.Family), 0)
		return
	}
	apiKey, appKey, err := readKeys(in.h.Creds, name)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	if r.ContentLength < 0 {
		in.refuse(w, r, 411, "datadog: chunked request bodies are not accepted; send a Content-Length", 0)
		return
	}
	var body io.Reader
	length := r.ContentLength
	if length > 0 {
		body = r.Body
	}
	if e.LogsQuery != "" {
		switch m.Scope {
		case BodyScope:
			if length > maxScopedBody {
				in.refuse(w, r, 413, fmt.Sprintf("datadog: a log search body over %d bytes is not accepted", maxScopedBody), 0)
				return
			}
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxScopedBody))
			if err != nil {
				in.refuse(w, r, 400, "datadog: the request body could not be read", 0)
				return
			}
			wrapped, err := WrapLogsBody(raw, e.LogsQuery)
			if err != nil {
				in.refuseErr(w, r, err)
				return
			}
			body, length = bytes.NewReader(wrapped), int64(len(wrapped))
		case ParamsScope:
			if query, err = WrapLogsParams(query, e.LogsQuery); err != nil {
				in.refuseErr(w, r, err)
				return
			}
		}
	}
	// Rebuilt from the approved pieces, never forwarded raw.
	target := "https://api." + e.site() + rawPath
	if query != "" {
		target += "?" + query
	}
	up, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		in.refuse(w, r, 502, "datadog: upstream request: "+err.Error(), 0)
		return
	}
	up.ContentLength = length
	// Credential headers are replaced, never merely overwritten.
	proxy.CopyHeaders(up.Header, r.Header, "dd-api-key", "dd-application-key", "authorization", "host", "content-length", "expect")
	up.Header.Set("DD-API-KEY", apiKey)
	up.Header.Set("DD-APPLICATION-KEY", appKey)
	resp, err := in.transport.RoundTrip(up)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, rawPath, err)
		in.refuse(w, r, 502, "datadog: upstream unavailable: "+err.Error(), 0)
		return
	}
	defer resp.Body.Close()
	// Writes are marked: the operator's tail is the only place a change
	// made through here shows up as a change.
	family, write := m.Family, ""
	if family == "" {
		family = "validate"
	}
	if m.Write {
		write = " WRITE"
	}
	in.logf("%s %s [%s%s env=%s] -> %d", r.Method, rawPath, family, write, name, resp.StatusCode)
	proxy.Relay(w, resp)
}

type probe struct {
	method, path string
	body         any
}

// familyProbes are one representative read per family, chosen to answer
// 200 iff the application key carries that family's scope.
func familyProbes(e Env, now time.Time) map[string]probe {
	scope := e.LogsQuery
	if scope == "" {
		scope = "*"
	}
	t := now.Unix()
	return map[string]probe{
		"monitors": {"GET", "/api/v1/monitor?page_size=1", nil},
		"logs": {"POST", "/api/v2/logs/events/search", map[string]any{
			"filter": map[string]any{"query": scope, "from": "now-15m", "to": "now"}, "page": map[string]any{"limit": 1}}},
		"metrics": {"GET", fmt.Sprintf("/api/v1/query?from=%d&to=%d&query=avg:datadog.agent.up%%7B*%%7D", t-300, t), nil},
		"events":  {"GET", fmt.Sprintf("/api/v1/events?start=%d&end=%d", t-300, t), nil},
		"traces": {"POST", "/api/v2/spans/events/search", map[string]any{"data": map[string]any{
			"type": "search_request", "attributes": map[string]any{
				"filter": map[string]any{"from": "now-15m", "to": "now"}, "page": map[string]any{"limit": 1}}}}},
		"dashboards": {"GET", "/api/v1/dashboard", nil},
	}
}

// Check asks Datadog, from the Mac, now, whether each allowed family opens:
// a stale key, a missing scope on the application key or a wrong site is
// found by the operator at the terminal, not by the agent as an opaque 403.
// Refusals: a wrong workspace token, before any key is read.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Method: "GET", Path: "/api/v1/validate",
		Header: map[string]string{"DD-API-KEY": proxy.WrongToken}, Status: 403, Want: "per-environment workspace token"}}
}

// Reads only, even where writes are open: the only honest probe of POST
// /api/v1/dashboard is a dashboard that now exists.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	apiKey, appKey, err := readKeys(c, name)
	if err != nil {
		return "", err
	}
	rt := s.transport()
	host := "api." + e.site()
	do := func(p probe) (int, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var body io.Reader
		if p.body != nil {
			b, _ := json.Marshal(p.body)
			body = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, p.method, "https://"+host+p.path, body)
		if err != nil {
			return 0, err
		}
		req.Header.Set("DD-API-KEY", apiKey)
		req.Header.Set("DD-APPLICATION-KEY", appKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := rt.RoundTrip(req)
		if err != nil {
			return 0, fmt.Errorf("could not reach %s: %w", host, err)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	status, err := do(probe{"GET", "/api/v1/validate", nil})
	if err != nil {
		return "", err
	}
	if status != 200 {
		return "", fmt.Errorf("the API key in [datadog %s] is not valid for %s (validate answered %d)", name, host, status)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "site       %s — API key valid\n", host)
	probes := familyProbes(e, time.Now())
	families := append([]string(nil), e.Families...)
	sort.Strings(families)
	writes := WritesFor(e)
	failed := 0
	for _, f := range families {
		p, ok := probes[f]
		if !ok {
			continue
		}
		status, err := do(p)
		if err != nil {
			return out.String(), err
		}
		mark := ""
		if contains(writes, f) {
			mark = " (WRITE)"
		}
		if status == 200 {
			fmt.Fprintf(&out, "%-10s open%s\n", f, mark)
		} else {
			fmt.Fprintf(&out, "%-10s answered %d — the application key likely lacks this family's scope\n", f, status)
			failed++
		}
	}
	if len(families) == 0 {
		out.WriteString("(no families allowed; only /api/v1/validate answers)\n")
	}
	if len(writes) > 0 {
		sorted := append([]string(nil), writes...)
		sort.Strings(sorted)
		fmt.Fprintf(&out, "writes     %s — not probed; the application key needs write scope for these\n", strings.Join(sorted, ", "))
	}
	if failed > 0 {
		return out.String(), fmt.Errorf("%d of %d families did not open with the keys in [datadog %s]", failed, len(families), name)
	}
	return out.String(), nil
}
