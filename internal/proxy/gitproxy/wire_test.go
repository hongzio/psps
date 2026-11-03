package gitproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/store"
)

// Ported from isolate's test_gitproxy_wire.py and test_ghapi_wire.py. The
// policy tests prove the decision; these prove the wire, on live sockets:
// that a public clone leaves with no Authorization header and the
// credential is not even fetched, that the caller's credential is never
// forwarded, and that refusals happen before upstream is dialled.

type seen struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

type wiretap struct {
	mu   sync.Mutex
	reqs []seen
}

func (w *wiretap) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	w.reqs = append(w.reqs, seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
	w.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/info/refs") {
		rw.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		rw.Write([]byte("# service=git-upload-pack\n"))
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Write([]byte(`{"ok": true}`))
}

func (w *wiretap) all() []seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]seen(nil), w.reqs...)
}

type harness struct {
	t        *testing.T
	tap      *wiretap
	in       *instance
	git, api string // base URLs of the proxy's two ports
	counter  string // a file the credential command appends to
	settings Settings
	envs     map[string]Env
}

func newHarness(t *testing.T, envs map[string]Env, public ...string) *harness {
	t.Helper()
	tap := &wiretap{}
	up := httptest.NewServer(tap)
	t.Cleanup(up.Close)
	h := &harness{t: t, tap: tap, settings: Settings{PublicReads: true, API: true}, envs: envs,
		counter: filepath.Join(t.TempDir(), "calls")}
	// The credential command records every run, so a test can say it never
	// ran on the anonymous path.
	for name, e := range envs {
		e.TokenCommand = []string{"/bin/sh", "-c", fmt.Sprintf("echo x >> %s; echo real-gh-token", h.counter)}
		envs[name] = e
	}
	host := proxy.Host{Name: func() string { return "dev" }, View: h.view, Log: log.New(io.Discard, "", 0)}
	svc := Service{GitUpstream: up.URL, APIUpstream: up.URL, Transport: http.DefaultTransport}
	h.in = svc.New(host).(*instance)
	h.in.public = newFakePublic(public...)
	for port, base := range map[int]*string{GitPort: &h.git, APIPort: &h.api} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go h.in.Serve(port, l)
		*base = "http://" + l.Addr().String()
	}
	return h
}

func (h *harness) view() (proxy.View, error) {
	v := proxy.View{Isolation: "dev", Enabled: true, Envs: map[string]json.RawMessage{}}
	v.Exposure = store.Exposure{IsolationID: "dev", Service: "git"}
	for name, e := range h.envs {
		b, _ := json.Marshal(e)
		v.Envs[name] = b
		v.Exposure.Envs = append(v.Exposure.Envs, name)
	}
	v.Exposure.Settings, _ = json.Marshal(h.settings)
	return v, nil
}

func (h *harness) credentialRuns() int {
	b, _ := os.ReadFile(h.counter)
	return strings.Count(string(b), "x")
}

func (h *harness) do(method, url string, header map[string]string, body []byte) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	if body == nil {
		req.Body = nil
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (h *harness) fetch(repo string, header map[string]string) (int, string) {
	code, body, _ := h.do("GET", h.git+"/"+repo+".git/info/refs?service=git-upload-pack", header, nil)
	return code, body
}

func TestAPublicCloneLeavesWithNoAuthorizationHeader(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}}, "pub/repo")
	if code, _ := h.fetch("pub/repo", nil); code != 200 {
		t.Fatal(code)
	}
	sent := h.tap.all()
	if len(sent) != 1 || sent[0].Header.Get("Authorization") != "" {
		t.Fatalf("sent %+v", sent)
	}
	if sent[0].Path != "/pub/repo.git/info/refs?service=git-upload-pack" {
		t.Fatal(sent[0].Path)
	}
	// The credential is never even read on that path.
	if n := h.credentialRuns(); n != 0 {
		t.Fatalf("the credential command ran %d times", n)
	}
}

