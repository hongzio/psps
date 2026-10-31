package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/cronproxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
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
	// live is the tunnel while it is up, which forwarded ports dial into.
	live *tunnel.Tunnel
}

// Proxies configures the manager's credential proxies. Without it the
// manager runs none.
func (m *Manager) SetProxies(reg *proxy.Registry, c *creds.Store) {
	m.services, m.creds = reg, c
	defer m.reforward()
}

func (m *Manager) registry() *proxy.Registry {
	if m.services == nil {
		return proxy.NewRegistry()
	}
	return m.services
}

// viewFor is the isolation's (by ID) configuration of svc, as of now.
func (m *Manager) viewFor(iso string, svc proxy.Service) (proxy.View, error) {
	v := proxy.View{Isolation: m.isoName(iso), Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
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
		in = svc.New(proxy.Host{Name: func() string { return m.isoName(iso) }, Creds: m.creds, Log: m.log,
			View: func() (proxy.View, error) { return m.viewFor(iso, svc) }, Mail: m.mailHandler(iso)})
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

// liveTunnel is an isolation's tunnel while it is up.
func (m *Manager) liveTunnel(iso string) *tunnel.Tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ts, ok := m.tunnels[iso]; ok {
		return ts.live
	}
	return nil
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
			m.log.Printf("isolation %s: proxy tunnel %s", m.isoName(iso), msg)
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
	if v := m.shimVersion(iso); v != "" && v < shimproto.ProxiesVersion {
		return fmt.Errorf("the isolation's image predates the credential proxies; run `gero iso upgrade %s`", rec.Name)
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
	var failed, older []string
	for _, svc := range m.registry().All() {
		in := m.instance(iso, svc)
		for _, port := range svc.Ports() {
			if !m.shimOpens(iso, port) {
				// The shim refuses the port; asking would fail the rest.
				older = append(older, fmt.Sprintf("%s:%d", svc.Name(), port))
				continue
			}
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
		m.log.Printf("isolation %s: proxy tunnel up", rec.Name)
		if len(older) > 0 {
			m.log.Printf("isolation %s: its image predates %s; `gero iso upgrade %s` opens them", rec.Name, strings.Join(older, ", "), rec.Name)
		}
	}
	m.setTunnelStatus(ts, status)
	m.mu.Lock()
	ts.live = t
	m.mu.Unlock()
	// A Mac port another program held may be free by now.
	m.retryForwards()
	select {
	case <-t.Done():
	case <-ts.stop:
	case <-m.ctx.Done():
	}
	m.mu.Lock()
	ts.live = nil
	m.mu.Unlock()
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
	// mail: the isolation takes mail, so each agent starts with its own
	// address (mailEnv).
	mail bool
	// cron: its agents set crons, each at its own address (cronEnv).
	cron bool
	// skills are the files of the services whose files come and go
	// (proxy.Pruner), each with its content's hash: an agent CLI lists
	// skills as it starts, so a change to them wants a new process.
	skills []string
}

// signature changes whenever what an agent process starts with does.
func (w isoWorkspace) signature() string {
	sig := strings.Join(w.env, "\n")
	for _, s := range w.mcp {
		sig += "\nmcp " + s.Name + " " + s.URL + " " + s.TokenEnv
	}
	if w.mail {
		sig += "\nmail"
	}
	if w.cron {
		sig += "\ncron"
	}
	for _, f := range w.skills {
		sig += "\nskill " + f
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
		if p, ok := svc.(proxy.Pruner); ok {
			w.req.Prune = append(w.req.Prune, p.Prune()...)
		}
		if !v.Enabled {
			w.req.Remove = append(w.req.Remove, svc.Managed()...)
			continue
		}
		for _, n := range v.Missing {
			m.log.Printf("isolation %s: %s env %q is exposed but no longer defined; dropped", m.isoName(iso), svc.Name(), n)
		}
		ws, err := svc.Workspace(v)
		if err != nil {
			return w, fmt.Errorf("%s: %w", svc.Name(), err)
		}
		w.mail = w.mail || svc.Name() == mailproxy.Name
		w.cron = w.cron || svc.Name() == cronproxy.Name
		if _, ok := svc.(proxy.Pruner); ok {
			for _, f := range ws.Files {
				w.skills = append(w.skills, fmt.Sprintf("%s %x", f.Path, sha256.Sum256([]byte(f.Content))))
			}
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
// service off leaves nothing behind in them. Claude is also given the
// isolation's shared folders as directories of its own (--add-dir), so its
// permission modes treat them as they do its workspace.
func launchArgv(kind driver.Kind, argv []string, servers []proxy.MCPServer, shares []isolation.Mount) []string {
	if len(argv) == 0 || (len(servers) == 0 && (len(shares) == 0 || kind != driver.Claude)) {
		return argv
	}
	switch kind {
	case driver.Claude:
		out := append([]string(nil), argv...)
		for _, s := range shares {
			out = append(out, "--add-dir", s.Target)
		}
		if len(servers) > 0 {
			out = append(out, "--mcp-config", MCPFile)
		}
		return out
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
		m.log.Printf("isolation %s: %v", m.isoName(iso), err)
	}
	return w
}

// syncLocked brings the isolation's managed files up to date. Callers hold
// the isolation's lock with its container up.
func (m *Manager) syncLocked(ctx context.Context, iso store.Isolation) {
	if len(m.registry().All()) == 0 {
		return
	}
	if v := m.shimVersion(iso.ID); v != "" && v < shimproto.ProxiesVersion {
		return
	}
	w, err := m.workspace(iso.ID)
	if err != nil {
		m.log.Printf("isolation %s: workspace: %v", iso.Name, err)
		return
	}
	req := w.req
	if v := m.shimVersion(iso.ID); v != "" && v < shimproto.SkillsVersion {
		// It would write a binary file as its base64 and cannot prune.
		req.Files = slices.DeleteFunc(slices.Clone(req.Files), func(f shimproto.SyncFile) bool { return f.Base64 })
		req.Prune = nil
	}
	b, _ := json.Marshal(req)
	m.mu.Lock()
	same := m.synced[iso.ID] == string(b)
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
	m.synced[iso.ID] = string(b)
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

// shimOpens is whether the isolation's shim opens port in its tunnel; an
// unknown version is taken to be current.
func (m *Manager) shimOpens(iso string, port int) bool {
	v := m.shimVersion(iso)
	return v == "" || v >= shimproto.PortVersion(port)
}

// servicesChanged follows an edit to what isolations' proxies serve, by ID:
// their files are rewritten now if they are up, and agents whose
// environment changed get a new process at their next idle moment.
func (m *Manager) servicesChanged(ctx context.Context, ids ...string) {
	for _, id := range ids {
		iso, err := m.store.Isolation(id)
		if err != nil {
			continue
		}
		st := m.isoLock(id)
		st.mu.Lock()
		if state, err := m.cli.State(ctx, iso.Container); err == nil && state == "running" {
			m.syncLocked(ctx, iso)
		}
		st.mu.Unlock()

		sig := m.serviceWorkspace(id).signature()
		m.mu.Lock()
		changed := m.lastEnv[id] != sig
		m.lastEnv[id] = sig
		var rs []*runtime
		for _, r := range m.runtimes {
			if r.agent.IsolationID == id {
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
		_, replaces := s.(proxy.Mounter)
		_, forwards := s.(proxy.Forwarder)
		out = append(out, api.Service{Name: s.Name(), Summary: s.Summary(), Ports: s.Ports(), Fields: s.Fields(),
			Settings: s.Settings(), Secrets: creds.Fields[s.Name()], Login: creds.LoginServices[s.Name()], Replaces: replaces, Forwards: forwards})
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
	// A stored secret wins; only without one does the credential come from
	// elsewhere.
	if svc, err := m.registry().Get(e.Service); err == nil && len(v.MissingSecrets) > 0 {
		if so, ok := svc.(proxy.SecretOptional); ok && so.CredentialElsewhere(e.Config) {
			v.CredentialElsewhere, v.MissingSecrets = true, nil
		}
	}
	ids, _ := m.store.Exposing(e.Service, e.Name)
	v.ExposedTo = m.isoNames(ids)
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
	before := m.forwardErrs()
	if err := m.store.PutEnv(e); err != nil {
		return api.Env{}, err
	}
	holders, _ := m.store.Exposing(service, name)
	if _, ok := svc.(proxy.Forwarder); ok {
		// What is stored is what listens: a port the Mac cannot have puts
		// the env back as it was.
		if err := forwardErrors(before, m.reforward(), func(k forwardKey) bool { return k.service == service && k.env == name }); err != nil {
			m.store.PutEnv(cur)
			m.reforward()
			return api.Env{}, err
		}
	}
	e, _ = m.store.Env(service, name)
	v := m.envView(e)
	v.Warnings = warnings
	if _, ok := svc.(proxy.Mounter); ok {
		v.Warnings = append(v.Warnings, m.remountAll(ctx, holders)...)
	}
	m.servicesChanged(ctx, holders...)
	return v, nil
}

func (m *Manager) DeleteEnv(ctx context.Context, service, name string, force bool) error {
	svc, err := m.registry().Get(service)
	if err != nil {
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
	var problems []string
	if d, ok := svc.(proxy.Deleter); ok {
		if err := d.Deleted(name); err != nil {
			problems = append(problems, err.Error())
		}
	}
	if _, ok := svc.(proxy.Mounter); ok {
		problems = append(problems, m.remountAll(ctx, holders)...)
	}
	if _, ok := svc.(proxy.Forwarder); ok {
		m.reforward()
	}
	m.servicesChanged(ctx, holders...)
	if len(problems) > 0 {
		return fmt.Errorf("%s/%s is removed, but %s", service, name, strings.Join(problems, "; "))
	}
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

// Choices lists a Lookup field's choices, looked up with the credential
// the request carries or the env's stored one.
func (m *Manager) Choices(ctx context.Context, service, field string, req api.ChoicesRequest) ([]proxy.Choice, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return nil, badRequest(err)
	}
	lister, ok := svc.(proxy.ChoiceLister)
	lookup := false
	for _, f := range svc.Fields() {
		lookup = lookup || (f.Key == field && f.Lookup)
	}
	if !ok || !lookup {
		return nil, badRequest(fmt.Errorf("%s has no choices to look up for %s", service, field))
	}
	env := map[string]any{}
	secret := map[string]string{}
	if req.Env != "" {
		e, err := m.store.Env(service, req.Env)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			json.Unmarshal(e.Config, &env)
			if m.creds != nil {
				if v, err := m.creds.Read(service, req.Env); err == nil {
					secret = v
				}
			}
		}
	}
	for k, v := range req.Set {
		env[k] = v
	}
	for k, v := range req.Secret {
		if v = strings.TrimSpace(v); v != "" {
			secret[k] = v
		}
	}
	choices, err := lister.Choices(ctx, field, env, secret)
	var d *proxy.Denied
	if errors.As(err, &d) {
		return nil, badRequest(err)
	}
	if choices == nil {
		choices = []proxy.Choice{}
	}
	return choices, err
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

func (m *Manager) Exposures(name string) ([]api.Exposure, error) {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return nil, err
	}
	var out []api.Exposure
	for _, svc := range m.registry().All() {
		v, err := m.viewFor(iso.ID, svc)
		if err != nil {
			return nil, err
		}
		x := api.Exposure{Service: svc.Name(), Enabled: v.Enabled, Exposure: v.Exposure, Missing: v.Missing, Forwards: m.forwardsOf(iso.ID, svc, v)}
		x.Settings = m.settingsOut(svc, x.Settings)
		if !v.Enabled {
			x.Exposure = store.Exposure{IsolationID: iso.ID, Service: svc.Name(), Envs: []string{}}
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

func (m *Manager) Expose(ctx context.Context, name, service string, req api.Expose) (api.Exposure, error) {
	svc, err := m.registry().Get(service)
	if err != nil {
		return api.Exposure{}, badRequest(err)
	}
	iso, err := m.isolationNamed(name)
	if err != nil {
		return api.Exposure{}, err
	}
	cur, err := m.store.Exposure(iso.ID, service)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return api.Exposure{}, err
	}
	had := err == nil
	x := store.Exposure{IsolationID: iso.ID, Service: service, Envs: cur.Envs, Default: cur.Default, Settings: cur.Settings}
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
	settings, err := proxy.Apply(svc.Settings(), m.settingsOut(svc, x.Settings), req.Settings)
	if err != nil {
		return api.Exposure{}, badRequest(err)
	}
	if err := m.settingsIn(svc, settings); err != nil {
		return api.Exposure{}, badRequest(err)
	}
	if err := svc.NormalizeSettings(settings); err != nil {
		return api.Exposure{}, badRequest(err)
	}
	x.Settings, _ = json.Marshal(settings)
	if ec, ok := svc.(exposureChecker); ok {
		v := proxy.View{Isolation: iso.Name, Enabled: true, Exposure: x, Envs: map[string]json.RawMessage{}}
		for _, n := range x.Envs {
			e, _ := m.store.Env(service, n)
			v.Envs[n] = e.Config
		}
		if err := ec.CheckExposure(v); err != nil {
			return api.Exposure{}, badRequest(err)
		}
	}
	before := m.forwardErrs()
	if err := m.store.PutExposure(x); err != nil {
		return api.Exposure{}, err
	}
	if _, err := m.store.EnsureTokens(iso.ID, service, svc.TokenKeys(x)); err != nil {
		return api.Exposure{}, err
	}
	// What is stored is what runs: an exposure the container or the Mac
	// cannot have goes back to what they still have.
	putBack := func() {
		if had {
			m.store.PutExposure(cur)
		} else {
			m.store.DeleteExposure(iso.ID, service)
		}
	}
	if _, ok := svc.(proxy.Mounter); ok {
		if err := m.remount(ctx, iso.ID); err != nil {
			putBack()
			m.servicesChanged(ctx, iso.ID)
			return api.Exposure{}, err
		}
	}
	if _, ok := svc.(proxy.Forwarder); ok {
		if err := forwardErrors(before, m.reforward(), func(k forwardKey) bool { return k.iso == iso.ID && k.service == service }); err != nil {
			putBack()
			m.reforward()
			return api.Exposure{}, err
		}
	}
	m.servicesChanged(ctx, iso.ID)
	x.Settings = m.settingsOut(svc, x.Settings)
	out := api.Exposure{Service: service, Enabled: true, Exposure: x}
	if v, err := m.viewFor(iso.ID, svc); err == nil {
		out.Forwards = m.forwardsOf(iso.ID, svc, v)
	}
	return out, nil
}

func (m *Manager) Unexpose(ctx context.Context, name, service string) error {
	svc, err := m.registry().Get(service)
	if err != nil {
		return badRequest(err)
	}
	iso, err := m.isolationNamed(name)
	if err != nil {
		return err
	}
	if err := m.store.DeleteExposure(iso.ID, service); err != nil {
		return err
	}
	if _, ok := svc.(proxy.Mounter); ok {
		err = m.remount(ctx, iso.ID)
	}
	if _, ok := svc.(proxy.Forwarder); ok {
		m.reforward()
	}
	m.servicesChanged(ctx, iso.ID)
	return err
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
