package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

type fakeBackend struct {
	mu       sync.Mutex
	agents   []api.Agent
	isos     []api.Isolation
	events   map[string][]event.Event
	sent     []string
	approved []string
	created  []api.CreateAgent
	options  []driver.Options

	services  []api.Service
	envs      []api.Env
	exposures []api.Exposure
	patches   []string
	secrets   []string
	redirects []string
	finished  []string
	exposed   []string

	// The agent CLIs: claude not logged in, codex logged in; every call
	// to them, in order.
	clis     []api.CLI
	cliLogin api.CLILogin
	cliCalls []string
	accounts []api.Account
	// choiceAsks are the lookups asked for, as "field token=… set=…".
	choiceAsks []string
	// updates are the isolation settings sent, as "name name=… cpus=… memory=…".
	updates []string
	// deleted are the agents deleted, as "id purge=…".
	deleted []string
}

func (f *fakeBackend) Agents(context.Context) ([]api.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Agent(nil), f.agents...), nil
}
func (f *fakeBackend) Isolations(context.Context) ([]api.Isolation, error) { return f.isos, nil }
func (f *fakeBackend) CreateAgent(_ context.Context, req api.CreateAgent) (api.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	a := api.Agent{Agent: store.Agent{ID: "new1", Name: req.Prompt, Kind: req.Kind, Isolation: req.Isolation, CreatedAt: time.Now()}}
	f.agents = append(f.agents, a)
	return a, nil
}
func (f *fakeBackend) Send(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, id+":"+text)
	return nil
}
func (f *fakeBackend) Approve(_ context.Context, id, ap string, d driver.Decision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved = append(f.approved, id+":"+ap+":"+string(d))
	return nil
}
func (f *fakeBackend) Interrupt(context.Context, string) error { return nil }
func (f *fakeBackend) SetOptions(_ context.Context, _ string, o driver.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.options = append(f.options, o)
	return nil
}
func (f *fakeBackend) Archive(context.Context, string) error { return nil }
func (f *fakeBackend) DeleteAgent(_ context.Context, id string, purge bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, fmt.Sprintf("%s purge=%v", id, purge))
	var keep []api.Agent
	for _, a := range f.agents {
		if a.ID != id {
			keep = append(keep, a)
		}
	}
	f.agents = keep
	return nil
}
func (f *fakeBackend) Follow(ctx context.Context, id string, after int64, fn func(event.Event) error) error {
	f.mu.Lock()
	evs := f.events[id]
	f.mu.Unlock()
	for _, e := range evs {
		if e.ID > after {
			fn(e)
		}
	}
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeBackend) CreateIsolation(context.Context, api.CreateIsolation) (api.Isolation, error) {
	return api.Isolation{}, nil
}
func (f *fakeBackend) UpdateIsolation(_ context.Context, name string, req api.UpdateIsolation) (api.Isolation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := name
	if req.Name != "" {
		u += " name=" + req.Name
	}
	if req.CPUs != nil {
		u += fmt.Sprintf(" cpus=%d", *req.CPUs)
	}
	if req.Memory != nil {
		u += " memory=" + *req.Memory
	}
	f.updates = append(f.updates, u)
	isos := append([]api.Isolation(nil), f.isos...)
	for i := range isos {
		if isos[i].Name == name && req.Name != "" {
			isos[i].Name = req.Name
		}
	}
	f.isos = isos
	return api.Isolation{}, nil
}
func (f *fakeBackend) UpgradeIsolation(context.Context, string) (api.Isolation, error) {
	return api.Isolation{}, nil
}
func (f *fakeBackend) DeleteIsolation(context.Context, string, bool) error { return nil }
func (f *fakeBackend) SetEnv(_ context.Context, iso string, set api.SetEnv) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range set {
		if v == nil {
			f.cliCalls = append(f.cliCalls, "env "+iso+" "+k+" removed")
		} else {
			f.cliCalls = append(f.cliCalls, "env "+iso+" "+k+"="+*v)
		}
	}
	return nil
}
func (f *fakeBackend) Verify(_ context.Context, name string) (api.Verify, error) {
	return api.Verify{Isolation: name, Checks: []api.VerifyCheck{
		{Check: "Mac unreachable at 192.168.1.5", OK: true, Severity: api.SevIsolation, Got: "blocked"},
		{Check: "slack refuses a wrong workspace token", Severity: api.SevIsolation, Got: "200", Detail: "the proxy answered with something other than its refusal"},
		{Check: "slack env work answers from the Mac", Severity: api.SevConfig, Got: "refused", Detail: "no [slack work] section"},
	}}, nil
}

