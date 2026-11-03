package slackproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/store"
)

// Ported from isolate's test_slackproxy_wire.py: what actually leaves the
// Mac, on live sockets. The workspace token is swapped for the real Slack
// token and never forwarded, refusals happen before upstream is dialled,
// and the caller's body and params ride through untouched. slack.com and
// files.slack.com are two TLS test servers.

type seen struct {
	Host, Method, Path string
	Header             http.Header
	Body               string
}

type upstream struct {
	label string
	srv   *httptest.Server
	log   *[]seen
	mu    *sync.Mutex

	amu    sync.Mutex
	answer http.HandlerFunc
}

func (u *upstream) set(h http.HandlerFunc) {
	u.amu.Lock()
	u.answer = h
	u.amu.Unlock()
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	*u.log = append(*u.log, seen{u.label, r.Method, r.URL.RequestURI(), r.Header.Clone(), string(b)})
	u.mu.Unlock()
	u.amu.Lock()
	h := u.answer
	u.amu.Unlock()
	h(w, r)
}

func okAnswer(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-OAuth-Scopes", "lists:read,channels:read")
	w.Header().Set("Retry-After", "30")
	io.WriteString(w, `{"ok": true}`)
}

const token = "WORKSPACE-TOKEN"

type harness struct {
	t          *testing.T
	api, files *upstream
	base       string
	in         *instance

	mu   sync.Mutex
	reqs []seen

	vmu  sync.Mutex
	view proxy.View
}

var png = []byte("\x89PNG\r\n\x1a\nbytes")

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t}
	mk := func(label string, def http.HandlerFunc) *upstream {
		u := &upstream{label: label, log: &h.reqs, mu: &h.mu, answer: def}
		u.srv = httptest.NewTLSServer(u)
		t.Cleanup(u.srv.Close)
		return u
	}
	h.api = mk("slack.com", okAnswer)
	h.files = mk("files.slack.com", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("slack", "main", map[string]string{"token": "xoxp-real-token"})
	h.view = proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{}}
	h.view.Exposure = store.Exposure{IsolationID: "work", Service: "slack", Default: "main"}
	h.setEnv("main", Env{}, token)
	svc := Service{API: h.api.srv.URL, Files: h.files.srv.URL, Transport: h.api.srv.Client().Transport}
	h.in = svc.New(proxy.Host{Name: func() string { return "work" }, View: h.getView, Creds: cs, Log: log.New(io.Discard, "", 0)}).(*instance)
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
	h.vmu.Lock()
	defer h.vmu.Unlock()
	b, _ := json.Marshal(e)
	h.view.Envs[name] = b
	if !strings.Contains(","+strings.Join(h.view.Exposure.Envs, ",")+",", ","+name+",") {
		h.view.Exposure.Envs = append(h.view.Exposure.Envs, name)
	}
	h.view.Tokens[name] = tok
}

func (h *harness) edit(f func(v *proxy.View)) {
	h.vmu.Lock()
	defer h.vmu.Unlock()
	f(&h.view)
}

func (h *harness) getView() (proxy.View, error) {
	h.vmu.Lock()
	defer h.vmu.Unlock()
	return h.view, nil
}

func (h *harness) seen() []seen {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]seen(nil), h.reqs...)
}

func (h *harness) reset() {
	h.mu.Lock()
	h.reqs = nil
	h.mu.Unlock()
}

func (h *harness) do(method, path string, header map[string]string, body string) (int, []byte, http.Header) {
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
	return resp.StatusCode, b, resp.Header
}

func auth(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func errorOf(t *testing.T, body []byte) string {
	t.Helper()
	var a struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &a); err != nil || a.OK {
		t.Fatalf("not a Slack-shaped refusal: %q", body)
	}
	return a.Error
}

func TestTheWrongTokenIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("GET", "/api/auth.test", auth("not-the-token"), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "workspace token") || len(h.seen()) != 0 {
		t.Fatalf("%d %q %d", code, body, len(h.seen()))
	}
}

