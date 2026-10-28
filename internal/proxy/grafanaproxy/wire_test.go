package grafanaproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Ported from isolate's test_grafanaproxy_wire.py: what actually leaves the
// Mac, on live sockets. The workspace token is swapped for the real service
// account token and never forwarded, refusals happen before upstream is
// dialled, a query body that goes out is the serialisation of the object
// that was authorized, and a subpath base URL lands its prefix on the wire.

type seen struct {
	TLS          bool
	Host         string
	Method, Path string
	Header       http.Header
	Body         string
	Length       int64
}

type answer struct {
	status int
	body   string
}

// tap records every upstream request and answers from a script, by path.
type tap struct {
	mu      sync.Mutex
	reqs    []seen
	answers map[string]answer
}

func (tp *tap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	tp.mu.Lock()
	tp.reqs = append(tp.reqs, seen{r.TLS != nil, r.Host, r.Method, r.URL.RequestURI(), r.Header.Clone(), string(b), r.ContentLength})
	a, ok := tp.answers[r.URL.RequestURI()]
	tp.mu.Unlock()
	if !ok {
		a = answer{200, `{"database": "ok"}`}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Frame-Options", "deny")
	w.WriteHeader(a.status)
	io.WriteString(w, a.body)
}

func (tp *tap) all() []seen {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]seen(nil), tp.reqs...)
}

func (tp *tap) script(path string, status int, body string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.answers[path] = answer{status, body}
}

func (tp *tap) methods() []string {
	var out []string
	for _, s := range tp.all() {
		out = append(out, s.Method)
	}
	return out
}

const (
	prodToken = "WORKSPACE-TOKEN"
	devToken  = "WORKSPACE-TOKEN-DEV"
	sbxToken  = "WORKSPACE-TOKEN-SBX"
)

type harness struct {
	t          *testing.T
	tap        *tap
	tls, plain *httptest.Server
	in         *instance
	base       string

	mu   sync.Mutex
	view proxy.View
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	tp := &tap{answers: map[string]answer{}}
	h := &harness{t: t, tap: tp, tls: httptest.NewTLSServer(tp), plain: httptest.NewServer(tp)}
	t.Cleanup(h.tls.Close)
	t.Cleanup(h.plain.Close)
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	for env, tok := range map[string]string{"prod": "REAL-TOKEN", "dev": "REAL-DEV", "sandbox": "REAL-SBX"} {
		if err := cs.Write("grafana", env, map[string]string{"token": tok}); err != nil {
			t.Fatal(err)
		}
	}
	h.view = proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
	h.view.Exposure = store.Exposure{IsolationID: "work", Service: "grafana", Default: "prod"}
	h.setEnv("prod", Env{BaseURL: h.tls.URL, Families: []string{"dashboards", "query"}, Datasources: []string{"uid-prom"}}, prodToken)
	h.setEnv("dev", Env{BaseURL: h.plain.URL + "/grafana", Families: []string{"dashboards"}}, devToken)
	h.setEnv("sandbox", Env{BaseURL: h.tls.URL, Families: []string{"dashboards"}, Writes: []string{"dashboards"}, Folders: []string{"agent-sbx"}}, sbxToken)
	// The TLS server's client trusts its certificate and speaks plain HTTP
	// to the other one.
	h.in = Service{Transport: h.tls.Client().Transport}.New(proxy.Host{Name: func() string { return "work" }, View: h.getView, Creds: cs, Log: log.New(io.Discard, "", 0)}).(*instance)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go h.in.Serve(Port, l)
	h.base = "http://" + l.Addr().String()
	return h
}

func (h *harness) setEnv(name string, e Env, tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.view.Envs[name], _ = json.Marshal(e)
	if !contains(h.view.Exposure.Envs, name) {
		h.view.Exposure.Envs = append(h.view.Exposure.Envs, name)
	}
	h.view.Tokens[name] = tok
}

func (h *harness) getView() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.view, nil
}

func (h *harness) edit(f func(v *proxy.View)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.view)
}

