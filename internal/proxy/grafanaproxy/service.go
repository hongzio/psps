package grafanaproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Port = 3027

	upstreamTimeout = 60 * time.Second
	// MaxBody caps what the proxy buffers to authorize a body. A large
	// org's dashboard JSON runs to a few megabytes; nothing here is bulk.
	MaxBody = 32 << 20
)

// Env is one Grafana environment: an instance, the endpoint families that
// open, the data sources queries may run against, and where dashboard
// saves may land.
type Env struct {
	BaseURL     string   `json:"base_url"`
	Families    []string `json:"families"`
	Writes      []string `json:"writes"`
	Datasources []string `json:"datasources"`
	Folders     []string `json:"folders"`
}

type Service struct {
	// Transport defaults to a TLS-verifying one; tests point it at a tap.
	Transport http.RoundTripper
}

func (Service) Name() string { return "grafana" }
func (Service) Ports() []int { return []int{Port} }

// Refusals: a wrong workspace token, before any token is read.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: Port, Method: "GET", Path: "/api/health",
		Header: map[string]string{"Authorization": "Bearer " + proxy.WrongToken}, Status: 403, Want: "per-environment workspace token"}}
}
func (Service) Summary() string {
	return "Grafana reads and queries by endpoint family; dashboard saves only into allowlisted folders"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "base_url", Kind: proxy.String, Flag: "base-url", Metavar: "URL",
			Help: "the instance this env opens, http(s)://host[:port][/subpath] (required)"},
		{Key: "families", Kind: proxy.List, Flag: "allow-api", Clear: "clear-apis", Metavar: "FAMILY", Choices: Families,
			Help: "an endpoint family that opens (repeat; replaces the list)"},
		{Key: "writes", Kind: proxy.List, Flag: "allow-write", Clear: "clear-writes", Metavar: "FAMILY", Choices: Families,
			Help: "a family whose writes open (only dashboards, bounded by --allow-folder); its reads must be open too (--allow-api)"},
		{Key: "datasources", Kind: proxy.List, Flag: "allow-datasource", Clear: "clear-datasources", Metavar: "UID",
			Help: "a data source UID /api/ds/query may run against (repeat; replaces the list)"},
		{Key: "folders", Kind: proxy.List, Flag: "allow-folder", Clear: "clear-folders", Metavar: "UID",
			Help: "a folder UID dashboard saves may land in, exactly, not its subfolders (repeat; replaces the list)"},
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
			return nil, fmt.Errorf("the %q family has no write surface to open (writable: %s); its writes are org plumbing no folder allowlist bounds", f, strings.Join(Writable, ", "))
		}
	}
	for _, key := range []string{"datasources", "folders"} {
		for _, uid := range proxy.Strings(env, key) {
			if !UID.MatchString(uid) {
				what := map[string]string{"datasources": "data source", "folders": "folder"}[key]
				return nil, fmt.Errorf("%q is not a %s UID (letters, digits, - and _)", uid, what)
			}
		}
		env[key] = proxy.SortedSet(proxy.Strings(env, key))
	}
	// A write whose reads are shut is incoherent, and an env must never
	// describe a boundary nobody chose: the write goes, and the operator
	// hears exactly which flags keep it. (isolate let --allow-write open its
	// read too; a Normalize sees only an edit's result, not which flags it
	// named, so that convenience would also keep a read open that a later
	// edit closed. The narrower reading wins, as in the Datadog proxy.)
	env["families"] = proxy.SortedSet(families)
	var dropped []string
	keptWrites := []string{}
	for _, f := range proxy.SortedSet(writes) {
		if contains(families, f) {
			keptWrites = append(keptWrites, f)
		} else {
			dropped = append(dropped, f)
		}
	}
	env["writes"] = keptWrites
	writes = keptWrites
	base := proxy.StringOr(env, "base_url", "")
	if base == "" {
		return nil, fmt.Errorf("--base-url is required; it names the Grafana instance %q opens", name)
	}
	if _, err := UpstreamParts(base); err != nil {
		return nil, err
	}
	env["base_url"] = base

	var warn []string
	for _, f := range dropped {
		warn = append(warn, fmt.Sprintf("env %q: the %s write was dropped because %s is not read-open; `gero env edit grafana %s --allow-api ... --allow-api %s --allow-write %s` keeps both", name, f, f, name, f, f))
	}
	fams := env["families"].([]string)
	if len(fams) == 0 {
		warn = append(warn, fmt.Sprintf("env %q has an empty allowlist; only /api/health will answer until --allow-api", name))
	}
	if contains(fams, "query") && len(env["datasources"].([]string)) == 0 {
		warn = append(warn, "the query family is open with no --allow-datasource; every /api/ds/query will be refused until one is named")
	}
	if len(writes) > 0 {
		folders := strings.Join(env["folders"].([]string), ", ")
		if folders == "" {
			folders = "no folder yet"
		}
		warn = append(warn, fmt.Sprintf("env %q opens dashboard saves: the agent can create and overwrite dashboards in %s", name, folders))
		if len(env["folders"].([]string)) == 0 {
			warn = append(warn, "dashboard writes are open with no --allow-folder; every save will be refused until one is named")
		}
		warn = append(warn, "give the service account Edit on exactly these folders and keep its role Viewer — the proxy narrows, the role enforces")
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
			return nil, fmt.Errorf("grafana env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// Workspace points GRAFANA_URL at the proxy; the token variables carry
// workspace tokens whose value doubles as the env selector.
// GRAFANA_SERVICE_ACCOUNT_TOKEN rides along because mcp-grafana and friends
// read that name, and every exposed env gets a GRAFANA_TOKEN_<ENV> twin so
// switching is one variable swap.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	def := v.Tokens[v.Exposure.Default]
	ws := proxy.Workspace{Env: []string{
		fmt.Sprintf("GRAFANA_URL=http://127.0.0.1:%d", Port),
		"GRAFANA_TOKEN=" + def,
		"GRAFANA_SERVICE_ACCOUNT_TOKEN=" + def,
	}}
	names := append([]string(nil), v.Exposure.Envs...)
	sort.Strings(names)
	for _, n := range names {
		if tok := v.Tokens[n]; tok != "" {
			ws.Env = append(ws.Env, "GRAFANA_TOKEN_"+proxy.EnvSuffix(n)+"="+tok)
		}
	}
	ws.Files = proxy.SkillFiles("grafana", skillBody(v, envs))
	return ws, nil
}

func (Service) Managed() []string { return proxy.SkillDirs("grafana") }

func (s Service) transport() http.RoundTripper {
	if s.Transport != nil {
		return s.Transport
	}
	return &http.Transport{
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: upstreamTimeout,
		IdleConnTimeout:       90 * time.Second,
	}
}

func (s Service) New(h proxy.Host) proxy.Instance {
	in := &instance{h: h, transport: s.transport(), buckets: &proxy.Buckets{PerMinute: 120, Burst: 40}}
	in.srv = &http.Server{Handler: in, ReadHeaderTimeout: 30 * time.Second}
	return in
}

type instance struct {
	h         proxy.Host
	transport http.RoundTripper
	// Each env has its own budget: two environments are two Grafana
	// instances, and one looping caller must not spend the other's.
	buckets *proxy.Buckets
	srv     *http.Server
}

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("grafana proxy: no service on port %d", port)
	}
	return in.srv.Serve(l)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("grafana", format, a...) }

