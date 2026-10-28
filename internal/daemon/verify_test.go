package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/services"
	"github.com/hongzio/gero/internal/store"
)

// verifyEnv is a daemon serving reg to the dev isolation, every service
// exposed with one env, each proxy port carried by the test tunnel to a
// port of its own on this Mac, which stands in for the isolation.
func verifyEnv(t *testing.T, reg *proxy.Registry) (*testEnv, *Manager, *creds.Store) {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	t.Cleanup(stop)
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	m.SetProxies(reg, c)
	ports := map[int]int{}
	for _, svc := range reg.All() {
		for _, p := range svc.Ports() {
			ports[p] = freePort(t)
		}
	}
	m.tunnelPort = func(p int) int { return ports[p] }

	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(egress.Close)
	was := egressURL
	egressURL = egress.URL
	t.Cleanup(func() { egressURL = was })

	for _, svc := range reg.All() {
		// Stored as is: what is measured here is refused before any env
		// is read, and the Mac-side checks fail before reaching any real
		// service (git's would otherwise run gh and ask GitHub).
		cfg := json.RawMessage(`{}`)
		if svc.Name() == "git" {
			cfg = json.RawMessage(`{"token_command": ["false"]}`)
		}
		if err := e.st.PutEnv(store.Env{Service: svc.Name(), Name: "main", Config: cfg}); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Expose(ctx, "dev", svc.Name(), api.Expose{Envs: []string{"main"}}); err != nil {
			t.Fatal(svc.Name(), err)
		}
	}
	return e, m, c
}

func checkNamed(v api.Verify, prefix string) []api.VerifyCheck {
	var out []api.VerifyCheck
	for _, c := range v.Checks {
		if strings.HasPrefix(c.Check, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestVerifyMeasuresEveryProxyFromInsideTheIsolation(t *testing.T) {
	reg := services.Registry()
	_, m, c := verifyEnv(t, reg)
	// A credentials file, holding nothing the exposed envs use.
	if err := c.Write("slack", "unused", map[string]string{"token": "xoxb-1"}); err != nil {
		t.Fatal(err)
	}
	v, err := m.Verify(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range v.Checks {
		t.Logf("%-5v %-9s %-55s %s %s", ch.OK, ch.Severity, ch.Check, ch.Got, ch.Detail)
	}

	_, havePsql := exec.LookPath("psql")
	for _, svc := range reg.All() {
		for _, p := range svc.Ports() {
			if cs := checkNamed(v, fmt.Sprintf("%s reachable at 127.0.0.1:%d", svc.Name(), p)); len(cs) != 1 || !cs[0].OK {
				t.Errorf("%s port %d: %+v", svc.Name(), p, cs)
			}
		}
		refusals := checkNamed(v, svc.Name()+" refuses ")
		if len(refusals) == 0 {
			t.Errorf("%s has no refusal measured", svc.Name())
		}
		for _, r := range refusals {
			switch {
			case r.OK:
			case svc.Name() == "pg" && havePsql != nil && r.Severity == api.SevConfig && r.Got == "not measured":
				// No psql on this Mac to make the attempt with; the
				// pgproxy tests send the same refusal over the wire.
			default:
				t.Errorf("%s: %+v", svc.Name(), r)
			}
		}
		// The Mac's side of each env is not set up: a config failure,
		// never an isolation one.
		if cs := checkNamed(v, svc.Name()+" env main answers"); len(cs) != 1 || cs[0].Severity != api.SevConfig {
			t.Errorf("%s env check: %+v", svc.Name(), cs)
		}
	}
	for _, want := range []string{"fence rules cover bridge9", "Mac unreachable at 192.168.1.5", "Mac unreachable at fe80::1%eth0",
		"nothing reached the Mac's listener", "no Mac directory mounted", "proxy tunnel up", "credentials file is owner-only", "internet egress works"} {
		if cs := checkNamed(v, want); len(cs) != 1 || !cs[0].OK {
			t.Errorf("%s: %+v", want, cs)
		}
	}
	for _, ch := range v.Checks {
		if !ch.OK && ch.Severity == api.SevIsolation {
			t.Errorf("isolation failure: %+v", ch)
		}
	}
	if got := v.ExitCode(); got != api.VerifyConfig {
		t.Errorf("exit code %d, want %d (the envs' Mac side is not set up)", got, api.VerifyConfig)
	}

	os.Chmod(c.Path, 0o644)
	v, _ = m.Verify(context.Background(), "dev")
	if cs := checkNamed(v, "credentials file"); len(cs) != 1 || cs[0].OK || cs[0].Got != "0644" || cs[0].Severity != api.SevConfig {
		t.Errorf("a loose credentials file: %+v", cs)
	}
}

// laxService is a proxy that should refuse a wrong token and does not.
type laxService struct{ echoService }

func (laxService) Name() string { return "lax" }
func (laxService) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong workspace token", Port: 3020, Method: "GET", Path: "/",
		Header: map[string]string{"Authorization": proxy.WrongToken}, Status: 403, Want: "workspace token"}}
}
func (laxService) New(proxy.Host) proxy.Instance { return laxInstance{} }

type laxInstance struct{}

func (laxInstance) Serve(_ int, l net.Listener) error {
	return http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "here is everything")
	}))
}

