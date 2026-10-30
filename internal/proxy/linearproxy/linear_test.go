package linearproxy

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
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// notionproxy's tests over Linear's table (the table, the refresh and the
// login, against a fake Linear, and what leaves the Mac, on a live
// socket): the workspace token is swapped
// for the access token and never forwarded, refusals happen before
// upstream, a refused tool comes back as a tool result, tools/list and
// initialize are narrowed, an expired access token is refreshed once and
// retried, and nothing that would send the isolation's MCP client off to do
// OAuth of its own ever reaches it.

func msg(method string, params map[string]any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "id": json.Number("1"), "method": method}
	if params != nil {
		m["params"] = params
	}
	return m
}

func call(tool string) map[string]any {
	return msg("tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
}

func fams(l ...string) Env { return Env{Families: &l} }

func TestEveryToolBelongsToExactlyOneFamily(t *testing.T) {
	seen := map[string]string{}
	for f, tools := range Families {
		for _, tool := range tools {
			if prev, ok := seen[tool]; ok {
				t.Errorf("%s in %s and %s", tool, prev, f)
			}
			seen[tool] = f
		}
	}
	for _, p := range ProbeTools {
		if _, ok := seen[p]; ok {
			t.Errorf("probe %s is in a family", p)
		}
	}
}

func TestReadsAndWritesPartitionTheFamilies(t *testing.T) {
	all := append(append([]string(nil), ReadFamilies...), WriteFamilies...)
	sort.Strings(all)
	if !reflect.DeepEqual(all, familyNames()) {
		t.Fatalf("%v vs %v", all, familyNames())
	}
}

func TestWritesAreInWriteFamilies(t *testing.T) {
	for tool, family := range map[string]string{
		"save_issue": "issue-edits", "create_issue": "issue-edits", "update_issue": "issue-edits",
		"save_comment": "comments", "create_comment": "comments", "create_issue_label": "labels",
		"save_project": "project-edits", "save_document": "document-edits", "delete_comment": "deletes",
	} {
		if got := ToolFamily(tool); got != family || !isWrite(got) {
			t.Errorf("%s: %s", tool, got)
		}
	}
}

func TestTheDefaultIsEveryReadAndNoWrite(t *testing.T) {
	if !reflect.DeepEqual(DefaultFamilies, ReadFamilies) {
		t.Fatalf("%v", DefaultFamilies)
	}
	for _, d := range DefaultFamilies {
		if isWrite(d) {
			t.Errorf("%s is a default", d)
		}
	}
	if ToolFamily("new_thing") != "" {
		t.Fatal("an unknown tool has a family")
	}
}

func TestAMissingListMeansTheDefaultReadsAndAnEmptyOneTheProbes(t *testing.T) {
	if got := FamiliesFor(Env{}); !reflect.DeepEqual(got, DefaultFamilies) {
		t.Fatalf("%v", got)
	}
	empty := fams()
	if got := FamiliesFor(empty); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if f, err := Allowed(empty, "main", "search_documentation"); err != nil || f != Probe {
		t.Fatalf("%s %v", f, err)
	}
	if _, err := Allowed(empty, "main", "get_issue"); err == nil {
		t.Fatal("an empty list opened issues")
	}
}

func TestARetiredFamilyOpensNothing(t *testing.T) {
	if got := FamiliesFor(fams("issues", "retired")); !reflect.DeepEqual(got, []string{"issues"}) {
		t.Fatalf("%v", got)
	}
}

func TestTheDefaultEnvReadsIssuesButDoesNotComment(t *testing.T) {
	if f, err := Allowed(Env{}, "main", "get_issue"); err != nil || f != "issues" {
		t.Fatalf("%s %v", f, err)
	}
	if f, err := Allowed(Env{}, "main", "list_users"); err != nil || f != "users" {
		t.Fatalf("%s %v", f, err)
	}
	_, err := Allowed(Env{}, "main", "save_comment")
	if err == nil || !strings.Contains(err.Error(), "--allow-api comments") {
		t.Fatalf("%v", err)
	}
}

func TestAWriteFamilyIsRefusedUntilNamed(t *testing.T) {
	_, err := Allowed(Env{}, "main", "save_issue")
	if err == nil || !strings.Contains(err.Error(), "'issue-edits' write family") || !strings.Contains(err.Error(), "gero env edit linear main --allow-api issue-edits") {
		t.Fatalf("%v", err)
	}
	if f, err := Allowed(fams("issues", "issue-edits"), "main", "save_issue"); err != nil || f != "issue-edits" {
		t.Fatalf("%s %v", f, err)
	}
}

func TestAnUnmodeledToolIsRefusedForEveryEnv(t *testing.T) {
	if _, err := Allowed(fams(familyNames()...), "main", "new_thing"); err == nil || !strings.Contains(err.Error(), "not a modeled tool") {
		t.Fatalf("%v", err)
	}
}

func TestTheProtocolMethodsForward(t *testing.T) {
	for m, want := range map[string]Action{"initialize": {true, "initialize"}, "tools/list": {true, "tools/list"}, "ping": {true, ""}} {
		if got, err := Classify(msg(m, nil), Env{}, "m"); err != nil || got != want {
			t.Errorf("%s: %v %v", m, got, err)
		}
	}
	if got, _ := Classify(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, Env{}, "m"); got != (Action{Forward: true}) {
		t.Fatalf("%v", got)
	}
	if got, err := Classify(call("list_issues"), Env{}, "m"); err != nil || !got.Forward {
		t.Fatalf("%v %v", got, err)
	}
}

func status(err error) int {
	if d, ok := err.(*Denied); ok {
		return d.Status
	}
	return 0
}

func TestRefusalsCarryTheirStatus(t *testing.T) {
	if _, err := Classify(call("delete_comment"), Env{}, "m"); status(err) != 403 {
		t.Fatalf("a refused tool: %v", err)
	}
	for _, m := range []string{"resources/list", "resources/read", "prompts/get", "completion/complete", "logging/setLevel"} {
		if _, err := Classify(msg(m, nil), Env{}, "m"); status(err) != 404 {
			t.Errorf("%s: %v", m, err)
		}
	}
	if got, _ := Classify(map[string]any{"jsonrpc": "2.0", "method": "notifications/whatever"}, Env{}, "m"); got.Forward {
		t.Fatal("an unknown notification was forwarded")
	}
	if got, _ := Classify(map[string]any{"jsonrpc": "2.0", "id": 7, "result": map[string]any{}}, Env{}, "m"); !got.Forward {
		t.Fatal("a response to the server was not forwarded")
	}
	for _, junk := range []map[string]any{{}, {"id": 1, "method": "ping"}, {"jsonrpc": "2.0", "id": 1}, msg("tools/call", map[string]any{"arguments": map[string]any{}})} {
		if _, err := Classify(junk, Env{}, "m"); status(err) != 400 {
			t.Errorf("%v: %v", junk, err)
		}
	}
}

func TestInitializeOffersToolsOnly(t *testing.T) {
	r := Narrow("initialize", map[string]any{"capabilities": map[string]any{"tools": map[string]any{"listChanged": true},
		"resources": map[string]any{}, "prompts": map[string]any{}}, "serverInfo": map[string]any{"name": "Linear"}}, Env{}).(map[string]any)
	if !reflect.DeepEqual(r["capabilities"], map[string]any{"tools": map[string]any{"listChanged": true}}) || r["serverInfo"] == nil {
		t.Fatalf("%v", r)
	}
}

func toolNames(r any) []string {
	var out []string
	for _, t := range r.(map[string]any)["tools"].([]any) {
		out = append(out, t.(map[string]any)["name"].(string))
	}
	return out
}

func TestToolsListShowsTheEnvItsFamilies(t *testing.T) {
	var tools []any
	for _, n := range []string{"get_issue", "list_issues", "save_issue", "search_documentation", "linear-get-users", "new_thing"} {
		tools = append(tools, map[string]any{"name": n})
	}
	if got := toolNames(Narrow("tools/list", map[string]any{"tools": tools}, Env{})); !reflect.DeepEqual(got, []string{"get_issue", "list_issues", "search_documentation"}) {
		t.Fatalf("%v", got)
	}
}

func TestAnSSEAnswerIsNarrowedEventByEvent(t *testing.T) {
	answer := `{"jsonrpc": "2.0", "id": 3, "result": {"tools": [{"name": "get_issue"}, {"name": "delete_comment"}]}}`
	raw := []byte("event: message\r\ndata: " + answer + "\r\n\r\n")
	evs := SSEMessages(RewriteBody(raw, "text/event-stream", json.Number("3"), "tools/list", Env{}))
	if len(evs) != 1 || !reflect.DeepEqual(evs[0].Fields, []string{"event: message"}) {
		t.Fatalf("%+v", evs)
	}
	if got := toolNames(evs[0].Message["result"]); !reflect.DeepEqual(got, []string{"get_issue"}) {
		t.Fatalf("%v", got)
	}
}

func TestAnAnswerForAnotherIDIsLeftAlone(t *testing.T) {
	raw := []byte(`{"jsonrpc": "2.0", "id": 9, "result": {"tools": [{"name": "delete_comment"}]}}`)
	if got := RewriteBody(raw, "application/json", json.Number("3"), "tools/list", Env{}); !bytes.Equal(got, raw) {
		t.Fatalf("%s", got)
	}
}

func TestIdentityIsSummarised(t *testing.T) {
	r := map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"id": "u1", "name": "Hong", "email": "hong@example.com"}`}}}
	if got := IdentitySummary(r); got != "Hong <hong@example.com>" {
		t.Fatalf("%q", got)
	}
	r = map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"user": {"displayName": "hong"}}`}}}
	if got := IdentitySummary(r); got != "hong" {
		t.Fatalf("%q", got)
	}
	r = map[string]any{"content": []any{map[string]any{"type": "text", "text": "Hong\nmore"}}}
	if got := IdentitySummary(r); got != "Hong" {
		t.Fatalf("%q", got)
	}
}

