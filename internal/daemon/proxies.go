package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
	"github.com/hongzio/gero/internal/tunnel"
)

// The credential proxies run in this process. Each isolation that is up has
// one tunnel (internal/tunnel) carrying every proxy port; a connection on it
// goes straight to that isolation's Instance of the service, so the caller
// is identified by the tunnel it came through and nothing listens on the
// Mac. What a service puts in the isolation — its environment variables,
// git's URL rewrite, its skill — is written by `gero-shim sync` whenever the
// container (re)starts or an exposure or env changes.

// EnvFile carries the proxies' environment to shells; agents get the same
// variables directly when their process starts. The image sources it from
// /etc/profile.d and /etc/bash.bashrc.
const EnvFile = "/etc/gero/env.sh"

type tunnelState struct {
	kick    chan struct{}
	stop    chan struct{}
	status  string
	stopped bool
}

// Proxies configures the manager's credential proxies. Without it the
// manager runs none.
func (m *Manager) SetProxies(reg *proxy.Registry, c *creds.Store) {
	m.services, m.creds = reg, c
}

func (m *Manager) registry() *proxy.Registry {
	if m.services == nil {
		return proxy.NewRegistry()
	}
	return m.services
}

func (m *Manager) viewFor(iso string, svc proxy.Service) (proxy.View, error) {
	v := proxy.View{Isolation: iso, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
	x, err := m.store.Exposure(iso, svc.Name())
	if errors.Is(err, store.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	v.Enabled, v.Exposure = true, x
	for _, n := range x.Envs {
		e, err := m.store.Env(svc.Name(), n)
		if errors.Is(err, store.ErrNotFound) {
			v.Missing = append(v.Missing, n)
			continue
		}
		if err != nil {
			return v, err
		}
		v.Envs[n] = e.Config
	}
	v.Tokens, err = m.store.Tokens(iso, svc.Name())
	return v, err
}

func (m *Manager) instance(iso string, svc proxy.Service) proxy.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[iso] == nil {
		m.instances[iso] = map[string]proxy.Instance{}
	}
	in, ok := m.instances[iso][svc.Name()]
	if !ok {
		in = svc.New(proxy.Host{Isolation: iso, Creds: m.creds, Log: m.log,
			View: func() (proxy.View, error) { return m.viewFor(iso, svc) }})
		m.instances[iso][svc.Name()] = in
	}
	return in
}

// ensureTunnel keeps one tunnel loop per isolation, and wakes it when the
// container just came up so it does not sit out a backoff.
func (m *Manager) ensureTunnel(iso string) {
	if len(m.registry().All()) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ts, ok := m.tunnels[iso]
	if ok {
		select {
		case ts.kick <- struct{}{}:
		default:
		}
		return
	}
	ts = &tunnelState{kick: make(chan struct{}, 1), stop: make(chan struct{}), status: "starting"}
	m.tunnels[iso] = ts
	m.loops.Add(1)
	go func() {
		defer m.loops.Done()
		m.tunnelLoop(iso, ts)
	}()
}

func (m *Manager) stopTunnel(iso string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts, ok := m.tunnels[iso]; ok && !ts.stopped {
		ts.stopped = true
		close(ts.stop)
	}
	delete(m.tunnels, iso)
	delete(m.instances, iso)
}

func (m *Manager) tunnelStatus(iso string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts, ok := m.tunnels[iso]; ok {
		return ts.status
	}
	return ""
}

func (m *Manager) setTunnelStatus(ts *tunnelState, s string) {
	m.mu.Lock()
	ts.status = s
	m.mu.Unlock()
}

func (m *Manager) tunnelLoop(iso string, ts *tunnelState) {
	backoff := time.Second
	for {
		began := time.Now()
		err := m.runTunnel(iso, ts)
		if m.ctx.Err() != nil {
			return
		}
		select {
		case <-ts.stop:
			return
		default:
		}
		msg := "down: " + err.Error()
		if m.tunnelStatus(iso) != msg {
			m.log.Printf("isolation %s: proxy tunnel %s", iso, msg)
		}
		m.setTunnelStatus(ts, msg)
		if time.Since(began) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
			backoff = min(backoff*2, 30*time.Second)
		case <-ts.kick:
			backoff = time.Second
		case <-ts.stop:
			return
		case <-m.ctx.Done():
			return
		}
	}
}