func TestTheTokenIsSwappedForTheRealOne(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("GET", "/api/slackLists.items.list?list_id=F1", auth(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.seen()[0]
	if s.Host != "slack.com" || s.Header.Get("Authorization") != "Bearer xoxp-real-token" || !strings.Contains(s.Path, "list_id=F1") {
		t.Fatalf("%+v", s)
	}
	for k, vs := range s.Header {
		for _, v := range vs {
			if strings.Contains(v, token) {
				t.Fatalf("the workspace token went upstream in %s", k)
			}
		}
	}
}

func TestAMessagesCallOnAListsEnvNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("GET", "/api/conversations.history?channel=C0123", auth(token), "")
	if code != 404 || !strings.Contains(errorOf(t, body), "--allow-api messages") || len(h.seen()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestTheDenyClampHoldsOnTheWire(t *testing.T) {
	h := newHarness(t)
	h.setEnv("main", Env{Families: families("messages"), DenyChannels: []string{"C0666"}}, token)
	if code, _, _ := h.do("GET", "/api/conversations.history?channel=C0123&limit=1", auth(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	if !strings.Contains(h.seen()[0].Path, "channel=C0123") {
		t.Fatalf("%+v", h.seen())
	}
	code, body, _ := h.do("GET", "/api/conversations.history?channel=C0666", auth(token), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "C0666") || len(h.seen()) != 1 {
		t.Fatalf("%d %q %d", code, body, len(h.seen()))
	}
	// Nor in the body, behind an innocent query.
	code, _, _ = h.do("POST", "/api/conversations.history?channel=C0123",
		map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/x-www-form-urlencoded"}, "channel=C0666")
	if code != 403 || len(h.seen()) != 1 {
		t.Fatalf("a body channel rode through: %d", code)
	}
}

func TestSearchCarriesTheExclusionsUpstream(t *testing.T) {
	h := newHarness(t)
	h.in.names = fakeNames{"C0666": "secrets"}
	h.setEnv("main", Env{Families: families("search"), DenyChannels: []string{"C0666"}}, token)
	if code, _, _ := h.do("GET", "/api/search.messages?query=deploy", auth(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	u, _ := url.Parse(h.seen()[0].Path)
	if got := u.Query().Get("query"); got != "deploy -in:#secrets" {
		t.Fatalf("%q", got)
	}
}

func TestSearchWithoutADenyListRidesUntouched(t *testing.T) {
	h := newHarness(t)
	h.setEnv("main", Env{Families: families("search")}, token)
	if code, _, _ := h.do("GET", "/api/search.messages?query=deploy", auth(token), ""); code != 200 || !strings.Contains(h.seen()[0].Path, "query=deploy") {
		t.Fatalf("%d %+v", code, h.seen())
	}
}

func TestATokenQueryParamIsStrippedNeverForwarded(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.do("GET", "/api/slackLists.items.list?token="+token+"&list_id=F1", auth(token), ""); code != 200 {
		t.Fatalf("%d", code)
	}
	if p := h.seen()[0].Path; strings.Contains(p, "token=") || !strings.Contains(p, "list_id=F1") {
		t.Fatalf("%q", p)
	}
}

func TestAPostBodyRidesThroughUntouched(t *testing.T) {
	h := newHarness(t)
	payload := `{"list_id": "F1", "limit": 5}`
	code, _, _ := h.do("POST", "/api/slackLists.items.list",
		map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}, payload)
	if code != 200 || h.seen()[0].Body != payload {
		t.Fatalf("%d %+v", code, h.seen())
	}
}

func TestAnUnmodeledMethodNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.do("POST", "/api/chat.postMessage", auth(token), "{}")
	if code != 404 || errorOf(t, body) == "" || len(h.seen()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAMissingSecretIsARefusalNotACrash(t *testing.T) {
	h := newHarness(t)
	h.setEnv("ghost", Env{}, "GHOST-TOKEN")
	code, body, _ := h.do("GET", "/api/auth.test", auth("GHOST-TOKEN"), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "[slack ghost]") || len(h.seen()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestSlackRateHeadersComeBackThrough(t *testing.T) {
	h := newHarness(t)
	if _, _, hdr := h.do("GET", "/api/auth.test", auth(token), ""); hdr.Get("Retry-After") != "30" {
		t.Fatalf("%v", hdr)
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.in.bucket = proxy.NewTokenBucket(60, 1, nil)
	first, _, _ := h.do("GET", "/api/auth.test", auth(token), "")
	second, _, hdr := h.do("GET", "/api/auth.test", auth(token), "")
	if first != 200 || second != 429 || hdr.Get("Retry-After") == "" || len(h.seen()) != 1 {
		t.Fatalf("%d %d %v %d", first, second, hdr, len(h.seen()))
	}
}

func TestAChunkedBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("POST", h.base+"/api/slackLists.items.list", io.NopCloser(strings.NewReader("list_id=F1")))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 411 || len(h.seen()) != 0 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// --- psps's own ---

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) { v.Enabled = false })
	code, body, _ := h.do("GET", "/api/auth.test", auth(token), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "psps iso expose work slack") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAnEnvWithNoTokenMintedRefusesAnEmptyBearer(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) { v.Tokens = map[string]string{} })
	for _, a := range []map[string]string{auth(""), {"Authorization": "Bearer"}, nil} {
		if code, _, _ := h.do("GET", "/api/auth.test", a, ""); code != 403 {
			t.Fatalf("%v: %d", a, code)
		}
	}
	if len(h.seen()) != 0 {
		t.Fatal("upstream was dialled")
	}
}

func TestAnEnvRemovedFromTheRegistrySaysSo(t *testing.T) {
	h := newHarness(t)
	h.edit(func(v *proxy.View) {
		v.Exposure.Envs = append(v.Exposure.Envs, "old")
		v.Tokens["old"] = "OLD-TOKEN"
		v.Missing = []string{"old"}
	})
	code, body, _ := h.do("GET", "/api/auth.test", auth("OLD-TOKEN"), "")
	if code != 503 || !strings.Contains(errorOf(t, body), "removed") || len(h.seen()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestAnOversizedBodyIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.do("POST", "/api/slackLists.items.list", auth(token), strings.Repeat("x", maxBody+1))
	if code != 413 || len(h.seen()) != 0 {
		t.Fatalf("%d", code)
	}
}

// --- files ---

func (h *harness) info(u string, channels []string, mime string) []byte {
	if u == "" {
		u = h.files.srv.URL + "/files-pri/T1-F0123/download/a.png"
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "file": map[string]any{
		"id": "F0123", "mimetype": mime, "channels": channels, "url_private": u, "url_private_download": u}})
	return b
}

func (h *harness) script(info []byte, files http.HandlerFunc) {
	h.api.set(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(info)
	})
	if files != nil {
		h.files.set(files)
	}
	h.setEnv("main", Env{Families: families("files"), DenyChannels: []string{"C0666"}}, token)
}

func (h *harness) download() (int, []byte, http.Header) {
	return h.do("GET", "/files/F0123", auth(token), "")
}

func hosts(s []seen) string {
	var l []string
	for _, x := range s {
		l = append(l, x.Host)
	}
	return strings.Join(l, ",")
}

func TestFilesOnAListsEnvNeverReachUpstream(t *testing.T) {
	h := newHarness(t)
	code, body, _ := h.download()
	if code != 404 || !strings.Contains(errorOf(t, body), "--allow-api files") || len(h.seen()) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestADownloadRelaysTheBytesWithTheRealToken(t *testing.T) {
	h := newHarness(t)
	h.script(h.info("", []string{"C0123"}, "image/png"), nil)
	code, body, hdr := h.download()
	if code != 200 || !bytes.Equal(body, png) || hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("%d %q %v", code, body, hdr)
	}
	s := h.seen()
	if len(s) != 2 || s[0].Host != "slack.com" || !strings.Contains(s[0].Path, "/api/files.info?file=F0123") ||
		s[1].Host != "files.slack.com" || s[1].Path != "/files-pri/T1-F0123/download/a.png" {
		t.Fatalf("%+v", s)
	}
	for _, x := range s {
		if x.Header.Get("Authorization") != "Bearer xoxp-real-token" {
			t.Fatalf("%s got %q", x.Host, x.Header.Get("Authorization"))
		}
	}
}

func TestAFileInADeniedChannelIsNeverFetched(t *testing.T) {
	h := newHarness(t)
	h.script(h.info("", []string{"C0123", "C0666"}, "image/png"), nil)
	code, body, _ := h.download()
	if code != 403 || !strings.Contains(errorOf(t, body), "C0666") || hosts(h.seen()) != "slack.com" {
		t.Fatalf("%d %q %s", code, body, hosts(h.seen()))
	}
}

func TestTheTokenNeverGoesToAnotherHostOnTheWire(t *testing.T) {
	h := newHarness(t)
	fu, _ := url.Parse(h.files.srv.URL)
	other := httptest.NewTLSServer(http.NotFoundHandler())
	defer other.Close()
	ou, _ := url.Parse(other.URL)
	for _, u := range []string{
		"https://evil.example/x.png",
		"http://" + fu.Host + "/x.png",
		"https://" + fu.Host + ".evil.example/x.png",
		"https://user@" + fu.Host + "/x.png",
		"https://" + ou.Host + "/x.png",
	} {
		h.reset()
		h.script(h.info(u, []string{"C0123"}, "image/png"), nil)
		code, _, _ := h.download()
		if code != 502 || hosts(h.seen()) != "slack.com" {
			t.Errorf("%s: %d %s", u, code, hosts(h.seen()))
		}
	}
}

func TestARedirectIsRefusedWithAReasonNotFollowed(t *testing.T) {
	h := newHarness(t)
	h.script(h.info("", []string{"C0123"}, "image/png"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://axis.slack.com/?redir=x")
		w.WriteHeader(302)
	})
	code, body, _ := h.download()
	if code != 502 || !strings.Contains(errorOf(t, body), "files:read") || len(h.seen()) != 2 {
		t.Fatalf("%d %q %d", code, body, len(h.seen()))
	}
}

func TestALoginPageInPlaceOfTheFileIsRefused(t *testing.T) {
	h := newHarness(t)
	h.script(h.info("", []string{"C0123"}, "image/png"), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>sign in</html>")
	})
	code, body, _ := h.download()
	if code != 502 || !strings.Contains(errorOf(t, body), "login") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestSlacksOwnRefusalOfTheLookupComesBackAsASentence(t *testing.T) {
	h := newHarness(t)
	h.script([]byte(`{"ok": false, "error": "file_not_found"}`), nil)
	code, body, _ := h.download()
	if code != 404 || !strings.Contains(errorOf(t, body), "file_not_found") || len(h.seen()) != 1 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestFilesInfoIsClampedOnTheAnswer(t *testing.T) {
	h := newHarness(t)
	h.script(h.info("", []string{"C0666"}, "image/png"), nil)
	code, body, _ := h.do("GET", "/api/files.info?file=F0123", auth(token), "")
	if code != 403 || !strings.Contains(errorOf(t, body), "C0666") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestFilesInfoInOpenChannelsRelaysUntouched(t *testing.T) {
	h := newHarness(t)
	want := h.info("", []string{"C0123"}, "image/png")
	h.script(want, nil)
	code, body, _ := h.do("GET", "/api/files.info?file=F0123", auth(token), "")
	if code != 200 || !bytes.Equal(body, want) {
		t.Fatalf("%d %q", code, body)
	}
}

// --- channel names ---

func TestResolutionAsksWithTheRealTokenAndCaches(t *testing.T) {
	h := newHarness(t)
	h.api.set(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok": true, "channel": {"name": "secrets"}}`)
	})
	api, _ := url.Parse(h.api.srv.URL)
	names := &ChannelNames{API: api, Transport: h.api.srv.Client().Transport, TTL: time.Hour}
	for range 2 {
		if n, d := names.Get("main", "xoxp-real", "C0666"); d != nil || n != "secrets" {
			t.Fatalf("%q %v", n, d)
		}
	}
	s := h.seen()
	if len(s) != 1 || !strings.Contains(s[0].Path, "conversations.info?channel=C0666") || s[0].Header.Get("Authorization") != "Bearer xoxp-real" {
		t.Fatalf("%+v", s) // the second answer is cached
	}
}

func TestAFailedResolutionRefusesRatherThanLeaking(t *testing.T) {
	h := newHarness(t)
	h.api.set(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok": false, "error": "channel_not_found"}`)
	})
	api, _ := url.Parse(h.api.srv.URL)
	_, d := (&ChannelNames{API: api, Transport: h.api.srv.Client().Transport}).Get("main", "xoxp-real", "C0000")
	mustDeny(t, d, "channel_not_found", "refused")
}

func TestAnUnresolvableDeniedChannelRefusesSearchOnTheWire(t *testing.T) {
	h := newHarness(t)
	h.api.set(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"ok": false, "error": "channel_not_found"}`)
	})
	h.setEnv("main", Env{Families: families("search"), DenyChannels: []string{"C0666"}}, token)
	code, _, _ := h.do("GET", "/api/search.messages?query=deploy", auth(token), "")
	if code != 502 {
		t.Fatalf("%d", code)
	}
	for _, s := range h.seen() {
		if strings.Contains(s.Path, "search.messages") {
			t.Fatal("search went upstream without its exclusions")
		}
	}
}

// --- `psps env check slack` ---

func checkHarness(t *testing.T, answer string, scopes string) (*harness, *creds.Store, Service) {
	h := newHarness(t)
	h.api.set(func(w http.ResponseWriter, _ *http.Request) {
		if scopes != "" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		io.WriteString(w, answer)
	})
	cs := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	cs.Write("slack", "main", map[string]string{"token": "xoxp-real-token"})
	return h, cs, Service{API: h.api.srv.URL, Transport: h.api.srv.Client().Transport}
}

func TestAuthTestIsProbedWithTheRealToken(t *testing.T) {
	h, cs, svc := checkHarness(t, `{"ok": true, "team": "axis", "user": "hongzio", "url": "https://axis.slack.com/"}`, "lists:read,channels:read")
	out, err := svc.Check(t.Context(), "main", json.RawMessage(`{"families": ["lists"]}`), cs)
	if err != nil || !strings.Contains(out, "https://axis.slack.com/ as hongzio (team axis)") || !strings.Contains(out, "lists      open (lists:read granted)") {
		t.Fatalf("%q %v", out, err)
	}
	s := h.seen()[0]
	if s.Path != "/api/auth.test" || s.Method != "POST" || s.Header.Get("Authorization") != "Bearer xoxp-real-token" {
		t.Fatalf("%+v", s)
	}
}

func TestAMissingScopeIsNamed(t *testing.T) {
	_, cs, svc := checkHarness(t, `{"ok": true, "team": "axis", "user": "hongzio"}`, "lists:read")
	out, err := svc.Check(t.Context(), "main", json.RawMessage(`{"families": ["lists", "messages"], "deny_channels": []}`), cs)
	if err == nil || !strings.Contains(out, "the token lacks channels:history") || !strings.Contains(out, "denied     (no channels") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestANotOkAnswerIsARefusalNamingSlacksError(t *testing.T) {
	_, cs, svc := checkHarness(t, `{"ok": false, "error": "invalid_auth"}`, "")
	if _, err := svc.Check(t.Context(), "main", json.RawMessage(`{}`), cs); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("%v", err)
	}
}

// --- what the isolation gets ---

func TestTheWorkspaceGetsATokenPerEnv(t *testing.T) {
	v := proxy.View{Isolation: "work", Enabled: true,
		Envs:     map[string]json.RawMessage{"main": []byte(`{"families": ["messages"], "deny_channels": ["C0666"]}`), "us-east": []byte(`{}`)},
		Tokens:   map[string]string{"main": "t1", "us-east": "t2"},
		Exposure: store.Exposure{Envs: []string{"main", "us-east"}, Default: "main"}}
	ws, err := Service{}.Workspace(v)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(ws.Env, "\n")
	for _, want := range []string{"SLACK_TOKEN=t1", "SLACK_API_URL=http://127.0.0.1:3024/api", "SLACK_TOKEN_MAIN=t1", "SLACK_TOKEN_US_EAST=t2"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in\n%s", want, env)
		}
	}
	skill := ws.Files[0].Content
	for _, want := range []string{"name: psps-slack", "- `main` (default) — messages — denied channels: C0666", "- `us-east` — lists", "${SLACK_API_URL%/api}/files/<F...>"} {
		if !strings.Contains(skill, want) {
			t.Errorf("the skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "%!") {
		t.Fatalf("a formatting slip:\n%s", skill)
	}
}
