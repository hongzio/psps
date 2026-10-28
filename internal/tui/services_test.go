package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

var gitFields = []proxy.Field{
	{Key: "repos", Kind: proxy.List, Flag: "allow-repo", Clear: "clear-repos"},
	{Key: "read_only", Kind: proxy.Bool, Flag: "read-only", Neg: "read-write"},
	{Key: "token_command", Kind: proxy.String, Flag: "token-command"},
	{Key: "api_writes", Kind: proxy.List, Flag: "api-write", Clear: "clear-api-writes", Choices: []string{"comments", "issues", "pulls"}},
}

func TestAFormSpellsTheCLIsEdit(t *testing.T) {
	cur := decodeMap(json.RawMessage(`{"repos":["a/b","c/d"],"read_only":true,"token_command":["/opt/gh","auth","token"],"api_writes":["comments","pulls"]}`))
	ff := genFields(gitFields, cur, false)
	if ff[0].value() != "a/b, c/d" || ff[1].value() != "on" || ff[2].value() != "" || ff[3].value() != "comments,pulls" {
		t.Fatalf("prefill: %q %q %q %q", ff[0].value(), ff[1].value(), ff[2].value(), ff[3].value())
	}
	// Narrow the repos, turn writes on, untick every API write; the pinned
	// token command, in a shape the field does not spell, is left alone.
	ff[0].text.SetValue("a/b")
	ff[1].idx = indexOf(ff[1].options, "off")
	ff[3].checked = map[string]bool{}
	p, err := patchFrom(gitFields, ff, cur)
	if err != nil {
		t.Fatal(err)
	}
	if got := patchString(p); got != "read_only=false repos=[a/b] clear:api_writes" {
		t.Fatalf("patch %q", got)
	}
	// While adding, an untouched bool is the service's default, not false.
	add := genFields(gitFields, map[string]any{}, true)
	if p, _ := patchFrom(gitFields, add, map[string]any{}); len(p.Set) != 0 || len(p.Clear) != 0 {
		t.Fatalf("an untouched form patched %s", patchString(p))
	}
}

func servicesHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.be.services = []api.Service{
		{Name: "pg", Summary: "PostgreSQL", Secrets: []string{"password"}, Fields: []proxy.Field{
			{Key: "host", Kind: proxy.String, Flag: "host"},
			{Key: "databases", Kind: proxy.List, Flag: "allow-db", Clear: "clear-dbs"},
		}},
		{Name: "notion", Summary: "Notion", Login: true, Secrets: []string{"client_id", "refresh_token"},
			Fields: []proxy.Field{{Key: "families", Kind: proxy.List, Flag: "allow-api", Choices: []string{"content", "pages", "search"}}}},
	}
	h.be.envs = []api.Env{{Env: store.Env{Service: "notion", Name: "work", Config: json.RawMessage(`{"families":["content","search"]}`)}, MissingSecrets: []string{"client_id"}}}
	return h
}

func TestTheServicesPageAddsAnEnvAndStoresItsSecret(t *testing.T) {
	h := servicesHarness(t)
	h.key("ctrl+r")
	if h.m.view != viewServices || !strings.Contains(h.m.View(), "PostgreSQL") || !strings.Contains(h.m.View(), "not logged in") {
		t.Fatalf("services page:\n%s", h.m.View())
	}
	h.key("n") // the cursor is on pg
	h.typeText("prod")
	h.key("tab")
	h.typeText("db.internal")
	h.key("tab")
	h.typeText("app, analytics")
	h.key("ctrl+s")
	if len(h.be.patches) != 1 || h.be.patches[0] != "add pg/prod databases=[app analytics] host=db.internal" {
		t.Fatalf("patches %v", h.be.patches)
	}
	// The new env is listed under pg; its secret goes in a masked form.
	h.key("down")
	if row, _ := h.m.svcPage.current(); row.env == nil || row.env.Name != "prod" {
		t.Fatalf("cursor on %+v", row)
	}
	h.key("s")
	h.typeText("s3cret")
	if strings.Contains(h.m.View(), "s3cret") {
		t.Fatal("the secret is shown while typed")
	}
	h.key("ctrl+s")
	if len(h.be.secrets) != 1 || h.be.secrets[0] != "pg/prod map[password:s3cret]" {
		t.Fatalf("secrets %v", h.be.secrets)
	}
	h.key("c")
	if !strings.Contains(h.m.View(), "PostgreSQL 16") {
		t.Fatalf("check output:\n%s", h.m.View())
	}
}

func TestABrowserLoginFinishesFromAPastedAddress(t *testing.T) {
	h := servicesHarness(t)
	var copied string
	h.m.clipboard = func(s string) { copied = s }
	h.key("ctrl+r")
	for i := 0; i < 3; i++ { // pg, notion, notion/work
		h.key("down")
	}
	if row, _ := h.m.svcPage.current(); row.env == nil || row.env.Name != "work" {
		t.Fatalf("cursor on %+v", row)
	}
	h.key("s")
	if h.m.svcPage.mode != svcLogin || !strings.Contains(h.m.View(), "https://login.example/authorize?state=s1") {
		t.Fatalf("login page:\n%s", h.m.View())
	}
	// It listens on loopback for a browser on this Mac.
	if len(h.be.redirects) != 1 || !strings.HasPrefix(h.be.redirects[0], "http://127.0.0.1:") {
		t.Fatalf("redirects %v", h.be.redirects)
	}
	h.key("y")
	if copied != "https://login.example/authorize?state=s1" {
		t.Fatalf("copied %q", copied)
	}
	h.typeText("http://127.0.0.1:61000/callback?code=c&state=s1")
	h.key("enter")
	if len(h.be.finished) != 1 || h.be.finished[0] != "notion/work l1 http://127.0.0.1:61000/callback?code=c&state=s1" {
		t.Fatalf("finished %v", h.be.finished)
	}
}

