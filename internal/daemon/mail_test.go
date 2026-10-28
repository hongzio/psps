package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

// withMail gives the manager the mail service, turns it on for dev, and
// lifts dev's sending budget, which a test that sends more than a few
// would run out of.
func withMail(t *testing.T, m *Manager) {
	t.Helper()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return freePort(t) }
	if _, err := m.Expose(context.Background(), "dev", "mail", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	m.mailBuckets[testIsoID] = proxy.NewTokenBucket(1e6, 1e6, nil)
}

// mailAs asks isolation iso's mail port, as the agent with key from.
func mailAs(t *testing.T, m *Manager, iso, from, method, path, body string) (int, string) {
	t.Helper()
	tok, err := m.store.MailToken(from)
	if err != nil {
		t.Fatal(err)
	}
	return mailAt(m, iso, method, "/a/"+tok+path, body)
}

func mailAt(m *Manager, iso, method, path, body string) (int, string) {
	rec := httptest.NewRecorder()
	m.mailHandler(iso).ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

func send(t *testing.T, m *Manager, from store.Agent, to, body string) (int, string) {
	t.Helper()
	return mailAs(t, m, from.IsolationID, from.ID, "POST", "/send?to="+url.QueryEscape(to), body)
}

func users(e *testEnv, id string) []string {
	var out []string
	for _, ev := range e.events(id) {
		if ev.Kind == event.KindUser {
			out = append(out, ev.Text)
		}
	}
	return out
}

func inbox(m *Manager, id string) int {
	a, err := m.Agent(id)
	if err != nil {
		return -1
	}
	return a.Inbox
}

// Mail waits in the inbox until its agent is idle and then arrives as a
// turn of its own, oldest first; an agent that was interrupted keeps it
// until the operator's next message, which goes first.
func TestMailArrivesOnePerTurnWhenTheAgentIsIdle(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	withMail(t, m)
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "bob"})

	for _, body := range []string{"hang", "two", "three"} {
		if code, out := send(t, m, a, "bob", body); code != http.StatusAccepted {
			t.Fatalf("send %s: %d %s", body, code, out)
		}
	}
	// The first starts a turn that does not end; the others wait behind it.
	e.waitFor("the first mail's turn", func() bool { return status(m, b.ID) == event.StatusWorking })
	time.Sleep(300 * time.Millisecond)
	got := users(e, b.ID)
	if len(got) != 1 || inbox(m, b.ID) != 2 {
		t.Fatalf("delivered %q, %d waiting", got, inbox(m, b.ID))
	}
	want := "[gero mail #1 from agent alice in isolation dev. "
	if !strings.HasPrefix(got[0], want) || !strings.HasSuffix(got[0], "\n\nhang") {
		t.Fatalf("the mail reads %q", got[0])
	}
	// An agent made without a prompt is named after what the mail says,
	// not after the line saying who wrote it.
	if v, _ := m.Agent(b.ID); v.Name != "hang" {
		t.Fatalf("named %q", v.Name)
	}

	// Its process dies mid-turn: interrupted, and the mail keeps waiting.
	if err := syscall.Kill(agentPID(t, m, b.ID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the interruption", func() bool { return status(m, b.ID) == event.StatusInterrupted })
	time.Sleep(300 * time.Millisecond)
	if n := len(users(e, b.ID)); n != 1 || inbox(m, b.ID) != 2 {
		t.Fatalf("an interrupted agent read mail: %d turns, %d waiting", n, inbox(m, b.ID))
	}
	if err := m.Send(b.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the inbox read", func() bool {
		return inbox(m, b.ID) == 0 && e.hasAssistant(b.ID, "echo: "+mailproxy.Letter(3, "alice", "dev", "three"))
	})
	got = users(e, b.ID)
	if len(got) != 4 || got[1] != "go on" || got[2] != mailproxy.Letter(2, "alice", "dev", "two") || got[3] != mailproxy.Letter(3, "alice", "dev", "three") {
		t.Fatalf("turns %q", got)
	}
	// One mail per turn: each began after the one before it ended.
	var order []string
	for _, ev := range e.events(b.ID) {
		switch ev.Kind {
		case event.KindUser, event.KindTurnEnd:
			order = append(order, string(ev.Kind))
		}
	}
	if s := strings.Join(order, ","); s != "user,turn_end,user,turn_end,user,turn_end,user,turn_end" {
		t.Fatalf("turns overlapped: %s", s)
	}

	// Read, the mail stays as both agents' history, newest first.
	hist := func(id string) string {
		ms, err := m.Mail(id)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ml := range ms {
			dir := "from " + ml.From + "@" + ml.FromIsolation
			if ml.Sent {
				dir = "to " + ml.To + "@" + ml.ToIsolation
			}
			out = append(out, fmt.Sprintf("#%d %s %s %s", ml.ID, dir, ml.State, ml.Text))
		}
		return strings.Join(out, "; ")
	}
	if got := hist("bob"); got != "#3 from alice@dev delivered three; #2 from alice@dev delivered two; #1 from alice@dev delivered hang" {
		t.Fatalf("bob's: %s", got)
	}
	if got := hist("alice"); got != "#3 to bob@dev delivered three; #2 to bob@dev delivered two; #1 to bob@dev delivered hang" {
		t.Fatalf("alice's: %s", got)
	}
}

