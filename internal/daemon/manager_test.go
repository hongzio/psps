package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/pf"
	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
	"github.com/hongzio/gero/internal/tunnel"
)

// The test binary doubles as a fake `codex app-server`.
func TestMain(m *testing.M) {
	if os.Getenv("GERO_FAKE_CODEX") == "1" {
		fakeCodex()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeCodex answers just enough of the app-server protocol: one thread whose
// id survives restarts, and turns that echo. A turn whose text is "hang"
// starts and never finishes.
func fakeCodex() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	reply := func(id any, result any) { enc.Encode(map[string]any{"id": id, "result": result}) }
	notify := func(method string, params any) { enc.Encode(map[string]any{"method": method, "params": params}) }
	const thread = "th-fake"
	turns := 0
	for in.Scan() {
		var m struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.Unmarshal(in.Bytes(), &m)
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{})
		case "account/read":
			reply(m.ID, map[string]any{"account": map[string]any{"type": "apiKey"}, "requiresOpenaiAuth": true})
		case "thread/start", "thread/resume":
			reply(m.ID, map[string]any{"thread": map[string]any{"id": thread}})
		case "turn/start":
			turns++
			tid := fmt.Sprintf("turn-%d", turns)
			reply(m.ID, map[string]any{"turn": map[string]any{"id": tid, "status": "inProgress"}})
			notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": tid}})
			text := m.Params["input"].([]any)[0].(map[string]any)["text"].(string)
			if text == "hang" {
				continue
			}
			if text == "env" {
				text = "env=" + os.Getenv("GERO_TEST_VAR")
			}
			notify("item/agentMessage/delta", map[string]any{"itemId": "m-" + tid, "delta": "echo", "turnId": tid})
			notify("item/completed", map[string]any{"turnId": tid,
				"item": map[string]any{"type": "agentMessage", "id": "m-" + tid, "text": "echo: " + text}})
			notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": tid, "status": "completed"}})
		}
	}
}

// fakeContainers runs each "container" as an in-process shim on a unix
// socket. Its root directory plays the part of the state volume.
type fakeContainers struct {
	t    *testing.T
	base string
	mu   sync.Mutex
	ctrs map[string]*fakeCtr
	// held containers cannot be started: the host is still down.
	held map[string]bool
	leak bool // the fence lets the probe through
	// probed is what each fence measurement tried, in order.
	probed [][]string
	// unsent makes link-local targets fail inside the isolation, as they do
	// while its own address is still tentative after boot.
	unsent bool
	// mounts is the isolation's /proc/mounts.
	mounts string
	// syncs are the workspace sync requests the daemon sent; tunnels the
	// isolation ends of open tunnels, which a reboot cuts.
	syncs   []shimproto.SyncRequest
	tunnels map[string][]pipeRW
	// bin goes first on PATH for StartAsAgent: the agent CLIs' fakes.
	bin string
}

type fakeCtr struct {
	srv  *shim.Server
	l    net.Listener
	sock string
}

func (f *fakeContainers) SystemRunning(context.Context) bool { return true }
func (f *fakeContainers) SystemStart(context.Context) error  { return nil }
func (f *fakeContainers) ImageExists(context.Context, string) bool {
	return true
}
func (f *fakeContainers) EnsureImage(context.Context, io.Writer) (string, error) {
	return "fake:1", nil
}
func (f *fakeContainers) VolumeRemove(context.Context, string) error { return nil }

func (f *fakeContainers) State(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ctrs[name]; ok {
		return "running", nil
	}
	if f.held[name] {
		return "unavailable", nil
	}
	if _, err := os.Stat(filepath.Join(f.base, name)); err == nil {
		return "stopped", nil
	}
	return "missing", nil
}

func (f *fakeContainers) boot(name string) error {
	root := filepath.Join(f.base, name)
	os.MkdirAll(root, 0o700)
	srv := &shim.Server{Root: root, BaseEnv: []string{"PATH=" + os.Getenv("PATH")}}
	if err := srv.Open(); err != nil {
		return err
	}
	sock := filepath.Join(root, "shim.sock")
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	go srv.Serve(l)
	f.mu.Lock()
	f.ctrs[name] = &fakeCtr{srv: srv, l: l, sock: sock}
	f.mu.Unlock()
	return nil
}

