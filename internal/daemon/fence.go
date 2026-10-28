package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hongzio/gero/internal/pf"
	"github.com/hongzio/gero/internal/store"
)

// The fence is trusted only once measured. After the helper reports the
// rules loaded, the daemon listens on every host address and tries each one
// from inside the isolation; a single connection that gets through, or an
// answer of any kind, means agents stay stopped. This is isolate's rule: a
// boundary nobody measured is not one.

const (
	fenceRetry   = 5 * time.Second
	fenceRecheck = 10 * time.Minute
)

func (m *Manager) fenceStateLocked(iso string) *fenceState {
	fs, ok := m.fences[iso]
	if !ok {
		fs = &fenceState{why: "not measured yet", stale: true}
		m.fences[iso] = fs
	}
	return fs
}

// fenceOK reports whether agents in iso may start turns. It runs on every
// record an agent produces, so the isolation's setting is read once.
func (m *Manager) fenceOK(iso string) (bool, string) {
	m.mu.Lock()
	fs := m.fenceStateLocked(iso)
	loaded := fs.loaded
	m.mu.Unlock()
	if !loaded {
		rec, err := m.store.Isolation(iso)
		m.mu.Lock()
		fs.off, fs.loaded = err == nil && rec.Fence == "off", err == nil
		m.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case fs.off:
		return true, "off"
	case fs.ok:
		return true, "verified"
	}
	return false, fs.why
}

// stopAgents ends every agent process in iso: the fence was measured to
// leak, and an agent must not keep acting behind it.
func (m *Manager) stopAgents(iso string) {
	m.mu.Lock()
	var rs []*runtime
	for _, r := range m.runtimes {
		if r.agent.IsolationID == iso {
			rs = append(rs, r)
		}
	}
	m.mu.Unlock()
	for _, r := range rs {
		r.mu.Lock()
		client, alive := r.client, r.drv.State().Alive
		r.mu.Unlock()
		if client != nil && alive {
			client.Signal(m.ctx, r.agent.ID, "TERM")
		}
	}
}

func (m *Manager) markFenceStale(iso string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fs := m.fenceStateLocked(iso)
	fs.ok, fs.stale, fs.why = false, true, "the container restarted; measuring again"
	// A recreated container has a fresh root filesystem: write the
	// proxies' files again.
	delete(m.synced, iso)
}

// checkFence measures the fence when it is due: never measured, stale, a
// failed measurement older than fenceRetry, or a good one older than
// fenceRecheck. Callers hold the isolation's lock with its container up.
func (m *Manager) checkFence(ctx context.Context, iso store.Isolation) {
	if iso.Fence == "off" {
		return
	}
	m.mu.Lock()
	fs := m.fenceStateLocked(iso.ID)
	// A pass with a caveat (DNS not answering yet, seconds after boot) is
	// measured again soon, so the caveat does not outlive its cause.
	clean := fs.why == "verified"
	due := fs.stale || ((!fs.ok || !clean) && time.Since(fs.tried) > fenceRetry) || (fs.ok && time.Since(fs.tried) > fenceRecheck)
	m.mu.Unlock()
	if due {
		m.measureFenceLocked(ctx, iso)
	}
}

// measureFenceLocked measures the fence now and acts on the answer: agents
// start on a pass and stop on a leak. Callers hold the isolation's lock with
// its container up.
func (m *Manager) measureFenceLocked(ctx context.Context, iso store.Isolation) fenceResult {
	r := m.verifyFence(ctx, iso)
	m.mu.Lock()
	fs := m.fenceStateLocked(iso.ID)
	wasOK, wasWhy := fs.ok, fs.why
	fs.ok, fs.why, fs.tried, fs.stale = r.ok, r.why, time.Now(), false
	m.mu.Unlock()
	if r.ok != wasOK || r.why != wasWhy {
		m.log.Printf("isolation %s: network fence %s", iso.Name, r.why)
	}
	if r.ok && !wasOK {
		go m.kickIsolation(iso.ID)
	}
	if !r.ok && strings.HasPrefix(r.why, leakPrefix) {
		go m.stopAgents(iso.ID)
	}
	return r
}

