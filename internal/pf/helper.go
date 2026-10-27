package pf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The helper runs as root under launchd. It holds three things and nothing
// else: a pf enable reference, the gero anchor's rules, and the list of
// networks to fence (persisted, so the fence is back at boot before anyone
// logs in). It is level-triggered: every few seconds it recomputes the rules
// from the Mac's current addresses and bridges and reloads them if they
// differ or if something emptied the anchor. A missed event can delay the
// fence by one interval; it can never leave it stale.

const (
	HelperLabel = "dev.gero.pfd"
	HelperPath  = "/Library/PrivilegedHelperTools/dev.gero.pfd"
	HelperPlist = "/Library/LaunchDaemons/dev.gero.pfd.plist"
	StateDir    = "/var/db/gero-pfd"
	SocketPath  = "/var/run/gero-pfd.sock"
	LogPath     = "/var/log/gero-pfd.log"
)

// Runner runs pfctl.
type Runner interface {
	Pfctl(stdin string, args ...string) (string, error)
}

type execRunner struct{}

func (execRunner) Pfctl(stdin string, args ...string) (string, error) {
	cmd := exec.Command("/sbin/pfctl", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("pfctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type Request struct {
	Op string `json:"op"` // status, apply
	// Owner identifies the daemon (its state directory). apply replaces only
	// that daemon's networks; the fence covers every owner's.
	Owner    string    `json:"owner,omitempty"`
	Networks []Network `json:"networks,omitempty"`
}

type Status struct {
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Enabled   bool      `json:"enabled"`
	Networks  []Network `json:"networks"`
	Loaded    []string  `json:"loaded"` // rules as pfctl reports them
	AppliedAt time.Time `json:"applied_at,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

type Helper struct {
	StateDir string
	Socket   string
	UID      int // the one user allowed to talk to the helper
	Run      Runner
	// HostAddrs and BridgeOf read the Mac's interfaces; tests replace them.
	HostAddrs func() ([]HostAddr, error)
	BridgeOf  func(gateway4 string) string
	Interval  time.Duration
	Log       *log.Logger

	mu        sync.Mutex
	owners    map[string][]Network
	networks  []Network // the union of every owner's, fenced together
	applied   string
	appliedAt time.Time
	lastErr   string
}

func NewHelper(uid int, logger *log.Logger) *Helper {
	return &Helper{StateDir: StateDir, Socket: SocketPath, UID: uid, Run: execRunner{},
		HostAddrs: SystemAddrs, BridgeOf: SystemBridge, Interval: 3 * time.Second, Log: logger}
}

func (h *Helper) Serve(ctx context.Context) error {
	if err := os.MkdirAll(h.StateDir, 0o700); err != nil {
		return err
	}
	h.owners = map[string][]Network{}
	if b, err := os.ReadFile(filepath.Join(h.StateDir, "networks.json")); err == nil {
		// Older helpers stored one flat list, for an unnamed owner.
		if json.Unmarshal(b, &h.owners) != nil {
			var flat []Network
			if json.Unmarshal(b, &flat) == nil {
				h.owners = map[string][]Network{"": flat}
			}
		}
	}
	h.networks = union(h.owners)
	h.reconcile()

	os.Remove(h.Socket)
	l, err := net.Listen("unix", h.Socket)
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chown(h.Socket, h.UID, -1); err != nil {
		return err
	}
	if err := os.Chmod(h.Socket, 0o600); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(h.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				l.Close()
				return
			case <-t.C:
				h.reconcile()
			}
		}
	}()
	h.Log.Printf("gero-pfd serving %s for uid %d", h.Socket, h.UID)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go h.handle(c)
	}
}

// peerAllowed is a second check behind the socket's 0600 mode: only the
// owning user (or root) may change what is fenced.
func (h *Helper) peerAllowed(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	allowed := false
	raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err == nil {
			allowed = int(cred.Uid) == h.UID || cred.Uid == 0
		}
	})
	return allowed
}

func (h *Helper) handle(c net.Conn) {
	defer c.Close()
	reply := func(s Status) {
		b, _ := json.Marshal(s)
		c.Write(append(b, '\n'))
	}
	if !h.peerAllowed(c) {
		reply(Status{Error: "not allowed"})
		return
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		return
	}
	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		reply(Status{Error: "bad request"})
		return
	}
	switch req.Op {
	case "apply":
		for _, n := range req.Networks {
			if _, err := Rules([]Network{n}, nil, func(string) string { return "" }); err != nil {
				reply(Status{Error: err.Error()})
				return
			}
		}
		h.mu.Lock()
		if req.Owner != "" {
			// A named daemon is speaking: the unnamed list a previous
			// helper version stored was one of these daemons' own.
			delete(h.owners, "")
		}
		if len(req.Networks) == 0 {
			delete(h.owners, req.Owner)
		} else {
			h.owners[req.Owner] = req.Networks
		}
		h.networks = union(h.owners)
		b, _ := json.Marshal(h.owners)
		h.mu.Unlock()
		if err := os.WriteFile(filepath.Join(h.StateDir, "networks.json"), b, 0o600); err != nil {
			reply(Status{Error: err.Error()})
			return
		}
		h.reconcile()
	case "status":
	default:
		reply(Status{Error: "unknown op " + req.Op})
		return
	}
	reply(h.status())
}

// union merges every owner's networks, by name, in a stable order.
func union(owners map[string][]Network) []Network {
	byName := map[string]Network{}
	for _, nets := range owners {
		for _, n := range nets {
			byName[n.Name] = n
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Network, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func (h *Helper) status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := Status{OK: true, Networks: h.networks, AppliedAt: h.appliedAt, LastError: h.lastErr}
	s.Enabled = h.enabled()
	s.Loaded = h.loaded()
	return s
}

func (h *Helper) enabled() bool {
	out, err := h.Run.Pfctl("", "-s", "info")
	return err == nil && strings.Contains(out, "Status: Enabled")
}

func (h *Helper) loaded() []string {
	out, _ := h.Run.Pfctl("", "-a", Anchor, "-s", "rules")
	var rules []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "No ALTQ") && !strings.HasPrefix(l, "ALTQ") {
			rules = append(rules, l)
		}
	}
	return rules
}

var tokenRE = regexp.MustCompile(`Token\s*:\s*(\d+)`)

// ensureEnabled keeps one pf enable reference for gero. The token is kept
// on disk so a restarted helper reuses it instead of piling up references.
// It is never released while installed: releasing the last reference
// would turn pf off, and the fence with it.
func (h *Helper) ensureEnabled() error {
	tokFile := filepath.Join(h.StateDir, "token")
	if b, err := os.ReadFile(tokFile); err == nil {
		tok := strings.TrimSpace(string(b))
		refs, _ := h.Run.Pfctl("", "-s", "References")
		if tok != "" && strings.Contains(refs, tok) && h.enabled() {
			return nil
		}
	}
	out, err := h.Run.Pfctl("", "-E")
	if err != nil {
		return err
	}
	m := tokenRE.FindStringSubmatch(out)
	if m == nil {
		return fmt.Errorf("pfctl -E: no token in %q", out)
	}
	return os.WriteFile(tokFile, []byte(m[1]+"\n"), 0o600)
}

func (h *Helper) reconcile() {
	h.mu.Lock()
	defer h.mu.Unlock()
	setErr := func(err error) {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != h.lastErr && msg != "" {
			h.Log.Print(msg)
		}
		h.lastErr = msg
	}
	if len(h.networks) > 0 {
		if err := h.ensureEnabled(); err != nil {
			setErr(err)
			return
		}
	}
	host, err := h.HostAddrs()
	if err != nil {
		setErr(err)
		return
	}
	rules, err := Rules(h.networks, host, h.BridgeOf)
	if err != nil {
		setErr(err)
		return
	}
	// Reload when the rules changed, or when the anchor is empty though it
	// should not be: something flushed it.
	if rules == h.applied && (len(h.networks) == 0 || len(h.loaded()) > 0) {
		setErr(nil)
		return
	}
	if _, err := h.Run.Pfctl(rules, "-a", Anchor, "-f", "-"); err != nil {
		setErr(err)
		return
	}
	h.applied, h.appliedAt = rules, time.Now().UTC()
	h.Log.Printf("fence applied for %d network(s)", len(h.networks))
	setErr(nil)
}

// Teardown removes the fence: flushes the anchor and releases gero's pf
// enable reference. pf stays on if anything else holds one.
func Teardown(r Runner, stateDir string) error {
	var errs []error
	if _, err := r.Pfctl("", "-a", Anchor, "-F", "all"); err != nil {
		errs = append(errs, err)
	}
	if b, err := os.ReadFile(filepath.Join(stateDir, "token")); err == nil {
		if tok := strings.TrimSpace(string(b)); tok != "" {
			if _, err := r.Pfctl("", "-X", tok); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// SystemAddrs lists every address on the Mac's interfaces.
func SystemAddrs() ([]HostAddr, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []HostAddr
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ones, _ := ipn.Mask.Size()
			out = append(out, HostAddr{Iface: ifc.Name, Prefix: netip.PrefixFrom(ip.Unmap(), ones)})
		}
	}
	return out, nil
}

// SystemBridge finds the interface that holds gateway4, or "".
func SystemBridge(gateway4 string) string {
	gw, err := netip.ParseAddr(gateway4)
	if err != nil {
		return ""
	}
	addrs, _ := SystemAddrs()
	for _, a := range addrs {
		if a.Prefix.Addr() == gw {
			return a.Iface
		}
	}
	return ""
}
