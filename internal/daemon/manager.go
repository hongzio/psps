package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/pf"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/shim"
	"github.com/hongzio/psps/internal/shimproto"
	"github.com/hongzio/psps/internal/store"
	"golang.org/x/sys/unix"
)

var (
	validIsolationName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	validAgentHandle   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
)

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
	Create(ctx context.Context, container, image string, cpus int, memory string, swap bool, network string, mounts []isolation.Mount) error
	Mounts(ctx context.Context, container string) ([]isolation.Mount, error)
	RunAsAgent(ctx context.Context, container string, argv ...string) (string, error)
	RunAsRoot(ctx context.Context, container string, argv ...string) (string, error)
	ShimDialer(container string) shim.Dialer
	NetworkInspect(ctx context.Context, name string) (isolation.NetworkInfo, error)
	NetworkRemove(ctx context.Context, name string) error
	Probe(ctx context.Context, container string, targets []string) (isolation.ProbeResult, error)
	DialTunnel(ctx context.Context, container string) (isolation.TunnelConn, error)
	Sync(ctx context.Context, container string, req shimproto.SyncRequest) (shimproto.SyncResult, error)
	StartAsAgent(ctx context.Context, container string, env []string, argv ...string) (isolation.Proc, error)
	// An eval isolation's (isolation/eval.go).
	EvalReset(ctx context.Context, container string, wipe bool) error
	CopyToEval(ctx context.Context, from, dir, to string) error
	EvalStart(ctx context.Context, container, run string, r isolation.EvalRun) error
	EvalPoll(ctx context.Context, container, run string, max int) (isolation.EvalState, error)
	EvalStop(ctx context.Context, container, run string) error
	EvalForget(ctx context.Context, container, run string) error
	// A runner isolation's jobs (isolation/job.go).
	JobPrepare(ctx context.Context, container, id string, p isolation.JobPrep) (bool, error)
	JobCopy(ctx context.Context, from, dir, to, id string, lim isolation.CopyLimits) (isolation.CopyStats, error)
	JobStart(ctx context.Context, container, id string, meta []byte, argv []string) error
	JobPoll(ctx context.Context, container, id string) (isolation.JobState, error)
	JobCollect(ctx context.Context, container, id string, maxResult, maxLog int) ([]byte, string, error)
	JobSignal(ctx context.Context, container, id string, kill bool) error
	JobForget(ctx context.Context, container, id string) error
	// Kill ends a container container no longer answers for (isolation/kill.go).
	Kill(ctx context.Context, container string) ([]int, error)
}

// Fence is the root helper that keeps isolation networks off the Mac and its
// LAN; pf.Client is the real one.
type Fence interface {
	Apply(ctx context.Context, networks []pf.Network) (pf.Status, error)
	Installed() bool
}

// Manager reconciles the store's desired state with the world: isolations
// running with their shims answering, and one runtime per live agent.
type Manager struct {
	ctx   context.Context
	store *store.Store
	cli   Containers
	fence Fence
	// reconciled: Reconcile has given every live agent a runtime, as it
	// does after the API is up; until then a live agent may have none.
	reconciled atomic.Bool
	// hostAddrs and bridgeOf read the Mac's interfaces for the fence
	// measurement; tests replace them.
	hostAddrs func() ([]pf.HostAddr, error)
	bridgeOf  func(gateway4 string) string
	log       *log.Logger
	hub       *hub
	changes   *changes
	// workRoot is where agent workspaces go inside an isolation, home the
	// agent user's home there.
	workRoot, home string
	// webURL / webErr: what Run managed to serve the web UI on.
	webURL, webErr string
	// hostRoot is the host shim's directory, where the admin agent's log
	// is kept (host.go); "" runs no agent on the Mac.
	hostRoot string
	host     hostState

	// services are the credential proxies; creds is where their secrets
	// live. tunnelPort maps a proxy's port to the one opened inside the
	// isolation (the same, except in tests).
	services   *proxy.Registry
	creds      *creds.Store
	tunnelPort func(int) int

	// swap is held while /new puts a new session in an agent's place, so a
	// list of agents shows it before or after, never halfway.
	swap     sync.RWMutex
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
	swapped   map[string]string // the swap last turned on since the container started (swap.go)
	versions  map[string]string
	lastEnv   map[string]string
	health    map[string]isoHealth    // what was last seen inside, by isolation (health.go)
	ssm       *ssmBook                // strict SSM requests, from asking to outcome (awsssm.go)
	logins    map[string]pendingLogin // browser logins in progress, by id
	cliLogins map[string]*cliLogin    // agent CLI logins, by isolation/tool
	// mailBuckets are each isolation's budget for sending mail.
	mailBuckets map[string]*proxy.TokenBucket
	// fwd is the Mac's side of the forwarded ports.
	fwd forwards
	// cronMu keeps a cron being set from crossing it coming due;
	// cronWake tells RunCrons one may be due sooner.
	cronMu   sync.Mutex
	cronWake chan struct{}
	// teamMu keeps the decisions about one lead's run from crossing
	// another's; teamsDue are the teams RunTeams looks at next, teamWake
	// tells it there are some. evaluating are the teams whose eval runs,
	// and busy the agents each lead's line made that are at work.
	teamMu     sync.Mutex
	teamsDue   map[string]bool
	teamWake   chan struct{}
	evaluating map[string]bool
	// dueSince: since when a team's round has been over with no eval
	// (evalnow.go); dueTold, that the log said so.
	dueSince map[string]time.Time
	// watchTick is when watchClocks last counted the watches' time.
	watchTick time.Time
	dueTold   map[string]bool
	busy      map[string]map[string]bool
	// background are, by lead, its agents that count as at work for
	// tasks they run in the background after their turn (leads.go).
	background map[string]map[string]bool
	// controlMu keeps counting the agents made through the control service
	// and making one from crossing another such pair.
	controlMu sync.Mutex
	// jobsMu keeps a job's moves between queued, starting and cancelled
	// from crossing; jobWake tells RunJobs a runner may have room or a job
	// waits. jobWaiters are closed as their job ends, and jobHooks are what
	// psps asked a job for (a team's eval), told as it ends (jobs.go).
	jobsMu     sync.Mutex
	jobWake    chan struct{}
	jobWaiters map[string]chan struct{}
	jobHooks   map[string]func(store.Job)
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
	// waiting is set while one caller waits for the container's shim to
	// answer, without mu: the others wait for it, not in turn.
	waiting *shimWait
	// gen counts the containers the isolation has been given, so a wait
	// for one replaced meanwhile does not answer for the new one.
	gen int
}

// replaced marks the isolation's container as a new one. Callers hold mu.
func (st *isoState) replaced() {
	st.healthy = time.Time{}
	st.gen++
}

// shimWait is one wait for a container's shim; err is how it ended.
type shimWait struct {
	done chan struct{}
	err  error
}

type agentView = api.Agent