func (f *fakeBackend) Services(context.Context) ([]api.Service, error) { return f.services, nil }
func (f *fakeBackend) Envs(context.Context, string) ([]api.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Env(nil), f.envs...), nil
}
func (f *fakeBackend) AddEnv(_ context.Context, svc, name string, p proxy.Patch) (api.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, "add "+svc+"/"+name+" "+patchString(p))
	cfg, _ := json.Marshal(p.Set)
	e := api.Env{Env: store.Env{Service: svc, Name: name, Config: cfg}, MissingSecrets: []string{"password"}}
	f.envs = append(f.envs, e)
	return e, nil
}
func (f *fakeBackend) EditEnv(_ context.Context, svc, name string, p proxy.Patch) (api.Env, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, "edit "+svc+"/"+name+" "+patchString(p))
	return api.Env{Env: store.Env{Service: svc, Name: name}}, nil
}
func (f *fakeBackend) DeleteEnv(_ context.Context, svc, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches = append(f.patches, fmt.Sprintf("delete %s/%s force=%v", svc, name, force))
	return nil
}
func (f *fakeBackend) SetSecret(_ context.Context, svc, name string, v map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.secrets = append(f.secrets, fmt.Sprintf("%s/%s %v", svc, name, v))
	return nil
}
func (f *fakeBackend) CheckEnv(context.Context, string, string) (api.Check, error) {
	return api.Check{Output: "server     db:5432, PostgreSQL 16\n"}, nil
}
func (f *fakeBackend) StartLogin(_ context.Context, svc, name, redirect string) (api.Login, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redirects = append(f.redirects, redirect)
	return api.Login{ID: "l1", URL: "https://login.example/authorize?state=s1", Redirect: redirect, Expires: time.Now().Add(time.Minute)}, nil
}
func (f *fakeBackend) FinishLogin(_ context.Context, svc, name, id, cb string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished = append(f.finished, svc+"/"+name+" "+id+" "+cb)
	return nil
}
func (f *fakeBackend) Exposures(context.Context, string) ([]api.Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Exposure(nil), f.exposures...), nil
}
func (f *fakeBackend) Expose(_ context.Context, iso, svc string, req api.Expose) (api.Exposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed = append(f.exposed, fmt.Sprintf("%s %s envs=%v default=%q settings=%s", iso, svc, req.Envs, req.Default, patchString(req.Settings)))
	return api.Exposure{Service: svc, Enabled: true, Exposure: store.Exposure{Isolation: iso, Service: svc, Envs: req.Envs, Default: req.Default}}, nil
}
func (f *fakeBackend) Unexpose(_ context.Context, iso, svc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exposed = append(f.exposed, "off "+iso+" "+svc)
	return nil
}

func (f *fakeBackend) CLIs(_ context.Context, iso string) ([]api.CLI, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clis == nil {
		f.clis = []api.CLI{{Tool: driver.Claude, Version: "2.1.311"}, {Tool: driver.Codex, Version: "0.164.0", LoggedIn: true, Method: "ChatGPT"}}
	}
	return append([]api.CLI(nil), f.clis...), nil
}
func (f *fakeBackend) StartCLILogin(_ context.Context, iso, tool string) (api.CLILogin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "start "+iso+" "+tool)
	f.cliLogin = api.CLILogin{Tool: driver.Kind(tool), State: api.CLILoginOpen, URL: "https://claude.com/cai/oauth/authorize?state=abc",
		NeedsCode: true, Expires: time.Now().Add(15 * time.Minute)}
	if tool == "codex" {
		f.cliLogin.URL, f.cliLogin.Code, f.cliLogin.NeedsCode = "https://auth.openai.com/codex/device", "LK6M-R8WQK", false
	}
	return f.cliLogin, nil
}
func (f *fakeBackend) CLILogin(context.Context, string, string) (api.CLILogin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cliLogin, nil
}
func (f *fakeBackend) SendCLICode(_ context.Context, iso, tool, code string) (api.CLILogin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "code "+iso+" "+tool+" "+code)
	f.cliLogin.State, f.cliLogin.Message, f.cliLogin.URL = api.CLILoginDone, "Login successful.", ""
	f.clis[0].LoggedIn, f.clis[0].Method = true, "Claude subscription (max)"
	return f.cliLogin, nil
}
func (f *fakeBackend) CancelCLILogin(_ context.Context, iso, tool string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "cancel "+iso+" "+tool)
	return nil
}