func (h *harness) do(method, path string, header map[string]string, body string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.base+path, strings.NewReader(body))
	if body == "" {
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

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func errorOf(t *testing.T, body string) string {
	t.Helper()
	var e struct{ Errors []string }
	if err := json.Unmarshal([]byte(body), &e); err != nil || len(e.Errors) != 1 {
		t.Fatalf("not a Grafana-shaped error: %q", body)
	}
	return e.Errors[0]
}

func (h *harness) save(payload any) (int, string) {
	raw, _ := json.Marshal(payload)
	hdr := bearer(sbxToken)
	hdr["Content-Type"] = "application/json"
	code, body, _ := h.do("POST", "/api/dashboards/db", hdr, string(raw))
	return code, body
}

func TestTheWrongTokenIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("GET", "/api/search", bearer("not-the-token"), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "workspace token") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestTheTokenIsSwappedAndTheCookieIsStripped(t *testing.T) {
	h := newHarness(t)
	hdr := bearer(prodToken)
	hdr["Cookie"] = "grafana_session=stolen"
	if code, _, _ := h.do("GET", "/api/search", hdr, ""); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.tap.all()[0]
	if !s.TLS || s.Host != strings.TrimPrefix(h.tls.URL, "https://") || s.Header.Get("Authorization") != "Bearer REAL-TOKEN" || s.Header.Get("Cookie") != "" {
		t.Fatalf("upstream saw %+v", s)
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			if strings.Contains(v, prodToken) {
				t.Fatalf("the workspace token went upstream in %s", k)
			}
		}
	}
}