// --- a scripted Linear: the MCP endpoint, /token and /register ---

type seen struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

type fakeLinear struct {
	srv *httptest.Server

	mu       sync.Mutex
	mcp      []seen
	forms    []url.Values
	register []map[string]any
	script   []func(w http.ResponseWriter) // the next MCP answers
	tokens   []func(w http.ResponseWriter) // the next /token answers
}

func newLinear(t *testing.T) *fakeLinear {
	n := &fakeLinear{}
	n.srv = httptest.NewServer(n)
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeLinear) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	n.mu.Lock()
	defer n.mu.Unlock()
	switch r.URL.Path {
	case "/token":
		q, _ := url.ParseQuery(string(body))
		n.forms = append(n.forms, q)
		if len(n.tokens) == 0 {
			w.WriteHeader(500)
			return
		}
		next := n.tokens[0]
		n.tokens = n.tokens[1:]
		next(w)
	case "/register":
		var m map[string]any
		json.Unmarshal(body, &m)
		n.register = append(n.register, m)
		w.WriteHeader(201)
		io.WriteString(w, `{"client_id": "cid"}`)
	default:
		n.mcp = append(n.mcp, seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Set-Cookie", "__cf_bm=x")
		w.Header().Set("WWW-Authenticate", `Bearer realm="OAuth"`)
		if len(n.script) > 0 {
			next := n.script[0]
			n.script = n.script[1:]
			next(w)
			return
		}
		var sent map[string]any
		json.Unmarshal(body, &sent)
		w.Header().Set("Content-Type", "application/json")
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": sent["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		w.Write(b)
	}
}

