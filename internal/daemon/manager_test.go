package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
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
// id survives restarts, and turns that echo. A turn whose text is "hang", or
// a mail that says only that, starts and never finishes; "recall" answers
// with the thread's earlier messages, which thread/revert cuts.
func fakeCodex() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	reply := func(id any, result any) { enc.Encode(map[string]any{"id": id, "result": result}) }
	notify := func(method string, params any) { enc.Encode(map[string]any{"method": method, "params": params}) }
	const thread = "th-fake"
	turns := 0
	type turn struct{ id, text string }
	var history []turn
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
		case "thread/revert":
			i := slices.IndexFunc(history, func(t turn) bool { return t.id == m.Params["beforeTurnId"] })
			if i < 0 {
				enc.Encode(map[string]any{"id": m.ID, "error": map[string]any{"code": -32600, "message": "no such turn"}})
				continue
			}
			history = history[:i]
			notify("thread/reverted", map[string]any{"threadId": thread})
			reply(m.ID, map[string]any{"thread": map[string]any{"id": thread}})
		case "turn/start":
			turns++
			tid := fmt.Sprintf("turn-%d", turns)
			reply(m.ID, map[string]any{"turn": map[string]any{"id": tid, "status": "inProgress"}})
			notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": tid}})
			text := m.Params["input"].([]any)[0].(map[string]any)["text"].(string)
			var earlier []string
			for _, t := range history {
				earlier = append(earlier, t.text)
			}
			history = append(history, turn{tid, text})
			// A mail ends with its body, after the line saying who wrote it.
			if text == "hang" || strings.HasSuffix(text, "\n\nhang") {
				continue
			}
			switch text {
			case "env":
				text = "env=" + os.Getenv("GERO_TEST_VAR")
			case "mail address":
				text = "mail=" + os.Getenv("GERO_MAIL_URL")
			case "recall":
				text = "recall=" + strings.Join(earlier, ",")
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
	// created is every Create, as "base cpus memory"; refuse is a memory
	// size Create fails for, the way container refuses one.
	created []string
	refuse  string
	// shares are the Mac's folders each container was started with, and
	// refuseShare a folder Create fails for, as macOS privacy protection
	// makes container fail.
	shares      map[string][]isolation.Mount
	refuseShare string
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
func (f *fakeContainers) Create(_ context.Context, base, _ string, cpus int, memory, _ string, mounts []isolation.Mount) error {
	f.mu.Lock()
	c := fmt.Sprintf("%s %d %s", base, cpus, memory)
	for _, m := range mounts {
		c += fmt.Sprintf(" %s:%s:ro=%v", m.Source, m.Target, m.ReadOnly)
	}
	f.created = append(f.created, c)
	refused := f.refuse != "" && memory == f.refuse
	for _, m := range mounts {
		if f.refuseShare != "" && m.Source == f.refuseShare {
			f.mu.Unlock()
			return fmt.Errorf("container: failed to bootstrap: A directory sharing device configuration is invalid (Operation not permitted)")
		}
	}
	if !refused {
		if f.shares == nil {
			f.shares = map[string][]isolation.Mount{}
		}
		f.shares["gero-"+base] = append([]isolation.Mount(nil), mounts...)
	}
	f.mu.Unlock()
	if refused {
		return fmt.Errorf("container: cannot give a VM %s", memory)
	}
	return f.boot("gero-" + base)
}

func (f *fakeContainers) Mounts(_ context.Context, name string) ([]isolation.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ctrs[name]; !ok {
		return nil, fmt.Errorf("container %s not found", name)
	}
	return f.shares[name], nil
}

func (f *fakeContainers) lastCreated() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.created) == 0 {
		return ""
	}
	return f.created[len(f.created)-1]
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
func (f *fakeContainers) RunAsAgent(ctx context.Context, name string, argv ...string) (string, error) {
	if len(argv) == 2 && argv[0] == "cat" && argv[1] == "/proc/mounts" {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.mounts == "" {
			out := "/dev/vdb / ext4 rw,relatime 0 0\n/dev/vdc /home/agent ext4 rw,relatime 0 0\nproc /proc proc rw 0 0"
			for _, m := range f.shares[name] {
				{
					opt := "rw"
					if m.ReadOnly {
						opt = "ro"
					}
					out += fmt.Sprintf("\nvirtiofs %s virtiofs %s,relatime 0 0", m.Target, opt)
				}
			}
			return out, nil
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

// Sync records each request and does its removals under the shim's state,
// which is the container's own directory here.
func (f *fakeContainers) Sync(_ context.Context, name string, req shimproto.SyncRequest) (shimproto.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncs = append(f.syncs, req)
	var res shimproto.SyncResult
	for _, p := range req.Remove {
		if rel, ok := strings.CutPrefix(p, isolation.StateRoot+"/"); ok {
			os.RemoveAll(filepath.Join(f.base, name, rel))
			res.Removed = append(res.Removed, p)
		}
	}
	return res, nil
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

// The test isolation is called dev and its ID is not: a name passed where an
// ID belongs, or the other way round, fails here instead of passing by luck,
// as it would for an isolation from before IDs, whose ID is its name.
const (
	testIsoID = "i1dev"
	testCtr   = "gero-" + testIsoID
)

// id is the ID of the test isolation called name.
func (e *testEnv) id(name string) string {
	e.t.Helper()
	iso, err := e.st.IsolationNamed(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return iso.ID
}

type testEnv struct {
	t        *testing.T
	st       *store.Store
	fc       *fakeContainers
	fence    *fakeFence
	workRoot string
	home     string // the agent user's, in the isolation

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
	if err := st.PutIsolation(store.Isolation{ID: testIsoID, Name: "dev", Container: testCtr, Image: "fake:1", Network: "gero-" + testIsoID + "-net",
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := fc.boot(testCtr); err != nil {
		t.Fatal(err)
	}
	return &testEnv{t: t, st: st, fc: fc, fence: &fakeFence{installed: true}, workRoot: filepath.Join(base, "work"),
		home: filepath.Join(base, "home")}
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
	m.workRoot, m.home = e.workRoot, e.home
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
	e.fc.reboot(testCtr, true)

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
	e.fc.reboot(testCtr, false)
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

	e.fc.reboot(testCtr, false)
	e.fc.mu.Lock()
	e.fc.held[testCtr] = true
	e.fc.mu.Unlock()
	m2, stop2 := e.daemon()
	if err := m2.Send(a.ID, "two"); err != nil {
		t.Fatal(err)
	}
	stop2()

	e.fc.mu.Lock()
	delete(e.fc.held, testCtr)
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

// A message sent while the idle process is being replaced goes to the next
// one, not into the process on its way out, where it would be cut off.
func TestAMessageSentAsTheProcessIsReplacedGoesToTheNextOne(t *testing.T) {
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
	if err := m.Send(a.ID, "env"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the new process's answer", func() bool { return e.hasAssistant(a.ID, "echo: env=v1") })
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindTurnEnd && ev.Text == "interrupted" {
			t.Fatal("the message went to the process being replaced")
		}
	}
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
	m.markFenceStale(testIsoID)
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
	m.markFenceStale(testIsoID)
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
	iso, _ := e.st.Isolation(testIsoID)
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
	m.markFenceStale(testIsoID)
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
	m.markFenceStale(testIsoID)
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
	iso, _ := e.st.Isolation(testIsoID)
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
	iso, _ := e.st.Isolation(testIsoID)
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

// A mode the drivers do not know would run as never, unattended; the daemon
// refuses it on the way in instead.
func TestAnUnknownApprovalModeIsRefused(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	_, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one", Options: driver.Options{Approvals: "aks"}})
	var he httpError
	if !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("create: %v", err)
	}
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one", Options: driver.Options{Approvals: driver.ApprovalsAuto}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetOptions(a.ID, driver.Options{Approvals: "sometimes"}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("set: %v", err)
	}
	got, _ := e.st.Agent(a.ID)
	if got.Options.Approvals != driver.ApprovalsAuto {
		t.Fatalf("stored approvals = %q", got.Options.Approvals)
	}
}

// agentPID is the agent's process as the shim reports it, 0 if none runs.
func agentPID(t *testing.T, m *Manager, id string) int {
	t.Helper()
	r, err := m.runtime(id)
	if err != nil {
		return 0
	}
	r.mu.Lock()
	client := r.client
	r.mu.Unlock()
	if client == nil {
		return 0
	}
	infos, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range infos {
		if in.ID == id && in.Running {
			return in.PID
		}
	}
	return 0
}

// A rename is the name alone: the agent, its process and the VM it runs in
// know the isolation by ID and carry on as they were.
func TestRenamingAnIsolationKeepsItsAgentsRunning(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	v1 := "v1"
	if err := m.SetEnv("dev", api.SetEnv{"GERO_TEST_VAR": &v1}); err != nil {
		t.Fatal(err)
	}
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })
	pid := agentPID(t, m, a.ID)
	if pid == 0 {
		t.Fatal("no agent process")
	}
	r1, _ := m.runtime(a.ID)

	iso, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if iso.Name != "work" || iso.ID != testIsoID || iso.Container != testCtr {
		t.Fatalf("renamed: %+v", iso.Isolation)
	}
	if _, err := e.st.IsolationNamed("dev"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old name: %v", err)
	}
	if v, _ := m.Agent(a.ID); v.Isolation != "work" || v.IsolationID != testIsoID {
		t.Fatalf("the agent is in %q (%s)", v.Isolation, v.IsolationID)
	}
	if r2, _ := m.runtime(a.ID); r2 != r1 {
		t.Fatal("the rename replaced the agent's runtime")
	}
	if got := agentPID(t, m, a.ID); got != pid {
		t.Fatalf("agent process %d became %d: the rename restarted it", pid, got)
	}
	if err := m.Send(a.ID, "env"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("a turn under the new name", func() bool { return e.hasAssistant(a.ID, "echo: env=v1") })

	// The old name is free again, and a new "dev" is a new isolation: an
	// ID, a container and volumes of its own.
	nd, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if nd.ID == testIsoID || nd.Container != "gero-"+nd.ID || nd.Network != "gero-"+nd.ID+"-net" || e.fc.lastCreated() != nd.ID+" 0 " {
		t.Fatalf("new dev: %+v, created %q", nd, e.fc.lastCreated())
	}
	if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "work"}, nil); !errors.Is(err, store.ErrExists) {
		t.Fatalf("a second isolation called work: %v", err)
	}
}

func TestARenameIsCheckedBeforeAnythingMoves(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	if err := e.st.PutIsolation(store.Isolation{ID: "i2other", Name: "other", Container: "gero-i2other", Image: "fake:1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var he httpError
	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "other"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("renamed onto another isolation: %v", err)
	}
	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "Not OK"}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("bad name: %v", err)
	}
	// A bad size refuses the rename that came with it, too.
	bad := "8"
	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "work", Memory: &bad}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("bare-number memory: %v", err)
	}
	if _, err := e.st.IsolationNamed("dev"); err != nil {
		t.Fatalf("dev after refused updates: %v", err)
	}
}

// container cannot resize a VM: the container is replaced on its volumes,
// and put back as it was when the new size will not start.
func TestResizingAnIsolationReplacesItsContainer(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	cpus, mem := 2, "8G"
	iso, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{CPUs: &cpus, Memory: &mem})
	if err != nil {
		t.Fatal(err)
	}
	if iso.CPUs != 2 || iso.Memory != "8G" || e.fc.lastCreated() != testIsoID+" 2 8G" || iso.State != "running" {
		t.Fatalf("resized: %+v, created %q", iso.Isolation, e.fc.lastCreated())
	}

	e.fc.mu.Lock()
	e.fc.refuse = "3G"
	e.fc.mu.Unlock()
	big := "3G"
	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Memory: &big}); err == nil || !strings.Contains(err.Error(), "back at its old size") {
		t.Fatalf("a size container refused: %v", err)
	}
	stored, _ := e.st.Isolation(testIsoID)
	if stored.Memory != "8G" || e.fc.lastCreated() != testIsoID+" 2 8G" {
		t.Fatalf("after a refused size: memory %q, created %q", stored.Memory, e.fc.lastCreated())
	}
	if st, _ := e.fc.State(ctx, testCtr); st != "running" {
		t.Fatalf("container is %s", st)
	}

	e.fc.mu.Lock()
	n := len(e.fc.created)
	e.fc.mu.Unlock()
	for _, bad := range []api.UpdateIsolation{{Memory: ptr("8")}, {Memory: ptr("100M")}, {Memory: ptr("1P")}, {CPUs: ptr(-1)}, {CPUs: ptr(1 << 10)}} {
		if _, err := m.UpdateIsolation(ctx, "dev", bad); err == nil {
			t.Fatalf("%+v was taken", bad)
		}
	}
	e.fc.mu.Lock()
	defer e.fc.mu.Unlock()
	if len(e.fc.created) != n {
		t.Fatalf("a refused size replaced the container: %v", e.fc.created[n:])
	}
}