func (f *fakeContainers) Start(_ context.Context, name string) error { return f.boot(name) }
func (f *fakeContainers) Create(_ context.Context, iso, _ string, _ int, _, _ string) error {
	return f.boot("gero-" + iso)
}

func (f *fakeContainers) NetworkInspect(context.Context, string) (isolation.NetworkInfo, error) {
	return isolation.NetworkInfo{Subnet4: "192.168.99.0/24", Gateway4: "192.168.99.1", Subnet6: "fd00:99::/64"}, nil
}
func (f *fakeContainers) NetworkRemove(context.Context, string) error { return nil }

// Probe reports every target blocked unless the test opened a hole.
func (f *fakeContainers) Probe(_ context.Context, _ string, targets []string) (isolation.ProbeResult, error) {
	f.mu.Lock()
	leak := f.leak
	f.probed = append(f.probed, append([]string(nil), targets...))
	f.mu.Unlock()
	r := isolation.ProbeResult{Resolve: "ok", TCP: map[string]string{}}
	f.mu.Lock()
	unsent := f.unsent
	f.mu.Unlock()
	for _, t := range targets {
		if strings.HasPrefix(t, "127.0.0.1:") {
			// The isolation's own loopback, where the tunnel listens:
			// the test's isolation end runs on this Mac's.
			r.TCP[t] = "open"
			if c, err := net.DialTimeout("tcp", t, time.Second); err != nil {
				r.TCP[t] = "refused"
			} else {
				c.Close()
			}
			continue
		}
		r.TCP[t] = "blocked"
		if leak {
			r.TCP[t] = "open"
		}
		if unsent && strings.Contains(t, "%eth0") {
			r.TCP[t] = "error: dial tcp " + t + ": connect: cannot assign requested address"
		}
	}
	return r, nil
}

// fakeFence is the root helper: it either is installed or is not. Its
// loaded rules cover the test bridge's IPv6 once lag applies have passed,
// the way the real helper's do once it sees the bridge.
type fakeFence struct {
	mu        sync.Mutex
	installed bool
	applied   [][]pf.Network
	lag       int
}

const testBridge = "bridge9"

func (f *fakeFence) Apply(_ context.Context, nets []pf.Network) (pf.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.installed {
		return pf.Status{}, pf.ErrNotInstalled
	}
	f.applied = append(f.applied, nets)
	st := pf.Status{OK: true, Enabled: true, Networks: nets,
		Loaded: []string{"block drop quick inet from 192.168.99.0/24 to <gero_private4>"}}
	if f.lag > 0 {
		f.lag--
		return st, nil
	}
	for _, d := range []string{"::1", "fc00::/7", "fe80::/10", "ff00::/8"} {
		st.Loaded = append(st.Loaded, "block drop in quick on "+testBridge+" inet6 from any to "+d)
	}
	return st, nil
}

func (f *fakeFence) applies() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.applied) }

func (f *fakeFence) set(installed bool) { f.mu.Lock(); f.installed = installed; f.mu.Unlock() }

func (f *fakeContainers) Remove(_ context.Context, name string) error {
	f.reboot(name, false)
	return nil
}

// RunAsAgent runs argv on this Mac, which stands in for the isolation:
// curl to 127.0.0.1 reaches the test tunnel's isolation end.
func (f *fakeContainers) RunAsAgent(ctx context.Context, _ string, argv ...string) (string, error) {
	if len(argv) == 2 && argv[0] == "cat" && argv[1] == "/proc/mounts" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.mounts == "" {
			return "/dev/vdb / ext4 rw,relatime 0 0\n/dev/vdc /home/agent ext4 rw,relatime 0 0\nproc /proc proc rw 0 0", nil
		}
		return f.mounts, nil
	}
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// StartAsAgent runs argv on this Mac with the fakes in bin first on PATH,
// and HOME the container's own directory, as the agent's home.
func (f *fakeContainers) StartAsAgent(ctx context.Context, name string, env []string, argv ...string) (isolation.Proc, error) {
	f.mu.Lock()
	bin := f.bin
	f.mu.Unlock()
	// Through sh, so argv[0] is looked up on that PATH.
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", `exec "$@"`, "sh"}, argv...)...)
	// Its own process group, signalled whole: in the isolation timeout(1)
	// passes the SIGTERM on to everything the login started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	home := filepath.Join(f.base, name, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return isolation.Proc{}, err
	}
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+home)
	cmd.Env = append(cmd.Env, env...)
	return isolation.StartProc(cmd)
}