// Choices answers like git's: nothing without a token or a token command,
// otherwise two repositories.
func (f *fakeBackend) Choices(_ context.Context, svc, field string, req api.ChoicesRequest) ([]proxy.Choice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.choiceAsks = append(f.choiceAsks, fmt.Sprintf("%s env=%s token=%s set=%v", field, req.Env, req.Secret["token"], req.Set))
	if req.Secret["token"] == "" && req.Set["token_command"] == nil && req.Env == "" {
		return nil, fmt.Errorf("enter a token first: the repositories listed are the ones it opens")
	}
	return []proxy.Choice{{Value: "hongzio/gero", Note: "private"}, {Value: "hongzio/isolate"}}, nil
}
func (f *fakeBackend) Accounts(context.Context) ([]api.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.Account(nil), f.accounts...), nil
}
func (f *fakeBackend) AddAccount(_ context.Context, name, kind, secret string) (api.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "add "+name+" "+kind+" "+secret)
	k, _ := api.AccountKindNamed(kind)
	a := api.Account{Account: store.Account{Name: name, Kind: kind}, Tool: k.Tool}
	f.accounts = append(f.accounts, a)
	return a, nil
}
func (f *fakeBackend) SetAccountSecret(_ context.Context, name, secret string) (api.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "secret "+name+" "+secret)
	return api.Account{Account: store.Account{Name: name}}, nil
}
func (f *fakeBackend) DeleteAccount(_ context.Context, name string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, fmt.Sprintf("rm %s force=%v", name, force))
	return nil
}
func (f *fakeBackend) UseAccount(_ context.Context, iso, tool, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "use "+iso+" "+tool+" "+account)
	for i := range f.clis {
		if string(f.clis[i].Tool) == tool {
			f.clis[i].Account = account
		}
	}
	return nil
}
func (f *fakeBackend) CLILogout(_ context.Context, iso, tool string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cliCalls = append(f.cliCalls, "logout "+iso+" "+tool)
	return nil
}

func patchString(p proxy.Patch) string {
	keys := make([]string, 0, len(p.Set))
	for k := range p.Set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, p.Set[k]))
	}
	for _, k := range p.Clear {
		parts = append(parts, "clear:"+k)
	}
	return strings.Join(parts, " ")
}

func agent(id, name string, status event.Status, created int) api.Agent {
	return api.Agent{
		Agent: store.Agent{ID: id, Name: name, Kind: driver.Codex, Isolation: "dev", CreatedAt: time.Unix(int64(created), 0)},
		State: driver.State{Status: status},
	}
}

// harness runs a Model without a terminal: commands are executed inline and
// their messages fed back, the way the Bubble Tea runtime would.
type harness struct {
	t  *testing.T
	m  *Model
	be *fakeBackend
}

func newHarness(t *testing.T) *harness {
	be := &fakeBackend{
		agents: []api.Agent{agent("aaaa", "first", event.StatusIdle, 1), agent("bbbb", "second", event.StatusIdle, 2)},
		isos:   []api.Isolation{{Isolation: store.Isolation{Name: "dev", Container: "gero-dev"}, State: "running"}},
		events: map[string][]event.Event{
			"aaaa": {{ID: 1, Agent: "aaaa", Kind: event.KindUser, Text: "hi first"}},
			"bbbb": {{ID: 1, Agent: "bbbb", Kind: event.KindUser, Text: "hi second"}},
		},
	}
	h := &harness{t: t, m: New(be, defaultKeymap(), nil), be: be}
	h.send(tea.WindowSizeMsg{Width: 120, Height: 30})
	h.send(agentsMsg{agents: be.agents})
	h.send(isolationsMsg{isos: be.isos})
	return h
}

func (h *harness) send(msg tea.Msg) {
	h.t.Helper()
	_, cmd := h.m.Update(msg)
	h.run(cmd, 0)
}

// run executes cmd, skipping ticks and blocking listeners, and feeds back
// what it produces.
func (h *harness) run(cmd tea.Cmd, depth int) {
	if cmd == nil || depth > 5 {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		switch msg := msg.(type) {
		case tea.BatchMsg:
			for _, c := range msg {
				h.run(c, depth+1)
			}
		case tickMsg, nil:
		default:
			_, next := h.m.Update(msg)
			h.run(next, depth+1)
		}
	case <-time.After(100 * time.Millisecond):
		// A listener waiting on the feed; drain what the feed has.
	}
}