// A mail is read exactly once, whatever the daemon does in between: it waits
// through a restart and is not read again after one.
func TestMailOutlivesTheDaemonAndIsReadOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	withMail(t, m)
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "bob"})
	m.Send(b.ID, "hang")
	e.waitFor("bob busy", func() bool { return status(m, b.ID) == event.StatusWorking })
	if code, out := send(t, m, a, "bob", "after you"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	stop()

	m, stop = e.daemon()
	withMail(t, m)
	e.waitFor("bob busy again", func() bool { return status(m, b.ID) == event.StatusWorking })
	if n := inbox(m, b.ID); n != 1 {
		t.Fatalf("%d waiting after a restart", n)
	}
	syscall.Kill(agentPID(t, m, b.ID), syscall.SIGKILL)
	e.waitFor("the interruption", func() bool { return status(m, b.ID) == event.StatusInterrupted })
	m.Send(b.ID, "carry on")
	letter := mailproxy.Letter(1, "alice", "dev", "after you")
	e.waitFor("the mail read", func() bool { return e.hasAssistant(b.ID, "echo: "+letter) })
	stop()

	m, stop = e.daemon()
	defer stop()
	withMail(t, m)
	e.waitFor("bob caught up", func() bool { return status(m, b.ID) == event.StatusIdle })
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, u := range users(e, b.ID) {
		if u == letter {
			n++
		}
	}
	if n != 1 || inbox(m, b.ID) != 0 {
		t.Fatalf("read %d times, %d waiting", n, inbox(m, b.ID))
	}
}