func (n *fakeLinear) seen() []seen {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]seen(nil), n.mcp...)
}

func (n *fakeLinear) answer(f func(w http.ResponseWriter)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.script = append(n.script, f)
}

func (n *fakeLinear) token(status int, body string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tokens = append(n.tokens, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

func jsonAnswer(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func clock(sec int64) func() time.Time { return func() time.Time { return time.Unix(sec, 0) } }

// --- the refresh ---

func refreshSetup(t *testing.T, extra map[string]string) (*fakeLinear, oauth, *creds.Store) {
	n := newLinear(t)
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	vals := map[string]string{"client_id": "cid", "refresh_token": "r1"}
	for k, v := range extra {
		vals[k] = v
	}
	c.Write("linear", "main", vals)
	return n, oauth{issuer: n.srv.URL, transport: http.DefaultTransport, now: clock(1000)}, c
}

func TestAFreshTokenIsUsedAsStored(t *testing.T) {
	n, o, c := refreshSetup(t, map[string]string{"access_token": "a1", "expires_at": "2000"})
	if got, err := o.accessToken(t.Context(), c, "main", ""); err != nil || got != "a1" || len(n.forms) != 0 {
		t.Fatalf("%q %v %v", got, err, n.forms)
	}
}

func TestAStaleTokenIsRefreshedAndStored(t *testing.T) {
	n, o, c := refreshSetup(t, map[string]string{"access_token": "a1", "expires_at": "1030"})
	n.token(200, `{"access_token": "a2", "expires_in": 3600, "refresh_token": "r2"}`)
	if got, err := o.accessToken(t.Context(), c, "main", ""); err != nil || got != "a2" {
		t.Fatalf("%q %v", got, err)
	}
	f := n.forms[0]
	if f.Get("grant_type") != "refresh_token" || f.Get("refresh_token") != "r1" || f.Get("client_id") != "cid" || f.Get("resource") != o.resource() {
		t.Fatalf("%v", f)
	}
	stored, _ := c.Read("linear", "main")
	if stored["refresh_token"] != "r2" || stored["access_token"] != "a2" || stored["expires_at"] != "4600" {
		t.Fatalf("%v", stored)
	}
}

func TestARefreshWithoutRotationKeepsTheRefreshToken(t *testing.T) {
	n, o, c := refreshSetup(t, nil)
	n.token(200, `{"access_token": "a2"}`)
	o.accessToken(t.Context(), c, "main", "")
	stored, _ := c.Read("linear", "main")
	if _, has := stored["expires_at"]; stored["refresh_token"] != "r1" || has {
		t.Fatalf("%v", stored)
	}
}

func TestARejectedTokenIsRefreshedEvenBeforeExpiry(t *testing.T) {
	n, o, c := refreshSetup(t, map[string]string{"access_token": "a1", "expires_at": "9999"})
	n.token(200, `{"access_token": "a2"}`)
	if got, _ := o.accessToken(t.Context(), c, "main", "a1"); got != "a2" {
		t.Fatalf("%q", got)
	}
}

func TestSomeoneElsesRefreshIsNotRepeated(t *testing.T) {
	// The rejected token is already gone from the store: another holder
	// refreshed, and its token is the answer.
	n, o, c := refreshSetup(t, map[string]string{"access_token": "a2", "expires_at": "9999"})
	if got, _ := o.accessToken(t.Context(), c, "main", "a1"); got != "a2" || len(n.forms) != 0 {
		t.Fatalf("%q %v", got, n.forms)
	}
}

func TestARevokedLoginNamesTheFix(t *testing.T) {
	n, o, c := refreshSetup(t, nil)
	n.token(400, `{"error": "invalid_grant"}`)
	_, err := o.accessToken(t.Context(), c, "main", "")
	if status(err) != 503 || !strings.Contains(err.Error(), "gero env secret linear main") {
		t.Fatalf("%v", err)
	}
}

func TestNoLoginAtAllNamesTheFix(t *testing.T) {
	n := newLinear(t)
	o := oauth{issuer: n.srv.URL, transport: http.DefaultTransport, now: clock(1000)}
	_, err := o.accessToken(t.Context(), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}, "main", "")
	if status(err) != 503 || !strings.Contains(err.Error(), "gero env secret linear main") {
		t.Fatalf("%v", err)
	}
}

func TestConcurrentRefreshesSpendTheRefreshTokenOnce(t *testing.T) {
	// A rotated refresh token spent twice is a login lost.
	n, o, c := refreshSetup(t, nil)
	n.token(200, `{"access_token": "a2", "expires_in": 3600, "refresh_token": "r2"}`)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.accessToken(t.Context(), c, "main", "")
		}()
	}
	wg.Wait()
	if len(n.forms) != 1 {
		t.Fatalf("refreshed %d times", len(n.forms))
	}
}