// drainFeed delivers pending stream events.
func (h *harness) drainFeed() {
	for {
		select {
		case msg := <-h.m.feed:
			h.m.Update(msg)
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func (h *harness) key(s string) {
	h.t.Helper()
	var msg tea.KeyMsg
	switch s {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+n":
		msg = tea.KeyMsg{Type: tea.KeyCtrlN}
	case "ctrl+s":
		msg = tea.KeyMsg{Type: tea.KeyCtrlS}
	case "ctrl+a":
		msg = tea.KeyMsg{Type: tea.KeyCtrlA}
	case "ctrl+o":
		msg = tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+r":
		msg = tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+g":
		msg = tea.KeyMsg{Type: tea.KeyCtrlG}
	case "space":
		msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "right":
		msg = tea.KeyMsg{Type: tea.KeyRight}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	h.send(msg)
}

// typeText delivers s the way a paste arrives: one message, many runes.
func (h *harness) typeText(s string) {
	h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func TestOnlyAnArchivedAgentIsDeletedAndItsWorkspaceOnlyWhenAsked(t *testing.T) {
	h := newHarness(t)
	h.drainFeed()
	if h.m.sel != "bbbb" {
		t.Fatalf("sel %q", h.m.sel)
	}
	h.key("d")
	if h.m.confirm != nil || len(h.be.deleted) != 0 {
		t.Fatal("offered to delete a live agent")
	}
	now := time.Now()
	h.be.mu.Lock()
	h.be.agents[1].ArchivedAt = &now
	agents := append([]api.Agent(nil), h.be.agents...)
	h.be.mu.Unlock()
	h.send(agentsMsg{agents: agents})
	h.key("d")
	if h.m.confirm == nil || !strings.Contains(h.m.confirm.prompt, `delete "second"?`) {
		t.Fatalf("confirm %+v", h.m.confirm)
	}
	h.key("p")
	if len(h.be.deleted) != 1 || h.be.deleted[0] != "bbbb purge=true" {
		t.Fatalf("deleted %v", h.be.deleted)
	}
	if h.m.openID == "bbbb" {
		t.Fatal("still following a deleted agent")
	}
}

func TestIsolationSettingsRenameAtOnceAndAskBeforeAResize(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+g")
	h.key("s")
	if !strings.Contains(h.m.View(), "Settings for dev") {
		t.Fatalf("settings form:\n%s", h.m.View())
	}
	h.typeText("-2")
	h.key("ctrl+s")
	if len(h.be.updates) != 1 || h.be.updates[0] != "dev name=dev-2" {
		t.Fatalf("updates %v", h.be.updates)
	}
	if iso, _ := h.m.isoPage.current(h.m.isolations); iso.Name != "dev-2" {
		t.Fatalf("selection on %q after the rename", iso.Name)
	}

	// A new size replaces the container: nothing is sent until the
	// operator says yes.
	h.key("s")
	h.key("enter") // to CPUs
	h.typeText("2")
	h.key("enter") // to memory
	h.typeText("8G")
	h.key("ctrl+s")
	if len(h.be.updates) != 1 || h.m.confirm == nil || !strings.Contains(h.m.confirm.prompt, "resize dev-2 to 2 CPUs, 8G") {
		t.Fatalf("updates %v, confirm %+v", h.be.updates, h.m.confirm)
	}
	h.key("y")
	if len(h.be.updates) != 2 || h.be.updates[1] != "dev-2 cpus=2 memory=8G" {
		t.Fatalf("updates %v", h.be.updates)
	}
}

func TestLandsOnNewestAgentAndMovesWithKeys(t *testing.T) {
	h := newHarness(t)
	h.drainFeed()
	if h.m.openID != "bbbb" {
		t.Fatalf("landed on %q, want the newest agent", h.m.openID)
	}
	if !strings.Contains(h.m.View(), "hi second") {
		t.Fatal("conversation of the open agent not shown")
	}
	h.key("k")
	h.drainFeed()
	if h.m.sel != "aaaa" || h.m.openID != "aaaa" {
		t.Fatalf("after k: sel=%q open=%q", h.m.sel, h.m.openID)
	}
	if !strings.Contains(h.m.View(), "hi first") {
		t.Fatal("moving the cursor did not show the agent")
	}
}

func TestSendGoesToTheOpenAgent(t *testing.T) {
	h := newHarness(t)
	h.key("k")
	h.key("enter") // focus the input
	h.typeText("hello")
	h.key("enter")
	if len(h.be.sent) != 1 || h.be.sent[0] != "aaaa:hello" {
		t.Fatalf("sent = %v", h.be.sent)
	}
	if h.m.input.Value() != "" {
		t.Fatal("input not cleared after send")
	}
}

func TestNewAgentForm(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+n")
	if h.m.view != viewNewAgent || h.m.newForm == nil {
		t.Fatal("new agent form not open")
	}
	h.key("ctrl+s")
	if h.m.newForm.err == "" {
		t.Fatal("empty prompt accepted")
	}
	h.typeText("fix the tests") // the prompt has the cursor first
	h.key("tab")                // wraps to Isolation
	h.key("tab")                // Agent
	h.key("right")              // claude -> codex
	h.key("ctrl+s")
	if len(h.be.created) != 1 {
		t.Fatalf("created = %v", h.be.created)
	}
	req := h.be.created[0]
	if req.Isolation != "dev" || req.Kind != driver.Codex || req.Prompt != "fix the tests" {
		t.Fatalf("request = %+v", req)
	}
	// Untouched, the form leaves approvals to the CLI's own settings.
	if req.Options.Approvals != driver.ApprovalsDefault {
		t.Fatalf("approvals = %q", req.Options.Approvals)
	}
	if h.m.openID != "new1" || h.m.focus != focusInput {
		t.Fatalf("after create: open=%q focus=%v", h.m.openID, h.m.focus)
	}
}

func TestApprovalFlow(t *testing.T) {
	h := newHarness(t)
	h.be.mu.Lock()
	h.be.events["bbbb"] = append(h.be.events["bbbb"],
		event.Event{ID: 2, Agent: "bbbb", Kind: event.KindApproval,
			Approval: &event.Approval{ID: "1.7", Kind: "command", Summary: "rm -rf build"}},
		event.Event{ID: 3, Agent: "bbbb", Kind: event.KindStatus, Status: event.StatusBlocked})
	h.be.mu.Unlock()
	h.m.openID = ""
	h.m.selectItem("bbbb")
	h.drainFeed()
	if !strings.Contains(h.m.View(), "rm -rf build") {
		t.Fatal("pending approval not shown")
	}
	h.key("ctrl+a")
	h.key("y")
	h.be.mu.Lock()
	defer h.be.mu.Unlock()
	if len(h.be.approved) != 1 || h.be.approved[0] != "bbbb:1.7:accept" {
		t.Fatalf("approved = %v", h.be.approved)
	}
}

func TestApprovalsVoidWhenTheProcessDies(t *testing.T) {
	c := newConvo()
	c.apply(event.Event{ID: 1, Kind: event.KindApproval, Approval: &event.Approval{ID: "1.1", Summary: "x"}})
	c.apply(event.Event{ID: 2, Kind: event.KindStatus, Status: event.StatusBlocked})
	if len(c.pending()) != 1 {
		t.Fatal("approval not pending")
	}
	c.apply(event.Event{ID: 3, Kind: event.KindStatus, Status: event.StatusInterrupted})
	if len(c.pending()) != 0 {
		t.Fatal("approval of a dead process still pending")
	}
}

func TestDeltasGrowAndFinalReplaces(t *testing.T) {
	c := newConvo()
	c.apply(event.Event{Kind: event.KindDelta, Item: "m1", Text: "Hel", Ephemeral: true})
	c.apply(event.Event{Kind: event.KindDelta, Item: "m1", Text: "lo", Ephemeral: true})
	if out := c.render(60, "Codex", newTheme()); !strings.Contains(out, "Hello▍") {
		t.Fatalf("live text not shown: %q", out)
	}
	c.apply(event.Event{ID: 1, Kind: event.KindAssistant, Item: "m1", Text: "Hello there"})
	out := c.render(60, "Codex", newTheme())
	if !strings.Contains(out, "Hello there") || strings.Contains(out, "▍") || strings.Count(out, "Codex") != 1 {
		t.Fatalf("final did not replace the live entry: %q", out)
	}
}

func TestKeymapOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.toml")
	os.WriteFile(path, []byte("[keys]\nnew_agent = \"ctrl+t\"\nsend = [\"ctrl+enter\", \"enter\"]\nbogus = \"x\"\n"), 0o600)
	k, warnings := LoadKeymap(path)
	if k.Label("new_agent") != "ctrl+t" || len(k.keys["send"]) != 2 {
		t.Fatalf("overrides not applied: %v", k.keys)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "bogus") {
		t.Fatalf("warnings = %v", warnings)
	}
	if !strings.Contains(DefaultKeysTOML(), `new_agent        = ["ctrl+n"]`) {
		t.Fatal("default keys.toml does not list new_agent")
	}
}

// An agent stored without a mode has always run as never; its settings say
// so rather than showing the new-agent default.
func TestSettingsShowAnUnsetModeAsNever(t *testing.T) {
	h := newHarness(t)
	h.key("ctrl+o")
	if h.m.settings == nil {
		t.Fatal("settings not open")
	}
	if got := h.m.settings.fields[2].value(); got != string(driver.ApprovalsNever) {
		t.Fatalf("approvals shown as %q", got)
	}
}
