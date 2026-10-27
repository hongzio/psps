package awsproxy

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// The endpoint as botocore speaks to it, on a live socket, with the real
// exec path to a fake aws CLI.

const token = "workspace-token"

type harness struct {
	t    *testing.T
	aws  *fakeAWS
	base string

	mu      sync.Mutex
	enabled bool
	envs    map[string]Env
	order   []string
	def     string
	tokens  map[string]string
	missing []string
}

func newHarness(t *testing.T, envs map[string]Env, def string) *harness {
	t.Helper()
	h := &harness{t: t, aws: newFakeAWS(t), enabled: true, envs: envs, def: def, tokens: map[string]string{"": token}}
	for name, e := range envs {
		e.CLI = h.aws.path
		envs[name] = e
		h.order = append(h.order, name)
	}
	in := Service{}.New(proxy.Host{Isolation: "dev", View: h.view, Log: log.New(io.Discard, "", 0)})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go in.Serve(Port, l)
	h.base = "http://" + l.Addr().String()
	return h
}

func devEnv() map[string]Env {
	return map[string]Env{"dev": {RoleARN: role, Profile: "dev", Instances: []string{jp}, Buckets: []string{bucket}}}
}

func (h *harness) view() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v := proxy.View{Isolation: "dev", Enabled: h.enabled, Envs: map[string]json.RawMessage{}, Tokens: h.tokens, Missing: h.missing}
	v.Exposure = store.Exposure{Isolation: "dev", Service: "aws", Envs: append(h.order, h.missing...), Default: h.def}
	for name, e := range h.envs {
		b, _ := json.Marshal(e)
		v.Envs[name] = b
	}
	return v, nil
}

func (h *harness) edit(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
}

func (h *harness) fetch(method, path string, auth *string) (int, map[string]string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.base+path, nil)
	if auth != nil {
		req.Header.Set("Authorization", *auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &body); err != nil {
		h.t.Fatalf("not JSON: %q", b)
	}
	return resp.StatusCode, body
}

func str(s string) *string { return &s }

func TestCredentialsFlowWithTheRightToken(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	code, body := h.fetch("GET", "/creds", str(token))
	if code != 200 || body["AccessKeyId"] != "ASIAEXAMPLE" || body["Token"] != "token" {
		t.Fatalf("%d %v", code, body)
	}
	if got := h.aws.flag("--role-session-name"); got != "gero-dev" {
		t.Fatalf("session name = %q", got)
	}
}

func TestAMissingOrWrongTokenIs401(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	for _, auth := range []*string{nil, str("guess"), str("")} {
		if code, _ := h.fetch("GET", "/creds", auth); code != 401 {
			t.Errorf("auth %v: %d", auth, code)
		}
	}
	if h.aws.calls() != 0 {
		t.Fatal("the role was assumed for an unauthenticated caller")
	}
}

func TestAnIsolationWithNoTokenMintedRefusesAnEmptyHeader(t *testing.T) {
	// An empty expected token must not match an empty offered one.
	h := newHarness(t, devEnv(), "dev")
	h.edit(func() { h.tokens = map[string]string{} })
	if code, _ := h.fetch("GET", "/creds", str("")); code != 401 {
		t.Fatalf("%d", code)
	}
}

func TestIssuanceFailureIs503WithTheReason(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	h.aws.respond("", 255, "Your session has expired")
	code, body := h.fetch("GET", "/creds", str(token))
	if code != 503 || !strings.Contains(body["reason"], "aws login --profile dev") {
		t.Fatalf("%d %v", code, body)
	}
}

func TestAnEmptyAllowlistStillAnswersWithTheReason(t *testing.T) {
	// The broker answers even with nothing allowed, because "connection
	// refused" teaches nothing while a 503 names the fix.
	h := newHarness(t, map[string]Env{"dev": {RoleARN: role, Profile: "dev"}}, "dev")
	code, body := h.fetch("GET", "/creds", str(token))
	if code != 503 || !strings.Contains(body["reason"], "--allow-instance") || !strings.Contains(body["reason"], "gero env edit aws dev") {
		t.Fatalf("%d %v", code, body)
	}
	if h.aws.calls() != 0 {
		t.Fatal("the role was assumed with no policy")
	}
}

func TestOnlyTheCredsPathExists(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	if code, _ := h.fetch("GET", "/", str(token)); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _ := h.fetch("GET", "/credsx", str(token)); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _ := h.fetch("POST", "/creds", str(token)); code != 405 {
		t.Fatalf("POST: %d", code)
	}
}

func TestANamedEnvironmentIsServedByItsOwnSource(t *testing.T) {
	envs := devEnv()
	envs["prod"] = Env{RoleARN: "arn:aws:iam::111111111111:role/prod", Profile: "prod", Instances: []string{kr}}
	h := newHarness(t, envs, "dev")
	code, body := h.fetch("GET", "/creds/prod", str(token))
	if code != 200 || h.aws.flag("--profile") != "prod" || h.aws.flag("--role-arn") != "arn:aws:iam::111111111111:role/prod" {
		t.Fatalf("%d %v argv=%v", code, body, h.aws.argv())
	}
	// The bare path still serves the default environment.
	h.fetch("GET", "/creds", str(token))
	if h.aws.flag("--profile") != "dev" {
		t.Fatalf("bare path assumed via %q", h.aws.flag("--profile"))
	}
}

func TestAnUnknownEnvironmentNamesTheKnownOnes(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	code, body := h.fetch("GET", "/creds/staging", str(token))
	if code != 404 || !strings.Contains(body["reason"], "dev") {
		t.Fatalf("%d %v", code, body)
	}
}

func TestARemovedEnvironmentSaysSo(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	h.edit(func() { h.missing = []string{"old"} })
	code, body := h.fetch("GET", "/creds/old", str(token))
	if code != 404 || !strings.Contains(body["reason"], "removed") {
		t.Fatalf("%d %v", code, body)
	}
}

func TestANarrowedAllowlistTakesEffectAtOnce(t *testing.T) {
	// Cached credentials carry the policy they were issued under; an edit
	// must not wait for them to expire.
	h := newHarness(t, devEnv(), "dev")
	h.fetch("GET", "/creds", str(token))
	h.fetch("GET", "/creds", str(token))
	if h.aws.calls() != 1 {
		t.Fatalf("assumed %d times for one env", h.aws.calls())
	}
	h.edit(func() {
		e := h.envs["dev"]
		e.Instances = []string{jp + "=session"}
		e.Buckets = nil
		h.envs["dev"] = e
	})
	h.fetch("GET", "/creds", str(token))
	if h.aws.calls() != 2 {
		t.Fatal("the edit was served from the old cache")
	}
	var p Policy
	json.Unmarshal([]byte(h.aws.flag("--policy")), &p)
	for _, s := range p.Statement {
		if s.Sid == "Commands" || strings.HasPrefix(s.Sid, "Transfer") {
			t.Fatalf("the narrowed policy still has %s", s.Sid)
		}
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t, devEnv(), "dev")
	h.edit(func() { h.enabled = false })
	code, body := h.fetch("GET", "/creds", str(token))
	if code != 503 || !strings.Contains(body["reason"], "gero iso expose dev aws") {
		t.Fatalf("%d %v", code, body)
	}
}