func (m *Manager) runTunnel(iso string, ts *tunnelState) error {
	rec, err := m.store.Isolation(iso)
	if err != nil {
		return err
	}
	if v := m.shimVersion(iso); v != "" && v < shimproto.Version {
		return fmt.Errorf("the isolation's image predates the credential proxies; run `gero iso upgrade %s`", iso)
	}
	conn, err := m.cli.DialTunnel(m.ctx, rec.Container)
	if err != nil {
		return err
	}
	t, err := tunnel.Open(conn)
	if err != nil {
		if s := conn.Stderr(); s != "" {
			return fmt.Errorf("%v (%s)", err, s)
		}
		return err
	}
	defer t.Close()
	var listeners []net.Listener
	var failed []string
	for _, svc := range m.registry().All() {
		in := m.instance(iso, svc)
		for _, port := range svc.Ports() {
			l, err := t.Listen(m.tunnelPort(port))
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s:%d", svc.Name(), port))
				continue
			}
			listeners = append(listeners, l)
			go in.Serve(port, l)
		}
	}
	if len(failed) > 0 {
		// Something in the isolation holds them (a tunnel still exiting,
		// most likely). A half-open tunnel would serve some services and
		// not others; this one is closed and the loop tries again.
		for _, l := range listeners {
			l.Close()
		}
		why := strings.Join(failed, ", ")
		if s := conn.Stderr(); s != "" {
			why += " (" + s + ")"
		}
		return fmt.Errorf("these ports could not be opened in the isolation: %s", why)
	}
	status := "up"
	if m.tunnelStatus(iso) != "up" {
		m.log.Printf("isolation %s: proxy tunnel up", iso)
	}
	m.setTunnelStatus(ts, status)
	select {
	case <-t.Done():
	case <-ts.stop:
	case <-m.ctx.Done():
	}
	for _, l := range listeners {
		l.Close()
	}
	if err := t.Err(); err != nil {
		if s := conn.Stderr(); s != "" {
			return fmt.Errorf("%v (%s)", err, s)
		}
		return err
	}
	return errors.New("tunnel closed")
}

// isoWorkspace is everything the proxies want in one isolation: one sync
// request, the environment agents get, and the MCP servers they are
// started with.
type isoWorkspace struct {
	req shimproto.SyncRequest
	env []string
	mcp []proxy.MCPServer
}

// signature changes whenever what an agent process starts with does.
func (w isoWorkspace) signature() string {
	sig := strings.Join(w.env, "\n")
	for _, s := range w.mcp {
		sig += "\nmcp " + s.Name + " " + s.URL + " " + s.TokenEnv
	}
	return sig
}

func (m *Manager) workspace(iso string) (isoWorkspace, error) {
	var w isoWorkspace
	for _, svc := range m.registry().All() {
		v, err := m.viewFor(iso, svc)
		if err != nil {
			return w, err
		}
		if !v.Enabled {
			w.req.Remove = append(w.req.Remove, svc.Managed()...)
			continue
		}
		for _, n := range v.Missing {
			m.log.Printf("isolation %s: %s env %q is exposed but no longer defined; dropped", iso, svc.Name(), n)
		}
		ws, err := svc.Workspace(v)
		if err != nil {
			return w, fmt.Errorf("%s: %w", svc.Name(), err)
		}
		w.req.Files = append(w.req.Files, ws.Files...)
		w.env = append(w.env, ws.Env...)
		w.mcp = append(w.mcp, ws.MCP...)
	}
	sort.Strings(w.env)
	var b strings.Builder
	b.WriteString("# Written by gero: the credential proxies' addresses and workspace tokens.\n" +
		"# No real credential is here; the proxies hold those outside the isolation.\n")
	for _, kv := range w.env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
	}
	w.req.Files = append(w.req.Files, shimproto.SyncFile{Path: EnvFile, Mode: 0o644, Content: b.String()})
	if len(w.mcp) == 0 {
		w.req.Remove = append(w.req.Remove, MCPFile)
	} else {
		w.req.Files = append(w.req.Files, shimproto.SyncFile{Path: MCPFile, Mode: 0o600, Agent: true, Content: mcpConfig(w.mcp, w.env)})
	}
	return w, nil
}