func NewManager(ctx context.Context, st *store.Store, cli Containers, fence Fence, logger *log.Logger) *Manager {
	m := &Manager{ctx: ctx, store: st, cli: cli, fence: fence, hostAddrs: pf.SystemAddrs, bridgeOf: pf.SystemBridge,
		log: logger, hub: newHub(), changes: newChanges(), workRoot: isolation.WorkRoot, home: isolation.AgentHome,
		runtimes: map[string]*runtime{}, isos: map[string]*isoState{}, fences: map[string]*fenceState{},
		tunnelPort: func(p int) int { return p }, instances: map[string]map[string]proxy.Instance{},
		tunnels: map[string]*tunnelState{}, synced: map[string]string{}, swapped: map[string]string{}, versions: map[string]string{}, lastEnv: map[string]string{}, health: map[string]isoHealth{}, ssm: newSSMBook(),
		mailBuckets: map[string]*proxy.TokenBucket{}, cronWake: make(chan struct{}, 1),
		teamsDue: map[string]bool{}, teamWake: make(chan struct{}, 1), evaluating: map[string]bool{}, dueSince: map[string]time.Time{}, dueTold: map[string]bool{}, busy: map[string]map[string]bool{}, background: map[string]map[string]bool{},
		jobWake: make(chan struct{}, 1), jobWaiters: map[string]chan struct{}{}, jobHooks: map[string]func(store.Job){}}
	m.cli = hostCLI{Containers: cli, m: m}
	m.loops.Add(1)
	go func() {
		defer m.loops.Done()
		<-ctx.Done()
		m.closeForwards()
	}()
	return m
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
			// An agent a lead made is at work until its runtime has read
			// its log, so no round ends on its lead's coming back first.
			if a.MadeBy != "" {
				m.setChildBusy(a.MadeBy, a.ID, true, false)
			}
			if _, err := m.ensureRuntime(a); err != nil {
				m.log.Printf("agent %s: %v", a.ID, err)
				if a.MadeBy != "" {
					m.setChildBusy(a.MadeBy, a.ID, false, false)
				}
			}
		}
	}
	m.reconciled.Store(true)
	m.resumeEvals()
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

// runtime is the live agent idOrPrefix names: by its own ID, its key, or
// the only key starting with it.
func (m *Manager) runtime(idOrPrefix string) (*runtime, error) {
	m.swap.RLock()
	defer m.swap.RUnlock()
	return m.liveRuntime(idOrPrefix)
}

func (m *Manager) liveRuntime(idOrPrefix string) (*runtime, error) {
	if a, err := m.store.AgentByHandle(idOrPrefix); err == nil {
		idOrPrefix = a.ID
	}
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

func (m *Manager) isoLock(id string) *isoState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.isos[id]
	if !ok {
		st = &isoState{}
		m.isos[id] = st
	}
	return st
}

// isolationNamed is the isolation people call name. Every request that
// names an isolation comes in through it; past it, an isolation is its ID.
func (m *Manager) isolationNamed(name string) (store.Isolation, error) {
	iso, err := m.store.IsolationNamed(name)
	if err != nil {
		return iso, fmt.Errorf("isolation %s: %w", name, err)
	}
	return iso, nil
}

// isoName is what the isolation with that ID is called now, for what is
// said to people; the ID itself once it is gone.
func (m *Manager) isoName(id string) string {
	if id == store.HostIsolation {
		return "Admin (the Mac)"
	}
	if iso, err := m.store.Isolation(id); err == nil {
		return iso.Name
	}
	return id
}

// isoNames is isoName for a list, sorted.
func (m *Manager) isoNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, m.isoName(id))
	}
	sort.Strings(out)
	return out
}

// isolationEnv is what an agent process starts with: the proxies'
// addresses and workspace tokens, then whatever the user set, which wins.
func (m *Manager) isolationEnv(id string) []string {
	env, _ := m.launchEnv(id)
	return env
}

// launchEnv is what an agent process in the isolation starts with: the
// proxies' environment, then its accounts', then the user's (which wins),
// and the proxies' MCP servers.
func (m *Manager) launchEnv(id string) ([]string, []proxy.MCPServer) {
	iso, err := m.store.Isolation(id)
	if err != nil {
		return nil, nil
	}
	ws := m.serviceWorkspace(id)
	m.mu.Lock()
	m.lastEnv[id] = ws.signature()
	m.mu.Unlock()
	keys := make([]string, 0, len(iso.Env))
	for k := range iso.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := append(append([]string(nil), ws.env...), m.accountEnv(id)...)
	for _, k := range keys {
		env = append(env, k+"="+iso.Env[k])
	}
	return env, ws.mcp
}

