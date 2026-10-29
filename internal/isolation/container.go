// Package isolation drives Apple's `container` CLI: one VM-backed container
// per isolation, its durable volumes, and the exec channel the daemon uses to
// reach the shim inside without any IP route to the host.
package isolation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
)

// AgentUser is the uid:gid agents run as. The image's node user is renamed to
// it; Claude Code refuses to bypass permissions as root.
const (
	AgentUser = "1000:1000"
	AgentHome = "/home/agent"
	WorkRoot  = AgentHome + "/work"
	shimPath  = "/usr/local/bin/gero-shim"
	// StateRoot is the shim's durable state, on the state volume.
	StateRoot = "/var/lib/gero"
	// SharedRoot holds the folders of the Mac's shared with the isolation,
	// one directory each, named after its share env.
	SharedRoot = "/shared"
)

// AgentStateDir is where the shim keeps one agent's log.
func AgentStateDir(id string) string { return StateRoot + "/agents/" + id }

type CLI struct {
	Bin string
	// DNS are the nameservers containers and image builds resolve through;
	// none is container's own, the network's gateway.
	DNS []string
}

func (c CLI) dnsArgs() []string {
	var args []string
	for _, ip := range c.DNS {
		args = append(args, "--dns", ip)
	}
	return args
}

// FindCLI locates `container`. launchd starts the daemon with a PATH that has
// no Homebrew in it, so the usual install locations are checked explicitly.
func FindCLI() (CLI, error) {
	if p, err := exec.LookPath("container"); err == nil {
		return CLI{Bin: p}, nil
	}
	for _, p := range []string{"/opt/homebrew/bin/container", "/usr/local/bin/container"} {
		if _, err := os.Stat(p); err == nil {
			return CLI{Bin: p}, nil
		}
	}
	return CLI{}, errors.New("apple `container` CLI not found; install it with `brew install container`")
}

func (c CLI) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

func (c CLI) SystemRunning(ctx context.Context) bool {
	_, err := c.run(ctx, "system", "status")
	return err == nil
}

// SystemStart never offers to install a kernel: that prompt would hang a
// daemon with no terminal. A missing kernel surfaces as an error instead.
func (c CLI) SystemStart(ctx context.Context) error {
	_, err := c.run(ctx, "system", "start", "--disable-kernel-install")
	return err
}

// State is "running", "stopped", or "missing".
func (c CLI) State(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, "inspect", name)
	if err != nil {
		if strings.Contains(out, "not found") {
			return "missing", nil
		}
		return "", err
	}
	var info []struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info) == 0 {
		return "", fmt.Errorf("container inspect %s: unexpected output", name)
	}
	return info[0].Status.State, nil
}

// Mount is a directory of the Mac's in the isolation, as container's
// virtiofs shares it.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Mounts are the directories of the Mac's a container was started with,
// as container reports them; its volumes are not among them.
func (c CLI) Mounts(ctx context.Context, name string) ([]Mount, error) {
	out, err := c.run(ctx, "inspect", name)
	if err != nil {
		return nil, err
	}
	return parseMounts(out)
}