func TestVerifyFailsAProxyThatLetsAWrongTokenThrough(t *testing.T) {
	_, m, _ := verifyEnv(t, proxy.NewRegistry(laxService{}))
	v, err := m.Verify(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	cs := checkNamed(v, "lax refuses a wrong workspace token")
	if len(cs) != 1 || cs[0].OK || cs[0].Severity != api.SevIsolation || cs[0].Got != "200" || !strings.Contains(cs[0].Detail, "here is everything") {
		t.Fatalf("%+v", cs)
	}
	if v.ExitCode() != api.VerifyIsolation {
		t.Errorf("exit code %d", v.ExitCode())
	}
}

func TestVerifyFailsALeakAndAHostMount(t *testing.T) {
	e, m, _ := verifyEnv(t, proxy.NewRegistry(echoService{}))
	e.fc.mu.Lock()
	e.fc.leak = true
	e.fc.mounts = "/dev/vdb / ext4 rw 0 0\nvirtiofs /mnt/mac virtiofs rw,relatime 0 0"
	e.fc.mu.Unlock()
	v, err := m.Verify(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if cs := checkNamed(v, "Mac unreachable at 192.168.1.5"); len(cs) != 1 || cs[0].OK || cs[0].Severity != api.SevIsolation || cs[0].Got != "open" {
		t.Errorf("leak: %+v", cs)
	}
	if cs := checkNamed(v, "no Mac directory mounted"); len(cs) != 1 || cs[0].OK || cs[0].Got != "/mnt/mac (virtiofs)" {
		t.Errorf("mount: %+v", cs)
	}
	if v.ExitCode() != api.VerifyIsolation {
		t.Errorf("exit code %d", v.ExitCode())
	}
	// What verify measured is what the daemon now believes: the leak keeps
	// agents from starting.
	if ok, why := m.fenceOK(testIsoID); ok || !strings.HasPrefix(why, leakPrefix) {
		t.Errorf("fence state after a measured leak: %v %q", ok, why)
	}
}

func TestVerifyReportsAnUnfencedIsolationAsConfiguration(t *testing.T) {
	e, m, _ := verifyEnv(t, proxy.NewRegistry(echoService{}))
	iso, _ := e.st.Isolation(testIsoID)
	iso.Fence = "off"
	e.st.PutIsolation(iso)
	v, err := m.Verify(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	cs := checkNamed(v, "network fence")
	if len(cs) != 1 || cs[0].OK || cs[0].Severity != api.SevConfig || cs[0].Got != "off" {
		t.Fatalf("%+v", cs)
	}
	if len(checkNamed(v, "Mac unreachable")) != 0 {
		t.Error("an unfenced isolation was measured as if fenced")
	}
}