func (f *fakeContainers) ShimDialer(name string) shim.Dialer {
	return shim.UnixDialer(filepath.Join(f.base, name, "shim.sock"))
}

// DialTunnel runs the isolation end of the tunnel in this process, over a
// pair of OS pipes, as `container exec -i` would carry it.
func (f *fakeContainers) DialTunnel(_ context.Context, name string) (isolation.TunnelConn, error) {
	f.mu.Lock()
	_, up := f.ctrs[name]
	f.mu.Unlock()
	if !up {
		return nil, fmt.Errorf("container %s is not running", name)
	}
	ir, hw, _ := os.Pipe()
	hr, iw, _ := os.Pipe()
	f.mu.Lock()
	if f.tunnels == nil {
		f.tunnels = map[string][]pipeRW{}
	}
	f.tunnels[name] = append(f.tunnels[name], pipeRW{ir, iw})
	f.mu.Unlock()
	go func() {
		tunnel.Serve(pipeRW{ir, iw}, func(host string, _ int) bool { return host == "127.0.0.1" }, func(string, ...any) {})
		ir.Close()
		iw.Close()
	}()
	return pipeRW{hr, hw}, nil
}

type pipeRW struct{ r, w *os.File }

func (p pipeRW) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p pipeRW) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p pipeRW) Close() error                { p.w.Close(); return p.r.Close() }
func (p pipeRW) Stderr() string              { return "" }

func (f *fakeContainers) Sync(_ context.Context, name string, req shimproto.SyncRequest) (shimproto.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, req)
	return shimproto.SyncResult{}, nil
}

func (f *fakeContainers) lastSync() (shimproto.SyncRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.syncs) == 0 {
		return shimproto.SyncRequest{}, 0
	}
	return f.syncs[len(f.syncs)-1], len(f.syncs)
}

// reboot stages a machine going down: the shim stops logging, every agent
// process dies with no exit recorded, and (if boot) a new shim comes up on
// the same durable root.
func (f *fakeContainers) reboot(name string, boot bool) {
	f.mu.Lock()
	c := f.ctrs[name]
	delete(f.ctrs, name)
	cut := f.tunnels[name]
	delete(f.tunnels, name)
	f.mu.Unlock()
	for _, p := range cut {
		p.Close()
	}
	if c != nil {
		infos, _ := (&shim.Client{Dial: shim.UnixDialer(c.sock)}).List(context.Background())
		c.srv.Abandon()
		for _, in := range infos {
			if in.Running {
				syscall.Kill(-in.PID, syscall.SIGKILL)
			}
		}
		c.l.Close()
		time.Sleep(200 * time.Millisecond)
	}
	if boot {
		if err := f.boot(name); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fakeContainers) shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.ctrs {
		c.l.Close()
		c.srv.Shutdown(2 * time.Second)
	}
}

type testEnv struct {
	t        *testing.T
	st       *store.Store
	fc       *fakeContainers
	fence    *fakeFence
	workRoot string

	mu       sync.Mutex
	noBridge bool // the isolation's bridge is not up (yet)
}