func TestAnAllowlistedRepositoryStillGetsTheCredential(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}})
	if code, body := h.fetch("priv/repo", nil); code != 200 {
		t.Fatal(code, body)
	}
	if got := h.tap.all()[0].Header.Get("Authorization"); got != basic("real-gh-token") {
		t.Fatalf("Authorization = %q", got)
	}
	if n := h.credentialRuns(); n != 1 {
		t.Fatalf("the credential command ran %d times", n)
	}
}

func TestAClientSuppliedCredentialIsDroppedNotForwarded(t *testing.T) {
	// The isolation has no token, but it must not smuggle one through on
	// the anonymous path either.
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}}, "pub/repo")
	h.fetch("pub/repo", map[string]string{"Authorization": "Basic SMUGGLED"})
	if got := h.tap.all()[0].Header.Get("Authorization"); got != "" {
		t.Fatalf("forwarded %q", got)
	}
}

func TestAPrivateRepositoryNeverReachesUpstream(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}})
	if code, _ := h.fetch("secret/repo", nil); code != 403 {
		t.Fatal(code)
	}
	if len(h.tap.all()) != 0 || h.credentialRuns() != 0 {
		t.Fatal("a refused request reached upstream or spent the credential")
	}
}

func TestTheUpstreamPathIsRebuiltNotForwardedRaw(t *testing.T) {
	// The decision is made on the parsed pieces and exactly those go
	// upstream; a raw path the two ends could read differently never does.
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}})
	h.fetch("priv//repo", nil)
	if sent := h.tap.all(); len(sent) != 1 || sent[0].Path != "/priv/repo.git/info/refs?service=git-upload-pack" {
		t.Fatalf("sent %+v", sent)
	}
	if code, _ := h.fetch("priv/%72epo", nil); code != 403 {
		t.Fatal("an encoded path was accepted")
	}
}

func TestAPushStreamsItsBodyThrough(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}})
	pack := bytes.Repeat([]byte("PACK"), 64<<10)
	code, _, _ := h.do("POST", h.git+"/priv/repo.git/git-receive-pack",
		map[string]string{"Content-Type": "application/x-git-receive-pack-request"}, pack)
	if code != 200 {
		t.Fatal(code)
	}
	if sent := h.tap.all(); !bytes.Equal(sent[0].Body, pack) {
		t.Fatalf("upstream got %d bytes, want %d", len(sent[0].Body), len(pack))
	}
}

func TestReadOnlyRefusesAPushAtDiscovery(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}, ReadOnly: true}})
	code, body, _ := h.do("GET", h.git+"/priv/repo.git/info/refs?service=git-receive-pack", nil, nil)
	if code != 403 || !strings.Contains(body, "read-only") || len(h.tap.all()) != 0 {
		t.Fatal(code, body)
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t, nil)
	h.in.h.View = func() (proxy.View, error) { return proxy.View{Isolation: "dev"}, nil }
	code, body := h.fetch("pub/repo", nil)
	if code != 503 || !strings.Contains(body, "psps iso expose dev git") {
		t.Fatal(code, body)
	}
}

// The API side.

func apiHarness(t *testing.T) *harness {
	w := []string{"pulls", "comments"}
	return newHarness(t, map[string]Env{"work": {Repos: []string{"axis-to/api"}, APIWrites: &w}})
}

func TestTheHostsCredentialGoesOutAndTheCallersNeverDoes(t *testing.T) {
	h := apiHarness(t)
	code, _, _ := h.do("GET", h.api+"/repos/axis-to/api/pulls?state=open", map[string]string{"Authorization": "Bearer caller-junk"}, nil)
	if code != 200 {
		t.Fatal(code)
	}
	sent := h.tap.all()[0]
	if sent.Header.Get("Authorization") != "Bearer real-gh-token" || sent.Path != "/repos/axis-to/api/pulls?state=open" {
		t.Fatalf("%+v", sent)
	}
	for _, vs := range sent.Header {
		for _, v := range vs {
			if strings.Contains(v, "caller-junk") {
				t.Fatal("the caller's credential went upstream")
			}
		}
	}
}