// An agent starts with its own address while its isolation takes mail, and
// gets one at its next idle moment once it does; the address answers
// through the isolation's tunnel.
func TestAnAgentsOwnAddressReachesTheDaemonThroughTheTunnel(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	port := freePort(t)
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return port }
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	m.Send(a.ID, "mail address")
	e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo: mail=") })

	if _, err := m.Expose(ctx, "dev", "mail", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	tok, _ := e.st.MailToken(a.ID)
	m.Send(a.ID, "mail address")
	e.waitFor("its address", func() bool { return e.hasAssistant(a.ID, "echo: mail="+mailproxy.URL(tok)) })
	req, _ := e.fc.lastSync()
	var skill bool
	for _, f := range req.Files {
		skill = skill || strings.HasSuffix(f.Path, "gero-mail/SKILL.md")
		if f.Path == EnvFile && strings.Contains(f.Content, tok) {
			t.Fatal("an agent's address is in the file every shell reads")
		}
	}
	if !skill {
		t.Fatalf("no skill: %+v", req.Files)
	}

	e.waitFor("the tunnel", func() bool { return m.tunnelStatus(testIsoID) == "up" })
	inside := strings.Replace(mailproxy.URL(tok), fmt.Sprint(mailproxy.Port), fmt.Sprint(port), 1)
	resp, err := http.Get(inside + "/agents")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var dir mailDirectory
	if json.Unmarshal(body, &dir); resp.StatusCode != 200 || dir.You.ID != "alice" || dir.You.Isolation != "dev" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// Mail goes to agents of the sender's own isolation, and of the isolations
// the operator named, by ID: a rename keeps the way open, and another
// isolation given the old name gets none of it.
func TestMailCrossesIsolationsOnlyWhereTheOperatorSaid(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	withMail(t, m)
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	// Agents of other isolations, stored only: mail waits in their inboxes
	// all the same.
	other := func(id, name, agent string) store.Agent {
		t.Helper()
		if err := e.st.PutIsolation(store.Isolation{ID: id, Name: name, Container: "gero-" + id, Image: "fake:1", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		ag := store.Agent{ID: agent + "0000", Handle: agent, Name: agent, Kind: driver.Codex, IsolationID: id, Workdir: "/w", CreatedAt: time.Now()}
		if err := e.st.CreateAgent(ag); err != nil {
			t.Fatal(err)
		}
		return ag
	}
	p := other("i2prod", "prod", "pat")
	if _, err := m.Expose(ctx, "prod", "mail", api.Expose{}); err != nil {
		t.Fatal(err)
	}

	code, out := send(t, m, a, "pat", "deploy it")
	if code != http.StatusForbidden || !strings.Contains(out, "`gero iso expose dev mail --to prod`") {
		t.Fatalf("out of reach: %d %s", code, out)
	}
	x, err := m.Expose(ctx, "dev", "mail", api.Expose{Settings: proxy.Patch{Set: map[string]any{"to": []string{"prod"}}}})
	if err != nil || string(x.Settings) != `{"to":["prod"]}` {
		t.Fatalf("%s %v", x.Settings, err)
	}
	if stored, _ := e.st.Exposure(testIsoID, "mail"); string(stored.Settings) != `{"to":["i2prod"]}` {
		t.Fatalf("stored %s", stored.Settings)
	}
	if _, err := m.Expose(ctx, "dev", "mail", api.Expose{Settings: proxy.Patch{Set: map[string]any{"to": []string{"nope"}}}}); err == nil {
		t.Fatal("an isolation that does not exist was let in")
	}
	if code, out := send(t, m, a, "pat", "deploy it"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	// One way: prod's agents may not write back.
	if code, out := send(t, m, p, "alice", "done"); code != http.StatusForbidden {
		t.Fatalf("prod wrote to dev: %d %s", code, out)
	}

	// The way follows the isolation, not its name.
	if _, err := m.UpdateIsolation(ctx, "prod", api.UpdateIsolation{Name: "prod-old"}); err != nil {
		t.Fatal(err)
	}
	q := other("i3prod", "prod", "quinn")
	m.Expose(ctx, "prod", "mail", api.Expose{})
	if code, _ := send(t, m, a, "pat", "again"); code != http.StatusAccepted {
		t.Fatalf("a rename closed the way: %d", code)
	}
	if code, out := send(t, m, a, "quinn", "hello"); code != http.StatusForbidden || !strings.Contains(out, "--to prod-old --to prod`") {
		t.Fatalf("a new isolation under the old name: %d %s", code, out)
	}
	xs, _ := m.Exposures("dev")
	for _, x := range xs {
		if x.Service == "mail" && string(x.Settings) != `{"to":["prod-old"]}` {
			t.Fatalf("shown as %s", x.Settings)
		}
	}
	code, out = mailAs(t, m, testIsoID, a.ID, "GET", "/agents", "")
	var dir mailDirectory
	json.Unmarshal([]byte(out), &dir)
	var ids []string
	for _, d := range dir.Agents {
		ids = append(ids, d.ID+"@"+d.Isolation)
	}
	if code != 200 || strings.Join(ids, ",") != "pat@prod-old" {
		t.Fatalf("directory %d %s", code, out)
	}

	// An isolation that takes no mail gets none, whoever may write to it.
	if err := m.Unexpose(ctx, "prod-old", "mail"); err != nil {
		t.Fatal(err)
	}
	if code, out := send(t, m, a, "pat", "anyone?"); code != http.StatusForbidden || !strings.Contains(out, "takes no mail") {
		t.Fatalf("%d %s", code, out)
	}
	if got, _ := m.Inbox("pat"); len(got) != 2 || got[0].Text != "deploy it" || got[0].From != "alice" || got[0].FromIsolation != "dev" {
		t.Fatalf("pat's inbox: %+v", got)
	}
	if got, _ := m.Inbox(q.ID); len(got) != 0 {
		t.Fatalf("quinn got %+v", got)
	}
}

func TestMailRefusesWhatItCannotDeliver(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return freePort(t) }
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "bob"})

	if code, out := send(t, m, a, "bob", "hi"); code != http.StatusForbidden || !strings.Contains(out, "gero iso expose dev mail") {
		t.Fatalf("mail off: %d %s", code, out)
	}
	m.Expose(ctx, "dev", "mail", api.Expose{})

	// What `gero iso verify` tries.
	for _, r := range (mailproxy.Service{}).Refusals(proxy.View{}) {
		if code, out := mailAt(m, testIsoID, r.Method, r.Path, ""); code != r.Status || !strings.Contains(out, r.Want) {
			t.Fatalf("%s: %d %s", r.What, code, out)
		}
	}
	// A token is honoured through its own agent's isolation only.
	if err := e.st.PutIsolation(store.Isolation{ID: "i2prod", Name: "prod", Container: "gero-i2prod", Image: "fake:1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m.Expose(ctx, "prod", "mail", api.Expose{})
	if code, out := mailAs(t, m, "i2prod", a.ID, "GET", "/agents", ""); code != http.StatusForbidden {
		t.Fatalf("dev's token through prod: %d %s", code, out)
	}

	for _, c := range []struct {
		to, body string
		code     int
		want     string
	}{
		{"", "hi", 400, "say who it is for"},
		{"nobody", "hi", 404, `no agent \"nobody\"`},
		{b.ID[:4], "hi", 404, "no agent"}, // a prefix is not an address
		{"alice", "hi", 400, "your own address"},
		{"bob", " \n", 400, "empty"},
		{"bob", "\xff\xfe", 400, "UTF-8"},
		{"bob", strings.Repeat("x", mailproxy.MaxBody+1), 413, "at most"},
	} {
		if code, out := send(t, m, a, c.to, c.body); code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("to %q: %d %s", c.to, code, out)
		}
	}
	if n := inbox(m, b.ID); n != 0 {
		t.Fatalf("%d refused mails were kept", n)
	}

	// Sending has a budget per isolation.
	codes := ""
	for i := 0; i < mailproxy.Burst+1; i++ {
		code, _ := send(t, m, a, "bob", "hang")
		codes += fmt.Sprint(code, " ")
	}
	if want := strings.Repeat("202 ", mailproxy.Burst) + "429 "; codes != want {
		t.Fatalf("codes %s", codes)
	}

	// A full inbox refuses more rather than dropping any.
	m.mailBuckets[testIsoID] = proxy.NewTokenBucket(1e6, 1e6, nil)
	for inbox(m, b.ID) < mailproxy.MaxWaiting {
		if code, out := send(t, m, a, "bob", "more"); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, out)
		}
	}
	if code, out := send(t, m, a, "bob", "one too many"); code != http.StatusTooManyRequests || !strings.Contains(out, "inbox is full") {
		t.Fatalf("%d %s", code, out)
	}

	// Archived agents neither write nor read.
	if err := m.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := send(t, m, a, "bob", "still here?"); code != http.StatusForbidden {
		t.Fatalf("an archived agent wrote: %d", code)
	}
	c, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "carol"})
	if code, out := send(t, m, c, "alice", "hi"); code != http.StatusConflict {
		t.Fatalf("mail to an archived agent: %d %s", code, out)
	}
}

