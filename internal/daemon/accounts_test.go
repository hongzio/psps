package daemon

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// withCreds gives the manager a credentials file of its own.
func withCreds(t *testing.T, m *Manager) *creds.Store {
	t.Helper()
	c := &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")}
	m.SetProxies(proxy.NewRegistry(), c)
	return c
}

func addIsolation(t *testing.T, e *testEnv, name string, env map[string]string) {
	t.Helper()
	if err := e.st.PutIsolation(store.Isolation{Name: name, Container: "gero-" + name, Image: "fake:1", Network: "gero-" + name + "-net",
		Env: env, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func hasEnv(env []string, kv string) bool { return slices.Contains(env, kv) }

func TestAnAccountIsSharedByIsolationsAndReachesTheirAgents(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	c := withCreds(t, m)
	addIsolation(t, e, "lab", nil)

	if _, err := m.PutAccount("claude", api.PutAccount{Kind: "claude-token", Secret: "sk-ant-api03-key"}, true); err == nil ||
		!strings.Contains(err.Error(), "starts with sk-ant-oat") {
		t.Fatalf("an API key went in as a token: %v", err)
	}
	if _, err := m.PutAccount("claude", api.PutAccount{Kind: "gemini", Secret: "x"}, true); err == nil {
		t.Fatal("an unknown kind was taken")
	}
	a, err := m.PutAccount("claude", api.PutAccount{Kind: "claude-token", Secret: " sk-ant-oat01-one\n"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Tool != driver.Claude || a.SecretError != "" {
		t.Fatalf("%+v", a)
	}
	if _, err := m.PutAccount("claude", api.PutAccount{Kind: "claude-token", Secret: "sk-ant-oat01-two"}, true); err == nil {
		t.Fatal("added the same account twice")
	}
	if v, _ := c.Read("account", "claude"); v["token"] != "sk-ant-oat01-one" {
		t.Fatalf("stored %q", v["token"])
	}

	for _, iso := range []string{"dev", "lab"} {
		if err := m.UseAccount(iso, "claude", "claude"); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.UseAccount("dev", "codex", "claude"); err == nil {
		t.Fatal("codex took a Claude account")
	}
	if as, _ := m.Accounts(); len(as) != 1 || !slices.Equal(as[0].UsedBy, []string{"dev", "lab"}) {
		t.Fatalf("%+v", as)
	}
	for _, iso := range []string{"dev", "lab"} {
		if env, _ := m.launchEnv(iso); !hasEnv(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-one") {
			t.Fatalf("%s starts agents without the account: %v", iso, env)
		}
	}
	if isos, _ := m.Isolations(context.Background()); isos[0].Accounts[driver.Claude] != "claude" {
		t.Fatalf("%+v", isos[0].Accounts)
	}

	// A variable set on the isolation itself still wins.
	v := "sk-ant-oat01-mine"
	m.SetEnv("lab", api.SetEnv{"CLAUDE_CODE_OAUTH_TOKEN": &v})
	env, _ := m.launchEnv("lab")
	if i, j := slices.Index(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-one"), slices.Index(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-mine"); i < 0 || j < i {
		t.Fatalf("the isolation's own variable does not come last: %v", env)
	}

	if _, err := m.PutAccount("claude", api.PutAccount{Secret: "sk-ant-oat01-two"}, false); err != nil {
		t.Fatal(err)
	}
	if env, _ := m.launchEnv("dev"); !hasEnv(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-two") {
		t.Fatalf("the replaced secret did not reach the next agent: %v", env)
	}

	if err := m.DeleteAccount("claude", false); err == nil || !strings.Contains(err.Error(), "used by dev, lab") {
		t.Fatalf("deleted an account in use: %v", err)
	}
	if err := m.DeleteAccount("claude", true); err != nil {
		t.Fatal(err)
	}
	if env, _ := m.launchEnv("dev"); strings.Contains(strings.Join(env, " "), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("a deleted account still reaches agents: %v", env)
	}
	if _, err := c.Read("account", "claude"); err == nil {
		t.Fatal("the deleted account's secret is still in the credentials file")
	}
	if used, _ := e.st.AccountsUsed("dev"); len(used) != 0 {
		t.Fatalf("%v", used)
	}
}

// End to end with the fake codex: the agent restarts onto the account it
// is given, and again onto its replaced secret.
func TestAgentsRestartOntoTheirAccount(t *testing.T) {
	was := api.AccountKinds
	api.AccountKinds = append(slices.Clone(was), api.AccountKind{Name: "test-codex", Tool: driver.Codex, Env: "GERO_TEST_VAR"})
	t.Cleanup(func() { api.AccountKinds = was })

	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	withCreds(t, m)
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })

	if _, err := m.PutAccount("work", api.PutAccount{Kind: "test-codex", Secret: "v1"}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.UseAccount("dev", "codex", "work"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent restarted", func() bool { ag, _ := m.Agent(a.ID); return !ag.State.Alive })
	m.Send(a.ID, "env")
	e.waitFor("the account in the new process", func() bool { return e.hasAssistant(a.ID, "echo: env=v1") })
	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })

	if _, err := m.PutAccount("work", api.PutAccount{Secret: "v2"}, false); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent restarted again", func() bool { ag, _ := m.Agent(a.ID); return !ag.State.Alive })
	m.Send(a.ID, "env")
	e.waitFor("the replaced secret", func() bool { return e.hasAssistant(a.ID, "echo: env=v2") })
}

func TestTokensMoveFromIsolationEnvironmentsIntoAccountsOnce(t *testing.T) {
	e := newTestEnv(t)
	dev, _ := e.st.Isolation("dev")
	dev.Env = map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-a", "OTHER": "kept"}
	e.st.PutIsolation(dev)
	addIsolation(t, e, "lab", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-a"})
	addIsolation(t, e, "solo", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-b", "ANTHROPIC_API_KEY": "sk-ant-api03-c"})
	m, stop := e.daemon()
	defer stop()
	c := withCreds(t, m)

	if err := m.migrateEnvAccounts(); err != nil {
		t.Fatal(err)
	}
	for iso, want := range map[string]string{"dev": "claude", "lab": "claude", "solo": "solo-claude"} {
		used, _ := e.st.AccountsUsed(iso)
		if used[driver.Claude] != want {
			t.Errorf("%s uses %v, want %s", iso, used, want)
		}
		rec, _ := e.st.Isolation(iso)
		if _, still := rec.Env["CLAUDE_CODE_OAUTH_TOKEN"]; still {
			t.Errorf("%s still holds the token in its environment", iso)
		}
	}
	if rec, _ := e.st.Isolation("dev"); rec.Env["OTHER"] != "kept" {
		t.Errorf("dev lost an unrelated variable: %v", rec.Env)
	}
	// One account per CLI: the API key stays where it was.
	if rec, _ := e.st.Isolation("solo"); rec.Env["ANTHROPIC_API_KEY"] != "sk-ant-api03-c" {
		t.Errorf("%v", rec.Env)
	}
	if v, _ := c.Read("account", "solo-claude"); v["token"] != "sk-ant-oat01-b" {
		t.Errorf("%v", v)
	}
	if as, _ := e.st.Accounts(); len(as) != 2 {
		t.Errorf("%+v", as)
	}

	// Once: a token put back afterwards is the user's choice.
	dev, _ = e.st.Isolation("dev")
	dev.Env["CLAUDE_CODE_OAUTH_TOKEN"] = "sk-ant-oat01-z"
	e.st.PutIsolation(dev)
	m.UseAccount("dev", "claude", "")
	if err := m.migrateEnvAccounts(); err != nil {
		t.Fatal(err)
	}
	if rec, _ := e.st.Isolation("dev"); rec.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "sk-ant-oat01-z" {
		t.Errorf("moved again: %v", rec.Env)
	}
}

func TestCLIStatusCountsTheAccount(t *testing.T) {
	_, m, _ := cliEnv(t)
	withCreds(t, m)
	if _, err := m.PutAccount("claude", api.PutAccount{Kind: "claude-token", Secret: "sk-ant-oat01-x"}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.UseAccount("dev", "claude", "claude"); err != nil {
		t.Fatal(err)
	}
	if c := cliOf(t, m, driver.Claude); !c.LoggedIn || c.Method != "CLAUDE_CODE_OAUTH_TOKEN" || c.Account != "claude" {
		t.Fatalf("%+v", c)
	}
}