func TestHealthNeedsTheTokenButNoFamily(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("GET", "/api/health", bearer(prodToken), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := h.do("GET", "/api/health", nil, ""); code != 403 {
		t.Fatalf("no token: %d", code)
	}
}

func TestAFamilyOffTheAllowlistNamesTheOperatorCommand(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("GET", "/api/folders", bearer(prodToken), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "gero env edit grafana prod --allow-api folders") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestASaveWhereWritesAreShutNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("POST", "/api/dashboards/db", bearer(prodToken), "{}")
	if code != 403 || !strings.Contains(errorOf(t, body), "--allow-write dashboards") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAnUnmodeledWriteNeverReachesUpstreamEvenWriteOpen(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("DELETE", "/api/dashboards/uid/abc", bearer(sbxToken), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "read-only") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAFreshSaveIntoAnAllowedFolderIsForwarded(t *testing.T) {
	h := newHarness(t)
	if code, body := h.save(dash("dashboard", dash("title", "t"), "folderUid", "agent-sbx")); code != 200 {
		t.Fatalf("%d %q", code, body)
	}
	all := h.tap.all()
	if len(all) != 1 || all[0].Method != "POST" || all[0].Path != "/api/dashboards/db" || all[0].Header.Get("Authorization") != "Bearer REAL-SBX" {
		t.Fatalf("%+v", all)
	}
	var sent map[string]any
	json.Unmarshal([]byte(all[0].Body), &sent)
	if sent["folderUid"] != "agent-sbx" {
		t.Fatalf("%s", all[0].Body)
	}
}

func TestAnUpdateLooksUpTheCurrentFolderFirst(t *testing.T) {
	h := newHarness(t)
	h.tap.script("/api/dashboards/uid/abc", 200, `{"dashboard": {"uid": "abc"}, "meta": {"folderUid": "agent-sbx"}}`)
	if code, body := h.save(dash("dashboard", dash("uid", "abc", "id", 9, "title", "t"), "folderUid", "agent-sbx", "overwrite", true)); code != 200 {
		t.Fatalf("%d %q", code, body)
	}
	all := h.tap.all()
	if len(all) != 2 {
		t.Fatalf("%+v", all)
	}
	lookup, sv := all[0], all[1]
	if lookup.Method != "GET" || lookup.Path != "/api/dashboards/uid/abc" || lookup.Header.Get("Authorization") != "Bearer REAL-SBX" {
		t.Fatalf("lookup %+v", lookup)
	}
	var sent struct{ Dashboard map[string]any }
	json.Unmarshal([]byte(sv.Body), &sent)
	if _, ok := sent.Dashboard["id"]; ok {
		t.Fatalf("the id went out: %s", sv.Body)
	}
}

func TestPullingADashboardInFromAnotherFolderIsRefused(t *testing.T) {
	h := newHarness(t)
	h.tap.script("/api/dashboards/uid/abc", 200, `{"meta": {"folderUid": "prod-core"}}`)
	code, body := h.save(dash("dashboard", dash("uid", "abc", "title", "t"), "folderUid", "agent-sbx", "overwrite", true))
	if code != 403 || !strings.Contains(errorOf(t, body), "prod-core") || strings.Join(h.tap.methods(), ",") != "GET" {
		t.Fatalf("%d %q %v", code, body, h.tap.methods())
	}
}

func TestALookupTheTokenCannotAnswerRefusesTheSave(t *testing.T) {
	h := newHarness(t)
	h.tap.script("/api/dashboards/uid/abc", 403, "{}")
	code, _ := h.save(dash("dashboard", dash("uid", "abc", "title", "t"), "folderUid", "agent-sbx"))
	if code != 502 || strings.Join(h.tap.methods(), ",") != "GET" {
		t.Fatalf("%d %v", code, h.tap.methods())
	}
}

func TestAUIDGrafanaDoesNotKnowIsAFreshSave(t *testing.T) {
	h := newHarness(t)
	h.tap.script("/api/dashboards/uid/new-one", 404, "{}")
	code, _ := h.save(dash("dashboard", dash("uid", "new-one", "title", "t"), "folderUid", "agent-sbx"))
	if code != 200 || strings.Join(h.tap.methods(), ",") != "GET,POST" {
		t.Fatalf("%d %v", code, h.tap.methods())
	}
}

func TestASaveIntoAForeignFolderNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, body := h.save(dash("dashboard", dash("title", "t"), "folderUid", "prod-core"))
	if code != 403 || !strings.Contains(errorOf(t, body), "--allow-folder prod-core") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAQueryForAnAllowedUIDIsReencodedAndForwarded(t *testing.T) {
	h := newHarness(t)
	payload := `{"queries": [{"refId": "A", "datasource": {"uid": "uid-prom"}, "expr": "up"}], "from": "now-1h", "to": "now"}`
	hdr := bearer(prodToken)
	hdr["Content-Type"] = "application/json"
	if code, body, _ := h.do("POST", "/api/ds/query", hdr, payload); code != 200 {
		t.Fatalf("%d %q", code, body)
	}
	s := h.tap.all()[0]
	var sent struct {
		Queries []struct{ Datasource struct{ UID string } }
	}
	json.Unmarshal([]byte(s.Body), &sent)
	if sent.Queries[0].Datasource.UID != "uid-prom" || s.Length != int64(len(s.Body)) {
		t.Fatalf("%+v", s)
	}
	if s.Body == payload {
		t.Fatal("the body went out as sent, not as checked")
	}
}

func TestAQueryForAForeignUIDNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("POST", "/api/ds/query", bearer(prodToken), `{"queries": [{"refId": "A", "datasource": {"uid": "uid-secret"}}]}`)
	if code != 403 || !strings.Contains(errorOf(t, body), "uid-secret") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestASubpathBaseURLLandsThePrefixOnTheWire(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("GET", "/api/search?limit=5", bearer(devToken), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.tap.all()[0]
	if s.TLS || s.Host != strings.TrimPrefix(h.plain.URL, "http://") || s.Path != "/grafana/api/search?limit=5" || s.Header.Get("Authorization") != "Bearer REAL-DEV" {
		t.Fatalf("%+v", s)
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.in.buckets = &proxy.Buckets{PerMinute: 60, Burst: 1}
	first, _, _ := h.do("GET", "/api/health", bearer(prodToken), "")
	second, _, hdr := h.do("GET", "/api/health", bearer(prodToken), "")
	if first != 200 || second != 429 || hdr.Get("Retry-After") == "" || len(h.tap.all()) != 1 {
		t.Fatalf("%d %d %v %d", first, second, hdr, len(h.tap.all()))
	}
	// Another env's budget is its own.
	if code, _, _ := h.do("GET", "/api/health", bearer(devToken), ""); code != 200 {
		t.Fatalf("dev: %d", code)
	}
}

// --- gero's own ---

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) { v.Enabled = false })
	code, body, _ := h.do("GET", "/api/health", bearer(prodToken), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "gero iso expose work grafana") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAnEnvWithNoTokenMintedRefusesAnEmptyBearer(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) { v.Tokens = map[string]string{} })
	for _, hdr := range []map[string]string{bearer(""), {"Authorization": "Bearer"}, nil} {
		if code, _, _ := h.do("GET", "/api/health", hdr, ""); code != 403 {
			t.Fatalf("%v: %d", hdr, code)
		}
	}
	if len(h.tap.all()) != 0 {
		t.Fatal("upstream was dialled")
	}
}

func TestARemovedEnvironmentSaysSo(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) {
		delete(v.Envs, "dev")
		v.Missing = []string{"dev"}
	})
	code, body, _ := h.do("GET", "/api/search", bearer(devToken), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "removed") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAMissingSecretIsARefusalNotACrash(t *testing.T) {
	h := newHarness(t)
	h.setEnv("ghost", Env{BaseURL: h.tls.URL, Families: []string{"dashboards"}}, "GHOST-TOKEN")
	code, body, _ := h.do("GET", "/api/search", bearer("GHOST-TOKEN"), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "[grafana ghost]") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAChunkedBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.base+"/api/ds/query", io.NopCloser(strings.NewReader(`{"queries": []}`)))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+prodToken)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 411 || len(h.tap.all()) != 0 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// --- the operator's own measurement, and the pickers ---

func hostSide(t *testing.T) (*tap, *httptest.Server, http.RoundTripper) {
	tp := &tap{answers: map[string]answer{}}
	up := httptest.NewTLSServer(tp)
	t.Cleanup(up.Close)
	return tp, up, up.Client().Transport
}

