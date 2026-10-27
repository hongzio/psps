package ddproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Ported from isolate's test_ddproxy_wire.py: what actually leaves the Mac,
// on live sockets. The workspace token is swapped for the real key pair and
// never forwarded, refusals happen before upstream is dialled, and the
// enforced log scope is already welded on by the time the request leaves.

type seen struct {
	URL    string // as the proxy addressed it: https://api.<site>/...
	Method string
	Header http.Header
	Body   string
}

// tap is the upstream. The proxy addresses https://api.<site>; the
// transport records that and delivers the request here instead.
type tap struct {
	srv *httptest.Server

	mu   sync.Mutex
	reqs []seen
}

func newTap(t *testing.T) *tap {
	tp := &tap{}
	tp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tp.mu.Lock()
		tp.reqs[len(tp.reqs)-1].Body = string(b)
		tp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "42")
		io.WriteString(w, `{"valid": true}`)
	}))
	t.Cleanup(tp.srv.Close)
	return tp
}

func (tp *tap) RoundTrip(r *http.Request) (*http.Response, error) {
	tp.mu.Lock()
	tp.reqs = append(tp.reqs, seen{URL: r.URL.String(), Method: r.Method, Header: r.Header.Clone()})
	tp.mu.Unlock()
	u, _ := url.Parse(tp.srv.URL)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func (tp *tap) all() []seen {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return append([]seen(nil), tp.reqs...)
}

func (tp *tap) reset() {
	tp.mu.Lock()
	tp.reqs = nil
	tp.mu.Unlock()
}

const token = "WORKSPACE-TOKEN"

type harness struct {
	t    *testing.T
	tap  *tap
	in   *instance
	base string

	mu   sync.Mutex
	view proxy.View
}

func newHarness(t *testing.T, prod Env) *harness {
	t.Helper()
	cs := &creds.Store{Path: t.TempDir() + "/credentials"}
	cs.Write("datadog", "prod", map[string]string{"api_key": "REAL-API", "app_key": "REAL-APP"})
	h := &harness{t: t, tap: newTap(t)}
	h.view = proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
	h.view.Exposure = store.Exposure{Isolation: "work", Service: "datadog", Default: "prod"}
	h.setEnv("prod", prod, token)
	h.in = Service{Transport: h.tap}.New(proxy.Host{Isolation: "work", View: h.getView, Creds: cs, Log: log.New(io.Discard, "", 0)}).(*instance)
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
	b, _ := json.Marshal(e)
	h.view.Envs[name] = b
	if !strings.Contains(","+strings.Join(h.view.Exposure.Envs, ",")+",", ","+name+",") {
		h.view.Exposure.Envs = append(h.view.Exposure.Envs, name)
	}
	h.view.Tokens[name] = tok
}

func (h *harness) edit(f func(v *proxy.View)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(&h.view)
}

func (h *harness) getView() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.view, nil
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

func reason(t *testing.T, body string) string {
	t.Helper()
	var e struct{ Errors []string }
	if err := json.Unmarshal([]byte(body), &e); err != nil || len(e.Errors) != 1 {
		t.Fatalf("not Datadog's error shape: %q", body)
	}
	return e.Errors[0]
}

func key(tok string) map[string]string { return map[string]string{"DD-API-KEY": tok} }

func readEnv() Env {
	return Env{Site: "datadoghq.com", Families: []string{"monitors", "logs"}, LogsQuery: "service:project"}
}

func TestTheWrongTokenIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t, readEnv())
	code, body, _ := h.do("GET", "/api/v1/monitor", key("not-the-token"), "")
	if code != 403 || !strings.Contains(reason(t, body), "workspace token") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q %d", code, body, len(h.tap.all()))
	}
}

func TestTheTokenIsSwappedForTheRealPair(t *testing.T) {
	h := newHarness(t, readEnv())
	code, _, _ := h.do("GET", "/api/v1/monitor", map[string]string{"DD-API-KEY": token, "DD-APPLICATION-KEY": "smuggled", "Authorization": "Bearer smuggled"}, "")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.tap.all()[0]
	if s.URL != "https://api.datadoghq.com/api/v1/monitor" || s.Header.Get("DD-API-KEY") != "REAL-API" || s.Header.Get("DD-APPLICATION-KEY") != "REAL-APP" {
		t.Fatalf("upstream saw %+v", s)
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			if strings.Contains(v, token) || strings.Contains(v, "smuggled") {
				t.Fatalf("%s: %q rode through", k, v)
			}
		}
	}
}

func TestTheDatadogpyParamFormIsAcceptedAndStripped(t *testing.T) {
	h := newHarness(t, readEnv())
	code, _, _ := h.do("GET", "/api/v1/monitor?api_key="+token+"&application_key=x&page_size=1", nil, "")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	u := h.tap.all()[0].URL
	if strings.Contains(u, "api_key") || strings.Contains(u, "application_key") || !strings.Contains(u, "page_size=1") {
		t.Fatalf("%s", u)
	}
}