// --- the login ---

func TestALoginRegistersAuthorizesAndStores(t *testing.T) {
	n := newLinear(t)
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	svc := Service{Issuer: n.srv.URL, Transport: http.DefaultTransport, Now: clock(1000)}
	p, err := svc.StartLogin(t.Context(), "main", "http://127.0.0.1:5555/callback")
	if err != nil {
		t.Fatal(err)
	}
	if reg := n.register[0]; reg["token_endpoint_auth_method"] != "none" || reg["redirect_uris"].([]any)[0] != "http://127.0.0.1:5555/callback" {
		t.Fatalf("%v", reg)
	}
	u, _ := url.Parse(p.URL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("scope") != "read write" || q.Get("resource") != n.srv.URL+"/mcp" || q.Get("state") != p.State.(Pending).State {
		t.Fatalf("%v", q)
	}
	n.token(200, `{"access_token": "a1", "expires_in": 3600, "refresh_token": "r1"}`)
	// What the user pastes: the address the browser landed on.
	back := "http://127.0.0.1:5555/callback?code=the-code&state=" + url.QueryEscape(p.State.(Pending).State) + "&iss=" + url.QueryEscape(n.srv.URL)
	if err := svc.FinishLogin(t.Context(), c, "main", p, back); err != nil {
		t.Fatal(err)
	}
	f := n.forms[0]
	if f.Get("grant_type") != "authorization_code" || f.Get("code") != "the-code" || f.Get("redirect_uri") != p.Redirect || f.Get("code_verifier") == "" {
		t.Fatalf("%v", f)
	}
	stored, _ := c.Read("linear", "main")
	if !reflect.DeepEqual(stored, map[string]string{"client_id": "cid", "refresh_token": "r1", "access_token": "a1", "expires_at": "4600"}) {
		t.Fatalf("%v", stored)
	}
}

func TestACodeWithTheWrongStateIsWorthless(t *testing.T) {
	n := newLinear(t)
	svc := Service{Issuer: n.srv.URL, Transport: http.DefaultTransport}
	p, _ := svc.StartLogin(t.Context(), "main", "http://127.0.0.1:5555/callback")
	err := svc.FinishLogin(t.Context(), &creds.Store{Path: filepath.Join(t.TempDir(), "c")}, "main", p, "?code=the-code&state=forged")
	if err == nil || !strings.Contains(err.Error(), "wrong state") || len(n.forms) != 0 {
		t.Fatalf("%v, exchanged %d", err, len(n.forms))
	}
}

func TestARefusedLoginSaysWhy(t *testing.T) {
	n := newLinear(t)
	svc := Service{Issuer: n.srv.URL, Transport: http.DefaultTransport}
	p, _ := svc.StartLogin(t.Context(), "main", "http://127.0.0.1:5555/callback")
	err := svc.FinishLogin(t.Context(), &creds.Store{Path: filepath.Join(t.TempDir(), "c")}, "main", p, "code=&error=access_denied&state="+url.QueryEscape(p.State.(Pending).State))
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("%v", err)
	}
}