func TestAnUnlistedRepoIsRefusedBeforeUpstream(t *testing.T) {
	h := apiHarness(t)
	code, body, _ := h.do("GET", h.api+"/repos/axis-to/secret/pulls", nil, nil)
	if code != 404 || !strings.Contains(body, "psps github api proxy:") || !strings.Contains(body, "--allow-repo axis-to/secret") {
		t.Fatal(code, body)
	}
	if len(h.tap.all()) != 0 {
		t.Fatal("reached upstream")
	}
}

func TestAnAllowedWriteCarriesItsBodyUntouched(t *testing.T) {
	h := apiHarness(t)
	payload := []byte(`{"title": "fix", "head": "branch", "base": "main", "body": "why"}`)
	code, _, _ := h.do("POST", h.api+"/repos/axis-to/api/pulls", map[string]string{"Content-Type": "application/json"}, payload)
	if code != 200 {
		t.Fatal(code)
	}
	sent := h.tap.all()[0]
	if sent.Method != "POST" || sent.Path != "/repos/axis-to/api/pulls" || !bytes.Equal(sent.Body, payload) || sent.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%+v", sent)
	}
}

func TestADisallowedFamilyNeverReachesUpstream(t *testing.T) {
	h := apiHarness(t)
	code, body, _ := h.do("POST", h.api+"/repos/axis-to/api/pulls/3/reviews", nil, []byte("{}"))
	if code != 403 || !strings.Contains(body, `"reviews"`) || len(h.tap.all()) != 0 {
		t.Fatal(code, body)
	}
}

func TestAnUnmodeledPathNeverReachesUpstream(t *testing.T) {
	h := apiHarness(t)
	code, body, _ := h.do("GET", h.api+"/graphql", nil, nil)
	if code != 404 || !strings.Contains(body, "not modeled") || len(h.tap.all()) != 0 {
		t.Fatal(code, body)
	}
}

func TestTheGlobalReadUsesTheFirstEnvsCredential(t *testing.T) {
	h := apiHarness(t)
	if code, _, _ := h.do("GET", h.api+"/user", nil, nil); code != 200 {
		t.Fatal(code)
	}
	if sent := h.tap.all()[0]; sent.Path != "/user" || sent.Header.Get("Authorization") != "Bearer real-gh-token" {
		t.Fatalf("%+v", sent)
	}
}