// MCPFile is Claude Code's --mcp-config for the proxies' MCP servers. It
// carries workspace tokens, so it is the agent's alone.
const MCPFile = "/etc/gero/mcp.json"

func mcpConfig(servers []proxy.MCPServer, env []string) string {
	vals := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vals[k] = v
	}
	type server struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	cfg := map[string]map[string]server{"mcpServers": {}}
	for _, s := range servers {
		cfg["mcpServers"][s.Name] = server{Type: "http", URL: s.URL, Headers: map[string]string{"Authorization": "Bearer " + vals[s.TokenEnv]}}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b) + "\n"
}

// launchArgv hands an agent process the proxies' MCP servers: Claude Code
// through --mcp-config, Codex through -c overrides naming the token's
// variable. Neither tool's own configuration is touched, so turning a
// service off leaves nothing behind in them.
func launchArgv(kind driver.Kind, argv []string, servers []proxy.MCPServer) []string {
	if len(servers) == 0 || len(argv) == 0 {
		return argv
	}
	switch kind {
	case driver.Claude:
		return append(append([]string(nil), argv...), "--mcp-config", MCPFile)
	case driver.Codex:
		out := []string{argv[0]}
		for _, s := range servers {
			out = append(out, "-c", fmt.Sprintf("mcp_servers.%s.url=%q", s.Name, s.URL),
				"-c", fmt.Sprintf("mcp_servers.%s.bearer_token_env_var=%q", s.Name, s.TokenEnv))
		}
		return append(out, argv[1:]...)
	}
	return argv
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// serviceWorkspace is the proxies' side of an agent process: its
// environment and MCP servers.
func (m *Manager) serviceWorkspace(iso string) isoWorkspace {
	if len(m.registry().All()) == 0 {
		return isoWorkspace{}
	}
	w, err := m.workspace(iso)
	if err != nil {
		m.log.Printf("isolation %s: %v", iso, err)
	}
	return w
}

// syncLocked brings the isolation's managed files up to date. Callers hold
// the isolation's lock with its container up.
func (m *Manager) syncLocked(ctx context.Context, iso store.Isolation) {
	if len(m.registry().All()) == 0 {
		return
	}
	if v := m.shimVersion(iso.Name); v != "" && v < shimproto.Version {
		return
	}
	w, err := m.workspace(iso.Name)
	if err != nil {
		m.log.Printf("isolation %s: workspace: %v", iso.Name, err)
		return
	}
	req := w.req
	b, _ := json.Marshal(req)
	m.mu.Lock()
	same := m.synced[iso.Name] == string(b)
	m.mu.Unlock()
	if same {
		return
	}
	res, err := m.cli.Sync(ctx, iso.Container, req)
	if err != nil {
		m.log.Printf("isolation %s: %v", iso.Name, err)
		return
	}
	m.mu.Lock()
	m.synced[iso.Name] = string(b)
	m.mu.Unlock()
	if len(res.Written)+len(res.Removed) > 0 {
		m.log.Printf("isolation %s: synced %d file(s), removed %d", iso.Name, len(res.Written), len(res.Removed))
	}
}

func (m *Manager) shimVersion(iso string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.versions[iso]
}

// servicesChanged follows an edit to what an isolation's proxies serve: its
// files are rewritten now if it is up, and agents whose environment changed
// get a new process at their next idle moment.
func (m *Manager) servicesChanged(ctx context.Context, isos ...string) {
	for _, name := range isos {
		iso, err := m.store.Isolation(name)
		if err != nil {
			continue
		}
		st := m.isoLock(name)
		st.mu.Lock()
		if state, err := m.cli.State(ctx, iso.Container); err == nil && state == "running" {
			m.syncLocked(ctx, iso)
		}
		st.mu.Unlock()

		sig := m.serviceWorkspace(name).signature()
		m.mu.Lock()
		changed := m.lastEnv[name] != sig
		m.lastEnv[name] = sig
		var rs []*runtime
		for _, r := range m.runtimes {
			if r.agent.Isolation == name {
				rs = append(rs, r)
			}
		}
		m.mu.Unlock()
		if changed {
			for _, r := range rs {
				r.envChanged()
			}
		}
	}
}

// --- the API side ---------------------------------------------------------

func (m *Manager) Services() []api.Service {
	var out []api.Service
	for _, s := range m.registry().All() {
		out = append(out, api.Service{Name: s.Name(), Summary: s.Summary(), Ports: s.Ports(), Fields: s.Fields(),
			Settings: s.Settings(), Secrets: creds.Fields[s.Name()], Login: creds.LoginServices[s.Name()]})
	}
	return out
}

func (m *Manager) envView(e store.Env) api.Env {
	v := api.Env{Env: e}
	if _, keeps := creds.Fields[e.Service]; keeps && m.creds != nil {
		missing, err := m.creds.Status(e.Service, e.Name)
		v.MissingSecrets = missing
		if err != nil {
			v.SecretError = err.Error()
		}
	}
	v.ExposedTo, _ = m.store.Exposing(e.Service, e.Name)
	return v
}

func (m *Manager) Envs(service string) ([]api.Env, error) {
	if service != "" {
		if _, err := m.registry().Get(service); err != nil {
			return nil, badRequest(err)
		}
	}
	envs, err := m.store.Envs(service)
	if err != nil {
		return nil, err
	}
	out := []api.Env{}
	for _, e := range envs {
		out = append(out, m.envView(e))
	}
	return out, nil
}

// PutEnv adds (create) or edits an env definition.
func (m *Manager) PutEnv(ctx context.Context, service, name string, p proxy.Patch, create bool) (api.Env, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return api.Env{}, badRequest(err)
	}
	if err := proxy.ValidEnvName(name); err != nil {
		return api.Env{}, badRequest(err)
	}
	cur, err := m.store.Env(service, name)
	switch {
	case err == nil && create:
		return api.Env{}, badRequest(fmt.Errorf("%s env %s already exists; edit it with `gero env edit %s %s`", service, name, service, name))
	case errors.Is(err, store.ErrNotFound) && !create:
		return api.Env{}, err
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return api.Env{}, err
	}
	cfg, err := proxy.Apply(svc.Fields(), cur.Config, p)
	if err != nil {
		return api.Env{}, badRequest(err)
	}
	warnings, err := svc.Normalize(name, cfg)
	if err != nil {
		return api.Env{}, badRequest(err)
	}
	b, _ := json.Marshal(cfg)
	e := store.Env{Service: service, Name: name, Config: b, CreatedAt: cur.CreatedAt}
	if err := m.store.PutEnv(e); err != nil {
		return api.Env{}, err
	}
	e, _ = m.store.Env(service, name)
	v := m.envView(e)
	v.Warnings = warnings
	m.servicesChanged(ctx, v.ExposedTo...)
	return v, nil
}

