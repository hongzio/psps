package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Verify measures an isolation's guarantees now, from inside it where they
// are about what the isolation can do: the fence, what is mounted, what
// each exposed proxy refuses, and whether the Mac's side of each env still
// answers. It is isolate's `verify`, and its rule: every one of these was
// believed true at some point and was not, and none is visible in a
// configuration file.
//
// It changes nothing but the fence's own state, which a fresh measurement
// replaces: a leak found here stops agents as it does anywhere.
func (m *Manager) Verify(ctx context.Context, name string) (api.Verify, error) {
	rec, err := m.store.Isolation(name)
	if err != nil {
		return api.Verify{}, fmt.Errorf("isolation %s: %w", name, err)
	}
	v := api.Verify{Isolation: name, Checks: []api.VerifyCheck{}}
	add := func(c ...api.VerifyCheck) { v.Checks = append(v.Checks, c...) }
	if _, err := m.ensureIsolation(ctx, name); err != nil {
		add(api.VerifyCheck{Check: "isolation is up", Severity: api.SevConfig, Expected: "running", Got: "down", Detail: err.Error()})
		return v, nil
	}

	add(m.verifyFenceChecks(ctx, rec)...)
	add(m.verifyMounts(ctx, rec))
	add(m.verifyServices(ctx, rec)...)
	if c, ok := m.verifyCredsFile(); ok {
		add(c)
	}
	add(m.verifyEgress(ctx, rec))
	return v, nil
}

func pass(check, severity, got string) api.VerifyCheck {
	return api.VerifyCheck{Check: check, OK: true, Severity: severity, Got: got}
}

func fail(check, severity, expected, got, detail string) api.VerifyCheck {
	return api.VerifyCheck{Check: check, Severity: severity, Expected: expected, Got: got, Detail: detail}
}

// verifyFenceChecks measures the fence afresh, under the isolation's lock as
// every measurement is, and reports each address the isolation tried.
func (m *Manager) verifyFenceChecks(ctx context.Context, iso store.Isolation) []api.VerifyCheck {
	if iso.Fence == "off" {
		return []api.VerifyCheck{fail("network fence", api.SevConfig, "on", "off",
			"created with --fence off: the isolation reaches the Mac and the LAN, and agents start without a measured fence")}
	}
	st := m.isoLock(iso.Name)
	st.mu.Lock()
	r := m.measureFenceLocked(ctx, iso)
	st.mu.Unlock()
	if r.targets == nil {
		return []api.VerifyCheck{fail("network fence in place", api.SevIsolation, "rules loaded and measured", "not measured", r.why)}
	}
	out := []api.VerifyCheck{pass("fence rules cover "+r.bridge, api.SevIsolation, "loaded")}
	for _, t := range r.targets {
		check := "Mac unreachable at " + fenceAddr(t)
		switch outcome := r.outcome[t]; {
		case outcome == "blocked":
			out = append(out, pass(check, api.SevIsolation, "blocked"))
		case notSent(outcome):
			out = append(out, fail(check, api.SevConfig, "blocked", "not measured",
				"the probe never left the isolation ("+outcome+"); run verify again"))
		default:
			out = append(out, fail(check, api.SevIsolation, "blocked", outcome,
				"the isolation has a direct path to the Mac, around the proxies; agents in it were stopped"))
		}
	}
	if r.accepted > 0 {
		out = append(out, fail("nothing reached the Mac's listener", api.SevIsolation, "none", fmt.Sprintf("%d connection(s)", r.accepted),
			"a connection from the isolation arrived on the Mac; agents in it were stopped"))
	} else {
		out = append(out, pass("nothing reached the Mac's listener", api.SevIsolation, "none"))
	}
	return out
}

// fenceAddr is a probe target without the measurement's throwaway port.
func fenceAddr(target string) string {
	if host, _, err := net.SplitHostPort(target); err == nil {
		return host
	}
	return target
}

// hostShares are the filesystems that carry a directory of another
// machine: virtiofs is how `container run -v` shares the Mac's.
var hostShares = []string{"virtiofs", "9p", "nfs", "nfs4", "cifs", "smb3", "fuse"}