// ensureIsolation brings an isolation's container up and waits for its shim.
// A container that is gone is recreated from its volumes, so sessions and
// workspaces outlive it.
//
// Waiting for the shim, one caller waits and the rest share its answer;
// none holds the isolation's lock meanwhile, so what replaces or kills a
// container that does not answer is not kept waiting behind them.
func (m *Manager) ensureIsolation(ctx context.Context, id string) (*shim.Client, error) {
	if id == store.HostIsolation {
		return m.ensureHost(ctx)
	}
	st := m.isoLock(id)
	st.mu.Lock()
	for st.waiting != nil {
		w := st.waiting
		st.mu.Unlock()
		select {
		case <-w.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if w.err != nil {
			return nil, w.err
		}
		st.mu.Lock()
	}
	defer st.mu.Unlock()
	rec, err := m.store.Isolation(id)
	if err != nil {
		return nil, fmt.Errorf("isolation %s: %w", id, err)
	}
	client := &shim.Client{Dial: m.cli.ShimDialer(rec.Container)}
	// What is done in a container that answers is done under the lock, so
	// it is given a minute: a VM that stops answering mid-way does not
	// keep the lock, and with it what would replace or kill the VM.
	tend := func(swap bool) {
		tctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		m.checkFence(tctx, rec)
		m.syncLocked(tctx, rec)
		if swap {
			m.swapLocked(tctx, rec)
		}
	}
	if time.Since(st.healthy) < 5*time.Second {
		tend(false)
		m.ensureTunnel(id)
		return client, nil
	}
	ping := func() bool {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		v, err := client.Ping(pctx)
		if err == nil {
			m.mu.Lock()
			m.versions[id] = v
			m.mu.Unlock()
		}
		return err == nil
	}
	// up is the container answering: the fence is measured if due, the
	// proxies' files are brought up to date, the swap is turned on and
	// their tunnel is kept open.
	up := func() (*shim.Client, error) {
		st.healthy = time.Now()
		tend(true)
		m.ensureTunnel(id)
		return client, nil
	}
	// A VM just seen not to finish a command will not answer in time
	// either; it is said now rather than in a minute.
	m.mu.Lock()
	h := m.health[id]
	m.mu.Unlock()
	if h.label == api.HealthStuck && time.Since(h.at) < healthTTL {
		return nil, fmt.Errorf("isolation %s is %s (%s); psps iso kill %s ends it", rec.Name, h.label, h.about, rec.Name)
	}
	if ping() {
		return up()
	}
	m.markFenceStale(id)

	state, err := m.cli.State(ctx, rec.Container)
	if err != nil {
		return nil, err
	}
	switch state {
	case "running":
		// Up but the shim is not answering yet; wait below.
	case "stopped":
		m.log.Printf("isolation %s: starting stopped container", rec.Name)
		if err := m.cli.Start(ctx, rec.Container); err != nil {
			return nil, err
		}
	case "missing":
		m.log.Printf("isolation %s: container is gone; recreating it from its volumes", rec.Name)
		if !m.cli.ImageExists(ctx, rec.Image) {
			image, err := m.cli.EnsureImage(ctx, nil)
			if err != nil {
				return nil, err
			}
			rec.Image = image
			m.store.PutIsolation(rec)
		}
		mounts, missing := m.mountsFor(rec.ID)
		m.logMissing(rec.Name, missing)
		err := m.cli.Create(ctx, rec.Container, rec.Image, rec.CPUs, rec.Memory, rec.Swap != "", rec.Network, mounts)
		if err != nil && len(mounts) > 0 {
			// One that cannot start with its shares starts without them
			// rather than not at all; its skill says they are not there.
			m.log.Printf("isolation %s: does not start with its shared folders (%v); starting it without them", rec.Name, err)
			m.cli.Remove(ctx, rec.Container)
			err = m.cli.Create(ctx, rec.Container, rec.Image, rec.CPUs, rec.Memory, rec.Swap != "", rec.Network, nil)
		}
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("isolation %s is %s", rec.Name, state)
	}

	w := &shimWait{done: make(chan struct{})}
	st.waiting = w
	gen := st.gen
	st.mu.Unlock()
	answered := false
	deadline := time.Now().Add(60 * time.Second)
	for !answered && time.Now().Before(deadline) && ctx.Err() == nil {
		if answered = ping(); !answered {
			select {
			case <-ctx.Done():
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	st.mu.Lock()
	err = ctx.Err()
	if !answered && err == nil {
		err = fmt.Errorf("isolation %s: shim did not answer within 60s", rec.Name)
		// Unless the container it waited for is gone: the others then
		// wait for the new one themselves.
		if st.gen == gen {
			w.err = err
		}
	}
	st.waiting = nil
	close(w.done)
	if answered {
		return up()
	}
	return nil, err
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
		v.Proxies = m.tunnelStatus(iso.ID)
		v.Accounts = m.accountsOf(iso.ID)
		_, v.FenceState = m.fenceOK(iso.ID)
		if iso.Fence == "off" {
			v.FenceState = "off"
		}
		out = append(out, v)
	}
	m.checkHealth(ctx, out)
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

// newIsolationID is an ID no isolation has and no container is named after.
func (m *Manager) newIsolationID(ctx context.Context) (string, error) {
	for {
		id := newID()
		if _, err := m.store.Isolation(id); err == nil {
			continue
		}
		state, err := m.cli.State(ctx, isolation.ContainerName(id))
		if err != nil {
			return "", err
		}
		if state == "missing" {
			return id, nil
		}
	}
}

func (m *Manager) CreateIsolation(ctx context.Context, req api.CreateIsolation, progress io.Writer) (iso store.Isolation, err error) {
	if !validIsolationName.MatchString(req.Name) {
		return store.Isolation{}, badRequest(fmt.Errorf("isolation name %q: use lowercase letters, digits and dashes", req.Name))
	}
	if _, err := m.store.IsolationNamed(req.Name); err == nil {
		return store.Isolation{}, fmt.Errorf("isolation %s %w", req.Name, store.ErrExists)
	}
	if err := checkSize(req.CPUs, req.Memory); err != nil {
		return store.Isolation{}, err
	}
	swap, err := swapSize(req.Swap)
	if err != nil {
		return store.Isolation{}, err
	}
	fence := req.Fence
	switch fence {
	case "":
		fence = "on"
	case "on", "off":
	default:
		return store.Isolation{}, badRequest(fmt.Errorf("fence must be on or off, not %q", fence))
	}
	// Its agents would never start, and that is found minutes later, once
	// the image is built: refused now, before anything is made.
	if fence == "on" && (m.fence == nil || !m.fence.Installed()) {
		return store.Isolation{}, httpError{http.StatusConflict, fmt.Errorf("%w, or create the isolation with the fence off", pf.ErrNotInstalled)}
	}
	// Said in the log, since nobody may be waiting for the answer: it can
	// take minutes when the image is built first.
	m.log.Printf("isolation %s: creating", req.Name)
	defer func() {
		switch {
		case err != nil && iso.ID != "":
			m.log.Printf("isolation %s: created (%s), but: %v", req.Name, iso.Container, err)
		case err != nil:
			m.log.Printf("isolation %s: not created: %v", req.Name, err)
		default:
			m.log.Printf("isolation %s: created (%s)", req.Name, iso.Container)
		}
	}()
	id, err := m.newIsolationID(ctx)
	if err != nil {
		return store.Isolation{}, err
	}
	image, err := m.cli.EnsureImage(ctx, progress)
	if err != nil {
		return store.Isolation{}, err
	}
	iso = store.Isolation{ID: id, Name: req.Name, Container: isolation.ContainerName(id), Image: image, CPUs: req.CPUs,
		Memory: req.Memory, Swap: swap, Network: isolation.NetworkName(isolation.ContainerName(id)), Fence: fence, Env: map[string]string{}, CreatedAt: time.Now().UTC()}
	// A new isolation has no shares yet.
	if err := m.cli.Create(ctx, iso.Container, image, req.CPUs, req.Memory, iso.Swap != "", iso.Network, nil); err != nil {
		return store.Isolation{}, err
	}
	if err := m.store.PutIsolation(iso); err != nil {
		return store.Isolation{}, err
	}
	if _, err := m.ensureIsolation(ctx, id); err != nil {
		return iso, err
	}
	return iso, nil
}

// UpgradeIsolation moves an isolation onto the current base image. The
// container is replaced; its volumes, and with them every session and
// workspace, are kept. Running agents end ("lost" if they do not exit in
// time) and start again on their next message.
func (m *Manager) UpgradeIsolation(ctx context.Context, name string, progress io.Writer) (store.Isolation, error) {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return iso, err
	}
	image, err := m.cli.EnsureImage(ctx, progress)
	if err != nil {
		return iso, err
	}
	st := m.isoLock(iso.ID)
	st.mu.Lock()
	m.killIfStuck(ctx, iso)
	err = m.cli.Remove(ctx, iso.Container)
	if err == nil {
		iso.Image = image
		// An isolation from before the fence leaves the shared default
		// network for one of its own.
		if iso.Network == "" {
			iso.Network = isolation.NetworkName(iso.Container)
		}
		err = m.store.PutIsolation(iso)
	}
	if err == nil {
		mounts, missing := m.mountsFor(iso.ID)
		m.logMissing(iso.Name, missing)
		err = m.cli.Create(ctx, iso.Container, image, iso.CPUs, iso.Memory, iso.Swap != "", iso.Network, mounts)
	}
	st.replaced()
	st.mu.Unlock()
	m.markFenceStale(iso.ID)
	if err != nil {
		return iso, err
	}
	_, err = m.ensureIsolation(ctx, iso.ID)
	return iso, err
}

// StartIsolation brings an isolation up as an agent in it would: a stopped
// container started, a missing one made again from its volumes, and its
// shim answering, its fence measured and its proxies' tunnel open. An
// isolation with no agents stays down after a kill until this, or until
// something else needs it.
func (m *Manager) StartIsolation(ctx context.Context, name string) (store.Isolation, error) {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return iso, err
	}
	if _, err := m.ensureIsolation(ctx, iso.ID); err != nil {
		return iso, err
	}
	return m.store.Isolation(iso.ID)
}

// KillIsolation ends an isolation's container the hard way, past container
// itself: for a VM that has stopped answering, which holds up every exec,
// and container's stop with them. It takes no lock of the isolation's, as
// what holds one may be waiting on that VM. The agents in it are
// interrupted; the container starts again when something next needs it.
func (m *Manager) KillIsolation(ctx context.Context, name string) (api.Killed, error) {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return api.Killed{}, err
	}
	pids, err := m.cli.Kill(ctx, iso.Container)
	if len(pids) > 0 {
		m.markFenceStale(iso.ID)
		m.forgetHealth(iso.ID)
		m.log.Printf("isolation %s: killed (processes %v)", iso.Name, pids)
	}
	return api.Killed{Processes: append([]int{}, pids...)}, err
}

// checkSize refuses a size the VM cannot have, before anything is torn
// down for it. 0 CPUs and "" memory are container's defaults. container
// itself starts a VM with more memory than the Mac has (1P, measured): the
// VM then counts on memory that is not there.
func checkSize(cpus int, memory string) error {
	if n := goruntime.NumCPU(); cpus < 0 || cpus > n {
		return badRequest(fmt.Errorf("cpus %d: from 1 to %d (this Mac's), or 0 for container's default", cpus, n))
	}
	b, err := isolation.MemoryBytes(memory)
	if err != nil {
		return badRequest(err)
	}
	if host, err := unix.SysctlUint64("hw.memsize"); err == nil && b > host {
		return badRequest(fmt.Errorf("memory %s: more than this Mac's %dG", memory, host>>30))
	}
	return nil
}

// UpdateIsolation renames an isolation, resizes its VM and sets its swap, as
// req asks (see api.UpdateIsolation). Everything is checked before anything
// changes. A rename is the name alone: nothing else knows an isolation by
// it, so its agents, proxies and container carry on as they were.
func (m *Manager) UpdateIsolation(ctx context.Context, name string, req api.UpdateIsolation) (api.Isolation, error) {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return api.Isolation{}, err
	}
	cpus, memory, swap := iso.CPUs, iso.Memory, iso.Swap
	if req.CPUs != nil {
		cpus = *req.CPUs
	}
	if req.Memory != nil {
		memory = strings.TrimSpace(*req.Memory)
	}
	if err := checkSize(cpus, memory); err != nil {
		return api.Isolation{}, err
	}
	if req.Swap != nil {
		if swap, err = swapSize(*req.Swap); err != nil {
			return api.Isolation{}, err
		}
	}
	if to := strings.TrimSpace(req.Name); to != "" && to != iso.Name {
		if !validIsolationName.MatchString(to) {
			return api.Isolation{}, badRequest(fmt.Errorf("isolation name %q: use lowercase letters, digits and dashes", to))
		}
		if err := m.store.RenameIsolation(iso.ID, to); err != nil {
			return api.Isolation{}, err
		}
		m.log.Printf("isolation %s: renamed %s", iso.Name, to)
		name = to
	}
	// The first swap needs a container that may turn it on.
	if cpus != iso.CPUs || memory != iso.Memory || (iso.Swap == "" && swap != "") {
		if err := m.resizeIsolation(ctx, iso.ID, cpus, memory, swap); err != nil {
			return api.Isolation{}, err
		}
	} else if swap != iso.Swap {
		if err := m.setSwap(ctx, iso.ID, swap); err != nil {
			return api.Isolation{}, err
		}
	}
	return m.Isolation(ctx, name)
}