func (m *Manager) DeleteEnv(ctx context.Context, service, name string, force bool) error {
	if _, err := m.registry().Get(service); err != nil {
		return badRequest(err)
	}
	holders, _ := m.store.Exposing(service, name)
	if err := m.store.DeleteEnv(service, name, force); err != nil {
		var ex *store.ErrExposed
		if errors.As(err, &ex) {
			return httpError{409, err}
		}
		return err
	}
	m.servicesChanged(ctx, holders...)
	return nil
}

// SetSecret stores an env's credential, write-only: nothing reads a secret
// back out through the API. nil values remove it.
func (m *Manager) SetSecret(service, name string, values map[string]string) error {
	if m.creds == nil {
		return errors.New("no credentials store")
	}
	want, keeps := creds.Fields[service]
	if !keeps {
		return badRequest(fmt.Errorf("%s keeps no secret in the credentials file", service))
	}
	// Clearing works for an env already removed: its secret outlives it
	// until someone says otherwise.
	if values == nil {
		return m.creds.Write(service, name, nil)
	}
	if _, err := m.store.Env(service, name); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, f := range append(append([]string(nil), want...), creds.Optional[service]...) {
		allowed[f] = true
	}
	for k := range values {
		if !allowed[k] {
			return badRequest(fmt.Errorf("%s has no secret field %q (fields: %s)", service, k, strings.Join(want, ", ")))
		}
	}
	for _, f := range want {
		if strings.TrimSpace(values[f]) == "" {
			return badRequest(fmt.Errorf("%s needs %s", service, strings.Join(want, " and ")))
		}
	}
	if err := m.creds.Write(service, name, values); err != nil {
		var d *creds.Denied
		if errors.As(err, &d) {
			return badRequest(err)
		}
		return err
	}
	return nil
}