func TestAnIsolationsServicesAreTurnedOnAndOffFromItsPage(t *testing.T) {
	h := servicesHarness(t)
	h.be.envs = append(h.be.envs, api.Env{Env: store.Env{Service: "pg", Name: "prod", Config: json.RawMessage(`{}`)}})
	h.key("ctrl+g")
	h.key("p")
	if h.m.isoPage.expose == nil || !strings.Contains(h.m.View(), "Services in dev") {
		t.Fatalf("expose page:\n%s", h.m.View())
	}
	h.key("enter") // pg
	h.key("space") // tick prod
	h.key("ctrl+s")
	if len(h.be.exposed) != 1 || h.be.exposed[0] != `dev pg envs=[prod] default="" settings=` {
		t.Fatalf("exposed %v", h.be.exposed)
	}
	h.be.exposures = []api.Exposure{{Service: "pg", Enabled: true, Exposure: store.Exposure{IsolationID: "i-dev", Service: "pg", Envs: []string{"prod"}, Default: "prod"}}}
	h.send(exposuresMsg{iso: "dev", exposures: h.be.exposures, services: h.be.services, envs: h.be.envs})
	if !strings.Contains(h.m.View(), "prod (default prod)") {
		t.Fatalf("after expose:\n%s", h.m.View())
	}
	h.key("x")
	h.key("y")
	if len(h.be.exposed) != 2 || h.be.exposed[1] != "off dev pg" {
		t.Fatalf("exposed %v", h.be.exposed)
	}
}

func TestVerifyFromTheIsolationsPageShowsEachCheck(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+g")
	h.key("v")
	view := h.m.View()
	for _, want := range []string{"Verify dev", "PASS", "Mac unreachable at 192.168.1.5", "FAIL", "slack refuses a wrong workspace token",
		"something other than its refusal", "WARN", "2/3 failed (1 isolation)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q:\n%s", want, view)
		}
	}
	h.key("x") // any key closes
	if h.m.isoPage.report != nil || strings.Contains(h.m.View(), "Verify dev") {
		t.Fatal("the report did not close")
	}
}

func TestAGitEnvAsksForItsTokenThenOffersTheRepositoriesItOpens(t *testing.T) {
	h := newHarness(t)
	lookupFields := append([]proxy.Field(nil), gitFields...)
	lookupFields[0].Lookup = true
	h.be.services = []api.Service{{Name: "git", Summary: "git and the GitHub API", Secrets: []string{"token"}, Fields: lookupFields}}
	h.key("ctrl+r")
	h.key("n")
	// Nothing to look with yet: the repos field is shut and says why.
	h.viewHas("token", "enter a token first")
	h.typeText("work")
	h.key("tab")
	h.typeText("ghp_secret")
	if strings.Contains(h.m.View(), "ghp_secret") {
		t.Fatal("the token is shown while typed")
	}
	h.key("tab") // onto repos: the lookup runs with the typed token
	h.viewHas("0 of 2 ticked", "hongzio/gero", "private", "hongzio/isolate")
	h.key(" ") // space ticks hongzio/gero
	h.key("ctrl+s")
	if len(h.be.patches) != 1 || h.be.patches[0] != "add git/work repos=[hongzio/gero]" {
		t.Fatalf("patches %v", h.be.patches)
	}
	if len(h.be.secrets) != 1 || h.be.secrets[0] != "git/work map[token:ghp_secret]" {
		t.Fatalf("secrets %v", h.be.secrets)
	}
	if asks := h.be.choiceAsks; len(asks) != 2 || !strings.Contains(asks[1], "token=ghp_secret") {
		t.Fatalf("asks %v", asks)
	}
}

func TestAGitEnvWithACredentialLooksUpAsItOpens(t *testing.T) {
	h := newHarness(t)
	lookupFields := append([]proxy.Field(nil), gitFields...)
	lookupFields[0].Lookup = true
	h.be.services = []api.Service{{Name: "git", Secrets: []string{"token"}, Fields: lookupFields}}
	h.be.envs = []api.Env{{Env: store.Env{Service: "git", Name: "work",
		Config: json.RawMessage(`{"repos":["hongzio/old"],"token_command":["/opt/gh","auth","token"]}`)}, CredentialElsewhere: true}}
	h.key("ctrl+r")
	h.key("down")
	h.key("e")
	// The repository already listed stays ticked, though the credential
	// does not list it.
	h.viewHas("1 of 3 ticked", "hongzio/old", "not among what it opens", "hongzio/isolate")
	if asks := h.be.choiceAsks; len(asks) != 1 || !strings.Contains(asks[0], "env=work") {
		t.Fatalf("asks %v", asks)
	}
}