func TestAFamilyOffTheAllowlistNamesTheOperatorCommand(t *testing.T) {
	h := newHarness(t, readEnv())
	code, body, _ := h.do("GET", "/api/v1/dashboard", key(token), "")
	if code != 403 || !strings.Contains(reason(t, body), "gero env edit datadog prod --allow-api dashboards") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAWriteNeverReachesUpstreamWhenNoneAreOpen(t *testing.T) {
	h := newHarness(t, readEnv())
	code, body, _ := h.do("POST", "/api/v1/monitor", key(token), "{}")
	r := reason(t, body)
	if code != 403 || !strings.Contains(r, "is a write") || !strings.Contains(r, "gero env edit datadog prod --allow-write monitors") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, r)
	}
}

func TestLogSearchIsWrappedBeforeItLeaves(t *testing.T) {
	h := newHarness(t, readEnv())
	code, _, _ := h.do("POST", "/api/v2/logs/events/search", map[string]string{"DD-API-KEY": token, "Content-Type": "application/json"}, `{"filter": {"query": "status:error"}}`)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	var sent struct{ Filter struct{ Query string } }
	json.Unmarshal([]byte(h.tap.all()[0].Body), &sent)
	if sent.Filter.Query != "(status:error) AND (service:project)" {
		t.Fatalf("%q", h.tap.all()[0].Body)
	}
}

func TestTheGetLogVariantIsWrappedBeforeItLeaves(t *testing.T) {
	h := newHarness(t, readEnv())
	h.do("GET", "/api/v2/logs/events?filter%5Bquery%5D=service%3Aother&page%5Blimit%5D=5", key(token), "")
	u, _ := url.Parse(h.tap.all()[0].URL)
	if q := u.Query(); q.Get("filter[query]") != "(service:other) AND (service:project)" || len(q["filter[query]"]) != 1 || q.Get("page[limit]") != "5" {
		t.Fatalf("%s", u)
	}
}