func TestALoginFromAnotherIssuerOrTooLateIsRefused(t *testing.T) {
	n := newLinear(t)
	now := int64(1000)
	svc := Service{Issuer: n.srv.URL, Transport: http.DefaultTransport, Now: func() time.Time { return time.Unix(now, 0) }}
	p, _ := svc.StartLogin(t.Context(), "main", "http://127.0.0.1:5555/callback")
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "c")}
	if err := svc.FinishLogin(t.Context(), c, "main", p, "code=x&iss=https://evil.example&state="+url.QueryEscape(p.State.(Pending).State)); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("%v", err)
	}
	now += 3600
	if err := svc.FinishLogin(t.Context(), c, "main", p, "code=x&state="+url.QueryEscape(p.State.(Pending).State)); err == nil || !strings.Contains(err.Error(), "15 minutes") {
		t.Fatalf("%v", err)
	}
}

// --- the wire ---

const token = "WORKSPACE-TOKEN"

type harness struct {
	t     *testing.T
	n     *fakeLinear
	c     *creds.Store
	base  string
	inst  *instance
	mu    sync.Mutex
	view  proxy.View
	order []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	n := newLinear(t)
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	c.Write("linear", "main", map[string]string{"client_id": "cid", "refresh_token": "r1", "access_token": "real-access", "expires_at": "99999999999"})
	h := &harness{t: t, n: n, c: c}
	h.setEnv("main", Env{}, token)
	svc := Service{Issuer: n.srv.URL, Transport: http.DefaultTransport}
	h.inst = svc.New(proxy.Host{Name: func() string { return "work" }, View: h.getView, Creds: c, Log: log.New(io.Discard, "", 0)}).(*instance)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go h.inst.Serve(Port, l)
	h.base = "http://" + l.Addr().String()
	return h
}

func (h *harness) setEnv(name string, e Env, tok string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.view.Envs == nil {
		h.view = proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{}, Tokens: map[string]string{},
			Exposure: store.Exposure{IsolationID: "work", Service: "linear", Default: name}}
	}
	b, _ := json.Marshal(e)
	h.view.Envs[name] = b
	found := false
	for _, n := range h.view.Exposure.Envs {
		found = found || n == name
	}
	if !found {
		h.view.Exposure.Envs = append(h.view.Exposure.Envs, name)
	}
	h.view.Tokens[name] = tok
}

func (h *harness) getView() (proxy.View, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.view, nil
}

func (h *harness) request(method, path string, body []byte, tok string, header map[string]string) (int, []byte, http.Header) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
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

func (h *harness) rpc(m map[string]any, header map[string]string) (int, map[string]any, http.Header) {
	b, _ := json.Marshal(m)
	code, body, hdr := h.request("POST", "/mcp", b, token, header)
	var out map[string]any
	json.Unmarshal(body, &out)
	return code, out, hdr
}

func errMessage(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["message"].(string)
	return s
}

func toolText(m map[string]any) string {
	r, _ := m["result"].(map[string]any)
	c, _ := r["content"].([]any)
	if len(c) == 0 {
		return ""
	}
	return c[0].(map[string]any)["text"].(string)
}

func TestTheWrongTokenIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(msg("tools/list", nil))
	code, body, hdr := h.request("POST", "/mcp", b, "not-the-token", nil)
	var m map[string]any
	json.Unmarshal(body, &m)
	if code != 403 || !strings.Contains(errMessage(m), "workspace token") || hdr.Get("WWW-Authenticate") != "" || len(h.n.seen()) != 0 {
		t.Fatalf("%d %s %v", code, body, hdr)
	}
}