func (e *testEnv) bridge() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.noBridge {
		return ""
	}
	return testBridge
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	// The test binary stands in for codex: put a `codex` on PATH that runs it.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nGERO_FAKE_CODEX=1 exec " + exe + " \"$@\"\n"
	os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	// Short base: unix socket paths are capped at 104 bytes on macOS.
	base, err := os.MkdirTemp("", "gd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	st, err := store.Open(filepath.Join(base, "gero.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fc := &fakeContainers{t: t, base: base, ctrs: map[string]*fakeCtr{}, held: map[string]bool{}}
	t.Cleanup(fc.shutdown)
	if err := st.PutIsolation(store.Isolation{Name: "dev", Container: "gero-dev", Image: "fake:1", Network: "gero-dev-net",
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := fc.boot("gero-dev"); err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, st: st, fc: fc, fence: &fakeFence{installed: true}, workRoot: filepath.Join(base, "work")}
}

// daemon starts a manager the way daemon.Run does; stop ends it like a
// daemon exiting, leaving agents and shims alone.
func (e *testEnv) daemon() (m *Manager, stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	m = NewManager(ctx, e.st, e.fc, e.fence, log.New(io.Discard, "", 0))
	// The Mac as the fence measurement sees it: the isolation's bridge with
	// its link-local address, and a LAN address.
	m.bridgeOf = func(string) string { return e.bridge() }
	m.hostAddrs = func() ([]pf.HostAddr, error) {
		return []pf.HostAddr{
			{Iface: "en0", Prefix: netip.MustParsePrefix("192.168.1.5/24")},
			{Iface: testBridge, Prefix: netip.MustParsePrefix("192.168.99.1/24")},
			{Iface: testBridge, Prefix: netip.MustParsePrefix("fe80::1/64")},
		}, nil
	}
	m.workRoot = e.workRoot
	if err := m.Reconcile(); err != nil {
		e.t.Fatal(err)
	}
	return m, func() { cancel(); m.Wait() }
}

func (e *testEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

func (e *testEnv) events(id string) []event.Event {
	evs, err := e.st.Events(id, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func (e *testEnv) hasAssistant(id, text string) bool {
	for _, ev := range e.events(id) {
		if ev.Kind == event.KindAssistant && ev.Text == text {
			return true
		}
	}
	return false
}

func status(m *Manager, id string) event.Status {
	a, err := m.Agent(id)
	if err != nil {
		return ""
	}
	return a.State.Status
}

func TestAgentSurvivesDaemonRestartAndReboot(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// A first turn, end to end: create, handshake, turn, echo.
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") && status(m1, a.ID) == event.StatusIdle })
	stored, _ := e.st.Agent(a.ID)
	if stored.SessionID != "th-fake" {
		t.Fatalf("thread id not stored: %q", stored.SessionID)
	}
	before := len(e.events(a.ID))

	// The daemon restarts; the agent process does not. The new daemon
	// replays the log and must not store anything twice.
	stop1()
	m2, stop2 := e.daemon()
	e.waitFor("reattach", func() bool {
		v, _ := m2.Agent(a.ID)
		return v.State.Alive && v.State.Status == event.StatusIdle
	})
	if after := len(e.events(a.ID)); after != before {
		t.Fatalf("replay after a daemon restart stored %d events twice", after-before)
	}
	if err := m2.Send(a.ID, "again"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("answer after restart", func() bool { return e.hasAssistant(a.ID, "echo: again") })

	// The machine goes down mid-turn: shim and agent die together, nothing
	// gets to record an exit.
	m2.Send(a.ID, "hang")
	e.waitFor("hanging turn", func() bool { return status(m2, a.ID) == event.StatusWorking })
	stop2()
	e.fc.reboot("gero-dev", true)

	m3, stop3 := e.daemon()
	defer stop3()
	e.waitFor("interrupted after reboot", func() bool { return status(m3, a.ID) == event.StatusInterrupted })
	v, _ := m3.Agent(a.ID)
	if v.State.Alive {
		t.Fatal("an agent was started after reboot before the user asked")
	}

	// The user carries on: a new process resumes the same thread.
	if err := m3.Send(a.ID, "carry on"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("answer after reboot", func() bool {
		return e.hasAssistant(a.ID, "echo: carry on") && status(m3, a.ID) == event.StatusIdle
	})
	var resumed, lost bool
	for _, ev := range e.events(a.ID) {
		resumed = resumed || (ev.Kind == event.KindNotice && ev.Text == "thread/resume th-fake")
		lost = lost || (ev.Kind == event.KindTurnEnd && ev.Text == "interrupted")
	}
	if !resumed || !lost {
		t.Fatalf("resumed=%v interrupted-turn=%v; events: %v", resumed, lost, kindsOf(e.events(a.ID)))
	}
}

func TestMessageQueuedWhileDisconnectedIsDelivered(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") })
	stop1()

	// The isolation goes away; a new daemon accepts a message it cannot
	// deliver yet, then the isolation comes back.
	e.fc.reboot("gero-dev", false)
	m2, stop2 := e.daemon()
	defer stop2()
	if err := m2.Send(a.ID, "two"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("answer once the isolation is back", func() bool { return e.hasAssistant(a.ID, "echo: two") })
	n := 0
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindUser && ev.Text == "two" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message delivered %d times", n)
	}
}

func kindsOf(evs []event.Event) string {
	var ks []string
	for _, e := range evs {
		k := string(e.Kind)
		if e.Text != "" {
			k += "(" + e.Text + ")"
		}
		ks = append(ks, k)
	}
	return strings.Join(ks, " ")
}

// The defect this guards against: a message accepted while the isolation was
// down lived only in the daemon's memory, and died with the daemon.
func TestPendingMessageSurvivesDaemonDeath(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") })
	stop1()

	e.fc.reboot("gero-dev", false)
	e.fc.mu.Lock()
	e.fc.held["gero-dev"] = true
	e.fc.mu.Unlock()
	m2, stop2 := e.daemon()
	if err := m2.Send(a.ID, "two"); err != nil {
		t.Fatal(err)
	}
	stop2()

	e.fc.mu.Lock()
	delete(e.fc.held, "gero-dev")
	e.fc.mu.Unlock()
	m3, stop3 := e.daemon()
	defer stop3()
	e.waitFor("the message from the dead daemon", func() bool { return e.hasAssistant(a.ID, "echo: two") })
	n := 0
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindUser && ev.Text == "two" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message delivered %d times", n)
	}
	e.waitFor("outbox drained", func() bool {
		p, _ := e.st.Pending(a.ID)
		return len(p) == 0
	})
	_ = m3
}

func TestEnvChangeReplacesIdleProcess(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })

	v := "v1"
	if err := m.SetEnv("dev", api.SetEnv{"GERO_TEST_VAR": &v}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("old process replaced", func() bool {
		ag, _ := m.Agent(a.ID)
		return !ag.State.Alive
	})
	if err := m.Send(a.ID, "env"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("new process sees the new environment", func() bool { return e.hasAssistant(a.ID, "echo: env=v1") })
}

// Fail closed: without a measured fence no agent is created or started, and
// work that was waiting starts as soon as the fence holds.
func TestAgentsWaitForTheFence(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.fence.set(false)
	m, stop := e.daemon()
	defer stop()
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"}); err == nil ||
		!strings.Contains(err.Error(), "not installed") {
		t.Fatalf("created an agent behind no fence: %v", err)
	}

	e.fence.set(true)
	m.markFenceStale("dev")
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") })

	// The fence breaks (the probe gets through) after a container restart:
	// a new message waits instead of starting a process.
	r, _ := m.runtime(a.ID)
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	client.Signal(ctx, a.ID, "TERM")
	e.waitFor("process gone", func() bool { v, _ := m.Agent(a.ID); return !v.State.Alive })
	e.fc.mu.Lock()
	e.fc.leak = true
	e.fc.mu.Unlock()
	m.markFenceStale("dev")
	m.Send(a.ID, "two")
	e.waitFor("the agent to report the broken fence", func() bool {
		v, _ := m.Agent(a.ID)
		return strings.Contains(v.Problem, "does not hold")
	})
	if v, _ := m.Agent(a.ID); v.State.Alive {
		t.Fatal("an agent started behind a fence that leaks")
	}

	e.fc.mu.Lock()
	e.fc.leak = false
	e.fc.mu.Unlock()
	e.waitFor("the waiting message once the fence holds", func() bool { return e.hasAssistant(a.ID, "echo: two") })
}

func TestLegacyIsolationMustMoveOffTheDefaultNetwork(t *testing.T) {
	e := newTestEnv(t)
	iso, _ := e.st.Isolation("dev")
	iso.Network = ""
	e.st.PutIsolation(iso)
	m, stop := e.daemon()
	defer stop()
	_, err := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "gero iso upgrade dev") {
		t.Fatalf("err = %v", err)
	}
}

// The hole this pins: gating only process starts let an agent that was
// already running take new turns behind a fence nobody had measured.
func TestRunningAgentGetsNoTurnsBehindAnUnverifiedFence(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })

	// The helper goes away while the agent process is up.
	e.fence.set(false)
	m.markFenceStale("dev")
	m.Send(a.ID, "two")
	e.waitFor("the agent to say it is waiting", func() bool {
		v, _ := m.Agent(a.ID)
		return strings.HasPrefix(v.Problem, "waiting for the network fence")
	})
	time.Sleep(500 * time.Millisecond)
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindUser && ev.Text == "two" {
			t.Fatal("a running agent took a turn behind an unverified fence")
		}
	}
	if v, _ := m.Agent(a.ID); !v.State.Alive {
		t.Fatal("an unverified (not leaking) fence should hold turns, not kill the agent")
	}

	// The helper is back: the held message goes through on the next round.
	e.fence.set(true)
	e.waitFor("the held message", func() bool { return e.hasAssistant(a.ID, "echo: two") })

	// The fence is measured to leak: the agent process is stopped.
	e.fc.mu.Lock()
	e.fc.leak = true
	e.fc.mu.Unlock()
	m.markFenceStale("dev")
	m.Send(a.ID, "three")
	e.waitFor("the agent stopped behind a leaking fence", func() bool {
		v, _ := m.Agent(a.ID)
		return !v.State.Alive && strings.Contains(v.Problem, "does not hold")
	})
	if e.hasAssistant(a.ID, "echo: three") {
		t.Fatal("a turn ran behind a leaking fence")
	}
}

// After a container is recreated its bridge is new, and the helper loads
// IPv6 rules for it only once it sees it. A measurement before then would
// test rules that are not there; it waits instead, and never reports that
// as a leak.
func TestTheFenceIsMeasuredOnlyOnceItsIPv6RulesCoverTheBridge(t *testing.T) {
	defer func(step time.Duration) { fenceSettleStep = step }(fenceSettleStep)
	fenceSettleStep = time.Millisecond
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	iso, _ := e.st.Isolation("dev")
	probes := func() [][]string {
		e.fc.mu.Lock()
		defer e.fc.mu.Unlock()
		return append([][]string(nil), e.fc.probed...)
	}

	// The helper's rules lag three applies behind the bridge: the daemon
	// asks again until they cover it, then measures, link-local included.
	e.fence.mu.Lock()
	e.fence.lag = 3
	e.fence.mu.Unlock()
	before, applies := len(probes()), e.fence.applies()
	if ok, why := fenceVerdict(m.verifyFence(ctx, iso)); !ok {
		t.Fatalf("not verified: %s", why)
	}
	if got := e.fence.applies() - applies; got != 4 {
		t.Fatalf("asked the helper %d times", got)
	}
	p := probes()
	linkLocal := false
	if len(p) == before+1 {
		for _, target := range p[before] {
			linkLocal = linkLocal || strings.HasPrefix(target, "[fe80::1%eth0]:")
		}
	}
	if !linkLocal {
		t.Fatalf("the measurement after coverage: %v", p[before:])
	}

	// Rules that never cover the bridge: waiting, not measured, not a leak.
	e.fence.mu.Lock()
	e.fence.lag = 1 << 20
	e.fence.mu.Unlock()
	before = len(probes())
	ok, why := fenceVerdict(m.verifyFence(ctx, iso))
	if ok || !strings.Contains(why, "IPv6 rules to cover "+testBridge) || strings.HasPrefix(why, leakPrefix) {
		t.Fatalf("%v %q", ok, why)
	}
	if len(probes()) != before {
		t.Fatal("measured rules that do not cover the bridge")
	}

	// No bridge visible at all: the same, never "verified" on IPv4 alone.
	e.fence.mu.Lock()
	e.fence.lag = 0
	e.fence.mu.Unlock()
	e.mu.Lock()
	e.noBridge = true
	e.mu.Unlock()
	if ok, why := fenceVerdict(m.verifyFence(ctx, iso)); ok || !strings.Contains(why, "bridge to come up") {
		t.Fatalf("%v %q", ok, why)
	}
}

// A probe that could not even be sent measured nothing: the fence is not
// verified, so agents wait, but it is not a leak, so none is stopped.
func TestAProbeThatNeverLeftIsNeitherAPassNorALeak(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	iso, _ := e.st.Isolation("dev")
	e.fc.mu.Lock()
	e.fc.unsent = true
	e.fc.mu.Unlock()
	ok, why := fenceVerdict(m.verifyFence(ctx, iso))
	if ok || strings.HasPrefix(why, leakPrefix) || !strings.Contains(why, "waiting to measure the fence") {
		t.Fatalf("%v %q", ok, why)
	}
	e.fc.mu.Lock()
	e.fc.unsent = false
	e.fc.mu.Unlock()
	if ok, why := fenceVerdict(m.verifyFence(ctx, iso)); !ok {
		t.Fatalf("not verified once the probe could go: %q", why)
	}
}

func fenceVerdict(r fenceResult) (bool, string) { return r.ok, r.why }
