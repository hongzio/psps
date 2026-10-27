package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/pf"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

var validIsolationName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

// Containers is what the manager needs from the container runtime.
// isolation.CLI is the real one; tests substitute in-process shims.
type Containers interface {
	SystemRunning(ctx context.Context) bool
	SystemStart(ctx context.Context) error
	State(ctx context.Context, name string) (string, error)
	Start(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	VolumeRemove(ctx context.Context, name string) error
	ImageExists(ctx context.Context, ref string) bool
	EnsureImage(ctx context.Context, progress io.Writer) (string, error)
	Create(ctx context.Context, iso, image string, cpus int, memory, network string) error
	RunAsAgent(ctx context.Context, container string, argv ...string) (string, error)
	ShimDialer(container string) shim.Dialer
	NetworkInspect(ctx context.Context, name string) (isolation.NetworkInfo, error)
	NetworkRemove(ctx context.Context, name string) error
	Probe(ctx context.Context, container string, targets []string) (isolation.ProbeResult, error)
	DialTunnel(ctx context.Context, container string) (isolation.TunnelConn, error)
	Sync(ctx context.Context, container string, req shimproto.SyncRequest) (shimproto.SyncResult, error)
}

// Fence is the root helper that keeps isolation networks off the Mac and its
// LAN; pf.Client is the real one.
type Fence interface {
	Apply(ctx context.Context, networks []pf.Network) (pf.Status, error)
}

// Manager reconciles the store's desired state with the world: isolations
// running with their shims answering, and one runtime per live agent.
type Manager struct {
	ctx   context.Context
	store *store.Store
	cli   Containers
	fence Fence
	// hostAddrs and bridgeOf read the Mac's interfaces for the fence
	// measurement; tests replace them.
	hostAddrs func() ([]pf.HostAddr, error)
	bridgeOf  func(gateway4 string) string
	log       *log.Logger
	hub       *hub
	// workRoot is where agent workspaces go inside an isolation.
	workRoot string
	// webURL / webErr: what Run managed to serve the web UI on.
	webURL, webErr string

	// services are the credential proxies; creds is where their secrets
	// live. tunnelPort maps a proxy's port to the one opened inside the
	// isolation (the same, except in tests).
	services   *proxy.Registry
	creds      *creds.Store
	tunnelPort func(int) int

	mu       sync.Mutex
	runtimes map[string]*runtime
	isos     map[string]*isoState
	fences   map[string]*fenceState
	loops    sync.WaitGroup
	// Per isolation: its proxies' instances and tunnel, the sync request
	// last applied since the container started, the shim version it
	// answered with, and the proxies' environment agents last started with.
	instances map[string]map[string]proxy.Instance
	tunnels   map[string]*tunnelState
	synced    map[string]string
	versions  map[string]string
	lastEnv   map[string]string
	logins    map[string]pendingLogin // browser logins in progress, by id
}

// fenceState is whether an isolation's network fence was measured to hold.
// Agents start only while ok.
type fenceState struct {
	ok    bool
	why   string
	tried time.Time
	stale bool // the container (re)started since the last measurement
	// retrying: a waitForFence loop is running for this isolation.
	retrying bool
	// off and loaded cache the isolation's own setting.
	off, loaded bool
}

type isoState struct {
	mu      sync.Mutex
	healthy time.Time
}

type agentView = api.Agent

func NewManager(ctx context.Context, st *store.Store, cli Containers, fence Fence, logger *log.Logger) *Manager {
	return &Manager{ctx: ctx, store: st, cli: cli, fence: fence, hostAddrs: pf.SystemAddrs, bridgeOf: pf.SystemBridge,
		log: logger, hub: newHub(), workRoot: isolation.WorkRoot,
		runtimes: map[string]*runtime{}, isos: map[string]*isoState{}, fences: map[string]*fenceState{},
		tunnelPort: func(p int) int { return p }, instances: map[string]map[string]proxy.Instance{},
		tunnels: map[string]*tunnelState{}, synced: map[string]string{}, versions: map[string]string{}, lastEnv: map[string]string{}}
}

// Reconcile is what the daemon does on every start, whatever the previous
// one died of: bring the container system up, then give every live agent a
// runtime. Each runtime brings its own isolation up and replays its log, so
// an agent that was mid-turn when the machine went down comes back
// "interrupted", with its context, waiting for the user.
func (m *Manager) Reconcile() error {
	if !m.cli.SystemRunning(m.ctx) {
		m.log.Printf("container system is not running; starting it")
		if err := m.cli.SystemStart(m.ctx); err != nil {
			return err
		}
	}
	agents, err := m.store.Agents()
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.ArchivedAt == nil {
			if _, err := m.ensureRuntime(a); err != nil {
				m.log.Printf("agent %s: %v", a.ID, err)
			}
		}
	}
	return nil
}

