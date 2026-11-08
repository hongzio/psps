package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/shim"
	"github.com/hongzio/psps/internal/store"
)

// Admin mode: one agent, Claude Code or Codex, on the Mac itself, as the
// user, in no isolation. It is an agent like any other to the rest of
// psps (its log, its turns, its approvals), in the host's row in place of an
// isolation (store.HostIsolation), and its process belongs to a shim the
// daemon starts on the Mac (`psps host-shim`), in a session of its own, so
// it outlives the daemon as an isolation's shim does.

// adminRecentKey keeps the directories admin agents were started in, the
// latest first; adminRecentMax is how many.
const (
	adminRecentKey = "admin.recent"
	adminRecentMax = 8
)

// hostState is what the daemon knows of the Mac's side: the environment
// the host shim starts with, read once from the user's login shell.
type hostState struct {
	mu    sync.Mutex
	env   []string
	admin sync.Mutex // one change to admin mode at a time
}

func (m *Manager) hostSocket() string { return filepath.Join(m.hostRoot, "shim.sock") }

// ensureHost is ensureIsolation for the Mac: the host shim answering,
// started if it does not.
func (m *Manager) ensureHost(ctx context.Context) (*shim.Client, error) {
	if m.hostRoot == "" {
		return nil, errors.New("this daemon runs no agent on the Mac")
	}
	st := m.isoLock(store.HostIsolation)
	st.mu.Lock()
	defer st.mu.Unlock()
	client := &shim.Client{Dial: shim.UnixDialer(m.hostSocket())}
	ping := func() bool {
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err := client.Ping(pctx)
		return err == nil
	}
	if ping() {
		return client, nil
	}
	if err := m.startHostShim(ctx); err != nil {
		return nil, fmt.Errorf("starting the shim on the Mac: %w", err)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if ping() {
			return client, nil
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("the shim on the Mac does not answer; see %s", filepath.Join(m.hostRoot, "shim.log"))
}

// startHostShim starts `psps host-shim` in a session of its own, so
// neither the daemon's exit nor launchd's stopping it ends the agent.
func (m *Manager) startHostShim(ctx context.Context) error {
	if err := os.MkdirAll(m.hostRoot, 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(filepath.Join(m.hostRoot, "shim.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	home, _ := os.UserHomeDir()
	cmd := exec.Command(exe, "host-shim", "--root", m.hostRoot)
	cmd.Env = m.hostEnv(ctx)
	cmd.Dir = home
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.log.Printf("host: shim %d started", cmd.Process.Pid)
	// Reaped if it ends while this daemon runs; init reaps it otherwise.
	go cmd.Wait()
	return nil
}

// hostEnv is the environment agents on the Mac start with: the user's
// login shell's, so claude and codex are found where the user finds them.
// launchd starts the daemon with next to nothing.
func (m *Manager) hostEnv(ctx context.Context) []string {
	h := &m.host
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.env == nil {
		h.env = loginEnv(ctx, m.log.Printf)
	}
	return slices.Clone(h.env)
}

// envMark sets the environment apart from whatever the shell's rc files
// print on the way.
const envMark = "__PSPS_LOGIN_ENV__"

func loginEnv(ctx context.Context, logf func(string, ...any)) []string {
	home, _ := os.UserHomeDir()
	shell := loginShell()
	// A login shell, not an interactive one: with no terminal an rc file
	// can wait for one forever.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-l", "-c", "printf '\\n"+envMark+"\\n'; exec /usr/bin/env -0")
	cmd.Dir = home
	cmd.Stderr = io.Discard
	out, err := cmd.Output()
	var env []string
	if i := bytes.LastIndex(out, []byte(envMark+"\n")); err == nil && i >= 0 {
		for kv := range strings.SplitSeq(string(out[i+len(envMark)+1:]), "\x00") {
			k, _, ok := strings.Cut(kv, "=")
			if !ok || k == "" || slices.Contains([]string{"PWD", "OLDPWD", "SHLVL", "_", "CLAUDECODE"}, k) {
				continue
			}
			env = append(env, kv)
		}
	}
	if len(env) == 0 {
		logf("host: reading %s's login environment: %v; the daemon's own instead", shell, cmp.Or(err, errors.New("nothing read")))
		env = append(os.Environ(), "HOME="+home, "SHELL="+shell)
	}
	// Where claude and codex are put, whether or not the profile says so:
	// many put them on PATH in an rc file a login shell does not read.
	path := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	dirs := strings.Split(path, ":")
	for _, d := range []string{filepath.Join(home, ".local/bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"} {
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	env = append(env, "PATH="+strings.Trim(strings.Join(dirs, ":"), ":"))
	// The admin agent's psps is this daemon's, a development one too.
	if h := os.Getenv("PSPS_HOME"); h != "" {
		env = append(env, "PSPS_HOME="+h)
	}
	return env
}

// loginShell is the user's shell: $SHELL, else what the directory says.
func loginShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if u, err := user.Current(); err == nil {
		out, err := exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output()
		if _, s, ok := strings.Cut(strings.TrimSpace(string(out)), ":"); err == nil && ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return "/bin/zsh"
}

// hostCLI is the container runtime, but for the host's container, which
// is the Mac: what psps runs "as the agent" there (a workspace's branch,
// its files) it runs as the user, in the login environment.
type hostCLI struct {
	Containers
	m *Manager
}

var errOnHost = errors.New("not on the Mac: the admin agent has no isolation")

func (h hostCLI) RunAsAgent(ctx context.Context, container string, argv ...string) (string, error) {
	if container != store.HostContainer {
		return h.Containers.RunAsAgent(ctx, container, argv...)
	}
	if len(argv) == 0 {
		return "", errors.New("nothing to run")
	}
	home, _ := os.UserHomeDir()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = home, h.m.hostEnv(ctx)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, s)
	}
	return s, nil
}

func (h hostCLI) RunAsRoot(ctx context.Context, container string, argv ...string) (string, error) {
	if container == store.HostContainer {
		return "", errOnHost
	}
	return h.Containers.RunAsRoot(ctx, container, argv...)
}

func (h hostCLI) StartAsAgent(ctx context.Context, container string, env []string, argv ...string) (isolation.Proc, error) {
	if container == store.HostContainer {
		return isolation.Proc{}, errOnHost
	}
	return h.Containers.StartAsAgent(ctx, container, env, argv...)
}

func (h hostCLI) ShimDialer(container string) shim.Dialer {
	if container == store.HostContainer {
		return shim.UnixDialer(h.m.hostSocket())
	}
	return h.Containers.ShimDialer(container)
}

func (h hostCLI) State(ctx context.Context, container string) (string, error) {
	if container == store.HostContainer {
		return "running", nil
	}
	return h.Containers.State(ctx, container)
}

// adminAgents are the live agents on the Mac, the newest first: one, or
// none, unless one was restored beside another.
func (m *Manager) adminAgents() ([]store.Agent, error) {
	all, err := m.store.Agents()
	if err != nil {
		return nil, err
	}
	var out []store.Agent
	for _, a := range all {
		if a.ArchivedAt == nil && a.IsolationID == store.HostIsolation {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b store.Agent) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (m *Manager) adminRecent() []string {
	var out []string
	if v, ok, err := m.store.Meta(adminRecentKey); err == nil && ok {
		json.Unmarshal([]byte(v), &out)
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func (m *Manager) noteAdminDir(dir string) {
	recent := append([]string{dir}, slices.DeleteFunc(m.adminRecent(), func(d string) bool { return d == dir })...)
	if len(recent) > adminRecentMax {
		recent = recent[:adminRecentMax]
	}
	b, _ := json.Marshal(recent)
	if err := m.store.SetMeta(adminRecentKey, string(b)); err != nil {
		m.log.Printf("admin: recent directories: %v", err)
	}
}

// Admin is admin mode as it is now.
func (m *Manager) Admin() (api.Admin, error) {
	home, _ := os.UserHomeDir()
	v := api.Admin{Recent: m.adminRecent(), Home: home}
	as, err := m.adminAgents()
	if err != nil || len(as) == 0 {
		return v, err
	}
	a := as[0]
	v.On, v.Agent, v.Kind, v.Dir = true, a.ID, a.Kind, a.Workdir
	return v, nil
}

// hostDir is the directory typed, ~ for the user's home, made absolute
// and clean: one that is there.
func hostDir(typed string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := strings.TrimSpace(typed)
	switch {
	case d == "":
		return "", errors.New("a working directory is required")
	case d == "~":
		d = home
	case strings.HasPrefix(d, "~/"):
		d = filepath.Join(home, d[2:])
	}
	if !filepath.IsAbs(d) {
		return "", fmt.Errorf("%q is not absolute; start it with / or ~", typed)
	}
	d = filepath.Clean(d)
	st, err := os.Stat(d)
	if err != nil {
		return "", fmt.Errorf("no directory %s on the Mac", d)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", d)
	}
	return d, nil
}

// SetAdmin turns admin mode on with an agent of that kind in that
// directory, on the Mac. One already running there as asked is kept; any
// other is archived and a new one takes its place.
func (m *Manager) SetAdmin(ctx context.Context, req api.SetAdmin) (api.Admin, error) {
	if req.Kind != driver.Claude && req.Kind != driver.Codex {
		return api.Admin{}, badRequest(fmt.Errorf("unknown kind %q (claude or codex)", req.Kind))
	}
	dir, err := hostDir(req.Dir)
	if err != nil {
		return api.Admin{}, badRequest(err)
	}
	m.host.admin.Lock()
	defer m.host.admin.Unlock()
	live, err := m.adminAgents()
	if err != nil {
		return api.Admin{}, err
	}
	if len(live) == 1 && live[0].Kind == req.Kind && live[0].Workdir == dir {
		m.noteAdminDir(dir)
		return m.Admin()
	}
	if err := m.store.EnsureHostIsolation(); err != nil {
		return api.Admin{}, fmt.Errorf("the host's row: %w", err)
	}
	// The shim first: a Mac that cannot run it leaves admin mode as it was.
	if _, err := m.ensureHost(ctx); err != nil {
		return api.Admin{}, err
	}
	for _, a := range live {
		if err := m.Archive(ctx, a.ID); err != nil {
			return api.Admin{}, fmt.Errorf("archiving admin agent %s: %w", a.ID, err)
		}
	}
	id, err := m.newAgentID()
	if err != nil {
		return api.Admin{}, err
	}
	// Approvals as the user's own settings have them, asked here: it
	// runs as them, with nothing around it.
	opts := driver.Options{Approvals: driver.ApprovalsDefault}
	drv, err := driver.New(req.Kind, id, "", opts)
	if err != nil {
		return api.Admin{}, err
	}
	handle := "admin"
	if m.store.RefFree(handle) != nil {
		handle = forkHandle(handle)
	}
	a := store.Agent{ID: id, Handle: handle, Name: "Admin", Kind: req.Kind, IsolationID: store.HostIsolation, Workdir: dir,
		Options: opts, SessionID: drv.State().SessionID, CreatedAt: time.Now().UTC()}
	if err := m.store.CreateAgent(a); err != nil {
		return api.Admin{}, err
	}
	if a, err = m.store.Agent(id); err != nil {
		return api.Admin{}, err
	}
	m.noteAdminDir(dir)
	m.log.Printf("admin: agent %s (%s) in %s on the Mac", a.ID, a.Kind, dir)
	if _, err := m.ensureRuntime(a); err != nil {
		return api.Admin{}, err
	}
	m.pokeChanges()
	return m.Admin()
}

// StopAdmin turns admin mode off: its agent is stopped and archived, its
// conversation kept with the archived ones.
func (m *Manager) StopAdmin(ctx context.Context) (api.Admin, error) {
	m.host.admin.Lock()
	defer m.host.admin.Unlock()
	live, err := m.adminAgents()
	if err != nil {
		return api.Admin{}, err
	}
	for _, a := range live {
		if err := m.Archive(ctx, a.ID); err != nil {
			return api.Admin{}, fmt.Errorf("archiving admin agent %s: %w", a.ID, err)
		}
		m.log.Printf("admin: agent %s archived; admin mode is off", a.ID)
	}
	m.pokeChanges()
	return m.Admin()
}

// HostDirs completes a directory typed on the Mac, as Dirs does in an
// isolation.
func (m *Manager) HostDirs(typed string) (api.Dirs, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return api.Dirs{}, err
	}
	t := strings.TrimSpace(typed)
	if t == "" {
		t = "~/"
	}
	full := t
	switch {
	case t == "~":
		full = home + "/"
	case strings.HasPrefix(t, "~/"):
		full = home + t[1:]
	}
	if !filepath.IsAbs(full) {
		return api.Dirs{}, badRequest(fmt.Errorf("%q is not absolute; start it with / or ~", typed))
	}
	parent, prefix := filepath.Dir(full), filepath.Base(full)
	if strings.HasSuffix(full, "/") {
		parent, prefix = filepath.Clean(full), ""
	}
	if !strings.HasSuffix(parent, "/") {
		parent += "/"
	}
	ds := api.Dirs{Parent: parent, Dirs: []string{}, Home: home}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ds, nil
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) || strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		isDir := e.IsDir()
		if !isDir && e.Type()&os.ModeSymlink != 0 {
			st, err := os.Stat(filepath.Join(parent, name))
			isDir = err == nil && st.IsDir()
		}
		if !isDir {
			continue
		}
		ds.Dirs = append(ds.Dirs, parent+name+"/")
		if len(ds.Dirs) == dirsMax {
			break
		}
	}
	return ds, nil
}

// notOnHost refuses what an admin agent is not made by: a fork, an agent
// made in its place.
func notOnHost(a store.Agent, what string) error {
	if a.IsolationID == store.HostIsolation {
		return httpError{http.StatusBadRequest, fmt.Errorf("the admin agent is not %s; turn admin mode on in Settings", what)}
	}
	return nil
}