func (m *Manager) verifyMounts(ctx context.Context, iso store.Isolation) api.VerifyCheck {
	const check = "no Mac directory mounted in the isolation"
	out, err := m.cli.RunAsAgent(ctx, iso.Container, "cat", "/proc/mounts")
	if err != nil {
		return fail(check, api.SevConfig, "none", "not measured", err.Error())
	}
	var shared []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		for _, t := range hostShares {
			if f[2] == t || strings.HasPrefix(f[2], t+".") {
				shared = append(shared, f[1]+" ("+f[2]+")")
			}
		}
	}
	if len(shared) > 0 {
		return fail(check, api.SevIsolation, "none", strings.Join(shared, ", "),
			"the isolation sees the Mac's files there, around every proxy; gero never mounts any, so something else created this container")
	}
	return pass(check, api.SevIsolation, "none")
}

// verifyServices covers each exposed proxy: that the tunnel carries its
// ports, that it refuses what it must, measured as the agent from inside,
// and that each of its envs answers from the Mac.
func (m *Manager) verifyServices(ctx context.Context, iso store.Isolation) []api.VerifyCheck {
	type exposed struct {
		svc  proxy.Service
		view proxy.View
	}
	var xs []exposed
	for _, svc := range m.registry().All() {
		v, err := m.viewFor(iso.Name, svc)
		if err != nil {
			return []api.VerifyCheck{fail(svc.Name()+" configuration readable", api.SevConfig, "read", "error", err.Error())}
		}
		if v.Enabled {
			xs = append(xs, exposed{svc, v})
		}
	}
	if len(xs) == 0 {
		return nil
	}
	var out []api.VerifyCheck

	// The tunnel was just kicked by ensureIsolation; give it the moment it
	// takes to come up rather than report it starting.
	status := m.tunnelStatus(iso.Name)
	for deadline := time.Now().Add(10 * time.Second); status != "up" && time.Now().Before(deadline); status = m.tunnelStatus(iso.Name) {
		select {
		case <-ctx.Done():
			return append(out, fail("proxy tunnel up", api.SevConfig, "up", "not measured", ctx.Err().Error()))
		case <-time.After(200 * time.Millisecond):
		}
	}
	if status == "up" {
		out = append(out, pass("proxy tunnel up", api.SevConfig, "up"))
	} else {
		out = append(out, fail("proxy tunnel up", api.SevConfig, "up", "down", orNone(status)))
	}

	var targets []string
	for _, x := range xs {
		for _, p := range x.svc.Ports() {
			targets = append(targets, net.JoinHostPort("127.0.0.1", strconv.Itoa(m.tunnelPort(p))))
		}
	}
	reach, err := m.cli.Probe(ctx, iso.Container, targets)

	// Everything that waits on the network runs at once; the report keeps
	// its order.
	var jobs []func() []api.VerifyCheck
	for _, x := range xs {
		name := x.svc.Name()
		jobs = append(jobs, func() []api.VerifyCheck {
			var cs []api.VerifyCheck
			for _, p := range x.svc.Ports() {
				addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(m.tunnelPort(p)))
				check := fmt.Sprintf("%s reachable at 127.0.0.1:%d", name, p)
				switch got := reach.TCP[addr]; {
				case err != nil:
					cs = append(cs, fail(check, api.SevConfig, "open", "not measured", err.Error()))
				case got == "open":
					cs = append(cs, pass(check, api.SevConfig, "open"))
				default:
					cs = append(cs, fail(check, api.SevConfig, "open", got, "the tunnel is not carrying this port; `gero iso ls` shows why"))
				}
			}
			for _, n := range x.view.Missing {
				cs = append(cs, fail(fmt.Sprintf("%s env %s defined", name, n), api.SevConfig, "defined", "gone",
					fmt.Sprintf("exposed to %s but no longer defined: re-create it with `gero env add %s %s ...`, or expose the envs that remain with `gero iso expose %s %s ENV,...`",
						iso.Name, name, n, iso.Name, name)))
			}
			return cs
		})
		for _, r := range x.svc.Refusals(x.view) {
			jobs = append(jobs, func() []api.VerifyCheck { return []api.VerifyCheck{m.verifyRefusal(ctx, iso, name, r)} })
		}
		for _, env := range x.view.Exposure.Envs {
			cfg, ok := x.view.Envs[env]
			if !ok {
				continue
			}
			jobs = append(jobs, func() []api.VerifyCheck {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				check := fmt.Sprintf("%s env %s answers from the Mac", name, env)
				answer, err := x.svc.Check(cctx, env, cfg, m.creds)
				if err != nil {
					return []api.VerifyCheck{fail(check, api.SevConfig, "answers", "refused", err.Error())}
				}
				return []api.VerifyCheck{pass(check, api.SevConfig, firstLine(answer))}
			})
		}
	}
	results := make([][]api.VerifyCheck, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = job()
		}()
	}
	wg.Wait()
	for _, r := range results {
		out = append(out, r...)
	}
	return out
}