func ptr[T any](v T) *T { return &v }

func TestARenameDoesNotInterruptATurn(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the turn", func() bool { return status(m, a.ID) == event.StatusWorking })
	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "work"}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent back under the new name", func() bool {
		v, _ := m.Agent(a.ID)
		return v.Isolation == "work" && v.State.Alive
	})
	time.Sleep(200 * time.Millisecond)
	if s := status(m, a.ID); s != event.StatusWorking {
		t.Fatalf("status %s after the rename", s)
	}
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindTurnEnd {
			t.Fatalf("the turn ended: %v", kindsOf(e.events(a.ID)))
		}
	}
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Deleting forgets an archived agent everywhere gero keeps it; what it left
// in the isolation goes only when asked to.
func TestDeletingAnAgent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	newAgent := func(prompt string) store.Agent {
		t.Helper()
		a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: prompt})
		if err != nil {
			t.Fatal(err)
		}
		e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo: "+prompt) && status(m, a.ID) == event.StatusIdle })
		return a
	}
	shimDir := func(id string) string { return filepath.Join(e.fc.base, testCtr, "agents", id) }
	rollout := filepath.Join(e.home, ".codex/sessions/2026/10/28/rollout-2026-10-28T05-38-13-th-fake.jsonl")
	other := filepath.Join(e.home, ".codex/sessions/2026/10/28/rollout-2026-10-28T05-38-22-th-other.jsonl")
	touch(t, rollout)
	touch(t, other)

	a := newAgent("one")
	var he httpError
	if err := m.DeleteAgent(ctx, a.ID, false); !errors.As(err, &he) || he.code != http.StatusConflict {
		t.Fatalf("deleted a live agent: %v", err)
	}
	if err := m.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("its process to end", func() bool { return m.DeleteAgent(ctx, a.ID, false) == nil })
	if _, err := e.st.Agent(a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("agent still stored: %v", err)
	}
	if evs, _ := e.st.Events(a.ID, 0, 0); len(evs) != 0 {
		t.Fatalf("%d events kept", len(evs))
	}
	if exists(shimDir(a.ID)) {
		t.Fatal("the shim's log of it is still in the isolation")
	}
	if !exists(a.Workdir) || !exists(rollout) {
		t.Fatal("without purge, what it left in the isolation went too")
	}

	b := newAgent("two")
	if err := m.Archive(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("its process to end", func() bool { return m.DeleteAgent(ctx, b.ID, true) == nil })
	if exists(b.Workdir) || exists(rollout) || exists(shimDir(b.ID)) {
		t.Fatalf("purge left workspace=%v rollout=%v log=%v", exists(b.Workdir), exists(rollout), exists(shimDir(b.ID)))
	}
	if !exists(other) || !exists(a.Workdir) {
		t.Fatal("purge reached another session or another agent's workspace")
	}
	if err := m.DeleteAgent(ctx, b.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
}

func TestPurgingAClaudeAgentTouchesOnlyItsOwnPaths(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	iso, _ := e.st.Isolation(testIsoID)
	const sid = "7ff11a91-1572-4c8d-b0b9-c1444836c7d0"
	// As Claude Code names them: dev has -home-agent-work-77b17c2c.
	if got := projectName.ReplaceAllString(isolation.WorkRoot+"/77b17c2c", "-"); got != "-home-agent-work-77b17c2c" {
		t.Fatalf("project name %q", got)
	}
	a := store.Agent{ID: "c1", Kind: driver.Claude, IsolationID: testIsoID, Workdir: m.workRoot + "/c1", SessionID: sid}
	project := filepath.Join(e.home, ".claude/projects", projectName.ReplaceAllString(a.Workdir, "-"))
	mine := []string{filepath.Join(a.Workdir, "main.go"), filepath.Join(project, sid+".jsonl"), filepath.Join(e.home, ".claude/session-env", sid, "x")}
	theirs := []string{filepath.Join(m.workRoot, "c2", "main.go"), filepath.Join(e.home, ".claude/projects", projectName.ReplaceAllString(m.workRoot+"/c2", "-"), "s.jsonl"),
		filepath.Join(e.home, ".claude/session-env", "other", "x"), filepath.Join(e.home, ".claude/settings.json")}
	for _, p := range append(append([]string(nil), mine...), theirs...) {
		touch(t, p)
	}
	if err := m.purgeAgent(ctx, iso, a); err != nil {
		t.Fatal(err)
	}
	for _, p := range mine {
		if exists(p) {
			t.Errorf("%s kept", p)
		}
	}
	for _, p := range theirs {
		if !exists(p) {
			t.Errorf("%s removed", p)
		}
	}

	// A workspace gero did not make is not gero's to remove.
	odd := a
	odd.Workdir = theirs[0][:len(theirs[0])-len("/main.go")]
	if err := m.purgeAgent(ctx, iso, odd); err == nil || !exists(theirs[0]) {
		t.Fatalf("purged a workspace elsewhere: %v", err)
	}
}

func TestAnAmbiguousPrefixDeletesNothing(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	now := time.Now()
	for _, id := range []string{"ab12", "ab34"} {
		if err := e.st.CreateAgent(store.Agent{ID: id, Name: id, Kind: driver.Codex, IsolationID: testIsoID, Workdir: m.workRoot + "/" + id, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		e.st.SetArchived(id, &now)
	}
	var he httpError
	if err := m.DeleteAgent(context.Background(), "ab", false); !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if all, _ := e.st.Agents(); len(all) != 2 {
		t.Fatalf("%d agents left", len(all))
	}
}

// serveAPI serves m's API on a socket of its own, as the daemon does, and
// returns a client of it and the socket.
func serveAPI(t *testing.T, m *Manager) (*client.Client, string) {
	t.Helper()
	// A unix socket's path has to be short; a test's temp dir is not.
	dir, err := os.MkdirTemp("", "gero")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: m.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return client.New(sock), sock
}

// A followed agent's history ends with a ready event, which a browser can
// tell "none yet" by; the Go client never passes it on as an event.
func TestFollowingSaysWhenTheHistoryIsAllThere(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	c, sock := serveAPI(t, m)
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://gero/v1/agents/"+a.ID+"/events?follow=1", nil)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	resp.Body.Close()
	if err != nil || line != "event: ready\n" {
		t.Fatalf("an agent with no history: %q, %v", line, err)
	}

	if err := m.Send(a.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") })
	// Past the history, ready included.
	fctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	var kinds []event.Kind
	c.Follow(fctx, a.ID, 0, func(ev event.Event) error {
		kinds = append(kinds, ev.Kind)
		return nil
	})
	for _, k := range kinds {
		if k == "" {
			t.Fatalf("the client passed on the ready event: %v", kinds)
		}
	}
	if !slices.Contains(kinds, event.KindAssistant) {
		t.Fatalf("followed %v", kinds)
	}
}

// A new name is all that changes: a running turn carries on, an archived
// agent takes one as well, and a name is one line of text.
func TestRenamingAnAgent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	c, _ := serveAPI(t, m)

	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hang", Name: "two\nlines"}); err == nil {
		t.Fatal("created an agent named two lines")
	}
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the turn", func() bool { return status(m, a.ID) == event.StatusWorking })
	v, err := c.UpdateAgent(ctx, a.ID[:4], api.UpdateAgent{Name: ptr("  the flaky test  ")})
	if err != nil || v.ID != a.ID || v.Name != "the flaky test" {
		t.Fatalf("rename: %+v, %v", v, err)
	}
	if got, _ := m.Agent(a.ID); got.Name != "the flaky test" {
		t.Fatalf("the live agent is still %q", got.Name)
	}
	if got, _ := e.st.Agent(a.ID); got.Name != "the flaky test" {
		t.Fatalf("stored as %q", got.Name)
	}
	time.Sleep(200 * time.Millisecond)
	if s := status(m, a.ID); s != event.StatusWorking {
		t.Fatalf("status %s after the rename", s)
	}
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindTurnEnd {
			t.Fatalf("the turn ended: %v", kindsOf(e.events(a.ID)))
		}
	}

	for _, bad := range []string{"", "  ", "two\nlines", "tab\there"} {
		var he httpError
		if _, err := m.UpdateAgent(a.ID, api.UpdateAgent{Name: ptr(bad)}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	if _, err := c.UpdateAgent(ctx, "nope", api.UpdateAgent{Name: ptr("x")}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("an unknown agent: %v", err)
	}
	if got, _ := m.Agent(a.ID); got.Name != "the flaky test" {
		t.Fatalf("a refused name changed it to %q", got.Name)
	}

	if err := m.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := c.UpdateAgent(ctx, a.ID, api.UpdateAgent{Name: ptr("fixed")}); err != nil || v.Name != "fixed" || v.ArchivedAt == nil {
		t.Fatalf("archived rename: %+v, %v", v, err)
	}
}

// Without a prompt an agent waits, with no process, for its first message,
// which names it unless it was named first.
func TestAnAgentWithoutAPromptStartsOnItsFirstMessage(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "" || !exists(a.Workdir) {
		t.Fatalf("created %+v, workspace %v", a, exists(a.Workdir))
	}
	time.Sleep(300 * time.Millisecond)
	if v, _ := m.Agent(a.ID); v.State.Alive || len(e.events(a.ID)) != 0 {
		t.Fatalf("started with nothing to do: %+v, %v", v.State, kindsOf(e.events(a.ID)))
	}

	if err := m.Send(a.ID, "  fix the\tflaky test"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo:   fix the\tflaky test") })
	if v, _ := m.Agent(a.ID); v.Name != "fix the flaky test" {
		t.Fatalf("named %q", v.Name)
	}
	if st, _ := e.st.Agent(a.ID); st.Name != "fix the flaky test" {
		t.Fatalf("stored as %q", st.Name)
	}
	if err := m.Send(a.ID, "and the next one"); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Agent(a.ID); v.Name != "fix the flaky test" {
		t.Fatalf("a second message renamed it %q", v.Name)
	}

	b, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.UpdateAgent(b.ID, api.UpdateAgent{Name: ptr("mine")}); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(b.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Agent(b.ID); v.Name != "mine" {
		t.Fatalf("its first message renamed it %q", v.Name)
	}
}

// An agent's own ID names it wherever its key does, and no other agent,
// archived ones included, may have it: a new one is refused before any
// workspace is made.
func TestAnAgentsOwnID(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: " fix-login "})
	if err != nil || a.Handle != "fix-login" {
		t.Fatalf("created %+v, %v", a, err)
	}
	if err := m.Send("fix-login", "hello"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") })
	if v, err := m.Agent("fix-login"); err != nil || v.ID != a.ID {
		t.Fatalf("by its own ID: %+v, %v", v, err)
	}

	workspaces := func() int { d, _ := os.ReadDir(m.workRoot); return len(d) }
	n := workspaces()
	var he httpError
	for _, h := range []string{"fix-login", a.ID} {
		if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: h}); !errors.Is(err, store.ErrExists) {
			t.Errorf("a second agent took %q: %v", h, err)
		}
	}
	for _, h := range []string{"Fix", "fix login", "-fix", strings.Repeat("x", 33)} {
		if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: h}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
			t.Errorf("own ID %q: %v", h, err)
		}
	}
	if workspaces() != n {
		t.Fatal("a refused agent left a workspace")
	}

	b, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Archive(ctx, "fix-login"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"fix-login", a.ID} {
		if _, err := m.UpdateAgent(b.ID, api.UpdateAgent{Handle: ptr(h)}); !errors.Is(err, store.ErrExists) {
			t.Errorf("b took %q from an archived agent: %v", h, err)
		}
	}
	if v, err := m.UpdateAgent("fix-login", api.UpdateAgent{Handle: ptr("")}); err != nil || v.Handle != "" {
		t.Fatalf("giving it back: %+v, %v", v, err)
	}
	if v, err := m.UpdateAgent(b.ID, api.UpdateAgent{Handle: ptr("fix-login")}); err != nil || v.Handle != "fix-login" {
		t.Fatalf("a freed ID: %+v, %v", v, err)
	}
	if v, _ := m.Agent("fix-login"); v.ID != b.ID {
		t.Fatalf("fix-login is %s", v.ID)
	}
}