func parseMounts(inspect string) ([]Mount, error) {
	var info []struct {
		Configuration struct {
			Mounts []struct {
				Source      string                     `json:"source"`
				Destination string                     `json:"destination"`
				Options     []string                   `json:"options"`
				Type        map[string]json.RawMessage `json:"type"`
			} `json:"mounts"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(inspect), &info); err != nil || len(info) == 0 {
		return nil, errors.New("container inspect: unexpected output")
	}
	var out []Mount
	for _, m := range info[0].Configuration.Mounts {
		if _, ok := m.Type["virtiofs"]; !ok {
			continue
		}
		out = append(out, Mount{Source: m.Source, Target: m.Destination, ReadOnly: slices.Contains(m.Options, "ro")})
	}
	return out, nil
}

func (c CLI) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

func (c CLI) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, "stop", name)
	return err
}

// Remove stops the container first, so the shim gets SIGTERM and agents log
// real exits, then deletes it.
func (c CLI) Remove(ctx context.Context, name string) error {
	c.run(ctx, "stop", "-t", "10", name)
	out, err := c.run(ctx, "rm", "-f", name)
	if err != nil && strings.Contains(out, "not found") {
		return nil
	}
	return err
}

func (c CLI) ImageExists(ctx context.Context, ref string) bool {
	_, err := c.run(ctx, "image", "inspect", ref)
	return err == nil
}

func (c CLI) VolumeEnsure(ctx context.Context, name string) error {
	if _, err := c.run(ctx, "volume", "inspect", name); err == nil {
		return nil
	}
	_, err := c.run(ctx, "volume", "create", name)
	return err
}

func (c CLI) VolumeRemove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "volume", "rm", name)
	return err
}

func (c CLI) Build(ctx context.Context, tag, dir string, progress io.Writer) error {
	args := append([]string{"build", "--progress", "plain"}, c.dnsArgs()...)
	cmd := exec.CommandContext(ctx, c.Bin, append(args, "-t", tag, dir)...)
	var tail bytes.Buffer
	w := io.Writer(&tail)
	if progress != nil {
		w = io.MultiWriter(&tail, progress)
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Run(); err != nil {
		s := tail.String()
		if len(s) > 2000 {
			s = s[len(s)-2000:]
		}
		return fmt.Errorf("container build: %w\n%s", err, s)
	}
	return nil
}

// memorySize is container's --memory with its unit: K to P, 1024-based,
// written 4G, 4GB or 4GiB. container also takes a bare number, as bytes;
// gero does not, since "4096" is never meant as 4 KiB.
var memorySize = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?) ?([KMGTP])(?:i?B)?$`)

// MinMemory is the least container starts a VM with.
const MinMemory = 200 << 20

// MemoryBytes is the size a --memory names, refusing one container would,
// before anything is torn down for it. "" (container's default) is 0.
func MemoryBytes(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	m := memorySize.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("memory %q: give a size with its unit, like 4G or 512M", s)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	shift := strings.Index("KMGTP", strings.ToUpper(m[2]))*10 + 10
	b := uint64(n * float64(uint64(1)<<shift))
	if b < MinMemory {
		return 0, fmt.Errorf("memory %s: container starts a VM with no less than 200M", s)
	}
	return b, nil
}

type RunSpec struct {
	Name    string
	Image   string
	CPUs    int
	Memory  string
	Network string
	Volumes [][2]string // volume name, mount path
	Mounts  []Mount
	Labels  map[string]string
}

// NetworkInfo is what the fence needs to know about a container network.
type NetworkInfo struct {
	Subnet4  string
	Gateway4 string
	Subnet6  string
}

func (c CLI) NetworkInspect(ctx context.Context, name string) (NetworkInfo, error) {
	out, err := c.run(ctx, "network", "inspect", name)
	if err != nil {
		return NetworkInfo{}, err
	}
	var info []struct {
		Status struct {
			IPv4Gateway string `json:"ipv4Gateway"`
			IPv4Subnet  string `json:"ipv4Subnet"`
			IPv6Subnet  string `json:"ipv6Subnet"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil || len(info) == 0 {
		return NetworkInfo{}, fmt.Errorf("container network inspect %s: unexpected output", name)
	}
	st := info[0].Status
	return NetworkInfo{Subnet4: st.IPv4Subnet, Gateway4: st.IPv4Gateway, Subnet6: st.IPv6Subnet}, nil
}

// NetworkEnsure creates a network unless it exists. Each isolation gets its
// own: the fence then applies to gero's containers only, and isolations
// cannot reach one another.
func (c CLI) NetworkEnsure(ctx context.Context, name string) error {
	if _, err := c.NetworkInspect(ctx, name); err == nil {
		return nil
	}
	_, err := c.run(ctx, "network", "create", name)
	return err
}

func (c CLI) NetworkRemove(ctx context.Context, name string) error {
	out, err := c.run(ctx, "network", "rm", name)
	if err != nil && strings.Contains(out, "not found") {
		return nil
	}
	return err
}

// ProbeResult is what gero-shim's probe saw from inside a container.
type ProbeResult struct {
	Resolve string            `json:"resolve"` // "ok" or the error
	TCP     map[string]string `json:"tcp"`     // target -> open, blocked, refused, error...
}

// Probe connects from inside the container to each target and resolves a
// name through the container's resolver.
func (c CLI) Probe(ctx context.Context, name string, targets []string) (ProbeResult, error) {
	args := append([]string{"exec", name, shimPath, "probe", "--timeout", "2s", "--resolve", "example.com"}, targets...)
	out, err := c.run(ctx, args...)
	var r ProbeResult
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		return r, fmt.Errorf("probe: unexpected output %q", out)
	}
	return r, nil
}

func (c CLI) Run(ctx context.Context, s RunSpec) error {
	args := []string{"run", "-d", "--init", "--name", s.Name}
	if s.CPUs > 0 {
		args = append(args, "--cpus", strconv.Itoa(s.CPUs))
	}
	if s.Memory != "" {
		args = append(args, "--memory", s.Memory)
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	args = append(args, c.dnsArgs()...)
	for _, v := range s.Volumes {
		args = append(args, "-v", v[0]+":"+v[1])
	}
	for _, m := range s.Mounts {
		spec := "type=bind,source=" + m.Source + ",target=" + m.Target
		if m.ReadOnly {
			spec += ",readonly"
		}
		args = append(args, "--mount", spec)
	}
	for k, v := range s.Labels {
		args = append(args, "-l", k+"="+v)
	}
	args = append(args, s.Image)
	_, err := c.run(ctx, args...)
	return err
}

type ExecOpts struct {
	Interactive bool
	TTY         bool
	User        string
	Workdir     string
	Env         []string
}

// Exec builds `container exec` for argv inside name.
func (c CLI) Exec(ctx context.Context, name string, o ExecOpts, argv ...string) *exec.Cmd {
	args := []string{"exec"}
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.User != "" {
		args = append(args, "-u", o.User)
	}
	if o.Workdir != "" {
		args = append(args, "-w", o.Workdir)
	}
	for _, e := range o.Env {
		args = append(args, "-e", e)
	}
	args = append(append(args, name), argv...)
	return exec.CommandContext(ctx, c.Bin, args...)
}

// RunAsAgent runs argv to completion as the agent user and returns its output.
func (c CLI) RunAsAgent(ctx context.Context, name string, argv ...string) (string, error) {
	cmd := c.Exec(ctx, name, ExecOpts{User: AgentUser, Workdir: AgentHome, Env: []string{"HOME=" + AgentHome}}, argv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		return s, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, s)
	}
	return s, nil
}

// Proc is a process in an isolation, started by StartAsAgent, with its
// stdin and its output (stdout and stderr together) piped.
type Proc struct {
	Stdin  io.WriteCloser
	Output io.ReadCloser
	// Wait waits for the process to exit. Something it started may still
	// hold the output open after that.
	Wait func() error
}

// StartAsAgent starts argv as the agent user. env is handed over by name
// (`-e KEY`), the values in the `container` process's own environment, so a
// credential among them never shows in the Mac's process list. Cancelling
// ctx sends SIGTERM, which `container exec` passes on: a SIGKILL would end
// the client and leave the process running inside.
func (c CLI) StartAsAgent(ctx context.Context, name string, env []string, argv ...string) (Proc, error) {
	args := []string{"exec", "-i", "-u", AgentUser, "-w", AgentHome, "-e", "HOME=" + AgentHome}
	cmd := exec.CommandContext(ctx, c.Bin)
	cmd.Env = os.Environ()
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		args = append(args, "-e", k)
		cmd.Env = append(cmd.Env, kv)
	}
	cmd.Args = append(append(append([]string{c.Bin}, args...), name), argv...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	return StartProc(cmd)
}

// StartProc starts cmd with its stdin and output piped.
func StartProc(cmd *exec.Cmd) (Proc, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Proc{}, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return Proc{}, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		r.Close()
		return Proc{}, err
	}
	return Proc{Stdin: stdin, Output: r, Wait: cmd.Wait}, nil
}

// ShimDialer reaches the shim through `container exec -i ... gero-shim client`.
// Every connection is one exec process whose stdio is the byte stream.
func (c CLI) ShimDialer(name string) shim.Dialer {
	return func(ctx context.Context) (io.ReadWriteCloser, error) {
		// Not CommandContext: an attachment outlives the dial, and the shim
		// client closes the connection itself when its own context ends.
		cmd := exec.Command(c.Bin, "exec", "-i", name, shimPath, "client")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &execConn{cmd: cmd, stdin: stdin, stdout: stdout}, nil
	}
}

type execConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	once   sync.Once
}

func (e *execConn) Read(p []byte) (int, error)  { return e.stdout.Read(p) }
func (e *execConn) Write(p []byte) (int, error) { return e.stdin.Write(p) }

// Close ends the session the way the shim expects: stdin EOF detaches, the
// relay exits, and so does the exec. A process that lingers is killed —
// SIGTERM, not SIGKILL, because a SIGKILLed exec client leaves its process
// running inside the container.
func (e *execConn) Close() error {
	e.once.Do(func() {
		e.stdin.Close()
		go func() {
			done := make(chan struct{})
			go func() { e.cmd.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				e.cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					e.cmd.Process.Kill()
				}
			}
		}()
	})
	return nil
}

// TunnelConn is the exec stream of `gero-shim tunnel`. Stderr says why it
// ended when it ends early (an image whose shim predates the tunnel says
// "unknown command").
type TunnelConn interface {
	io.ReadWriteCloser
	Stderr() string
}

// DialTunnel starts the isolation end of the proxy tunnel, as root, so an
// agent can neither end it nor take its ports.
func (c CLI) DialTunnel(ctx context.Context, name string) (TunnelConn, error) {
	cmd := exec.Command(c.Bin, "exec", "-i", name, shimPath, "tunnel")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tc := &tunnelConn{execConn: execConn{cmd: cmd, stdin: stdin, stdout: stdout}}
	cmd.Stderr = &tc.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return tc, nil
}

type tunnelConn struct {
	execConn
	stderr tailBuffer
}

func (t *tunnelConn) Stderr() string { return strings.TrimSpace(t.stderr.String()) }

// tailBuffer keeps the last few KB written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > 4096 {
		b.buf = b.buf[len(b.buf)-4096:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Sync writes the files gero manages in the isolation (see shimproto).
func (c CLI) Sync(ctx context.Context, name string, req shimproto.SyncRequest) (shimproto.SyncResult, error) {
	in, _ := json.Marshal(req)
	cmd := exec.CommandContext(ctx, c.Bin, "exec", "-i", name, shimPath, "sync")
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	var res shimproto.SyncResult
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("sync: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return res, fmt.Errorf("sync: unexpected output %q", out.String())
	}
	return res, nil
}