func TestEveryAllowedFamilyIsProbedWithTheRealToken(t *testing.T) {
	tp, up, rt := hostSide(t)
	res, err := CheckEnv(t.Context(), rt, Env{BaseURL: up.URL, Families: []string{"dashboards", "query"}, Datasources: []string{"uid-prom"}}, "REAL-TOKEN")
	if err != nil || len(res.Families) != 2 || res.Families["dashboards"] != 200 || res.Families["query"] != 200 {
		t.Fatalf("%+v %v", res, err)
	}
	all := tp.all()
	// health first, then one probe per family (query probes its one UID)
	if len(all) != 3 {
		t.Fatalf("%+v", all)
	}
	for _, s := range all {
		if s.Header.Get("Authorization") != "Bearer REAL-TOKEN" {
			t.Fatalf("%+v", s)
		}
	}
}

func TestAllowlistedFoldersAreReadOnlyWhereWritesAreOpen(t *testing.T) {
	tp, up, rt := hostSide(t)
	e := Env{BaseURL: up.URL, Families: []string{"dashboards"}, Folders: []string{"agent-sbx"}}
	if res, _ := CheckEnv(t.Context(), rt, e, "REAL-TOKEN"); len(res.Folders) != 0 {
		t.Fatalf("%+v", res)
	}
	e.Writes = []string{"dashboards"}
	res, _ := CheckEnv(t.Context(), rt, e, "REAL-TOKEN")
	if res.Folders["agent-sbx"] != 200 {
		t.Fatalf("%+v", res)
	}
	var paths []string
	for _, s := range tp.all() {
		paths = append(paths, s.Path)
		if s.Method != "GET" {
			t.Fatalf("a probe wrote: %+v", s)
		}
	}
	if !contains(paths, "/api/folders/agent-sbx") {
		t.Fatalf("%v", paths)
	}
}

func TestAQueryFamilyWithNoUIDsReportsNone(t *testing.T) {
	_, up, rt := hostSide(t)
	res, err := CheckEnv(t.Context(), rt, Env{BaseURL: up.URL, Families: []string{"query"}}, "REAL-TOKEN")
	if err != nil || res.Families["query"] != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestAnInstanceThatIsNotGrafanaIsNamed(t *testing.T) {
	tp, up, rt := hostSide(t)
	tp.script("/api/health", 404, "")
	if _, err := CheckEnv(t.Context(), rt, Env{BaseURL: up.URL}, "REAL-TOKEN"); err == nil || !strings.Contains(err.Error(), "not answering as a Grafana instance") {
		t.Fatalf("%v", err)
	}
}

func TestCheckReportsAFamilyTheAccountCannotRead(t *testing.T) {
	tp, up, rt := hostSide(t)
	tp.script("/api/v1/provisioning/alert-rules", 403, "{}")
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("grafana", "prod", map[string]string{"token": "REAL-TOKEN"})
	cfg, _ := json.Marshal(Env{BaseURL: up.URL, Families: []string{"alerts", "dashboards"}})
	out, err := Service{Transport: rt}.Check(t.Context(), "prod", cfg, cs)
	if err == nil || !strings.Contains(out, "alerts       answered 403") || !strings.Contains(out, "dashboards   open") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRowsComeBackAsUIDAndDescriptionPairs(t *testing.T) {
	tp, up, rt := hostSide(t)
	tp.script("/api/datasources", 200, `[{"uid": "uid-prom", "name": "Prometheus", "type": "prometheus"}, {"uid": "uid-loki", "name": "Loki"}, {"name": "broken, no uid"}]`)
	rows, err := ListDatasources(t.Context(), rt, Env{BaseURL: up.URL}, "REAL-TOKEN")
	if err != nil || fmt.Sprint(rows) != "[{uid-prom Prometheus (prometheus)} {uid-loki Loki}]" {
		t.Fatalf("%v %v", rows, err)
	}
	if tp.all()[0].Header.Get("Authorization") != "Bearer REAL-TOKEN" {
		t.Fatal("no real token")
	}
}

func TestNestedFoldersCarryTheirParentTitle(t *testing.T) {
	tp, up, rt := hostSide(t)
	tp.script("/api/search?type=dash-folder&limit=5000", 200, `[{"uid": "ops", "title": "Ops"}, {"uid": "agent-sbx", "title": "Agent", "folderTitle": "Ops"}, {"title": "no uid"}]`)
	rows, err := ListFolders(t.Context(), rt, Env{BaseURL: up.URL}, "REAL-TOKEN")
	if err != nil || fmt.Sprint(rows) != "[{ops Ops} {agent-sbx Ops / Agent}]" {
		t.Fatalf("%v %v", rows, err)
	}
}