// verifyRefusal makes one request the proxy must refuse, from inside the
// isolation as the agent, with curl or psql: the tools an agent has.
func (m *Manager) verifyRefusal(ctx context.Context, iso store.Isolation, service string, r proxy.Refusal) api.VerifyCheck {
	check := fmt.Sprintf("%s refuses %s", service, r.What)
	port := strconv.Itoa(m.tunnelPort(r.Port))
	if r.Postgres != "" {
		conn := fmt.Sprintf("host=127.0.0.1 port=%s user=%s dbname=%s sslmode=disable connect_timeout=8", port, r.Postgres, r.Postgres)
		out, err := m.cli.RunAsAgent(ctx, iso.Container, "env", "PGPASSWORD="+proxy.WrongToken, "PGPASSFILE=/dev/null",
			"psql", "-X", "-At", "-d", conn, "-c", "SELECT 1")
		switch {
		case strings.Contains(out, r.Want):
			return pass(check, api.SevIsolation, "refused")
		case err == nil:
			return fail(check, api.SevIsolation, "refused", "accepted",
				"a wrong workspace token got past the proxy's authentication: "+firstLine(out))
		case strings.Contains(out, "FATAL") || strings.Contains(out, "ERROR"):
			return fail(check, api.SevIsolation, "refused", "a different answer",
				"the proxy answered a wrong workspace token with something other than its refusal: "+firstLine(out))
		}
		return fail(check, api.SevConfig, "refused", "not measured", firstLine(orNone(out)))
	}
	argv := []string{"curl", "-sS", "--max-time", "10", "-w", "\n%{http_code}", "-X", r.Method}
	keys := make([]string, 0, len(r.Header))
	for k := range r.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "-H", k+": "+r.Header[k])
	}
	if r.Body != "" {
		argv = append(argv, "--data-binary", r.Body)
	}
	argv = append(argv, "http://127.0.0.1:"+port+r.Path)
	out, err := m.cli.RunAsAgent(ctx, iso.Container, argv...)
	if err != nil {
		return fail(check, api.SevConfig, "refused", "not measured", "curl inside the isolation: "+lastLine(out))
	}
	body, code := out, ""
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		body, code = out[:i], out[i+1:]
	} else {
		body, code = "", out
	}
	status, _ := strconv.Atoi(strings.TrimSpace(code))
	if status == r.Status && strings.Contains(body, r.Want) {
		return pass(check, api.SevIsolation, fmt.Sprintf("refused (%d)", status))
	}
	return fail(check, api.SevIsolation, fmt.Sprintf("refused (%d)", r.Status), fmt.Sprintf("%d", status),
		"the proxy answered with something other than its refusal: "+clip(firstLine(orNone(strings.TrimSpace(body))), 200))
}

func (m *Manager) verifyCredsFile() (api.VerifyCheck, bool) {
	const check = "credentials file is owner-only"
	if m.creds == nil {
		return api.VerifyCheck{}, false
	}
	st, err := os.Stat(m.creds.Path)
	if errors.Is(err, os.ErrNotExist) {
		return api.VerifyCheck{}, false
	}
	if err != nil {
		return fail(check, api.SevConfig, "0600", "unreadable", err.Error()), true
	}
	mode := fmt.Sprintf("%04o", st.Mode().Perm())
	if st.Mode().Perm()&0o077 != 0 {
		return fail(check, api.SevConfig, "0600", mode,
			"the proxies refuse to read it until it is: run `chmod 600 "+m.creds.Path+"`"), true
	}
	return pass(check, api.SevConfig, mode), true
}

// egressURL is what the isolation must still reach: the fence keeps it off
// the Mac and the LAN, not off the internet its agents work through.
var egressURL = "https://example.com"

func (m *Manager) verifyEgress(ctx context.Context, iso store.Isolation) api.VerifyCheck {
	const check = "internet egress works"
	out, err := m.cli.RunAsAgent(ctx, iso.Container, "curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10", egressURL)
	if err != nil {
		return fail(check, api.SevConfig, "200", "unreachable", "the isolation cannot reach "+egressURL+": "+lastLine(out))
	}
	code, _ := strconv.Atoi(strings.TrimSpace(out))
	if code < 200 || code >= 400 {
		return fail(check, api.SevConfig, "200", strconv.Itoa(code), egressURL+" answered "+strconv.Itoa(code))
	}
	return pass(check, api.SevConfig, strconv.Itoa(code))
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(s)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