func (m *Manager) CheckEnv(ctx context.Context, service, name string) (api.Check, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return api.Check{}, badRequest(err)
	}
	e, err := m.store.Env(service, name)
	if err != nil {
		return api.Check{}, err
	}
	out, err := svc.Check(ctx, name, e.Config, m.creds)
	c := api.Check{Output: out}
	if err != nil {
		c.Error = err.Error()
	}
	return c, nil
}

func (m *Manager) Exposures(iso string) ([]api.Exposure, error) {
	if _, err := m.store.Isolation(iso); err != nil {
		return nil, err
	}
	var out []api.Exposure
	for _, svc := range m.registry().All() {
		v, err := m.viewFor(iso, svc)
		if err != nil {
			return nil, err
		}
		x := api.Exposure{Service: svc.Name(), Enabled: v.Enabled, Exposure: v.Exposure, Missing: v.Missing}
		if !v.Enabled {
			x.Exposure = store.Exposure{Isolation: iso, Service: svc.Name(), Envs: []string{}}
		}
		out = append(out, x)
	}
	return out, nil
}

// exposureChecker is a service's own check on what an isolation may be
// given together (git refuses one repository listed by two envs).
type exposureChecker interface {
	CheckExposure(v proxy.View) error
}

func (m *Manager) Expose(ctx context.Context, iso, service string, req api.Expose) (api.Exposure, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return api.Exposure{}, badRequest(err)
	}
	if _, err := m.store.Isolation(iso); err != nil {
		return api.Exposure{}, err
	}
	cur, err := m.store.Exposure(iso, service)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return api.Exposure{}, err
	}
	x := store.Exposure{Isolation: iso, Service: service, Envs: cur.Envs, Default: cur.Default, Settings: cur.Settings}
	if req.Envs != nil {
		x.Envs = nil
		seen := map[string]bool{}
		var missing []string
		for _, n := range req.Envs {
			if n = strings.TrimSpace(n); n == "" || seen[n] {
				continue
			}
			seen[n] = true
			if _, err := m.store.Env(service, n); errors.Is(err, store.ErrNotFound) {
				missing = append(missing, n)
			}
			x.Envs = append(x.Envs, n)
		}
		if len(missing) > 0 {
			have, _ := m.store.Envs(service)
			var names []string
			for _, e := range have {
				names = append(names, e.Name)
			}
			return api.Exposure{}, badRequest(fmt.Errorf("no %s env named %s; create it with `gero env add %s <name> ...` (have: %s)",
				service, strings.Join(missing, ", "), service, orNone(strings.Join(names, ", "))))
		}
	}
	if x.Envs == nil {
		x.Envs = []string{}
	}
	if req.Default != "" {
		if !containsString(x.Envs, req.Default) {
			return api.Exposure{}, badRequest(fmt.Errorf("cannot default to %q: not exposed (exposed: %s)", req.Default, orNone(strings.Join(x.Envs, ", "))))
		}
		x.Default = req.Default
	}
	if !containsString(x.Envs, x.Default) {
		x.Default = ""
		if len(x.Envs) > 0 {
			x.Default = x.Envs[0]
		}
	}
	settings, err := proxy.Apply(svc.Settings(), x.Settings, req.Settings)
	if err != nil {
		return api.Exposure{}, badRequest(err)
	}
	if err := svc.NormalizeSettings(settings); err != nil {
		return api.Exposure{}, badRequest(err)
	}
	x.Settings, _ = json.Marshal(settings)
	if ec, ok := svc.(exposureChecker); ok {
		v := proxy.View{Isolation: iso, Enabled: true, Exposure: x, Envs: map[string]json.RawMessage{}}
		for _, n := range x.Envs {
			e, _ := m.store.Env(service, n)
			v.Envs[n] = e.Config
		}
		if err := ec.CheckExposure(v); err != nil {
			return api.Exposure{}, badRequest(err)
		}
	}
	if err := m.store.PutExposure(x); err != nil {
		return api.Exposure{}, err
	}
	if _, err := m.store.EnsureTokens(iso, service, svc.TokenKeys(x)); err != nil {
		return api.Exposure{}, err
	}
	m.servicesChanged(ctx, iso)
	return api.Exposure{Service: service, Enabled: true, Exposure: x}, nil
}

