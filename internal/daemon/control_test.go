package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/controlproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
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
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, controlproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
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
	if len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "lets agents change psps (agent.new, agent.send, agent.set, agent.cron)") {
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
	if env := m.controlEnv(boss); len(env) != 2 || !strings.HasPrefix(env[1], "GERO_CONTROL_URL=") || !strings.HasPrefix(env[0], "PSPS_CONTROL_URL=http://127.0.0.1:") || !strings.HasSuffix(env[0], "/control") {
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
		return slices.Contains(users(e, mate.ID), "[psps control: from agent boss in isolation dev, through psps's control service]\n\nthe notes are in notes.md")
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
		return slices.Contains(users(e, made.ID), "[psps control: agent boss in isolation dev made you, through psps's control service. Your operator may not be watching.]\n\nmeasure it")
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

	// The operator may give the ones it makes elsewhere auto, as much as
	// its own; never is not theirs to give that way.
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}, Settings: proxy.Patch{Set: map[string]any{"approvals": "never"}}}); err == nil {
		t.Fatal("never given to the agents made elsewhere")
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}, Settings: proxy.Patch{Set: map[string]any{"approvals": "auto"}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Archive(ctx, made.ID); err != nil {
		t.Fatal(err)
	}
	code, out = ctl(boss, "GET", "", "")
	if !strings.Contains(out, `"elsewhere_approvals":"auto"`) || !strings.Contains(out, `"far"`) {
		t.Fatalf("the view: %d %s", code, out)
	}
	code, out = ctl(boss, "POST", "/agents", `{"kind": "codex", "prompt": "measure again", "isolation": "far"}`)
	json.Unmarshal([]byte(out), &made)
	if code != http.StatusCreated || made.Isolation != "far" || made.Options.Approvals != driver.ApprovalsAuto {
		t.Fatalf("one made elsewhere under auto: %d %s", code, out)
	}
	if err := m.Archive(ctx, made.ID); err != nil {
		t.Fatal(err)
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
	if l := cronLetter(cr); !strings.Contains(l, "Agent boss in isolation dev set this message, through psps's control service") {
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

// An agent sets a team's eval through the control service to run in its
// own isolation, on a copy of a directory in the lead's: never in the
// lead's isolation, on nothing else of it, and nothing else of the run.
func TestControlSetsATeamsEvalToRunWhereItMayRunCommands(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	for _, name := range []string{"ev", "ev2"} {
		if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: name}, nil); err != nil {
			t.Fatal(err)
		}
	}
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000))})
	helper, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "ev", Kind: driver.Codex, Handle: "helper"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("helper", func() bool { return status(m, helper.ID) == event.StatusIdle })
	if _, err := m.PutEnv(ctx, "control", "grade", proxy.Patch{Set: map[string]any{"allow": []string{"team.show", "team.evalset"}}}, true); err != nil {
		t.Fatal(err)
	}
	for _, iso := range []string{"ev", "dev"} {
		if _, err := m.Expose(ctx, iso, "control", api.Expose{Envs: []string{"grade"}, Settings: proxy.Patch{Set: map[string]any{"reach": map[string][]string{"ev": {"dev", "ev2"}, "dev": {"ev"}}[iso]}}}); err != nil {
			t.Fatal(err)
		}
	}
	set := func(from store.Agent, body string) (int, string) {
		t.Helper()
		return mailAs(t, m, from.IsolationID, from.ID, "PUT", "/control/teams/lab/eval", body)
	}
	if code, out := set(helper, `{"eval": "sh /home/agent/evaluator/run.sh", "eval_dir": "/home/agent"}`); code != http.StatusForbidden || !strings.Contains(out, "the lead's working directory") {
		t.Fatalf("a directory outside the lead's: %d %s", code, out)
	}
	if code, out := set(helper, `{"eval": "x", "eval_dir": "`+boss.Workdir+`/../.."}`); code != http.StatusForbidden {
		t.Fatalf("out by ..: %d %s", code, out)
	}
	if code, out := set(helper, `{"eval": "x", "eval_isolation": "dev"}`); code != http.StatusForbidden || !strings.Contains(out, "which is the lead's") {
		t.Fatalf("the lead's isolation, named: %d %s", code, out)
	}
	if code, out := set(helper, `{"eval": "x", "eval_isolation": "far"}`); code != http.StatusNotFound {
		t.Fatalf("an isolation it does not reach: %d %s", code, out)
	}
	if code, out := set(helper, `{"eval": "x", "eval_isolation": "ev2"}`); code != http.StatusForbidden || !strings.Contains(out, "may not run commands") {
		t.Fatalf("one where it may not run commands: %d %s", code, out)
	}
	if code, out := set(boss, `{"eval": "x"}`); code != http.StatusForbidden || !strings.Contains(out, "which is the lead's") {
		t.Fatalf("from the lead's isolation: %d %s", code, out)
	}

	// A run psps stopped stays stopped; a round that ended before the eval
	// is not its to score.
	if err := m.store.SetLeadStopped(team.ID, "the budget is used"); err != nil {
		t.Fatal(err)
	}
	if err := m.store.SetLeadDirty(team.ID, true); err != nil {
		t.Fatal(err)
	}
	code, out := set(helper, `{"eval": "sh /home/agent/evaluator/run.sh", "eval_dir": "`+boss.Workdir+`/app", "eval_minutes": 5}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	run, _ := m.TeamRun("lab")
	s := run.Settings
	if s.Eval != "sh /home/agent/evaluator/run.sh" || s.EvalDir != boss.Workdir+"/app" || s.EvalMinutes != 5 || run.EvalIsolation != "ev" ||
		s.EvalSetBy != "helper in isolation ev" || s.Budget != 1_000_000 || run.Stopped != "the budget is used" {
		t.Fatalf("%+v", run)
	}
	if l, _ := m.store.Lead(team.ID); l.Dirty {
		t.Fatal("a round that ended before the eval is still to score")
	}

	// Where it may run commands it may run the eval, and what it leaves out
	// stays as it is, where the eval runs among it.
	if _, err := m.Expose(ctx, "ev", "control", api.Expose{Envs: []string{"grade"}, Settings: proxy.Patch{Set: map[string]any{"approvals": "auto"}}}); err != nil {
		t.Fatal(err)
	}
	if code, out := set(helper, `{"eval_isolation": "ev2"}`); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := set(helper, `{"eval_minutes": 60}`); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if run, _ = m.TeamRun("lab"); run.EvalIsolation != "ev2" || run.Settings.EvalMinutes != 60 || run.Settings.Eval != "sh /home/agent/evaluator/run.sh" {
		t.Fatalf("%+v", run)
	}
	if _, err := m.Expose(ctx, "ev", "control", api.Expose{Envs: []string{"grade"}, Settings: proxy.Patch{Set: map[string]any{"approvals": "ask"}}}); err != nil {
		t.Fatal(err)
	}
	if code, out := set(helper, `{"eval_minutes": 30}`); code != http.StatusForbidden || !strings.Contains(out, "runs in isolation ev2") {
		t.Fatalf("kept where it may no longer run commands: %d %s", code, out)
	}

	// It reads the run as text, as psps team show prints it.
	code, out = mailAs(t, m, helper.IsolationID, helper.ID, "GET", "/control/teams/lab/run?format=text", "")
	if code != 200 || !strings.HasPrefix(out, `team "lab", led by `) || !strings.Contains(out, "  eval   sh /home/agent/evaluator/run.sh (in isolation ev2") || strings.HasPrefix(out, "{") {
		t.Fatalf("as text: %d %s", code, out)
	}
	if code, out := mailAs(t, m, helper.IsolationID, helper.ID, "GET", "/control/teams/lab/run?format=yaml", ""); code != http.StatusBadRequest {
		t.Fatalf("an unknown format: %d %s", code, out)
	}
	if code, out := mailAs(t, m, helper.IsolationID, helper.ID, "GET", "/control/teams/nope/run?format=text", ""); code != http.StatusNotFound || !strings.HasPrefix(out, "psps: ") {
		t.Fatalf("an error, as text: %d %s", code, out)
	}

	// Changed by the operator, it is theirs again.
	if run, err = m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Eval: ptr("make score")}); err != nil || run.Settings.EvalSetBy != "" {
		t.Fatalf("%+v %v", run.Settings, err)
	}
}

// A command runs in an isolation only where an agent its caller made would
// run under auto, and what it printed comes back with its exit code.
func TestControlRunsACommandWhereAnAgentItMadeWouldRunUnderAuto(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	fakeTimeout(t)
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, controlproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "far"}, nil); err != nil {
		t.Fatal(err)
	}
	agent := func(handle string, approvals driver.Approvals) store.Agent {
		a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: handle, Options: driver.Options{Approvals: approvals}})
		if err != nil {
			t.Fatal(err)
		}
		e.waitFor(handle, func() bool { return status(m, a.ID) == event.StatusIdle })
		return a
	}
	boss := agent("boss", driver.ApprovalsAuto)
	asker := agent("asker", driver.ApprovalsAsk)
	if _, err := m.PutEnv(ctx, "control", "ops", proxy.Patch{Set: map[string]any{"allow": []string{"iso.exec"}}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}, Settings: proxy.Patch{Set: map[string]any{"reach": []string{"far"}}}}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	run := func(from store.Agent, iso string, req map[string]any) (int, api.ControlExec, string) {
		t.Helper()
		body, _ := json.Marshal(req)
		code, out := mailAs(t, m, from.IsolationID, from.ID, "POST", "/control/isolations/"+iso+"/exec", string(body))
		var v api.ControlExec
		json.Unmarshal([]byte(out), &v)
		return code, v, out
	}

	code, v, out := run(boss, "dev", map[string]any{"command": "echo hi; pwd; echo oops >&2; exit 3", "dir": dir})
	if code != 200 || v.Exit != 3 || v.Cut || !strings.Contains(v.Output, "hi") || !strings.Contains(v.Output, "oops") || !strings.Contains(v.Output, filepath.Base(dir)) {
		t.Fatalf("%d %s", code, out)
	}
	code, v, out = run(boss, "dev", map[string]any{"command": "head -c 70000 /dev/zero | tr '\\0' a", "dir": dir})
	if code != 200 || v.Exit != 0 || !v.Cut || len(v.Output) != 64<<10 {
		t.Fatalf("%d %d %v", code, len(v.Output), v.Cut)
	}
	// The body itself as the script: no quoting, dir and seconds in the
	// query, and what it printed back as text, its exit code after it.
	script := func(from store.Agent, iso, query, ctype, body string) (int, string, http.Header) {
		t.Helper()
		tok, err := m.store.MailToken(from.ID)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/a/"+tok+"/control/isolations/"+iso+"/exec"+query, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		m.mailHandler(from.IsolationID).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), rec.Header()
	}
	sh := "n=$(printf '%s\\n' \"a b\" 'c' | wc -l)\necho \"lines: $((n+0))\" \"it's\"\nsed -n '1p' <<'X'\n{\"first\": 1}\nX\npwd\nexit 4\n"
	code, text, h := script(boss, "dev", "?dir="+url.QueryEscape(dir)+"&seconds=30", "", sh)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "text/plain") || h.Get("X-Exit-Code") != "4" ||
		text != "lines: 2 it's\n{\"first\": 1}\n"+dir+"\n[psps: exit 4]\n" {
		t.Fatalf("a script: %d %q %v", code, text, h)
	}
	if code, text, _ := script(boss, "dev", "?dir="+url.QueryEscape(dir), "text/plain", `{ echo grouped; }`); code != 200 || text != "grouped\n" {
		t.Fatalf("a script that starts with {, said to be text: %d %q", code, text)
	}
	if code, text, _ := script(boss, "dev", "", "", `{"command": "echo json", "dir": "`+dir+`"}`); code != 200 || !strings.HasPrefix(text, "{") || !strings.Contains(text, `"output":"json"`) {
		t.Fatalf("JSON with no type said: %d %q", code, text)
	}
	if code, text, _ := script(boss, "dev", "?format=text", "application/json", `{"command": "echo hi; exit 2", "dir": "`+dir+`"}`); code != 200 || text != "hi\n[psps: exit 2]\n" {
		t.Fatalf("JSON asking for text: %d %q", code, text)
	}
	if code, text, _ := script(boss, "dev", "?format=json&dir="+url.QueryEscape(dir), "", "echo hi"); code != 200 || !strings.Contains(text, `"output":"hi"`) {
		t.Fatalf("a script asking for JSON: %d %q", code, text)
	}
	if code, text, _ := script(boss, "dev", "?dir="+url.QueryEscape(dir), "", "head -c 70000 /dev/zero | tr '\\0' a"); code != 200 || !strings.HasPrefix(text, "[psps: only the last 64 KiB of what it printed]\n") {
		t.Fatalf("cut: %d %.80q", code, text)
	}
	for _, q := range []string{"?seconds=x", "?seconds=601", "?dir=rel", "?format=yaml"} {
		if code, text, _ := script(boss, "dev", q, "", "true"); code != http.StatusBadRequest {
			t.Fatalf("%s: %d %q", q, code, text)
		}
	}
	if code, text, _ := script(asker, "dev", "?dir="+url.QueryEscape(dir), "", "true"); code != http.StatusForbidden || !strings.HasPrefix(text, "psps: control: ") || !strings.Contains(text, "yours ask") {
		t.Fatalf("refused, as text: %d %q", code, text)
	}

	for _, req := range []map[string]any{{"command": " "}, {"command": "true", "seconds": 601}, {"command": "true", "dir": "rel"}} {
		if code, _, out := run(boss, "dev", req); code != http.StatusBadRequest {
			t.Fatalf("%v: %d %s", req, code, out)
		}
	}
	if code, _, out := run(asker, "dev", map[string]any{"command": "true", "dir": dir}); code != http.StatusForbidden || !strings.Contains(out, "yours ask") {
		t.Fatalf("an agent under ask: %d %s", code, out)
	}
	if code, _, out := run(boss, "far", map[string]any{"command": "true", "dir": dir}); code != http.StatusForbidden || !strings.Contains(out, "elsewhere_approvals") {
		t.Fatalf("elsewhere, without auto there: %d %s", code, out)
	}
	if code, _, _ := run(boss, "nowhere", map[string]any{"command": "true", "dir": dir}); code != http.StatusNotFound {
		t.Fatalf("an isolation it does not reach: %d", code)
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}, Settings: proxy.Patch{Set: map[string]any{"approvals": "auto"}}}); err != nil {
		t.Fatal(err)
	}
	if code, v, out := run(boss, "far", map[string]any{"command": "echo there", "dir": dir}); code != 200 || v.Output != "there" {
		t.Fatalf("elsewhere, with auto there: %d %s", code, out)
	}
	if code, _, _ := run(asker, "far", map[string]any{"command": "true", "dir": dir}); code != http.StatusForbidden {
		t.Fatalf("an agent under ask, elsewhere: %d", code)
	}
}