const leakPrefix = "the fence does not hold"

// notSent is a probe outcome that means the packet never left the
// isolation: its own address not usable yet (IPv6 link-local addresses are
// tentative for the first seconds after boot, while duplicate detection
// runs) or no route at all. Every other outcome but "blocked" is an answer.
func notSent(outcome string) bool {
	return strings.Contains(outcome, "cannot assign requested address") ||
		strings.Contains(outcome, "network is unreachable")
}

// waitForFence re-measures in the background while agents in iso wait,
// one loop per isolation. Each round updates what the waiting agents show,
// and the round that finds the fence holding starts them.
func (m *Manager) waitForFence(iso string) {
	m.mu.Lock()
	fs := m.fenceStateLocked(iso)
	if fs.retrying {
		m.mu.Unlock()
		return
	}
	fs.retrying = true
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			fs.retrying = false
			m.mu.Unlock()
		}()
		for {
			// Renamed or deleted: its agents wait under another name, or
			// are gone.
			if _, err := m.store.Isolation(iso); errors.Is(err, store.ErrNotFound) {
				return
			}
			ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
			m.ensureIsolation(ctx, iso)
			cancel()
			ok, _ := m.fenceOK(iso)
			m.kickIsolation(iso)
			if ok || m.ctx.Err() != nil {
				return
			}
			select {
			case <-time.After(fenceRetry):
			case <-m.ctx.Done():
				return
			}
		}
	}()
}

func (m *Manager) kickIsolation(iso string) {
	m.mu.Lock()
	var rs []*runtime
	for _, r := range m.runtimes {
		if r.agent.IsolationID == iso {
			rs = append(rs, r)
		}
	}
	m.mu.Unlock()
	for _, r := range rs {
		r.mu.Lock()
		r.kickLocked()
		r.mu.Unlock()
	}
}

// fenceNetworks is every fenced isolation's network, for the helper.
func (m *Manager) fenceNetworks(ctx context.Context) ([]pf.Network, error) {
	isos, err := m.store.Isolations()
	if err != nil {
		return nil, err
	}
	var nets []pf.Network
	for _, iso := range isos {
		if iso.Fence == "off" || iso.Network == "" {
			continue
		}
		info, err := m.cli.NetworkInspect(ctx, iso.Network)
		if err != nil {
			return nil, err
		}
		nets = append(nets, pf.Network{Name: iso.Network, Subnet4: info.Subnet4, Gateway4: info.Gateway4, Subnet6: info.Subnet6})
	}
	return nets, nil
}

// fenceResult is one measurement: whether the fence holds and why, and,
// once it got as far as probing, what each target did.
type fenceResult struct {
	ok     bool
	why    string
	bridge string
	// targets are what the isolation tried, in order, and outcome what
	// happened to each; accepted is how many connections the Mac's
	// listener took. All empty when the rules were not in place to measure.
	targets  []string
	outcome  map[string]string
	accepted int
}

func fenceFailed(why string) fenceResult { return fenceResult{why: why} }