func (m *Manager) Unexpose(ctx context.Context, iso, service string) error {
	if _, err := m.registry().Get(service); err != nil {
		return badRequest(err)
	}
	if err := m.store.DeleteExposure(iso, service); err != nil {
		return err
	}
	m.servicesChanged(ctx, iso)
	return nil
}

func containsString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// pendingLogin is a browser login the daemon started and has not finished.
type pendingLogin struct {
	service, env string
	login        proxy.Login
}

// StartLogin begins a browser login for an env whose credential is one. The
// redirect must be loopback: it is registered as where the code goes.
func (m *Manager) StartLogin(ctx context.Context, service, name, redirect string) (api.Login, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return api.Login{}, badRequest(err)
	}
	ls, ok := svc.(proxy.LoginService)
	if !ok {
		return api.Login{}, badRequest(fmt.Errorf("%s does not log in through the browser; `gero env secret %s %s` stores its secret", service, service, name))
	}
	if _, err := m.store.Env(service, name); err != nil {
		return api.Login{}, err
	}
	if redirect == "" {
		var b [2]byte
		rand.Read(b[:])
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback", 49152+(int(b[0])<<8|int(b[1]))%16384)
	}
	if u, err := url.Parse(redirect); err != nil || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
		return api.Login{}, badRequest(fmt.Errorf("the login redirect must be an http loopback address, not %q", redirect))
	}
	l, err := ls.StartLogin(ctx, name, redirect)
	if err != nil {
		return api.Login{}, err
	}
	var idb [12]byte
	rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	m.mu.Lock()
	if m.logins == nil {
		m.logins = map[string]pendingLogin{}
	}
	now := time.Now()
	for k, p := range m.logins {
		if now.After(p.login.Expires) {
			delete(m.logins, k)
		}
	}
	m.logins[id] = pendingLogin{service: service, env: name, login: l}
	m.mu.Unlock()
	return api.Login{ID: id, URL: l.URL, Redirect: l.Redirect, Expires: l.Expires}, nil
}

// FinishLogin completes a login with the address the browser landed on. A
// login is spent by its first attempt, right or wrong.
func (m *Manager) FinishLogin(ctx context.Context, service, name, id, pasted string) error {
	m.mu.Lock()
	p, ok := m.logins[id]
	delete(m.logins, id)
	m.mu.Unlock()
	if !ok || p.service != service || p.env != name {
		return httpError{404, fmt.Errorf("no %s login %s in progress for %s; start one with `gero env secret %s %s`", service, id, name, service, name)}
	}
	svc, err := m.registry().Get(service)
	if err != nil {
		return badRequest(err)
	}
	if err := svc.(proxy.LoginService).FinishLogin(ctx, m.creds, name, p.login, pasted); err != nil {
		return badRequest(err)
	}
	m.log.Printf("%s/%s: logged in; stored [%s %s]", service, name, service, name)
	return nil
}