// resizeIsolation gives an isolation's VM new CPUs, memory and swap. container
// cannot resize a VM, so the container is replaced on the same image, the
// way an upgrade replaces it: the volumes, and every session and workspace,
// are kept; running agents end and start again on their next message. A
// container that will not start at the new size is put back at the old.
func (m *Manager) resizeIsolation(ctx context.Context, id string, cpus int, memory, swap string) error {
	iso, err := m.store.Isolation(id)
	if err != nil {
		return err
	}
	st := m.isoLock(id)
	st.mu.Lock()
	if !m.cli.ImageExists(ctx, iso.Image) {
		image, err := m.cli.EnsureImage(ctx, nil)
		if err != nil {
			st.mu.Unlock()
			return err
		}
		iso.Image = image
	}
	was := iso
	iso.CPUs, iso.Memory, iso.Swap = cpus, memory, swap
	mounts, _ := m.cli.Mounts(ctx, iso.Container)
	m.killIfStuck(ctx, iso)
	err = m.cli.Remove(ctx, iso.Container)
	if err == nil {
		if err = m.cli.Create(ctx, iso.Container, iso.Image, cpus, memory, swap != "", iso.Network, mounts); err == nil {
			err = m.store.PutIsolation(iso)
		} else {
			m.cli.Remove(ctx, iso.Container)
			if berr := m.cli.Create(ctx, was.Container, was.Image, was.CPUs, was.Memory, was.Swap != "", was.Network, mounts); berr != nil {
				err = fmt.Errorf("%w; putting it back at its old size failed too: %v", err, berr)
			} else {
				m.store.PutIsolation(was)
				err = fmt.Errorf("%w; it is back at its old size", err)
			}
		}
	}
	st.replaced()
	st.mu.Unlock()
	m.markFenceStale(id)
	if err != nil {
		return err
	}
	m.log.Printf("isolation %s: resized (cpus %d, memory %q, swap %q; 0 and \"\" are container's defaults)", iso.Name, cpus, memory, swap)
	_, err = m.ensureIsolation(ctx, id)
	return err
}

// DeleteIsolation removes an isolation and its container. Its volumes, and
// with them its sessions and workspaces, stay unless purge; nothing reuses
// them, since a new isolation gets an ID of its own.
func (m *Manager) DeleteIsolation(ctx context.Context, name string, purge bool) error {
	iso, err := m.isolationNamed(name)
	if err != nil {
		return err
	}
	if err := m.store.DeleteIsolation(iso.ID); err != nil {
		return fmt.Errorf("isolation %s: %w; archive and delete them first (`psps agent rm`)", name, err)
	}
	m.killIfStuck(ctx, iso)
	if err := m.cli.Remove(ctx, iso.Container); err != nil {
		return err
	}
	if iso.Network != "" {
		if err := m.cli.NetworkRemove(ctx, iso.Network); err != nil {
			return err
		}
	}
	m.stopTunnel(iso.ID)
	m.reforward()
	m.mu.Lock()
	delete(m.fences, iso.ID)
	delete(m.synced, iso.ID)
	delete(m.swapped, iso.ID)
	delete(m.versions, iso.ID)
	delete(m.lastEnv, iso.ID)
	m.mu.Unlock()
	if nets, err := m.fenceNetworks(ctx); err == nil && m.fence != nil {
		m.fence.Apply(ctx, nets)
	}
	if purge {
		for _, v := range []string{isolation.HomeVolume(iso.Container), isolation.StateVolume(iso.Container)} {
			if err := m.cli.VolumeRemove(ctx, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) SetEnv(name string, set api.SetEnv) error {
	iso, err := m.isolationNamed(name)
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
		if r.agent.IsolationID == iso.ID {
			affected = append(affected, r)
		}
	}
	m.mu.Unlock()
	for _, r := range affected {
		r.envChanged()
	}
	return nil
}

// newID is an agent's or an isolation's ID.
func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// nameFrom is a name from the first line of a message.
func nameFrom(text string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	first = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, first))
	if r := []rune(first); len(r) > 48 {
		first = string(r[:48]) + "…"
	}
	return first
}