func (m *Manager) ensureRuntime(a store.Agent) (*runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runtimes[a.ID]; ok {
		return r, nil
	}
	r, err := m.newRuntime(a)
	if err != nil {
		return nil, err
	}
	m.runtimes[a.ID] = r
	m.loops.Add(1)
	go func() { defer m.loops.Done(); r.loop() }()
	return r, nil
}

// Wait blocks until every runtime has stopped after the manager's context
// ended.
func (m *Manager) Wait() { m.loops.Wait() }

func (m *Manager) runtime(idOrPrefix string) (*runtime, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runtimes[idOrPrefix]; ok {
		return r, nil
	}
	var found *runtime
	for id, r := range m.runtimes {
		if strings.HasPrefix(id, idOrPrefix) {
			if found != nil {
				return nil, fmt.Errorf("agent prefix %q is ambiguous", idOrPrefix)
			}
			found = r
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no live agent %q: %w", idOrPrefix, store.ErrNotFound)
	}
	return found, nil
}

func (m *Manager) isoLock(name string) *isoState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.isos[name]
	if !ok {
		st = &isoState{}
		m.isos[name] = st
	}
	return st
}

// isolationEnv is what an agent process starts with: the proxies'
// addresses and workspace tokens, then whatever the user set, which wins.
func (m *Manager) isolationEnv(name string) []string {
	env, _ := m.launchEnv(name)
	return env
}

// launchEnv is what an agent process in the isolation starts with: the
// proxies' environment, then the user's (which wins), and the proxies' MCP
// servers.
func (m *Manager) launchEnv(name string) ([]string, []proxy.MCPServer) {
	iso, err := m.store.Isolation(name)
	if err != nil {
		return nil, nil
	}
	ws := m.serviceWorkspace(name)
	m.mu.Lock()
	m.lastEnv[name] = ws.signature()
	m.mu.Unlock()
	keys := make([]string, 0, len(iso.Env))
	for k := range iso.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := ws.env
	for _, k := range keys {
		env = append(env, k+"="+iso.Env[k])
	}
	return env, ws.mcp
}

