package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/askproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

func openAsks(m *Manager, id string) int {
	a, err := m.Agent(id)
	if err != nil {
		return -1
	}
	return a.Asks
}

// An agent leaves asks at its address and goes on; the operator answers
// them, or closes them without an answer, and each answer reaches the
// agent as a message of its own once it is idle.
func TestAnAgentLeavesAsksAndHearsTheAnswers(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, askproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })
	stored, _ := m.store.Agent(a.ID)

	// Off: no address, and the port refuses.
	if env := m.askEnv(stored); env != nil {
		t.Fatalf("an address while off: %q", env)
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", `{"title":"x"}`); code != http.StatusForbidden || !strings.Contains(out, "psps iso expose dev ask") {
		t.Fatalf("off: %d %s", code, out)
	}
	if _, err := m.Expose(ctx, "dev", "ask", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	tok, _ := m.store.MailToken(a.ID)
	if env := m.askEnv(stored); len(env) != 2 || env[0] != "PSPS_ASK_URL="+askproxy.URL(tok) {
		t.Fatalf("env %q", env)
	}
	if code, out := mailAt(m, testIsoID, "GET", "/a/nope/asks", ""); code != http.StatusForbidden {
		t.Fatalf("a wrong token: %d %s", code, out)
	}

	post := func(body string) (int, agentAsk) {
		t.Helper()
		code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", body)
		var v agentAsk
		json.Unmarshal([]byte(out), &v)
		return code, v
	}
	code, q := post(`{"title":"How long to keep logs?","body":"S3 went up.","options":[{"label":"30 days","note":"$45"},{"label":"90 days"}]}`)
	if code != http.StatusCreated || q.Kind != askproxy.Question || q.State != store.AskOpen || len(q.Options) != 2 || !strings.Contains(q.Note, "Do not poll") {
		t.Fatalf("question: %d %+v", code, q)
	}
	// The same title, still open, is the same ask.
	if code, again := post(`{"title":"How long to keep logs?"}`); code != http.StatusOK || again.ID != q.ID {
		t.Fatalf("again: %d %+v", code, again)
	}
	code, c := post(`{"kind":"confirm","title":"Apply the migration to prod?"}`)
	if code != http.StatusCreated || c.Kind != askproxy.Confirm {
		t.Fatalf("confirm: %d %+v", code, c)
	}
	code, k := post(`{"kind":"task","title":"Approve SSM session #8812"}`)
	if code != http.StatusCreated {
		t.Fatalf("task: %d %+v", code, k)
	}
	for _, body := range []string{`{}`, `{"title":"  "}`, `{"title":"two\nlines"}`, `{"kind":"poll","title":"x"}`,
		`{"kind":"confirm","title":"x","options":[{"label":"a"},{"label":"b"}]}`, `{"title":"x","options":[{"label":"a"}]}`,
		`{"title":"x","options":[{"label":""},{"label":"b"}]}`, `not json`} {
		if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
	if n := openAsks(m, a.ID); n != 3 {
		t.Fatalf("%d open", n)
	}

	// The agent withdraws one; it is not told of it, and it cannot be
	// answered any more.
	if code, out := mailAs(t, m, testIsoID, a.ID, "DELETE", fmt.Sprintf("/asks/%d", k.ID), ""); code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", code, out)
	}
	if code, _ := mailAs(t, m, testIsoID, a.ID, "DELETE", fmt.Sprintf("/asks/%d", k.ID), ""); code != http.StatusConflict {
		t.Fatalf("withdrawn twice: %d", code)
	}
	if _, err := m.AnswerAsk(k.ID, api.AnswerAsk{Outcome: askproxy.Done}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("answered a withdrawn ask: %v", err)
	}

	// What does not answer the ask is refused.
	for _, req := range []api.AnswerAsk{{}, {Outcome: askproxy.Yes}, {Pick: 3}, {Outcome: "dismiss", Pick: 1}} {
		if _, err := m.AnswerAsk(q.ID, req); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("question %+v: %v", req, err)
		}
	}
	for _, req := range []api.AnswerAsk{{}, {Text: "sure"}, {Outcome: askproxy.Done}, {Pick: 1, Outcome: askproxy.Yes}} {
		if _, err := m.AnswerAsk(c.ID, req); !isStatus(err, http.StatusBadRequest) {
			t.Errorf("confirm %+v: %v", req, err)
		}
	}
	if n := len(users(e, a.ID)); n != 0 {
		t.Fatalf("%d turns before any answer", n)
	}

	// Answered, each reaches the idle agent as a message of its own.
	got, err := m.AnswerAsk(q.ID, api.AnswerAsk{Pick: 1, Text: " but errors for 90 "})
	if err != nil || got.Verdict != "answered" || got.Pick != 1 || got.Text != "but errors for 90" {
		t.Fatalf("answer: %+v %v", got, err)
	}
	if _, err := m.AnswerAsk(q.ID, api.AnswerAsk{Pick: 2}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("answered twice: %v", err)
	}
	want := `[psps ask: your operator answered an ask of yours. This is their answer; act on it as you would on their message.]

#` + fmt.Sprint(q.ID) + ` "How long to keep logs?" (question): answered.
Picked: 30 days (option 1)
Their words: but errors for 90`
	e.waitFor("the answer", func() bool { return e.hasAssistant(a.ID, "echo: "+want) })
	if _, err := m.AnswerAsk(c.ID, api.AnswerAsk{Outcome: "dismiss"}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the second answer", func() bool { return len(users(e, a.ID)) == 2 })
	if u := users(e, a.ID); u[0] != want || !askproxy.IsAnswers(u[1]) || !strings.Contains(u[1], "(confirm): closed without an answer.") {
		t.Fatalf("turns %q", u)
	}
	if n := openAsks(m, a.ID); n != 0 {
		t.Fatalf("%d open after all were closed", n)
	}

	// The agent reads what became of them, newest first.
	_, out := mailAs(t, m, testIsoID, a.ID, "GET", "/asks", "")
	var list []agentAsk
	json.Unmarshal([]byte(out), &list)
	if len(list) != 3 || list[0].Verdict != "withdrawn" || list[1].Verdict != "closed without an answer" || list[2].Picked != "30 days" {
		t.Fatalf("list %s", out)
	}
	ops, _ := m.AgentAsks("alice")
	if len(ops) != 3 || ops[2].AgentID != "alice" || ops[2].Isolation != "dev" || ops[2].DeliveredAt == nil {
		t.Fatalf("the operator's list: %+v", ops)
	}

	// At most MaxOpen open at once.
	for i := range askproxy.MaxOpen {
		if code, _ := post(fmt.Sprintf(`{"title":"ask %d"}`, i)); code != http.StatusCreated {
			t.Fatalf("ask %d: %d", i, code)
		}
	}
	if code, out := mailAs(t, m, testIsoID, a.ID, "POST", "/asks", `{"title":"one more"}`); code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d %s", code, out)
	}
	if all, _ := m.OpenAsks(); len(all) != askproxy.MaxOpen {
		t.Fatalf("%d open in all", len(all))
	}

	// Deleting the agent deletes its asks.
	if err := m.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent deleted", func() bool { return m.DeleteAgent(ctx, a.ID, false) == nil })
	if all, _ := m.OpenAsks(); len(all) != 0 {
		t.Fatalf("%d open after the agent was deleted", len(all))
	}
}

func isStatus(err error, code int) bool {
	var h httpError
	return errors.As(err, &h) && h.code == code
}