// agentName is name as an agent's, trimmed: one line, so every list shows
// it whole.
func agentName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", badRequest(errors.New("an agent name is required"))
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", badRequest(errors.New("an agent name is one line of text"))
	}
	return name, nil
}

// agentHandle is handle as an agent's own ID, trimmed; "" is none.
func agentHandle(handle string) (string, error) {
	handle = strings.TrimSpace(handle)
	if handle != "" && !validAgentHandle.MatchString(handle) {
		return "", badRequest(fmt.Errorf("agent ID %q: use up to 32 lowercase letters, digits and dashes", handle))
	}
	return handle, nil
}

// newAgentID is a key no agent has, as its key or as its own ID.
func (m *Manager) newAgentID() (string, error) {
	for {
		id := newID()
		err := m.store.KeyFree(id)
		if !errors.Is(err, store.ErrExists) {
			return id, err
		}
	}
}

// CreateAgent makes an agent and sends it the prompt, if any. Without one
// it waits, with no process, for its first message, which names it unless
// it has a name by then. Given no ID of its own, it goes by an
// adjective-noun no other agent has.
func (m *Manager) CreateAgent(ctx context.Context, req api.CreateAgent) (store.Agent, error) {
	return m.createAgent(ctx, req, store.Agent{})
}

// createAgent is CreateAgent with what psps alone sets of an agent, from
// own: the lead that makes it, its turn limit, and the agent that makes it
// through the control service.
func (m *Manager) createAgent(ctx context.Context, req api.CreateAgent, own store.Agent) (store.Agent, error) {
	a, _, err := m.createAgentWatched(ctx, req, own, nil)
	return a, err
}

// createAgentWatched is createAgent, with a watch on its prompt if w is
// not nil (watches.go).
func (m *Manager) createAgentWatched(ctx context.Context, req api.CreateAgent, own store.Agent, w *store.Watch) (store.Agent, watched, error) {
	name := nameFrom(req.Prompt)
	if strings.TrimSpace(req.Name) != "" {
		var err error
		if name, err = agentName(req.Name); err != nil {
			return store.Agent{}, watched{}, err
		}
	}
	handle, err := agentHandle(req.Handle)
	if err != nil {
		return store.Agent{}, watched{}, err
	}
	if req.Isolation == "" {
		return store.Agent{}, watched{}, errors.New("an isolation is required")
	}
	if err := req.Options.Check(); err != nil {
		return store.Agent{}, watched{}, badRequest(err)
	}
	if strings.TrimSpace(req.Team) != "" {
		if _, err := teamName(req.Team); err != nil {
			return store.Agent{}, watched{}, err
		}
	}
	var ownDir string
	if strings.TrimSpace(req.Workdir) != "" {
		if ownDir, err = m.isoPath(req.Workdir); err != nil {
			return store.Agent{}, watched{}, badRequest(fmt.Errorf("working directory: %w", err))
		}
	}
	iso, err := m.isolationNamed(req.Isolation)
	if err != nil {
		return store.Agent{}, watched{}, err
	}
	// Taken is said before a clone, not after; storing it checks again.
	if err := m.store.RefFree(handle); err != nil {
		return store.Agent{}, watched{}, err
	}
	picked := handle == ""
	if picked {
		if handle, err = m.newAgentHandle(); err != nil {
			return store.Agent{}, watched{}, err
		}
	}
	id, err := m.newAgentID()
	if err != nil {
		return store.Agent{}, watched{}, err
	}
	drv, err := driver.New(req.Kind, id, "", req.Options)
	if err != nil {
		return store.Agent{}, watched{}, err
	}
	if _, err := m.ensureIsolation(ctx, iso.ID); err != nil {
		return store.Agent{}, watched{}, err
	}
	if ok, why := m.fenceOK(iso.ID); !ok {
		return store.Agent{}, watched{}, fmt.Errorf("isolation %s has no verified network fence: %s", iso.Name, why)
	}
	workdir := m.workRoot + "/" + id
	if ownDir != "" {
		workdir = ownDir
	}
	if req.Repo != "" {
		if _, err := m.cli.RunAsAgent(ctx, iso.Container, "git", "clone", "--", req.Repo, workdir); err != nil {
			return store.Agent{}, watched{}, fmt.Errorf("clone: %w", err)
		}
	} else if _, err := m.cli.RunAsAgent(ctx, iso.Container, "mkdir", "-p", workdir); err != nil {
		return store.Agent{}, watched{}, err
	}
	a := store.Agent{ID: id, Handle: handle, Name: name, Kind: req.Kind, IsolationID: iso.ID, Workdir: workdir, Repo: req.Repo,
		Options: req.Options, SessionID: drv.State().SessionID, CreatedAt: time.Now().UTC(), MadeBy: own.MadeBy, TurnMinutes: own.TurnMinutes, MadeVia: own.MadeVia}
	if a, err = m.storeNamed(a, picked); err != nil {
		return store.Agent{}, watched{}, err
	}
	// The agent is made by now; a team it could not join is no reason to
	// say it was not.
	if err := m.setTeam(a, req.Team); err != nil {
		m.log.Printf("agent %s: team %q: %v", a.ID, req.Team, err)
	}
	if err := m.placeNew(a); err != nil {
		m.log.Printf("agent %s: its place in the sidebar: %v", a.ID, err)
	}
	r, err := m.ensureRuntime(a)
	if err != nil {
		return a, watched{}, err
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return a, watched{}, nil
	}
	wd, err := r.sendWatched(req.Prompt, w)
	return a, wd, err
}

func (m *Manager) Agents() ([]api.Agent, error) {
	m.swap.RLock()
	defer m.swap.RUnlock()
	all, err := m.store.Agents()
	if err != nil {
		return nil, err
	}
	counts, err := m.store.InboxCounts()
	if err != nil {
		return nil, err
	}
	crons, err := m.store.CronCounts()
	if err != nil {
		return nil, err
	}
	todos, err := m.store.TodoCounts()
	if err != nil {
		return nil, err
	}
	order, err := m.store.Order()
	if err != nil {
		return nil, err
	}
	teams, err := m.store.LineTeams()
	if err != nil {
		return nil, err
	}
	out := make([]api.Agent, 0, len(all))
	for _, a := range all {
		out = append(out, m.view(a, counts[a.ID], crons[a.ID], todos[a.ID]))
	}
	if err := m.withTeams(out); err != nil {
		return nil, err
	}
	if err := m.withIcons(out); err != nil {
		return nil, err
	}
	if err := m.withRuns(out); err != nil {
		return nil, err
	}
	if err := m.withWatches(out); err != nil {
		return nil, err
	}
	sortAgents(out)
	orderAgents(out, order, teams)
	return out, nil
}