// refuse answers in Grafana's own error shape, so clients surface the
// sentence.
func (in *instance) refuse(w http.ResponseWriter, r *http.Request, status int, reason string, retry time.Duration) {
	b, _ := json.Marshal(map[string][]string{"errors": {reason}})
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	}
	// The request body, if any, was never read on this path; reusing the
	// connection would parse it as the next request.
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)+1))
	w.WriteHeader(status)
	w.Write(append(b, '\n'))
	in.logf("DENY %s %s: %s", r.Method, r.RequestURI, reason)
}

func (in *instance) refuseErr(w http.ResponseWriter, r *http.Request, err error) {
	if d, ok := err.(*Denied); ok {
		in.refuse(w, r, d.Status, d.Reason, 0)
		return
	}
	in.refuse(w, r, 502, "grafana: "+err.Error(), 0)
}

// presented is the caller's proof, from the one header Grafana clients use.
func presented(r *http.Request) string {
	scheme, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	if !strings.EqualFold(scheme, "bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// readToken is the real service account token, read per request from the
// credentials file, so rotation needs no restart.
func readToken(c *creds.Store, env string) (string, error) {
	v, err := c.Read("grafana", env)
	if err != nil {
		return "", deny(503, "%s", err.Error())
	}
	return v["token"], nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.ContentLength < 0 {
		return nil, deny(411, "grafana: chunked request bodies are not accepted; send a Content-Length")
	}
	if r.ContentLength > MaxBody {
		return nil, deny(413, "grafana: a request body over %d bytes is not accepted here", MaxBody)
	}
	if r.ContentLength == 0 {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if err != nil {
		return nil, deny(400, "grafana: the request body was cut short: %v", err)
	}
	return b, nil
}

func (in *instance) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v, err := in.h.View()
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	if !v.Enabled {
		in.refuse(w, r, 503, fmt.Sprintf("grafana: the grafana proxy is off for isolation %s; the operator turns it on with `gero iso expose %s grafana <env>`", in.h.Name(), in.h.Name()), 0)
		return
	}
	name := ResolveEnv(v.Tokens, v.Exposure.Envs, presented(r))
	if name == "" {
		in.refuse(w, r, 403, "grafana: this proxy authenticates the isolation by its per-environment workspace token (sent as Authorization: Bearer); the real service account token never enters the isolation", 0)
		return
	}
	if ok, retry := in.buckets.Get(name).Allow(); !ok {
		in.refuse(w, r, 429, "grafana: this isolation's own rate budget is exhausted; wait and retry — the instance's shared limit is deliberately not being spent by a loop", retry)
		return
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		in.refuse(w, r, 503, err.Error(), 0)
		return
	}
	e, ok := envs[name]
	if !ok {
		in.refuse(w, r, 503, fmt.Sprintf("grafana: environment %q was removed from gero's registry but is still exposed here; the operator re-exposes with `gero iso expose %s grafana ...`", name, in.h.Name()), 0)
		return
	}
	rawPath, rawQuery := proxy.RawTarget(r)
	family, kind, err := Route(r.Method, rawPath)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	// The read family is checked first even for a write: an env with
	// neither hears about the half to fix first, and --allow-write opens
	// both anyway.
	if family != "" && !contains(e.Families, family) {
		in.refuse(w, r, 403, fmt.Sprintf("grafana: endpoint family %q is not in the allowlist for environment %q. Ask the operator to run:\n  gero env edit grafana %s --allow-api %s", family, name, name, family), 0)
		return
	}
	if WriteKinds[kind] && !contains(e.Writes, family) {
		in.refuse(w, r, 403, fmt.Sprintf("grafana: %s %s is a write to the %q family, which environment %q reads but does not write. Ask the operator to run:\n  gero env edit grafana %s --allow-write %s --allow-folder <uid>", r.Method, rawPath, family, name, name, family), 0)
		return
	}
	up, err := UpstreamParts(e.BaseURL)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	token, err := readToken(in.h.Creds, name)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	body, err := readBody(r)
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}
	switch kind {
	case "query":
		body, err = AuthorizeQueryBody(body, e.Datasources)
	case "save":
		body, err = AuthorizeSave(body, e.Folders, func(uid string) (string, bool, error) {
			return in.existingFolder(r.Context(), up, token, uid)
		})
	}
	if err != nil {
		in.refuseErr(w, r, err)
		return
	}

	// Rebuilt from the approved pieces, never forwarded raw: send exactly
	// what the table approved.
	target := rawPath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = strings.NewReader(string(body))
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, up.url(target), reqBody)
	if err != nil {
		in.refuse(w, r, 502, "grafana: upstream request: "+err.Error(), 0)
		return
	}
	out.ContentLength = int64(len(body))
	// The isolation's token is replaced, never merely overwritten, and a
	// cookie is a second credential channel: Grafana honours session
	// cookies on the same endpoints.
	proxy.CopyHeaders(out.Header, r.Header, "authorization", "cookie", "host", "content-length", "expect")
	out.Header.Set("Authorization", "Bearer "+token)
	resp, err := in.transport.RoundTrip(out)
	if err != nil {
		in.logf("UPSTREAM-ERROR %s %s: %v", r.Method, rawPath, err)
		in.refuse(w, r, 502, "grafana: upstream unavailable: "+err.Error(), 0)
		return
	}
	defer resp.Body.Close()
	// Writes are marked: the operator's log is the only place a change
	// made through here shows up as a change.
	label := family
	if label == "" {
		label = "health"
	}
	if WriteKinds[kind] {
		label += " WRITE"
	}
	in.logf("%s %s [%s env=%s] -> %d", r.Method, rawPath, label, name, resp.StatusCode)
	proxy.Relay(w, resp)
}