// The operator sees what waits, and can take it out unread.
func TestTheOperatorReadsAndEmptiesAnInbox(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	withMail(t, m)
	cl, _ := serveAPI(t, m)
	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "alice"})
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "bob"})
	m.Send(b.ID, "hang")
	e.waitFor("bob busy", func() bool { return status(m, b.ID) == event.StatusWorking })
	for _, body := range []string{"one", "two\nmore", "three"} {
		send(t, m, a, "bob", body)
	}
	agents, _ := cl.Agents(ctx)
	for _, ag := range agents {
		if want := map[string]int{"alice": 0, "bob": 3}[ag.Handle]; ag.Inbox != want {
			t.Fatalf("%s: inbox %d", ag.Handle, ag.Inbox)
		}
	}
	got, err := cl.Inbox(ctx, "bob")
	if err != nil || len(got) != 3 || got[0].Text != "one" || got[1].Text != "two\nmore" || got[0].From != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	if err := cl.DiscardMail(ctx, "bob", got[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := cl.DiscardMail(ctx, "bob", got[1].ID); err == nil {
		t.Fatal("deleted twice")
	}
	if err := cl.DiscardMail(ctx, "alice", got[0].ID); err == nil {
		t.Fatal("deleted someone else's")
	}
	if n, err := cl.ClearInbox(ctx, "bob"); n != 2 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	hist, err := cl.Mail(ctx, "bob")
	if err != nil || len(hist) != 3 {
		t.Fatalf("%+v %v", hist, err)
	}
	for _, ml := range hist {
		if ml.State != api.MailDiscarded || ml.DiscardedAt == nil || ml.Sent {
			t.Fatalf("%+v", ml)
		}
	}
	if v, _ := cl.Agent(ctx, "bob"); v.Inbox != 0 {
		t.Fatalf("inbox %d", v.Inbox)
	}
	// A deleted agent's inbox goes with it.
	send(t, m, a, "bob", "last")
	m.Archive(ctx, b.ID)
	// Its process takes a moment to end.
	e.waitFor("bob deleted", func() bool { return cl.DeleteAgent(ctx, "bob", false) == nil })
	if n, _ := e.st.InboxCounts(); len(n) != 0 {
		t.Fatalf("left behind: %v", n)
	}
}