// view is a, live if it is, with inbox mails waiting, crons on and todos.
func (m *Manager) view(a store.Agent, inbox, crons, todos int) api.Agent {
	m.mu.Lock()
	r, ok := m.runtimes[a.ID]
	m.mu.Unlock()
	if !ok {
		return api.Agent{Agent: a, Isolation: m.isoName(a.IsolationID), Inbox: inbox, Crons: crons, Todos: todos, Host: a.IsolationID == store.HostIsolation}
	}
	live, st, problem := r.shown()
	return api.Agent{Agent: live, Isolation: m.isoName(live.IsolationID), State: st, Problem: problem, Inbox: inbox, Crons: crons, Todos: todos,
		Host: live.IsolationID == store.HostIsolation, Compacting: r.compacting()}
}

// viewOf is a as view has it, counting what it shows of a alone.
func (m *Manager) viewOf(a store.Agent) (api.Agent, error) {
	inbox, err := m.store.InboxCount(a.ID)
	if err != nil {
		return api.Agent{}, err
	}
	crons, err := m.store.CronCount(a.ID)
	if err != nil {
		return api.Agent{}, err
	}
	todos, err := m.store.TodoCount(a.ID)
	if err != nil {
		return api.Agent{}, err
	}
	v := []api.Agent{m.view(a, inbox, crons, todos)}
	if err := m.withTeams(v); err != nil {
		return v[0], err
	}
	if err := m.withIcons(v); err != nil {
		return v[0], err
	}
	if err := m.withWatches(v); err != nil {
		return v[0], err
	}
	return v[0], m.withRuns(v)
}

func (m *Manager) Agent(idOrPrefix string) (api.Agent, error) {
	m.swap.RLock()
	defer m.swap.RUnlock()
	var a store.Agent
	if r, err := m.liveRuntime(idOrPrefix); err == nil {
		a, _, _ = r.view()
	} else if a, err = m.storedAgent(idOrPrefix); err != nil {
		return api.Agent{}, err
	}
	return m.viewOf(a)
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

// Rewind takes the agent back to before one of its user messages, the
// event eventID, and sends text in its place.
func (m *Manager) Rewind(id string, eventID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return badRequest(errors.New("empty message"))
	}
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	e, err := m.store.Event(r.agent.ID, eventID)
	if errors.Is(err, store.ErrNotFound) {
		return badRequest(err)
	}
	if err != nil {
		return err
	}
	if e.Kind != event.KindUser {
		return badRequest(fmt.Errorf("event %d is not a message to the agent", eventID))
	}
	return r.rewind(e.Src, text)
}

func (m *Manager) Approve(id, approvalID string, d driver.Decision, answers map[string]string) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	// The daemon's own approvals (strict SSM) are not the agent CLI's.
	if strings.HasPrefix(approvalID, "ssm-") {
		return m.decideSSM(r.agent.ID, approvalID, d)
	}
	return r.approve(approvalID, d, answers)
}

func (m *Manager) Interrupt(id string) error {
	r, err := m.runtime(id)
	if err != nil {
		return err
	}
	return r.interrupt()
}

func (m *Manager) SetOptions(id string, o driver.Options) error {
	if err := o.Check(); err != nil {
		return badRequest(err)
	}
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

// UpdateAgent renames an agent, live or archived, or changes its own ID,
// which no other agent may have. A live one's runtime shows them from now
// on; nothing else about it changes.
func (m *Manager) UpdateAgent(idOrPrefix string, req api.UpdateAgent) (api.Agent, error) {
	var name, handle *string
	if req.Name != nil {
		n, err := agentName(*req.Name)
		if err != nil {
			return api.Agent{}, err
		}
		name = &n
	}
	if req.Handle != nil {
		h, err := agentHandle(*req.Handle)
		if err != nil {
			return api.Agent{}, err
		}
		handle = &h
	}
	if req.Team != nil && strings.TrimSpace(*req.Team) != "" {
		if _, err := teamName(*req.Team); err != nil {
			return api.Agent{}, err
		}
	}
	if req.Icon != nil {
		if err := checkIcon(*req.Icon); err != nil {
			return api.Agent{}, err
		}
	}
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return api.Agent{}, err
	}
	m.mu.Lock()
	r := m.runtimes[a.ID]
	m.mu.Unlock()
	if r != nil {
		err = r.relabel(name, handle)
	} else {
		err = m.store.RenameAgent(a.ID, name, handle)
	}
	if err != nil {
		return api.Agent{}, err
	}
	if name != nil {
		a.Name = *name
	}
	if handle != nil {
		a.Handle = *handle
	}
	if req.Parked != nil {
		// Only a live agent is idle or not; an archived one waits for
		// nothing.
		if r == nil {
			return api.Agent{}, httpError{http.StatusConflict, fmt.Errorf("agent %s is archived; only an idle agent is parked", a.ID)}
		}
		if err := r.park(*req.Parked); err != nil {
			return api.Agent{}, err
		}
		a.Parked = *req.Parked
	}
	if req.Team != nil {
		if err := m.setTeam(a, *req.Team); err != nil {
			return api.Agent{}, err
		}
	}
	if req.Icon != nil {
		if err := m.store.SetLineIcon(cmp.Or(a.Line, a.ID), *req.Icon); err != nil {
			return api.Agent{}, err
		}
	}
	return m.viewOf(a)
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
	m.watchesGone(r.agent, "was archived")
	// Its side agents are its conversation's: they go with it.
	m.deleteSides(r.agent.ID)
	return nil
}

// Renew starts the agent over: a new session in the same isolation and
// working directory, with its ID, options, crons and waiting mail, takes
// its place, and it is archived. The two are one line, shown together
// whatever they are called later.
func (m *Manager) Renew(ctx context.Context, id string) (store.Agent, error) {
	r, err := m.runtime(id)
	if err != nil {
		return store.Agent{}, err
	}
	old, _, _ := r.view()
	if old.SideOf != "" {
		return store.Agent{}, badRequest(errors.New("a side agent does not start over: delete it, and /btw again"))
	}
	key, err := m.newAgentID()
	if err != nil {
		return store.Agent{}, err
	}
	drv, err := driver.New(old.Kind, key, "", old.Options)
	if err != nil {
		return store.Agent{}, err
	}
	now := time.Now().UTC()
	a := store.Agent{ID: key, Kind: old.Kind, IsolationID: old.IsolationID, Workdir: old.Workdir, Repo: old.Repo,
		Options: old.Options, SessionID: drv.State().SessionID, CreatedAt: now, MadeBy: old.MadeBy, TurnMinutes: old.TurnMinutes, MadeVia: old.MadeVia}
	m.swap.Lock()
	defer m.swap.Unlock()
	if err := m.store.Renew(old.ID, a, now); err != nil {
		return store.Agent{}, err
	}
	if a, err = m.store.Agent(key); err != nil {
		return store.Agent{}, err
	}
	r.stop(ctx)
	m.mu.Lock()
	delete(m.runtimes, old.ID)
	m.mu.Unlock()
	m.log.Printf("agent %s: renewed as %s", old.ID, a.ID)
	m.watchesGone(old, fmt.Sprintf("started over as a new session (%s), which does not know of it,", a.ID))
	m.deleteSides(old.ID)
	_, err = m.ensureRuntime(a)
	return a, err
}