func TestAFailingCredentialCommandIsA503NotACrash(t *testing.T) {
	h := apiHarness(t)
	e := h.envs["work"]
	e.TokenCommand = []string{"false"}
	h.envs["work"] = e
	code, body, _ := h.do("GET", h.api+"/repos/axis-to/api/pulls", nil, nil)
	if code != 503 || !strings.Contains(body, "could not obtain") || len(h.tap.all()) != 0 {
		t.Fatal(code, body)
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := apiHarness(t)
	h.in.buckets = &proxy.Buckets{PerMinute: 60, Burst: 1}
	first, _, _ := h.do("GET", h.api+"/repos/axis-to/api/pulls", nil, nil)
	second, _, hdr := h.do("GET", h.api+"/repos/axis-to/api/pulls", nil, nil)
	if first != 200 || second != 429 || hdr.Get("Retry-After") == "" || len(h.tap.all()) != 1 {
		t.Fatal(first, second, hdr)
	}
}

func TestAnOversizedBodyIsRefusedBeforeUpstream(t *testing.T) {
	h := apiHarness(t)
	code, _, _ := h.do("POST", h.api+"/repos/axis-to/api/pulls", nil, bytes.Repeat([]byte("x"), MaxBody+1))
	if code != 413 || len(h.tap.all()) != 0 {
		t.Fatal(code)
	}
}

func TestACredentialShapedQueryParamIsStripped(t *testing.T) {
	h := apiHarness(t)
	h.do("GET", h.api+"/repos/axis-to/api/pulls?access_token=x&state=open", nil, nil)
	if sent := h.tap.all()[0]; sent.Path != "/repos/axis-to/api/pulls?state=open" {
		t.Fatal(sent.Path)
	}
}

func TestTheAPISideCanBeTurnedOff(t *testing.T) {
	h := apiHarness(t)
	h.settings.API = false
	code, body, _ := h.do("GET", h.api+"/repos/axis-to/api/pulls", nil, nil)
	if code != 503 || !strings.Contains(body, "off for this isolation") || len(h.tap.all()) != 0 {
		t.Fatal(code, body)
	}
}

// A token stored for the env is the credential: the command is never run,
// and the stored token is read per request, so replacing it takes effect on
// the next one.
func TestAStoredTokenIsTheCredentialAndTheCommandNeverRuns(t *testing.T) {
	h := newHarness(t, map[string]Env{"main": {Repos: []string{"priv/repo"}}})
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	h.in.h.Creds = c
	if err := c.Write("git", "main", map[string]string{"token": "stored-one"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.fetch("priv/repo", nil); code != 200 {
		t.Fatalf("clone: %d", code)
	}
	c.Write("git", "main", map[string]string{"token": "stored-two"})
	if code, _, _ := h.do("GET", h.api+"/repos/priv/repo/pulls", nil, nil); code != 200 {
		t.Fatalf("api: %d", code)
	}
	reqs := h.tap.all()
	if len(reqs) != 2 || reqs[0].Header.Get("Authorization") != basic("stored-one") || reqs[1].Header.Get("Authorization") != "Bearer stored-two" {
		t.Fatalf("upstream saw %q then %q", reqs[0].Header.Get("Authorization"), reqs[1].Header.Get("Authorization"))
	}
	if n := h.credentialRuns(); n != 0 {
		t.Fatalf("the token command ran %d time(s)", n)
	}
}

// An agent told only a repository's name finds its owner in the listing,
// which is the allowlist, answered here: GitHub is never asked.
func TestTheListingIsTheAllowlist(t *testing.T) {
	h := newHarness(t, map[string]Env{
		"axis": {Repos: []string{"Axis-Coordinate/trevia", "Axis-Coordinate/centaur.git", "hongziox/sqls"}},
		"ro":   {Repos: []string{"octo/docs"}, ReadOnly: true},
	})
	list := func(path string) []ListedRepo {
		t.Helper()
		code, body, hdr := h.do("GET", h.api+path, nil, nil)
		if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
			t.Fatalf("%s: %d %s", path, code, body)
		}
		var out []ListedRepo
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("%s: %v: %s", path, err, body)
		}
		return out
	}
	all := list("/user/repos?per_page=100")
	var names []string
	for _, r := range all {
		names = append(names, r.FullName+"@"+r.Env)
	}
	if want := "Axis-Coordinate/centaur@axis Axis-Coordinate/trevia@axis hongziox/sqls@axis octo/docs@ro"; strings.Join(names, " ") != want {
		t.Fatalf("listed %v", names)
	}
	if r := all[1]; r.Name != "trevia" || r.Owner.Login != "Axis-Coordinate" || !r.Permissions.Push {
		t.Fatalf("%+v", r)
	}
	if r := all[3]; r.Permissions.Push || !r.Permissions.Pull {
		t.Fatalf("a read-only env's repository takes a push: %+v", r)
	}
	if got := list("/orgs/axis-coordinate/repos"); len(got) != 2 {
		t.Fatalf("one owner's: %+v", got)
	}
	if got := list("/users/hongziox/repos"); len(got) != 1 || got[0].FullName != "hongziox/sqls" {
		t.Fatalf("one user's: %+v", got)
	}
	if got := list("/orgs/axis/repos"); len(got) != 0 {
		t.Fatalf("an owner listing nothing: %+v", got)
	}
	if got := list("/user/repos?page=2"); len(got) != 0 {
		t.Fatalf("the second page: %+v", got)
	}
	if code, _, _ := h.do("POST", h.api+"/user/repos", nil, []byte(`{"name":"x"}`)); code != 403 {
		t.Fatalf("a write to the listing: %d", code)
	}
	if len(h.tap.all()) != 0 || h.credentialRuns() != 0 {
		t.Fatal("the listing asked GitHub")
	}
	// A refusal of a repository no env lists says where to look.
	if _, body, _ := h.do("GET", h.api+"/repos/axis/trevia", nil, nil); !strings.Contains(body, "GET /user/repos lists those that are") {
		t.Fatal(body)
	}
}