func TestAnIsolationWithNoTokenMintedRefusesAnEmptyOne(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.view.Tokens = map[string]string{}
	h.mu.Unlock()
	b, _ := json.Marshal(msg("ping", nil))
	req, _ := http.NewRequest("POST", h.base+"/mcp", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer ")
	resp, _ := http.DefaultTransport.RoundTrip(req)
	resp.Body.Close()
	if resp.StatusCode != 403 || len(h.n.seen()) != 0 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestTheTokenIsSwappedForTheAccessToken(t *testing.T) {
	h := newHarness(t)
	if code, _, _ := h.rpc(call("list_issues"), nil); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.n.seen()[0]
	if s.Path != "/mcp" || s.Header.Get("Authorization") != "Bearer real-access" {
		t.Fatalf("%+v", s)
	}
	for k, vs := range s.Header {
		if strings.Contains(strings.Join(vs, ","), token) {
			t.Fatalf("the workspace token went upstream in %s", k)
		}
	}
}

func TestOnlyTransportHeadersReachLinear(t *testing.T) {
	h := newHarness(t)
	h.rpc(msg("ping", nil), map[string]string{"Mcp-Session-Id": "sess-1", "MCP-Protocol-Version": "2025-06-18", "Cookie": "workspace=secret", "X-Anything": "no"})
	s := h.n.seen()[0].Header
	if s.Get("Mcp-Session-Id") != "sess-1" || s.Get("Mcp-Protocol-Version") != "2025-06-18" || s.Get("Cookie") != "" || s.Get("X-Anything") != "" {
		t.Fatalf("%v", s)
	}
	if !strings.Contains(s.Get("Accept"), "text/event-stream") && s.Get("Accept") != "" {
		// The client sent none of its own here (Go's default Accept is
		// absent), so the proxy's must be the one that went.
		t.Fatalf("accept %q", s.Get("Accept"))
	}
}

func TestTheSessionComesBackButCookiesAndChallengesDoNot(t *testing.T) {
	h := newHarness(t)
	_, _, hdr := h.rpc(msg("ping", nil), nil)
	if hdr.Get("Mcp-Session-Id") != "sess-1" || hdr.Get("Set-Cookie") != "" || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("%v", hdr)
	}
}

func TestAWriteOnADefaultEnvNeverReachesUpstream(t *testing.T) {
	h := newHarness(t)
	code, m, _ := h.rpc(call("save_issue"), nil)
	r, _ := m["result"].(map[string]any)
	if code != 200 || m["id"] != float64(1) || r["isError"] != true || !strings.Contains(toolText(m), "--allow-api issue-edits") || len(h.n.seen()) != 0 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestAnUnmodeledToolIsRefusedBeforeUpstream(t *testing.T) {
	h := newHarness(t)
	_, m, _ := h.rpc(call("new_thing"), nil)
	if !strings.Contains(toolText(m), "not a modeled tool") || len(h.n.seen()) != 0 {
		t.Fatalf("%v", m)
	}
}

func TestAnUnservedMethodIsAJSONRPCError(t *testing.T) {
	h := newHarness(t)
	code, m, _ := h.rpc(msg("resources/read", map[string]any{"uri": "linear://x"}), nil)
	e, _ := m["error"].(map[string]any)
	if code != 404 || e["code"] != float64(-32601) || len(h.n.seen()) != 0 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestADuplicateKeyLeavesAsItWasJudged(t *testing.T) {
	h := newHarness(t)
	raw := []byte(`{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "delete_comment", "name": "get_issue", "arguments": {}}}`)
	h.request("POST", "/mcp", raw, token, nil)
	s := h.n.seen()
	if len(s) != 1 || bytes.Contains(s[0].Body, []byte("delete_comment")) {
		t.Fatalf("%s", s[0].Body)
	}
	var sent map[string]any
	json.Unmarshal(s[0].Body, &sent)
	if sent["params"].(map[string]any)["name"] != "get_issue" {
		t.Fatalf("%s", s[0].Body)
	}
}

func TestAnOpenedWriteRidesThroughAsSent(t *testing.T) {
	h := newHarness(t)
	h.setEnv("main", fams("issues", "issue-edits"), token)
	payload := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"save_issue","arguments":{"issues":[{"title":"Ünïcode <&>"}],"n":12345678901234567890}}}`
	if code, _, _ := h.request("POST", "/mcp", []byte(payload), token, nil); code != 200 {
		t.Fatalf("%d", code)
	}
	var want, got any
	json.Unmarshal([]byte(payload), &want)
	body := h.n.seen()[0].Body
	json.Unmarshal(body, &got)
	if !reflect.DeepEqual(want, got) || !bytes.Contains(body, []byte("12345678901234567890")) || !bytes.Contains(body, []byte("<&>")) {
		t.Fatalf("%s", body)
	}
}

func TestToolsListComesBackNarrowed(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(200, `{"jsonrpc": "2.0", "id": 1, "result": {"tools": [{"name": "get_issue"}, {"name": "save_issue"}, {"name": "search_documentation"}, {"name": "new_thing"}]}}`))
	b, _ := json.Marshal(msg("tools/list", nil))
	_, body, hdr := h.request("POST", "/mcp", b, token, nil)
	var m map[string]any
	json.Unmarshal(body, &m)
	if got := toolNames(m["result"]); !reflect.DeepEqual(got, []string{"get_issue", "search_documentation"}) || hdr.Get("Content-Length") != itoa(len(body)) {
		t.Fatalf("%v %v", got, hdr)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestAnSSEToolsListComesBackNarrowed(t *testing.T) {
	h := newHarness(t)
	h.n.answer(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message\ndata: "+`{"jsonrpc": "2.0", "id": 1, "result": {"tools": [{"name": "list_issues"}, {"name": "delete_comment"}]}}`+"\n\n")
	})
	b, _ := json.Marshal(msg("tools/list", nil))
	_, body, _ := h.request("POST", "/mcp", b, token, nil)
	evs := SSEMessages(body)
	if len(evs) != 1 || !reflect.DeepEqual(toolNames(evs[0].Message["result"]), []string{"list_issues"}) {
		t.Fatalf("%s", body)
	}
}

func TestInitializeOnTheWireOffersToolsOnly(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(200, `{"jsonrpc": "2.0", "id": 1, "result": {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}, "resources": {}}}}`))
	_, m, _ := h.rpc(msg("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}}), nil)
	if caps := m["result"].(map[string]any)["capabilities"]; !reflect.DeepEqual(caps, map[string]any{"tools": map[string]any{}}) {
		t.Fatalf("%v", caps)
	}
}

func TestAnExpiredAccessTokenIsRefreshedOnceAndRetried(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(401, ``))
	h.n.token(200, `{"access_token": "new-access", "expires_in": 3600}`)
	if code, _, _ := h.rpc(call("list_issues"), nil); code != 200 {
		t.Fatalf("%d", code)
	}
	s := h.n.seen()
	if len(h.n.forms) != 1 || len(s) != 2 || s[0].Header.Get("Authorization") != "Bearer real-access" || s[1].Header.Get("Authorization") != "Bearer new-access" || !bytes.Equal(s[0].Body, s[1].Body) {
		t.Fatalf("%d forms, %+v", len(h.n.forms), s)
	}
}

func TestALoginLinearKeepsRefusingIsA502NotA401(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(401, ``))
	h.n.answer(jsonAnswer(401, ``))
	h.n.token(200, `{"access_token": "new-access"}`)
	code, m, hdr := h.rpc(call("list_issues"), nil)
	if code != 502 || !strings.Contains(errMessage(m), "gero env secret linear main") || hdr.Get("WWW-Authenticate") != "" {
		t.Fatalf("%d %v %v", code, m, hdr)
	}
}

func TestAMissingLoginIsA503ThatNamesTheFix(t *testing.T) {
	h := newHarness(t)
	h.c.Write("linear", "main", map[string]string{})
	code, m, _ := h.rpc(call("list_issues"), nil)
	if code != 503 || !strings.Contains(errMessage(m), "gero env secret linear main") || len(h.n.seen()) != 0 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestTheServerStreamIsNotOffered(t *testing.T) {
	h := newHarness(t)
	code, _, hdr := h.request("GET", "/mcp", nil, token, nil)
	if code != 405 || hdr.Get("Allow") != "POST, DELETE" || len(h.n.seen()) != 0 {
		t.Fatalf("%d %v", code, hdr)
	}
}

func TestASessionEndRidesThrough(t *testing.T) {
	h := newHarness(t)
	code, _, _ := h.request("DELETE", "/mcp", nil, token, map[string]string{"Mcp-Session-Id": "sess-1"})
	s := h.n.seen()
	if code != 200 || s[0].Method != "DELETE" || s[0].Header.Get("Mcp-Session-Id") != "sess-1" {
		t.Fatalf("%d %+v", code, s)
	}
}

func TestOtherPathsAreNotServed(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(msg("ping", nil))
	for _, p := range []string{"/sse", "/v1/users/me", "/mcp/../token", "/mcp?x=1x"} {
		if code, _, _ := h.request("POST", p, b, token, nil); code != 404 && p != "/mcp?x=1x" {
			t.Errorf("%s: %d", p, code)
		}
	}
	for _, s := range h.n.seen() {
		if s.Path != "/mcp" {
			t.Fatalf("upstream saw %s", s.Path)
		}
	}
}

func TestTheRateBudgetRefusesWithRetryAfter(t *testing.T) {
	h := newHarness(t)
	h.inst.bucket = proxy.NewTokenBucket(1, 1, nil)
	h.rpc(msg("ping", nil), nil)
	code, _, hdr := h.rpc(msg("ping", nil), nil)
	if code != 429 || hdr.Get("Retry-After") == "" || len(h.n.seen()) != 1 {
		t.Fatalf("%d %v", code, hdr)
	}
}

func TestAnIsolationWithoutTheServiceIsToldHowToGetIt(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	h.view.Enabled = false
	h.mu.Unlock()
	code, m, _ := h.rpc(msg("ping", nil), nil)
	if code != 503 || !strings.Contains(errMessage(m), "gero iso expose work linear") {
		t.Fatalf("%d %v", code, m)
	}
}

func TestEachEnvGetsItsOwnServer(t *testing.T) {
	v := proxy.View{Isolation: "work", Enabled: true, Envs: map[string]json.RawMessage{"main": []byte(`{}`), "eng-wiki": []byte(`{"families":["issues","issue-edits"]}`)},
		Tokens: map[string]string{"main": "t1", "eng-wiki": "t2"}, Exposure: store.Exposure{Envs: []string{"main", "eng-wiki"}, Default: "main"}}
	ws, err := Service{}.Workspace(v)
	if err != nil {
		t.Fatal(err)
	}
	want := []proxy.MCPServer{
		{Name: "linear-eng-wiki", URL: "http://127.0.0.1:3030/mcp", TokenEnv: "LINEAR_TOKEN_ENG_WIKI"},
		{Name: "linear", URL: "http://127.0.0.1:3030/mcp", TokenEnv: "LINEAR_TOKEN_MAIN"},
	}
	if !reflect.DeepEqual(ws.MCP, want) {
		t.Fatalf("%+v", ws.MCP)
	}
	env := strings.Join(ws.Env, "\n")
	for _, w := range []string{"LINEAR_TOKEN=t1", "LINEAR_TOKEN_MAIN=t1", "LINEAR_TOKEN_ENG_WIKI=t2", "LINEAR_MCP_URL=http://127.0.0.1:3030/mcp"} {
		if !strings.Contains(env, w) {
			t.Errorf("missing %s", w)
		}
	}
	skill := ws.Files[0].Content
	if !strings.Contains(skill, "- `eng-wiki` (server `linear-eng-wiki`) — issue-edits (write), issues") || strings.Contains(skill, "%!") {
		t.Fatalf("%s", skill)
	}
}

func TestCheckNamesTheLoginAndTheUnmodeledTools(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}}`))
	h.n.answer(jsonAnswer(202, ``)) // notifications/initialized
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"name\":\"Hong\",\"email\":\"hong@example.com\"}"}]}}`))
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"get_issue"},{"name":"save_issue"},{"name":"shiny_tool"}]}}`))
	out, err := Service{Issuer: h.n.srv.URL, Transport: http.DefaultTransport}.Check(t.Context(), "main", []byte(`{}`), h.c)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"identity   Hong <hong@example.com>", "3 offered by Linear, 1 visible here", "unmodeled  shiny_tool"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in\n%s", w, out)
		}
	}
}

func TestCheckGoesOnWhenGetUserDoesNotAnswer(t *testing.T) {
	h := newHarness(t)
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}}`))
	h.n.answer(jsonAnswer(202, ``))
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"no such tool"}}`))
	h.n.answer(jsonAnswer(200, `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"get_issue"}]}}`))
	out, err := Service{Issuer: h.n.srv.URL, Transport: http.DefaultTransport}.Check(t.Context(), "main", []byte(`{}`), h.c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "identity   linear: tools/call failed: no such tool") || !strings.Contains(out, "1 offered by Linear, 1 visible here") {
		t.Fatalf("%s", out)
	}
	if s := h.n.seen(); len(s) < 3 || !bytes.Contains(s[2].Body, []byte(`"name":"get_user"`)) || !bytes.Contains(s[2].Body, []byte(`"query":"me"`)) {
		t.Fatalf("%v", s)
	}
}