// Restore brings an archived agent back where it left off, its session
// resumed in its working directory. It goes by its line's ID: a live
// session of the line is archived in its place, handing over its crons and
// waiting mail, as /new hands them on.
func (m *Manager) Restore(ctx context.Context, id string) (api.Agent, error) {
	a, err := m.storedAgent(id)
	if err != nil {
		return api.Agent{}, err
	}
	if a.ArchivedAt == nil {
		return api.Agent{}, httpError{http.StatusConflict, fmt.Errorf("agent %s is not archived", a.ID)}
	}
	if a.SideOf != "" {
		return api.Agent{}, httpError{http.StatusConflict, fmt.Errorf("agent %s is a side agent being deleted", a.ID)}
	}
	if a.IsolationID == store.HostIsolation {
		live, err := m.adminAgents()
		if err != nil {
			return api.Agent{}, err
		}
		if slices.ContainsFunc(live, func(b store.Agent) bool { return b.Line != a.Line }) {
			return api.Agent{}, httpError{http.StatusConflict, errors.New("another admin agent runs on the Mac; turn admin mode off in Settings first")}
		}
	}
	fallback, err := m.newAgentHandle()
	if err != nil {
		return api.Agent{}, err
	}
	m.swap.Lock()
	defer m.swap.Unlock()
	// A turn the live session is in is not cut short for it.
	m.mu.Lock()
	rs := slices.Collect(maps.Values(m.runtimes))
	m.mu.Unlock()
	for _, r := range rs {
		if live, st, _ := r.view(); live.Line == a.Line && st.Busy {
			return api.Agent{}, httpError{http.StatusConflict, fmt.Errorf("%s, there now, is working: stop its turn or let it finish, then restore", cmp.Or(live.Handle, live.ID))}
		}
	}
	replaced, err := m.store.Restore(a.ID, fallback, time.Now().UTC())
	if err != nil {
		return api.Agent{}, err
	}
	if a, err = m.store.Agent(a.ID); err != nil {
		return api.Agent{}, err
	}
	m.mu.Lock()
	r := m.runtimes[replaced]
	delete(m.runtimes, replaced)
	m.mu.Unlock()
	if r != nil {
		r.stop(ctx)
		m.log.Printf("agent %s: archived as %s was restored", replaced, a.ID)
	}
	if replaced != "" {
		if old, err := m.store.Agent(replaced); err == nil {
			m.watchesGone(old, "was archived, as another session of its line was restored,")
		}
	}
	m.log.Printf("agent %s: restored", a.ID)
	if _, err := m.ensureRuntime(a); err != nil {
		return api.Agent{}, err
	}
	return m.viewOf(a)
}

// Fork makes a new agent from one, live or archived: in the same isolation
// and directory, with its kind and settings, and its session as a copy of
// the one it forks, so it knows what that one was told and did, and its
// conversation starts with a copy of that one's, for the person. It has no
// crons or mail; it is an agent of its own, on a line of its own, going by
// the ID of the one it forks with a random suffix.
func (m *Manager) Fork(ctx context.Context, id string) (api.Agent, error) {
	return m.fork(ctx, id, "")
}

// fork is Fork, via the agent that forks it through the control service
// if one does.
func (m *Manager) fork(ctx context.Context, id, via string) (api.Agent, error) {
	from, err := m.storedAgent(id)
	if err != nil {
		return api.Agent{}, err
	}
	a, err := m.forkAgent(ctx, from, via, "")
	if err != nil {
		return api.Agent{}, err
	}
	return m.viewOf(a)
}

// forkAgent makes the fork of from, its runtime started. A side agent's
// (side, the question it is forked to answer) is from's alone: on no line
// of the list nor in a team, named after its question, its conversation
// only a note of where it starts, not a copy of from's.
func (m *Manager) forkAgent(ctx context.Context, from store.Agent, via, side string) (store.Agent, error) {
	if err := notOnHost(from, "forked"); err != nil {
		return store.Agent{}, err
	}
	session := from.SessionID
	if r, err := m.runtime(from.ID); err == nil {
		_, st, _ := r.view()
		session = st.SessionID
	}
	if _, err := m.ensureIsolation(ctx, from.IsolationID); err != nil {
		return store.Agent{}, err
	}
	if ok, why := m.fenceOK(from.IsolationID); !ok {
		return store.Agent{}, fmt.Errorf("isolation %s has no verified network fence: %s", m.isoName(from.IsolationID), why)
	}
	key, err := m.newAgentID()
	if err != nil {
		return store.Agent{}, err
	}
	drv, err := driver.New(from.Kind, key, "", from.Options)
	if err != nil {
		return store.Agent{}, err
	}
	a := store.Agent{ID: key, Name: from.Name, Kind: from.Kind, IsolationID: from.IsolationID, Workdir: from.Workdir, Repo: from.Repo,
		Options: from.Options, SessionID: drv.State().SessionID, CreatedAt: time.Now().UTC(), ForkedFrom: from.ID, ForkOf: session, MadeVia: via}
	if side != "" {
		a.SideOf, a.Name = from.ID, sideName(side)
	} else if a.Name != "" {
		a.Name += " (fork)"
	}
	base := cmp.Or(from.Handle, from.Was, from.ID)
	for try := 0; ; try++ {
		a.Handle = forkHandle(base)
		err = m.store.CreateAgent(a)
		if !errors.Is(err, store.ErrExists) || try == 4 {
			break
		}
	}
	if err != nil {
		return store.Agent{}, err
	}
	if a, err = m.store.Agent(a.ID); err != nil {
		return store.Agent{}, err
	}
	if side != "" {
		m.addEvents(a.ID, event.Event{Kind: event.KindNotice, Text: fmt.Sprintf("A side agent of %s: it knows that one's conversation up to here, and works on its own.", cmp.Or(from.Handle, from.ID))})
		m.log.Printf("agent %s: forked from %s, on the side", a.ID, from.ID)
		if _, err := m.ensureRuntime(a); err != nil {
			return store.Agent{}, err
		}
		return a, nil
	}
	// Its conversation shows where it starts from, as its session does.
	if err := m.store.CopyEvents(from.ID, a.ID, fmt.Sprintf("Forked from %s: the conversation above is its, copied; this agent goes on from here, on its own.", cmp.Or(from.Handle, from.ID))); err != nil {
		m.log.Printf("agent %s: its conversation: %v", a.ID, err)
	}
	m.log.Printf("agent %s: forked from %s", a.ID, from.ID)
	m.joinFork(a, from)
	if _, err := m.ensureRuntime(a); err != nil {
		return store.Agent{}, err
	}
	return a, nil
}

