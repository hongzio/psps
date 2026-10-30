package pf

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTheHelperSaysWhichItIs(t *testing.T) {
	if (Status{}).Running() != 1 || (Status{Version: HelperVersion}).Running() != HelperVersion {
		t.Fatal("version")
	}
}

// fakePfctl models just enough of pfctl: an enable reference count and one
// anchor's rules.
type fakePfctl struct {
	mu      sync.Mutex
	tokens  []string
	rules   string
	enables int
	loads   int
}

func (f *fakePfctl) Pfctl(stdin string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch strings.Join(args, " ") {
	case "-E":
		f.enables++
		tok := "1000" + string(rune('0'+f.enables))
		f.tokens = append(f.tokens, tok)
		return "pf enabled\nToken : " + tok + "\n", nil
	case "-s info":
		if len(f.tokens) > 0 {
			return "Status: Enabled for 0 days\n", nil
		}
		return "Status: Disabled\n", nil
	case "-s References":
		return "TOKEN  TIMESTAMP  PROCESS\n" + strings.Join(f.tokens, "\n"), nil
	case "-a " + Anchor + " -f -":
		f.loads++
		var kept []string
		for _, l := range strings.Split(stdin, "\n") {
			if l != "" && !strings.HasPrefix(l, "#") && !strings.HasPrefix(l, "table") {
				kept = append(kept, l)
			}
		}
		f.rules = strings.Join(kept, "\n")
		return "", nil
	case "-a " + Anchor + " -s rules":
		return f.rules, nil
	case "-a " + Anchor + " -F all":
		f.rules = ""
		return "", nil
	}
	if len(args) == 2 && args[0] == "-X" {
		for i, t := range f.tokens {
			if t == args[1] {
				f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
			}
		}
		return "", nil
	}
	return "", nil
}

func (f *fakePfctl) snapshot() (rules string, enables, loads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules, f.enables, f.loads
}

type bridges struct {
	mu   sync.Mutex
	name string
}

func (b *bridges) of(string) string { b.mu.Lock(); defer b.mu.Unlock(); return b.name }
func (b *bridges) set(n string)     { b.mu.Lock(); b.name = n; b.mu.Unlock() }

func startHelper(t *testing.T, dir string, run Runner, br *bridges) (Client, context.CancelFunc) {
	t.Helper()
	h := &Helper{StateDir: dir, Socket: filepath.Join(dir, "pfd.sock"), UID: os.Getuid(), Run: run,
		HostAddrs: func() ([]HostAddr, error) { return nil, nil }, BridgeOf: br.of,
		Interval: 30 * time.Millisecond, Log: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	c := Client{Socket: h.Socket}
	waitFor(t, "helper socket", func() bool { _, err := c.Status(context.Background()); return err == nil })
	return c, func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHelperAppliesAndKeepsTheFence(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pfd")
	defer os.RemoveAll(dir)
	pfctl, br := &fakePfctl{}, &bridges{}
	c, stop := startHelper(t, dir, pfctl, br)

	st, err := c.Apply(context.Background(), []Network{dev})
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || len(st.Loaded) == 0 {
		t.Fatalf("status after apply = %+v", st)
	}
	if rules, _, _ := pfctl.snapshot(); strings.Contains(rules, "inet6") {
		t.Fatal("IPv6 rules before the bridge exists")
	}

	// The container starts: its bridge appears, and the IPv6 fence follows.
	br.set("bridge101")
	waitFor(t, "IPv6 rules", func() bool { r, _, _ := pfctl.snapshot(); return strings.Contains(r, "on bridge101 inet6") })

	// Something flushes the anchor; the fence comes back on its own.
	pfctl.Pfctl("", "-a", Anchor, "-F", "all")
	waitFor(t, "reload after flush", func() bool { r, _, _ := pfctl.snapshot(); return r != "" })

	// A restarted helper reuses its enable reference and its networks.
	stop()
	_, enablesBefore, _ := pfctl.snapshot()
	pfctl.Pfctl("", "-a", Anchor, "-F", "all") // as a reboot would
	c2, stop2 := startHelper(t, dir, pfctl, br)
	defer stop2()
	waitFor(t, "fence back after restart", func() bool { r, _, _ := pfctl.snapshot(); return r != "" })
	if _, enables, _ := pfctl.snapshot(); enables != enablesBefore {
		t.Fatalf("restart took a new pf reference (%d -> %d)", enablesBefore, enables)
	}
	st, _ = c2.Status(context.Background())
	if len(st.Networks) != 1 || st.Networks[0].Name != "gero-dev" {
		t.Fatalf("networks after restart = %+v", st.Networks)
	}
}

func TestHelperRejectsBadNetworks(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pfd")
	defer os.RemoveAll(dir)
	c, stop := startHelper(t, dir, &fakePfctl{}, &bridges{})
	defer stop()
	bad := dev
	bad.Subnet4 = "not-a-subnet"
	if _, err := c.Apply(context.Background(), []Network{bad}); err == nil {
		t.Fatal("bad network accepted")
	}
}

func TestTeardownReleasesOnlyGerosReference(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pfd")
	defer os.RemoveAll(dir)
	pfctl := &fakePfctl{tokens: []string{"999"}} // someone else's
	c, stop := startHelper(t, dir, pfctl, &bridges{})
	c.Apply(context.Background(), []Network{dev})
	stop()
	if err := Teardown(pfctl, dir); err != nil {
		t.Fatal(err)
	}
	rules, _, _ := pfctl.snapshot()
	if rules != "" || len(pfctl.tokens) != 1 || pfctl.tokens[0] != "999" {
		t.Fatalf("after teardown: rules=%q tokens=%v", rules, pfctl.tokens)
	}
}

// Two daemons (a real one and a dev one) share the helper: neither may drop
// the other's networks from the fence.
func TestOwnersDoNotOverwriteEachOther(t *testing.T) {
	dir, _ := os.MkdirTemp("", "pfd")
	defer os.RemoveAll(dir)
	pfctl := &fakePfctl{}
	c, stop := startHelper(t, dir, pfctl, &bridges{})
	defer stop()
	other := Network{Name: "gero-other-net", Subnet4: "192.168.70.0/24", Gateway4: "192.168.70.1"}

	a := Client{Socket: c.Socket, Owner: "/home/a"}
	b := Client{Socket: c.Socket, Owner: "/home/b"}
	a.Apply(context.Background(), []Network{dev})
	st, _ := b.Apply(context.Background(), []Network{other})
	if len(st.Networks) != 2 {
		t.Fatalf("after two owners applied: %+v", st.Networks)
	}
	rules, _, _ := pfctl.snapshot()
	if !strings.Contains(rules, "192.168.65.0/24") || !strings.Contains(rules, "192.168.70.0/24") {
		t.Fatalf("fence does not cover both owners:\n%s", rules)
	}
	// One owner leaving keeps the other fenced.
	st, _ = b.Apply(context.Background(), nil)
	if len(st.Networks) != 1 || st.Networks[0].Name != "gero-dev" {
		t.Fatalf("after b left: %+v", st.Networks)
	}
}
