package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/controlproxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

// An isolation's agents do, through the control service, what its env
// ticks, on the agents of their own isolation and of those it reaches, as
// themselves: what they make or write reads so, approvals stay the
// operator's, and they have no more made than the env allows.
func TestControlOpensWhatItsEnvTicksOnTheAgentsItReaches(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, controlproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return freePort(t) }
	for _, name := range []string{"far", "other"} {
		if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	agent := func(iso, handle string, approvals driver.Approvals) store.Agent {
		a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: iso, Kind: driver.Codex, Handle: handle, Options: driver.Options{Approvals: approvals}})
		if err != nil {
			t.Fatal(err)
		}
		e.waitFor(handle, func() bool { return status(m, a.ID) == event.StatusIdle })
		return a
	}
	boss := agent("dev", "boss", driver.ApprovalsAuto)
	mate := agent("dev", "mate", "")
	faraway := agent("far", "faraway", "")
	hidden := agent("other", "hidden", "")

	v, err := m.PutEnv(ctx, "control", "ops", proxy.Patch{Set: map[string]any{
		"allow": []string{"team.ls", "agent.send", "agent.ls", "agent.new", "agent.set", "agent.cron"}, "max_live": 1}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "lets agents change gero (agent.new, agent.send, agent.set, agent.cron)") {
		t.Fatalf("%q", v.Warnings)
	}
	if _, err := m.PutEnv(ctx, "control", "ro", proxy.Patch{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutEnv(ctx, "control", "bad", proxy.Patch{Set: map[string]any{"allow": []string{"account.add"}}}, true); err == nil {
		t.Fatal("an env opened what is not an action")
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops", "ro"}}); err == nil || !strings.Contains(err.Error(), "one control env") {
		t.Fatalf("two envs: %v", err)
	}
	if len(m.controlEnv(boss)) != 0 {
		t.Fatal("an address before the service is on")
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}, Settings: proxy.Patch{Set: map[string]any{"reach": []string{"far"}}}}); err != nil {
		t.Fatal(err)
	}
	if env := m.controlEnv(boss); len(env) != 1 || !strings.HasPrefix(env[0], "GERO_CONTROL_URL=http://127.0.0.1:") || !strings.HasSuffix(env[0], "/control") {
		t.Fatalf("%q", env)
	}
	ctl := func(from store.Agent, method, path, body string) (int, string) {
		t.Helper()
		return mailAs(t, m, from.IsolationID, from.ID, method, "/control"+path, body)
	}

	code, out := ctl(boss, "GET", "", "")
	var cv api.ControlView
	json.Unmarshal([]byte(out), &cv)
	if code != 200 || cv.Env != "ops" || !slices.Equal(cv.Reach, []string{"dev", "far"}) || cv.MaxLive != 1 || cv.Live != 0 ||
		!slices.Equal(cv.Allow, []string{"agent.ls", "agent.new", "agent.send", "agent.set", "agent.cron", "team.ls"}) {
		t.Fatalf("%d %s", code, out)
	}

	// It sees the agents it reaches, and no other.
	_, out = ctl(boss, "GET", "/agents", "")
	var as []api.Agent
	json.Unmarshal([]byte(out), &as)
	var ids []string
	for _, a := range as {
		ids = append(ids, a.Handle)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"boss", "faraway", "mate"}) {
		t.Fatalf("%q", ids)
	}
	if code, out := ctl(boss, "GET", "/agents/hidden", ""); code != http.StatusNotFound || !strings.Contains(out, "no agent you reach is hidden") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(boss, "GET", "/agents/mate/events", ""); code != http.StatusForbidden || !strings.Contains(out, "agent.log is not open to the agents of isolation dev") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := mailAs(t, m, testIsoID, faraway.ID, "GET", "/control", ""); code != http.StatusForbidden || !strings.Contains(out, "not the address of an agent in isolation dev") {
		t.Fatalf("another isolation's address: %d %s", code, out)
	}
	if code, out := ctl(faraway, "GET", "", ""); code != http.StatusForbidden || !strings.Contains(out, "isolation far has no control") {
		t.Fatalf("an isolation without it: %d %s", code, out)
	}

	// What it writes reads as its own.
	if code, out := ctl(boss, "POST", "/agents/mate/send", "the notes are in notes.md"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("mate's message", func() bool {
		return slices.Contains(users(e, mate.ID), "[gero control: from agent boss in isolation dev, through gero's control service]\n\nthe notes are in notes.md")
	})
	if code, out := ctl(boss, "POST", "/agents/boss/send", "me"); code != http.StatusBadRequest || !strings.Contains(out, "that is you") {
		t.Fatalf("%d %s", code, out)
	}
	// It spends its isolation's sending budget, mail's.
	sent := 1
	for ; sent < mailproxy.Burst; sent++ {
		if code, out := ctl(boss, "POST", "/agents/mate/send", "again"); code != http.StatusAccepted {
			t.Fatalf("send %d: %d %s", sent, code, out)
		}
	}
	if code, out := ctl(boss, "POST", "/agents/mate/send", "again"); code != http.StatusTooManyRequests || !strings.Contains(out, "mail included") {
		t.Fatalf("past the budget: %d %s", code, out)
	}

	// One it makes elsewhere asks the operator; it may have one live.
	if code, out := ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "hi", "approvals": "never"}`); code != http.StatusForbidden || !strings.Contains(out, "approvals are your operator's") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "hi", "isolation": "other"}`); code != http.StatusNotFound {
		t.Fatalf("an isolation it does not reach: %d %s", code, out)
	}
	code, out = ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "measure it", "isolation": "far", "name": "prober"}`)
	var made api.Agent
	json.Unmarshal([]byte(out), &made)
	if code != http.StatusCreated || made.Isolation != "far" || made.Options.Approvals != driver.ApprovalsAsk || made.MadeVia != boss.ID || made.Name != "prober" {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("its prompt", func() bool {
		return slices.Contains(users(e, made.ID), "[gero control: agent boss in isolation dev made you, through gero's control service. Your operator may not be watching.]\n\nmeasure it")
	})
	if code, out := ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "more"}`); code != http.StatusTooManyRequests || !strings.Contains(out, "1 agents made through it") {
		t.Fatalf("past max live: %d %s", code, out)
	}
	if err := m.Archive(ctx, made.ID); err != nil {
		t.Fatal(err)
	}
	code, out = ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "here"}`)
	json.Unmarshal([]byte(out), &made)
	if code != http.StatusCreated || made.Isolation != "dev" || made.Options.Approvals != driver.ApprovalsAuto {
		t.Fatalf("one in its own isolation takes its approvals: %d %s", code, out)
	}

	// Model and effort are its to set; approvals are not.
	if code, out := ctl(boss, "PUT", "/agents/mate/options", `{"approvals": "never"}`); code != http.StatusForbidden {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(boss, "PUT", "/agents/mate/options", `{"effort": "high"}`); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if a, _ := m.store.Agent(mate.ID); a.Options.Effort != "high" || a.Options.Approvals != "" {
		t.Fatalf("%+v", a.Options)
	}
	if code, _ := ctl(boss, "PATCH", "/agents/mate", `{"name": "x"}`); code != http.StatusForbidden {
		t.Fatalf("a rename it was not opened: %d", code)
	}

	// Crons it sets say so; the operator's are not its to change.
	code, out = ctl(boss, "POST", "/agents/mate/crons", `{"schedule": "@every 2h", "prompt": "check in"}`)
	var cr store.Cron
	json.Unmarshal([]byte(out), &cr)
	if code != http.StatusCreated || cr.SetBy != "boss in isolation dev" {
		t.Fatalf("%d %s", code, out)
	}
	if l := cronLetter(cr); !strings.Contains(l, "Agent boss in isolation dev set this message, through gero's control service") {
		t.Fatal(l)
	}
	if code, out := ctl(boss, "POST", "/agents/mate/crons", `{"schedule": "@every 1m", "prompt": "spam"}`); code != http.StatusBadRequest {
		t.Fatalf("too often: %d %s", code, out)
	}
	ops, err := m.AddCron(mate.ID, api.PutCron{Schedule: ptr("@daily"), Prompt: ptr("yours")})
	if err != nil {
		t.Fatal(err)
	}
	if code, out := ctl(boss, "DELETE", "/agents/mate/crons/"+itoa(ops.ID), ""); code != http.StatusForbidden || !strings.Contains(out, "is your operator's") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(boss, "DELETE", "/agents/mate/crons/"+itoa(cr.ID), ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}

	// Off, it answers no one, and the agents start without its address.
	if err := m.Unexpose(ctx, "dev", "control"); err != nil {
		t.Fatal(err)
	}
	if code, out := ctl(boss, "GET", "/agents", ""); code != http.StatusForbidden || !strings.Contains(out, "isolation dev has no control") {
		t.Fatalf("%d %s", code, out)
	}
	if len(m.controlEnv(boss)) != 0 {
		t.Fatal("an address after the service is off")
	}
	_ = hidden
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