// joinFork puts the fork a in the list, after from, and in from's team: its
// work is the same work's.
func (m *Manager) joinFork(a, from store.Agent) {
	if team, err := m.store.LineTeam(cmp.Or(from.Line, from.ID)); err != nil || team != "" {
		if err == nil {
			err = m.store.SetTeam(cmp.Or(a.Line, a.ID), team)
		}
		if err != nil {
			m.log.Printf("agent %s: its team: %v", a.ID, err)
		}
	}
	if err := m.placeFork(a, from); err != nil {
		m.log.Printf("agent %s: its place: %v", a.ID, err)
	}
}

// forkHandle is base, cut to leave room, with a random suffix.
func forkHandle(base string) string {
	const suffix = 4
	const digits = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, suffix)
	rand.Read(b)
	for i := range b {
		b[i] = digits[int(b[i])%len(digits)]
	}
	if max := 32 - 1 - suffix; len(base) > max {
		base = base[:max]
	}
	return strings.TrimRight(base, "-") + "-" + string(b)
}

// DeleteAgent forgets an archived agent: its history here and its log in
// the isolation. Its workspace and the CLI's transcript of its session stay
// in the isolation's volume unless purge, the way an isolation's volumes
// stay unless it is deleted with purge.
func (m *Manager) DeleteAgent(ctx context.Context, idOrPrefix string, purge bool) error {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return err
	}
	m.mu.Lock()
	_, live := m.runtimes[a.ID]
	m.mu.Unlock()
	// A side agent is deleted live: it is never kept archived.
	if a.SideOf != "" && (a.ArchivedAt == nil || live) {
		return m.deleteSide(a)
	}
	if a.ArchivedAt == nil || live {
		return httpError{http.StatusConflict, fmt.Errorf("agent %s is live; archive it first (`psps agent archive %s`)", a.ID, a.ID)}
	}
	iso, err := m.store.Isolation(a.IsolationID)
	if err != nil {
		return err
	}
	client, err := m.ensureIsolation(ctx, iso.ID)
	if err != nil {
		return err
	}
	// Its log goes only once nothing writes to it.
	infos, err := client.List(ctx)
	if err != nil {
		return err
	}
	for _, in := range infos {
		if in.ID == a.ID && in.Running {
			return httpError{http.StatusConflict, fmt.Errorf("agent %s's process is still exiting; try again in a moment", a.ID)}
		}
	}
	if purge {
		if err := m.purgeAgent(ctx, iso, a); err != nil {
			return err
		}
	}
	if iso.ID == store.HostIsolation {
		// The host shim keeps its log on the Mac, where the daemon is.
		if err := os.RemoveAll(filepath.Join(m.hostRoot, "agents", a.ID)); err != nil {
			return err
		}
	} else if _, err := m.cli.Sync(ctx, iso.Container, shimproto.SyncRequest{Remove: []string{isolation.AgentStateDir(iso.Container, a.ID)}}); err != nil {
		return err
	}
	if err := m.store.DeleteAgent(a.ID); err != nil {
		return err
	}
	// Its conversation is gone from the file too, not left in free pages.
	if err := m.store.Vacuum(); err != nil {
		m.log.Printf("store: vacuum: %v", err)
	}
	m.log.Printf("agent %s: deleted (purge %v)", a.ID, purge)
	return nil
}

// storedAgent is the agent, live or archived, whose own ID or key is
// idOrPrefix, or whose key is the only one starting with it.
func (m *Manager) storedAgent(idOrPrefix string) (store.Agent, error) {
	all, err := m.store.Agents()
	if err != nil {
		return store.Agent{}, err
	}
	var found []store.Agent
	for _, a := range all {
		// No agent's own ID is another's key: an exact match is the one.
		if a.ID == idOrPrefix || (a.Handle != "" && a.Handle == idOrPrefix) {
			return a, nil
		}
		if idOrPrefix != "" && strings.HasPrefix(a.ID, idOrPrefix) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 0:
		return store.Agent{}, fmt.Errorf("agent %s: %w", idOrPrefix, store.ErrNotFound)
	case 1:
		return found[0], nil
	}
	return store.Agent{}, badRequest(fmt.Errorf("agent prefix %q is ambiguous", idOrPrefix))
}

var (
	validSession = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z-]*$`)
	// Claude Code names a project after its working directory, every
	// character but a letter or digit made a dash.
	projectName = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// purgeAgent removes what the agent left in the isolation's home: its
// workspace and its CLI's transcript of the session. Every path is derived
// from the agent's own ID, workspace and session, nothing else.
//
// A workspace another agent works in too, a session of its line before or
// after it, stays, and so does what its CLI keeps per directory: only this
// session's own transcript goes.
func (m *Manager) purgeAgent(ctx context.Context, iso store.Isolation, a store.Agent) error {
	all, err := m.store.Agents()
	if err != nil {
		return err
	}
	// One psps made: the agent's, or the one it shares with the agents it
	// took over from or, a fork, was forked from.
	made := false
	for b, seen := a, map[string]bool{}; !seen[b.ID]; {
		seen[b.ID] = true
		made = made || a.Workdir == m.workRoot+"/"+b.ID || a.Workdir == m.workRoot+"/"+b.Line
		i := slices.IndexFunc(all, func(c store.Agent) bool { return c.ID == b.ForkedFrom })
		if b.ForkedFrom == "" || i < 0 {
			break
		}
		b = all[i]
	}
	if !made {
		return badRequest(fmt.Errorf("agent %s's workspace %s is not where psps makes one; delete it without purge and remove that yourself", a.ID, a.Workdir))
	}
	shared := slices.ContainsFunc(all, func(b store.Agent) bool {
		return b.ID != a.ID && b.IsolationID == a.IsolationID && b.Workdir == a.Workdir
	})
	var steps [][]string
	if !shared {
		steps = append(steps, []string{"rm", "-rf", "--", a.Workdir})
	}
	sid := a.SessionID
	if !validSession.MatchString(sid) {
		sid = ""
	}
	switch a.Kind {
	case driver.Claude:
		project := m.home + "/.claude/projects/" + projectName.ReplaceAllString(a.Workdir, "-")
		switch {
		case !shared:
			steps = append(steps, []string{"rm", "-rf", "--", project})
		case sid != "":
			steps = append(steps, []string{"rm", "-rf", "--", project + "/" + sid + ".jsonl", project + "/" + sid})
		}
		if sid != "" {
			steps = append(steps, []string{"rm", "-rf", "--", m.home + "/.claude/session-env/" + sid})
		}
	case driver.Codex:
		sessions := m.home + "/.codex/sessions"
		if _, err := m.cli.RunAsAgent(ctx, iso.Container, "test", "-d", sessions); sid != "" && err == nil {
			steps = append(steps, []string{"find", sessions, "-name", "rollout-*-" + sid + ".jsonl", "-delete"})
		}
	}
	for _, argv := range steps {
		if _, err := m.cli.RunAsAgent(ctx, iso.Container, argv...); err != nil {
			return err
		}
	}
	return nil
}