func (m *Manager) verifyFence(ctx context.Context, iso store.Isolation) fenceResult {
	if iso.Network == "" {
		return fenceFailed(fmt.Sprintf("it is on the shared default network; run `gero iso upgrade %s` to move it onto its own fenced network", iso.Name))
	}
	if m.fence == nil {
		return fenceFailed(pf.ErrNotInstalled.Error())
	}
	nets, err := m.fenceNetworks(ctx)
	if err != nil {
		return fenceFailed(err.Error())
	}
	info, err := m.cli.NetworkInspect(ctx, iso.Network)
	if err != nil {
		return fenceFailed(err.Error())
	}
	// IPv6 is fenced per bridge, and a container's bridge appears only once
	// it is attached; the helper re-reads the interfaces every few seconds.
	// So the helper is asked again, right away, until the rules pf actually
	// holds cover the bridge this container is on, and only then measured:
	// a measurement taken before that would test rules that are not there.
	// Until they are, the fence is waiting, not leaking.
	var bridge string
	for attempt := 0; ; attempt++ {
		st, err := m.fence.Apply(ctx, nets)
		if err != nil {
			return fenceFailed(err.Error())
		}
		if !st.Enabled {
			return fenceFailed("pf is not enabled")
		}
		if st.LastError != "" {
			return fenceFailed("the helper could not load the rules: " + st.LastError)
		}
		bridge = m.bridgeOf(info.Gateway4)
		if pf.CoversBridge(st.Loaded, bridge) {
			break
		}
		if attempt == fenceSettleTries-1 {
			if bridge == "" {
				return fenceFailed("waiting for the isolation's network bridge to come up")
			}
			return fenceFailed("waiting for the fence's IPv6 rules to cover " + bridge)
		}
		select {
		case <-time.After(fenceSettleStep):
		case <-ctx.Done():
			return fenceFailed(ctx.Err().Error())
		}
	}
	r := m.measureFence(ctx, iso, bridge)
	r.bridge = bridge
	return r
}

// The helper's own interval is 3s; asking for this long covers it.
var (
	fenceSettleTries = 20
	fenceSettleStep  = 250 * time.Millisecond
)

// measureFence tries every address the Mac holds, from inside the isolation,
// against a listener that accepts on all of them.
func (m *Manager) measureFence(ctx context.Context, iso store.Isolation, bridge string) fenceResult {
	l, err := net.Listen("tcp", ":0") // dual-stack, every interface
	if err != nil {
		return fenceFailed(err.Error())
	}
	defer l.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	port := fmt.Sprint(l.Addr().(*net.TCPAddr).Port)

	addrs, err := m.hostAddrs()
	if err != nil {
		return fenceFailed(err.Error())
	}
	seen := map[string]bool{}
	var targets []string
	for _, a := range addrs {
		ip := a.Prefix.Addr()
		var t string
		switch {
		case ip.IsLoopback():
			continue
		case ip.Is4():
			t = net.JoinHostPort(ip.String(), port)
		case a.Iface == bridge && ip.IsLinkLocalUnicast():
			// The Mac's link-local address on this bridge, as the
			// container sees it.
			t = net.JoinHostPort(ip.WithZone("").String()+"%eth0", port)
		case a.Iface == bridge:
			t = net.JoinHostPort(ip.String(), port)
		default:
			continue
		}
		if !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	sort.Strings(targets)

	res, err := m.cli.Probe(ctx, iso.Container, targets)
	if err != nil {
		return fenceFailed("could not measure the fence: " + err.Error())
	}
	r := fenceResult{targets: targets, outcome: res.TCP}
	var leaks, unmeasured []string
	for _, t := range targets {
		switch out := res.TCP[t]; {
		case out == "blocked":
		case notSent(out):
			unmeasured = append(unmeasured, t+" ("+out+")")
		default:
			leaks = append(leaks, t+" ("+out+")")
		}
	}
	r.accepted = int(accepted.Load())
	switch {
	case r.accepted > 0 || len(leaks) > 0:
		r.why = fmt.Sprintf("%s: the isolation reached %s", leakPrefix, strings.Join(leaks, ", "))
	case len(unmeasured) > 0:
		// Nothing left the isolation, so nothing was learned: not a leak,
		// and not a pass either. The next round measures again.
		r.why = "waiting to measure the fence: " + strings.Join(unmeasured, ", ")
	case res.Resolve != "ok":
		r.ok, r.why = true, "verified, but DNS through the gateway failed: "+res.Resolve
	default:
		r.ok, r.why = true, "verified"
	}
	return r
}