// An agent given no ID of its own goes by an adjective-noun no agent has,
// as its ID or its key; once all are taken, by one with a number.
func TestAnAgentWithNoIDOfItsOwnGetsAnAdjectiveNoun(t *testing.T) {
	for _, l := range [][]string{adjectives, nouns} {
		if len(l) != 500 || len(slices.Compact(slices.Sorted(slices.Values(l)))) != 500 {
			t.Fatalf("%d words, some twice", len(l))
		}
	}
	// Lowercase words, the longest pair with a number still an agent ID,
	// and none on both lists (no lemon-lemon).
	longest := map[bool]int{}
	for i, l := range [][]string{adjectives, nouns} {
		for _, w := range l {
			if strings.Trim(w, "abcdefghijklmnopqrstuvwxyz") != "" || (i == 1 && slices.Contains(adjectives, w)) {
				t.Fatalf("%q", w)
			}
			longest[i == 0] = max(longest[i == 0], len(w))
		}
	}
	if n := strings.Repeat("a", longest[true]) + "-" + strings.Repeat("a", longest[false]) + "-99"; !validAgentHandle.MatchString(n) {
		t.Fatalf("%s is no agent ID", n)
	}
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	savedA, savedN := adjectives, nouns
	defer func() { adjectives, nouns = savedA, savedN }()
	adjectives, nouns = []string{"red"}, []string{"fox", "owl"}
	// One is already someone's own ID.
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "red-fox"}); err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil || b.Handle != "red-owl" {
		t.Fatalf("created %+v, %v", b, err)
	}
	if v, err := m.Agent("red-owl"); err != nil || v.ID != b.ID {
		t.Fatalf("by its ID: %+v, %v", v, err)
	}
	c, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil || (c.Handle != "red-fox-2" && c.Handle != "red-owl-2") {
		t.Fatalf("with every one taken: %+v, %v", c, err)
	}
	// Its key is still random: an ID that came back would otherwise find
	// a deleted agent's workspace.
	if c.ID == c.Handle || !strings.HasSuffix(c.Workdir, "/"+c.ID) {
		t.Fatalf("key %q, workspace %s", c.ID, c.Workdir)
	}
}

// A name picked for an agent that another agent takes before it is stored
// is picked again; an ID the operator chose is refused instead.
func TestAPickedNameTakenMeanwhileIsPickedAgain(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	savedA, savedN := adjectives, nouns
	defer func() { adjectives, nouns = savedA, savedN }()
	adjectives, nouns = []string{"red"}, []string{"fox", "owl"}
	now := time.Now()
	if err := e.st.CreateAgent(store.Agent{ID: "k1", Handle: "red-fox", Name: "x", Kind: driver.Codex, IsolationID: testIsoID, Workdir: "/w/k1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	a := store.Agent{ID: "k2", Handle: "red-fox", Name: "y", Kind: driver.Codex, IsolationID: testIsoID, Workdir: "/w/k2", CreatedAt: now}
	if _, err := m.storeNamed(a, false); !errors.Is(err, store.ErrExists) {
		t.Fatalf("an operator's taken ID: %v", err)
	}
	got, err := m.storeNamed(a, true)
	if err != nil || got.Handle != "red-owl" {
		t.Fatalf("stored %+v, %v", got, err)
	}
	if st, _ := e.st.Agent("k2"); st.Handle != "red-owl" {
		t.Fatalf("k2 stored with %q", st.Handle)
	}
}
