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
	e.waitFor("the agent deleted", func() bool { return errOf(m.DeleteAgent(ctx, a.ID, false)) == nil })
	if all, _ := m.OpenAsks(); len(all) != 0 {
		t.Fatalf("%d open after the agent was deleted", len(all))
	}
}

func isStatus(err error, code int) bool {
	var h httpError
	return errors.As(err, &h) && h.code == code
}

// An open ask is a thread: the operator replies and keeps it open, the
// agent hears the reply when idle and adds to the same ask, with new
// options, and the operator answers from those; or the agent resolves it.
func TestAnAskStaysOpenThroughRepliesAndAdditions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, askproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })
	if _, err := m.Expose(ctx, "dev", "ask", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	send := func(method, path, body string) (int, agentAsk, string) {
		t.Helper()
		code, out := mailAs(t, m, testIsoID, a.ID, method, path, body)
		var v agentAsk
		json.Unmarshal([]byte(out), &v)
		return code, v, out
	}
	counts := func() (int, int) {
		x, _ := m.Agent(a.ID)
		return x.Asks, x.AsksWaiting
	}
	_, q, _ := send("POST", "/asks", `{"title":"How long to keep logs?","options":[{"label":"30 days"},{"label":"90 days"}]}`)
	at := fmt.Sprintf("/asks/%d", q.ID)

	// The operator replies: it stays open, and waits on the agent.
	if _, err := m.ReplyAsk(q.ID, api.ReplyAsk{Text: "  "}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("an empty reply: %v", err)
	}
	got, err := m.ReplyAsk(q.ID, api.ReplyAsk{Text: "Do audit logs live there too?"})
	if err != nil || got.State != store.AskOpen || got.Turn != store.TurnAgent || len(got.Notes) != 1 {
		t.Fatalf("reply: %+v %v", got, err)
	}
	if o, w := counts(); o != 0 || w != 1 {
		t.Fatalf("counts after the reply: %d %d", o, w)
	}
	want := `[psps ask: your operator replied to an ask of yours and kept it open. Act on what they say as you would on their message.]

#` + fmt.Sprint(q.ID) + ` "How long to keep logs?" (question): still open, your turn.
Their reply: Do audit logs live there too?
Add to it with POST "$PSPS_ASK_URL/` + fmt.Sprint(q.ID) + `" (what you found, or your answer to theirs), or resolve it there if this settles it; do not leave a new ask for it.`
	e.waitFor("the reply", func() bool { return e.hasAssistant(a.ID, "echo: "+want) })
	if !askproxy.IsAnswers(want) {
		t.Fatal("a reply is not read as answers")
	}

	// The agent adds to it, with new options: it waits on the operator.
	for _, body := range []string{`{}`, `{"text":"x","options":[{"label":"one"}]}`, `not json`} {
		if code, _, out := send("POST", at, body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}
	if code, _, _ := send("POST", "/asks/99999", `{"text":"x"}`); code != http.StatusNotFound {
		t.Fatalf("someone else's ask: %d", code)
	}
	code, v, out := send("POST", at, `{"text":"Yes, under audit/.","options":[{"label":"30 days, audit a year"},{"label":"60 days"}]}`)
	if code != http.StatusOK || v.Turn != "operator" || len(v.Notes) != 2 || v.Notes[0].By != "operator" || v.Notes[1].By != "you" || v.Options[0].Label != "30 days, audit a year" {
		t.Fatalf("added: %d %s", code, out)
	}
	if o, w := counts(); o != 1 || w != 0 {
		t.Fatalf("counts after the addition: %d %d", o, w)
	}

	// The answer picks from the options it offers now.
	if _, err := m.AnswerAsk(q.ID, api.AnswerAsk{Pick: 3}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("a pick past the new options: %v", err)
	}
	if _, err := m.AnswerAsk(q.ID, api.AnswerAsk{Pick: 1}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the answer", func() bool {
		u := users(e, a.ID)
		return len(u) == 2 && strings.Contains(u[1], "(question): answered.\nPicked: 30 days, audit a year (option 1)")
	})
	if code, _, _ := send("POST", at, `{"text":"more"}`); code != http.StatusConflict {
		t.Fatalf("added to a closed ask: %d", code)
	}
	ops, _ := m.AgentAsks("alice")
	if len(ops[0].Notes) != 2 || ops[0].Notes[0].DeliveredAt == nil {
		t.Fatalf("the operator's view: %+v", ops[0])
	}

	// The agent resolves one its operator's reply settled; nothing more
	// reaches it, and the operator can no longer answer it.
	_, c, _ := send("POST", "/asks", `{"kind":"confirm","title":"Apply the migration?"}`)
	if _, err := m.ReplyAsk(c.ID, api.ReplyAsk{Text: "Only after the backup."}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the second reply", func() bool { return len(users(e, a.ID)) == 3 })
	if code, _, _ := send("POST", fmt.Sprintf("/asks/%d", c.ID), `{"kind":"confirm","options":[{"label":"a"},{"label":"b"}]}`); code != http.StatusBadRequest {
		t.Fatalf("options on a confirm: %d", code)
	}
	code, v, out = send("POST", fmt.Sprintf("/asks/%d", c.ID), `{"resolve":true,"text":"Backed up and applied."}`)
	if code != http.StatusOK || v.State != store.AskResolved || v.Verdict != "resolved by the agent" {
		t.Fatalf("resolve: %d %s", code, out)
	}
	if _, err := m.AnswerAsk(c.ID, api.AnswerAsk{Outcome: askproxy.Yes}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("answered a resolved ask: %v", err)
	}
	if _, err := m.ReplyAsk(c.ID, api.ReplyAsk{Text: "x"}); !isStatus(err, http.StatusConflict) {
		t.Fatalf("replied to a resolved ask: %v", err)
	}
	if n, _ := m.store.AnswersWaiting(a.ID); n != 0 {
		t.Fatalf("%d answers waiting after resolve", n)
	}

	// An ask holds at most MaxNotes.
	_, k, _ := send("POST", "/asks", `{"title":"Long one"}`)
	for i := range askproxy.MaxNotes {
		if code, _, out := send("POST", fmt.Sprintf("/asks/%d", k.ID), fmt.Sprintf(`{"text":"note %d"}`, i)); code != http.StatusOK {
			t.Fatalf("note %d: %d %s", i, code, out)
		}
	}
	if code, _, _ := send("POST", fmt.Sprintf("/asks/%d", k.ID), `{"text":"one more"}`); code != http.StatusTooManyRequests {
		t.Fatalf("over the notes' limit: %d", code)
	}
	if code, v, _ := send("POST", fmt.Sprintf("/asks/%d", k.ID), `{"resolve":true}`); code != http.StatusOK || v.State != store.AskResolved {
		t.Fatalf("resolving a full ask: %d", code)
	}

	// Deleting the agent deletes its asks' notes.
	if err := m.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent deleted", func() bool { return errOf(m.DeleteAgent(ctx, a.ID, false)) == nil })
	if all, err := m.store.AsksOf(a.ID, 100); err != nil || len(all) != 0 {
		t.Fatalf("asks left: %d %v", len(all), err)
	}
}