func TestValidateNeedsTheTokenButNoFamily(t *testing.T) {
	h := newHarness(t, Env{})
	if code, _, _ := h.do("GET", "/api/v1/validate", key(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
}

func TestRateLimitHeadersComeBackThrough(t *testing.T) {
	h := newHarness(t, readEnv())
	if _, _, hdr := h.do("GET", "/api/v1/validate", key(token), ""); hdr.Get("X-RateLimit-Remaining") != "42" {
		t.Fatalf("%v", hdr)
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := newHarness(t, readEnv())
	h.in.buckets = &proxy.Buckets{PerMinute: 60, Burst: 1}
	first, _, _ := h.do("GET", "/api/v1/validate", key(token), "")
	second, _, hdr := h.do("GET", "/api/v1/validate", key(token), "")
	if first != 200 || second != 429 || hdr.Get("Retry-After") == "" || len(h.tap.all()) != 1 {
		t.Fatalf("%d %d %v %d", first, second, hdr, len(h.tap.all()))
	}
}

// --- an env with one write family open: opening dashboards does not open
// monitors, since muting a monitor has the blast radius of a deploy ---

func writeEnv() Env {
	return Env{Site: "datadoghq.com", Families: []string{"monitors", "dashboards"}, Writes: []string{"dashboards"}}
}

func (h *harness) post(method, path string) (int, string) {
	code, body, _ := h.do(method, path, map[string]string{"DD-API-KEY": token, "Content-Type": "application/json"}, `{"title": "x"}`)
	return code, body
}

func TestAnOpenWriteReachesUpstreamWithTheRealPair(t *testing.T) {
	h := newHarness(t, writeEnv())
	if code, _ := h.post("POST", "/api/v1/dashboard"); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.tap.all()[0]
	if s.Method != "POST" || s.URL != "https://api.datadoghq.com/api/v1/dashboard" || s.Header.Get("DD-API-KEY") != "REAL-API" ||
		s.Header.Get("DD-APPLICATION-KEY") != "REAL-APP" || s.Body != `{"title": "x"}` {
		t.Fatalf("%+v", s)
	}
}

func TestAnUpdateAndADeleteAreOpenToo(t *testing.T) {
	h := newHarness(t, writeEnv())
	if code, _ := h.post("PUT", "/api/v1/dashboard/abc-def"); code != 200 {
		t.Fatalf("PUT %d", code)
	}
	if code, _, _ := h.do("DELETE", "/api/v1/dashboard/abc-def", key(token), ""); code != 200 {
		t.Fatalf("DELETE %d", code)
	}
}

func TestTheOtherFamiliesWritesStayShut(t *testing.T) {
	h := newHarness(t, writeEnv())
	for _, c := range [][2]string{{"POST", "/api/v1/monitor"}, {"POST", "/api/v1/monitor/1234/mute"}, {"POST", "/api/v1/downtime"}} {
		h.tap.reset()
		code, body := h.post(c[0], c[1])
		if code != 403 || !strings.Contains(reason(t, body), "--allow-write monitors") || len(h.tap.all()) != 0 {
			t.Errorf("%s: %d %q", c[1], code, body)
		}
	}
}

func TestMutingTheWholeOrgIsOffTheTableEntirely(t *testing.T) {
	h := newHarness(t, Env{Families: []string{"monitors"}, Writes: []string{"monitors"}})
	code, body := h.post("POST", "/api/v1/monitor/mute_all")
	if code != 404 || !strings.Contains(reason(t, body), "not a modeled write") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestReadsOnAWriteOpenFamilyStillWork(t *testing.T) {
	h := newHarness(t, writeEnv())
	if code, _, _ := h.do("GET", "/api/v1/dashboard", key(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
}

func TestAWriteToAFamilyThatIsNotEvenReadOpenNamesTheRead(t *testing.T) {
	// families is checked before writes, so the operator hears about the
	// missing half they have to fix first.
	h := newHarness(t, Env{Families: []string{"monitors"}, Writes: []string{"dashboards"}})
	code, body := h.post("POST", "/api/v1/dashboard")
	if code != 403 || !strings.Contains(reason(t, body), "--allow-api dashboards") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

// --- gero ---

func TestAChunkedBodyIsRefused(t *testing.T) {
	h := newHarness(t, writeEnv())
	req, _ := http.NewRequest("POST", h.base+"/api/v1/dashboard", io.NopCloser(strings.NewReader("{}")))
	req.ContentLength = -1
	req.Header.Set("DD-API-KEY", token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 411 || len(h.tap.all()) != 0 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestAnOversizedLogSearchIsRefusedUnread(t *testing.T) {
	h := newHarness(t, readEnv())
	big := fmt.Sprintf(`{"filter": {"query": "%s"}}`, strings.Repeat("x", maxScopedBody))
	code, _, _ := h.do("POST", "/api/v2/logs/events/search", key(token), big)
	if code != 413 || len(h.tap.all()) != 0 {
		t.Fatalf("%d", code)
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t, readEnv())
	h.edit(func(v *proxy.View) { v.Enabled = false })
	code, body, _ := h.do("GET", "/api/v1/monitor", key(token), "")
	if code != 503 || !strings.Contains(reason(t, body), "gero iso expose work datadog") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAnIsolationWithNoTokenMintedRefusesAnEmptyKey(t *testing.T) {
	h := newHarness(t, readEnv())
	h.edit(func(v *proxy.View) { v.Tokens = map[string]string{} })
	for _, hdr := range []map[string]string{nil, key("")} {
		if code, _, _ := h.do("GET", "/api/v1/validate?api_key=", hdr, ""); code != 403 {
			t.Fatalf("%d", code)
		}
	}
	if len(h.tap.all()) != 0 {
		t.Fatal("reached upstream")
	}
}

func TestARemovedEnvironmentSaysSo(t *testing.T) {
	h := newHarness(t, readEnv())
	h.edit(func(v *proxy.View) {
		delete(v.Envs, "prod")
		v.Missing = []string{"prod"}
	})
	code, body, _ := h.do("GET", "/api/v1/monitor", key(token), "")
	if code != 503 || !strings.Contains(reason(t, body), "removed from gero's registry") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAMissingSecretIsARefusalNotACrash(t *testing.T) {
	h := newHarness(t, readEnv())
	h.setEnv("ghost", readEnv(), "GHOST-TOKEN")
	code, body, _ := h.do("GET", "/api/v1/monitor", key("GHOST-TOKEN"), "")
	if code != 503 || !strings.Contains(reason(t, body), "[datadog ghost]") || len(h.tap.all()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestTheSiteDecidesTheUpstreamHost(t *testing.T) {
	h := newHarness(t, Env{Site: "datadoghq.eu", Families: []string{"monitors"}})
	h.do("GET", "/api/v1/monitor", key(token), "")
	if u := h.tap.all()[0].URL; u != "https://api.datadoghq.eu/api/v1/monitor" {
		t.Fatalf("%s", u)
	}
}

// --- `gero env check datadog` ---

func TestEveryAllowedFamilyIsProbedWithBothKeys(t *testing.T) {
	tp := newTap(t)
	cs := &creds.Store{Path: t.TempDir() + "/credentials"}
	cs.Write("datadog", "prod", map[string]string{"api_key": "REAL-API", "app_key": "REAL-APP"})
	cfg, _ := json.Marshal(Env{Site: "datadoghq.com", Families: []string{"monitors", "logs"}, Writes: []string{"monitors"}, LogsQuery: "service:project"})
	out, err := Service{Transport: tp}.Check(t.Context(), "prod", cfg, cs)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"api.datadoghq.com — API key valid", "logs       open\n", "monitors   open (WRITE)", "writes     monitors — not probed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	reqs := tp.all()
	// validate first, then one probe per family
	if len(reqs) != 3 || !strings.HasSuffix(reqs[0].URL, "/api/v1/validate") {
		t.Fatalf("%+v", reqs)
	}
	for _, s := range reqs {
		if s.Header.Get("DD-API-KEY") != "REAL-API" || s.Header.Get("DD-APPLICATION-KEY") != "REAL-APP" {
			t.Fatalf("%+v", s)
		}
		if s.Method != "GET" && s.Method != "POST" {
			t.Fatalf("the check wrote: %+v", s)
		}
	}
}
