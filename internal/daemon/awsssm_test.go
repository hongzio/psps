package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/awsproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
)

// In strict SSM mode an agent asks psps for a command on an instance; the
// request is an approval in its conversation, and the agent's request
// answers with the person's decision and, accepted, the run's outcome.
func TestStrictSSMAsksThePersonInTheAgentsConversation(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, awsproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("ops", func() bool { return status(m, a.ID) == event.StatusIdle })
	const inst = "i-0a66b609cd50bb881"
	// /bin/echo as the aws CLI: what it answers is no JSON, so a run fails.
	if _, err := m.PutEnv(ctx, "aws", "prod", proxy.Patch{Set: map[string]any{"role_arn": "arn:aws:iam::111122223333:role/w",
		"aws_profile": "dev", "aws_cli": "/bin/echo", "instances": []string{inst + "=session"}}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "aws", api.Expose{Envs: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	ask := func(body string) (int, string) {
		t.Helper()
		return mailAs(t, m, a.IsolationID, a.ID, "POST", "/aws/ssm", body)
	}
	if len(m.awsEnv(a)) != 0 {
		t.Fatal("an address before strict mode")
	}
	if code, body := ask(`{"instance":"` + inst + `","command":"uptime"}`); code != 409 || !strings.Contains(body, "not in strict SSM mode") {
		t.Fatalf("direct mode: %d %s", code, body)
	}
	if _, err := m.Expose(ctx, "dev", "aws", api.Expose{Settings: proxy.Patch{Set: map[string]any{"ssm": "strict"}}}); err != nil {
		t.Fatal(err)
	}
	if env := m.awsEnv(a); len(env) != 2 || !strings.HasPrefix(env[1], "GERO_AWS_URL=") || !strings.HasPrefix(env[0], "PSPS_AWS_URL=http://127.0.0.1:") || !strings.HasSuffix(env[0], "/aws") {
		t.Fatalf("%q", env)
	}
	if code, body := ask(`{"instance":"i-0fffffffffffffff0","command":"uptime"}`); code != 400 || !strings.Contains(body, "not on this environment's allowlist") {
		t.Fatalf("off the allowlist: %d %s", code, body)
	}

	// pending is the open ssm approval in the agent's conversation.
	pending := func() *event.Approval {
		t.Helper()
		var open *event.Approval
		e.waitFor("an ssm approval", func() bool {
			open = nil
			done := map[string]bool{}
			for _, ev := range e.events(a.ID) {
				if ev.Kind == event.KindApprovalDone {
					done[ev.Approval.ID] = true
				}
			}
			for _, ev := range e.events(a.ID) {
				if ev.Kind == event.KindApproval && ev.Approval.Kind == "ssm" && !done[ev.Approval.ID] {
					open = ev.Approval
				}
			}
			return open != nil
		})
		return open
	}
	type answer struct {
		code int
		body string
	}
	request := func(cmd string) chan answer {
		ch := make(chan answer, 1)
		b, _ := json.Marshal(map[string]any{"instance": inst, "command": cmd})
		go func() {
			code, body := ask(string(b))
			ch <- answer{code, body}
		}()
		return ch
	}
	outcome := func(ch chan answer) map[string]any {
		t.Helper()
		select {
		case got := <-ch:
			var v map[string]any
			if got.code != 200 || json.Unmarshal([]byte(got.body), &v) != nil {
				t.Fatalf("%d %s", got.code, got.body)
			}
			return v
		case <-time.After(20 * time.Second):
			t.Fatal("the request never answered")
		}
		return nil
	}

	ch := request("# why: the log\ntail -n 5 /var/log/x")
	ap := pending()
	if ap.Detail != "# why: the log\ntail -n 5 /var/log/x" || !strings.Contains(ap.Summary, inst) || !strings.Contains(ap.Summary, "# why: the log") {
		t.Fatalf("approval = %+v", ap)
	}
	if err := m.Approve(a.ID, ap.ID, driver.Decline, nil); err != nil {
		t.Fatal(err)
	}
	if v := outcome(ch); v["state"] != "declined" || !strings.Contains(v["note"].(string), "do not send it again") {
		t.Fatalf("%v", v)
	}
	if err := m.Approve(a.ID, ap.ID, driver.Accept, nil); err == nil {
		t.Fatal("a decided request was decided again")
	}

	ch = request("uptime")
	ap = pending()
	if err := m.Approve(a.ID, ap.ID, driver.Accept, nil); err != nil {
		t.Fatal(err)
	}
	if v := outcome(ch); v["state"] != "failed" || !strings.Contains(v["error"].(string), "describe-regions") {
		t.Fatalf("%v", v)
	}
	// Fetched again, the outcome is the same.
	if code, body := mailAs(t, m, a.IsolationID, a.ID, "GET", "/aws/ssm/"+ap.ID, ""); code != 200 || !strings.Contains(body, `"failed"`) {
		t.Fatalf("%d %s", code, body)
	}

	// One the daemon does not hold (it restarted) stops asking.
	m.addEvents(a.ID, event.Event{Kind: event.KindApproval, Approval: &event.Approval{ID: "ssm-gone", Kind: "ssm", Summary: "x"}})
	if err := m.Approve(a.ID, "ssm-gone", driver.Accept, nil); err == nil || !strings.Contains(err.Error(), "no longer waiting") {
		t.Fatalf("%v", err)
	}
	void := false
	for _, ev := range e.events(a.ID) {
		void = void || (ev.Kind == event.KindApprovalDone && ev.Approval.ID == "ssm-gone" && ev.Approval.Decision == "void")
	}
	if !void {
		t.Fatal("a request the daemon lost was not closed")
	}
}