// existingFolder is the folder the dashboard uid lives in now ("" for
// General), or found=false when Grafana has no such dashboard. Anything but
// a clean 200 or 404 refuses the save: a dashboard this token cannot even
// see is not one the proxy can prove sits in an allowlisted folder.
func (in *instance) existingFolder(ctx context.Context, up Upstream, token, uid string) (string, bool, error) {
	status, data, err := get(ctx, in.transport, up, token, "/api/dashboards/uid/"+uid)
	if err != nil {
		return "", false, deny(502, "grafana: could not look up dashboard %s before saving it: %v", uid, err)
	}
	if status == 404 {
		return "", false, nil
	}
	if status != 200 {
		return "", false, deny(502, "grafana: looking up dashboard %s before saving it answered %d; a save is only forwarded once its current folder is known", uid, status)
	}
	var d struct {
		Meta *struct {
			FolderUID string `json:"folderUid"`
		} `json:"meta"`
	}
	if json.Unmarshal(data, &d) != nil {
		return "", false, deny(502, "grafana: looking up dashboard %s before saving it returned something that is not a dashboard", uid)
	}
	if d.Meta == nil {
		return "", true, nil
	}
	return d.Meta.FolderUID, true, nil
}

// get is one authenticated GET against an env's instance.
func get(ctx context.Context, rt http.RoundTripper, up Upstream, token, pathAndQuery string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", up.url(pathAndQuery), nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	return resp.StatusCode, data, err
}