// ensureIsolation brings an isolation's container up and waits for its shim.
// A container that is gone is recreated from its volumes, so sessions and
// workspaces outlive it.
func (m *Manager) ensureIsolation(ctx context.Context, name string) (*shim.Client, error) {
	st := m.isoLock(name)
	st.mu.Lock()
	defer st.mu.Unlock()
	rec, err := m.store.Isolation(name)
	if err != nil {
		return nil, fmt.Errorf("isolation %s: %w", name, err)
	}
	client := &shim.Client{Dial: m.cli.ShimDialer(rec.Container)}
	if time.Since(st.healthy) < 5*time.Second {
		m.checkFence(ctx, rec)
		m.syncLocked(ctx, rec)
		m.ensureTunnel(name)
		return client, nil
	}
	ping := func() bool {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		v, err := client.Ping(pctx)
		if err == nil {
			m.mu.Lock()
			m.versions[name] = v
			m.mu.Unlock()
		}
		return err == nil
	}
	// up is the container answering: the fence is measured if due, the
	// proxies' files are brought up to date and their tunnel is kept open.
	up := func() (*shim.Client, error) {
		st.healthy = time.Now()
		m.checkFence(ctx, rec)
		m.syncLocked(ctx, rec)
		m.ensureTunnel(name)
		return client, nil
	}
	if ping() {
		return up()
	}
	m.markFenceStale(name)

	state, err := m.cli.State(ctx, rec.Container)
	if err != nil {
		return nil, err
	}
	switch state {
	case "running":
		// Up but the shim is not answering yet; wait below.
	case "stopped":
		m.log.Printf("isolation %s: starting stopped container", name)
		if err := m.cli.Start(ctx, rec.Container); err != nil {
			return nil, err
		}
	case "missing":
		m.log.Printf("isolation %s: container is gone; recreating it from its volumes", name)
		if !m.cli.ImageExists(ctx, rec.Image) {
			image, err := m.cli.EnsureImage(ctx, nil)
			if err != nil {
				return nil, err
			}
			rec.Image = image
			m.store.PutIsolation(rec)
		}
		if err := m.cli.Create(ctx, name, rec.Image, rec.CPUs, rec.Memory, rec.Network); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("isolation %s is %s", name, state)
	}

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ping() {
			return up()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("isolation %s: shim did not answer within 60s", name)
}

func (m *Manager) Isolations(ctx context.Context) ([]api.Isolation, error) {
	isos, err := m.store.Isolations()
	if err != nil {
		return nil, err
	}
	out := make([]api.Isolation, 0, len(isos))
	for _, iso := range isos {
		v := api.Isolation{Isolation: iso}
		for k := range iso.Env {
			v.EnvKeys = append(v.EnvKeys, k)
		}
		sort.Strings(v.EnvKeys)
		v.State, _ = m.cli.State(ctx, iso.Container)
		v.Proxies = m.tunnelStatus(iso.Name)
		_, v.FenceState = m.fenceOK(iso.Name)
		if iso.Fence == "off" {
			v.FenceState = "off"
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) Isolation(ctx context.Context, name string) (api.Isolation, error) {
	isos, err := m.Isolations(ctx)
	if err != nil {
		return api.Isolation{}, err
	}
	for _, iso := range isos {
		if iso.Name == name {
			return iso, nil
		}
	}
	return api.Isolation{}, fmt.Errorf("isolation %s: %w", name, store.ErrNotFound)
}

func (m *Manager) CreateIsolation(ctx context.Context, req api.CreateIsolation, progress io.Writer) (store.Isolation, error) {
	if !validIsolationName.MatchString(req.Name) {
		return store.Isolation{}, fmt.Errorf("isolation name %q: use lowercase letters, digits and dashes", req.Name)
	}
	if _, err := m.store.Isolation(req.Name); err == nil {
		return store.Isolation{}, fmt.Errorf("isolation %s already exists", req.Name)
	}
	container := isolation.ContainerName(req.Name)
	if state, err := m.cli.State(ctx, container); err != nil {
		return store.Isolation{}, err
	} else if state != "missing" {
		return store.Isolation{}, fmt.Errorf("a container named %s already exists and is not gero's to adopt", container)
	}
	fence := req.Fence
	switch fence {
	case "":
		fence = "on"
	case "on", "off":
	default:
		return store.Isolation{}, fmt.Errorf("fence must be on or off, not %q", fence)
	}
	image, err := m.cli.EnsureImage(ctx, progress)
	if err != nil {
		return store.Isolation{}, err
	}
	iso := store.Isolation{Name: req.Name, Container: container, Image: image, CPUs: req.CPUs, Memory: req.Memory,
		Network: isolation.NetworkName(req.Name), Fence: fence, Env: map[string]string{}, CreatedAt: time.Now().UTC()}
	if err := m.cli.Create(ctx, req.Name, image, req.CPUs, req.Memory, iso.Network); err != nil {
		return store.Isolation{}, err
	}
	if err := m.store.PutIsolation(iso); err != nil {
		return store.Isolation{}, err
	}
	if _, err := m.ensureIsolation(ctx, req.Name); err != nil {
		return iso, err
	}
	return iso, nil
}

// UpgradeIsolation moves an isolation onto the current base image. The
// container is replaced; its volumes, and with them every session and
// workspace, are kept. Running agents end ("lost" if they do not exit in
// time) and start again on their next message.
func (m *Manager) UpgradeIsolation(ctx context.Context, name string, progress io.Writer) (store.Isolation, error) {
	iso, err := m.store.Isolation(name)
	if err != nil {
		return iso, err
	}
	image, err := m.cli.EnsureImage(ctx, progress)
	if err != nil {
		return iso, err
	}
	st := m.isoLock(name)
	st.mu.Lock()
	err = m.cli.Remove(ctx, iso.Container)
	if err == nil {
		iso.Image = image
		// An isolation from before the fence leaves the shared default
		// network for one of its own.
		if iso.Network == "" {
			iso.Network = isolation.NetworkName(name)
		}
		err = m.store.PutIsolation(iso)
	}
	if err == nil {
		err = m.cli.Create(ctx, name, image, iso.CPUs, iso.Memory, iso.Network)
	}
	st.healthy = time.Time{}
	st.mu.Unlock()
	m.markFenceStale(name)
	if err != nil {
		return iso, err
	}
	_, err = m.ensureIsolation(ctx, name)
	return iso, err
}

func (m *Manager) DeleteIsolation(ctx context.Context, name string, purge bool) error {
	iso, err := m.store.Isolation(name)
	if err != nil {
		return err
	}
	if err := m.store.DeleteIsolation(name); err != nil {
		return err
	}
	if err := m.cli.Remove(ctx, iso.Container); err != nil {
		return err
	}
	if iso.Network != "" {
		if err := m.cli.NetworkRemove(ctx, iso.Network); err != nil {
			return err
		}
	}
	m.stopTunnel(name)
	m.mu.Lock()
	delete(m.fences, name)
	delete(m.synced, name)
	delete(m.versions, name)
	delete(m.lastEnv, name)
	m.mu.Unlock()
	if nets, err := m.fenceNetworks(ctx); err == nil && m.fence != nil {
		m.fence.Apply(ctx, nets)
	}
	if purge {
		for _, v := range []string{isolation.HomeVolume(name), isolation.StateVolume(name)} {
			if err := m.cli.VolumeRemove(ctx, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) SetEnv(name string, set api.SetEnv) error {
	iso, err := m.store.Isolation(name)
	if err != nil {
		return err
	}
	if iso.Env == nil {
		iso.Env = map[string]string{}
	}
	for k, v := range set {
		if v == nil {
			delete(iso.Env, k)
		} else {
			iso.Env[k] = *v
		}
	}
	if err := m.store.PutIsolation(iso); err != nil {
		return err
	}
	// Environment is read at process start; replace running agents at their
	// next idle moment so none keeps a stale credential.
	m.mu.Lock()
	var affected []*runtime
	for _, r := range m.runtimes {
		if r.agent.Isolation == name {
			affected = append(affected, r)
		}
	}
	m.mu.Unlock()
	for _, r := range affected {
		r.envChanged()
	}
	return nil
}

func newAgentID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func nameFrom(prompt string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	if r := []rune(first); len(r) > 48 {
		first = string(r[:48]) + "…"
	}
	return first
}

func (m *Manager) CreateAgent(ctx context.Context, req api.CreateAgent) (store.Agent, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return store.Agent{}, errors.New("an initial prompt is required")
	}
	if req.Isolation == "" {
		return store.Agent{}, errors.New("an isolation is required")
	}
	iso, err := m.store.Isolation(req.Isolation)
	if err != nil {
		return store.Agent{}, fmt.Errorf("isolation %s: %w", req.Isolation, err)
	}
	id := newAgentID()
	drv, err := driver.New(req.Kind, id, "", req.Options)
	if err != nil {
		return store.Agent{}, err
	}
	if _, err := m.ensureIsolation(ctx, iso.Name); err != nil {
		return store.Agent{}, err
	}
	if ok, why := m.fenceOK(iso.Name); !ok {
		return store.Agent{}, fmt.Errorf("isolation %s has no verified network fence: %s", iso.Name, why)
	}
	workdir := m.workRoot + "/" + id
	if req.Repo != "" {
		if _, err := m.cli.RunAsAgent(ctx, iso.Container, "git", "clone", "--", req.Repo, workdir); err != nil {
			return store.Agent{}, fmt.Errorf("clone: %w", err)
		}
	} else if _, err := m.cli.RunAsAgent(ctx, iso.Container, "mkdir", "-p", workdir); err != nil {
		return store.Agent{}, err
	}
	name := req.Name
	if name == "" {
		name = nameFrom(req.Prompt)
	}
	a := store.Agent{ID: id, Name: name, Kind: req.Kind, Isolation: iso.Name, Workdir: workdir, Repo: req.Repo,
		Options: req.Options, SessionID: drv.State().SessionID, CreatedAt: time.Now().UTC()}
	if err := m.store.CreateAgent(a); err != nil {
		return store.Agent{}, err
	}
	r, err := m.ensureRuntime(a)
	if err != nil {
		return a, err
	}
	return a, r.send(req.Prompt)
}

func (m *Manager) Agents() ([]api.Agent, error) {
	all, err := m.store.Agents()
	if err != nil {
		return nil, err
	}
	out := make([]api.Agent, 0, len(all))
	for _, a := range all {
		out = append(out, m.view(a))
	}
	sortAgents(out)
	return out, nil
}

func (m *Manager) view(a store.Agent) api.Agent {
	m.mu.Lock()
	r, ok := m.runtimes[a.ID]
	m.mu.Unlock()
	if !ok {
		return api.Agent{Agent: a}
	}
	live, st, problem := r.view()
	return api.Agent{Agent: live, State: st, Problem: problem}
}

func (m *Manager) Agent(idOrPrefix string) (api.Agent, error) {
	if r, err := m.runtime(idOrPrefix); err == nil {
		live, st, problem := r.view()
		return api.Agent{Agent: live, State: st, Problem: problem}, nil
	}
	all, err := m.store.Agents()
	if err != nil {
		return api.Agent{}, err
	}
	for _, a := range all {
		if a.ID == idOrPrefix || strings.HasPrefix(a.ID, idOrPrefix) {
			return m.view(a), nil
		}
	}
	return api.Agent{}, fmt.Errorf("agent %s: %w", idOrPrefix, store.ErrNotFound)
}

func (m *Manager) Send(id, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("empty message")
	}
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	return r.send(text)
}

func (m *Manager) Approve(id, approvalID string, d driver.Decision) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	return r.approve(approvalID, d)
}

func (m *Manager) Interrupt(id string) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	return r.interrupt()
}

func (m *Manager) SetOptions(id string, o driver.Options) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	if err := m.store.SetOptions(r.agent.ID, o); err != nil {
		return err
	}
	r.setOptions(o)
	return nil
}

// Archive stops the agent and keeps its history. Its session and workspace
// stay in the isolation's volumes.
func (m *Manager) Archive(ctx context.Context, id string) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := m.store.SetArchived(r.agent.ID, &now); err != nil {
		return err
	}
	r.stop(ctx)
	m.mu.Lock()
	delete(m.runtimes, r.agent.ID)
	m.mu.Unlock()
	return nil
}
