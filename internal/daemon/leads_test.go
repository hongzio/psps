package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/controlproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/proxy/teamproxy"
	"github.com/hongzio/psps/internal/store"
)

// withTeams gives the manager the team service, on for dev, and a lead:
// boss, idle, leading team lab on terms.
func withTeams(t *testing.T, e *testEnv, m *Manager, terms api.PutLead) (store.Agent, store.Team) {
	t.Helper()
	ctx := context.Background()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, teamproxy.Service{}, controlproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	if _, err := m.Expose(ctx, "dev", "team", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	boss, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "boss", Options: driver.Options{Approvals: driver.ApprovalsAuto}})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the lead", func() bool { return status(m, boss.ID) == event.StatusIdle })
	team, err := m.CreateTeam("lab")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := teamAs(t, m, boss.ID, "GET", "", ""); code != http.StatusForbidden || !strings.Contains(out, "you lead no team") {
		t.Fatalf("an agent that leads nothing: %d %s", code, out)
	}
	terms.Agent = "boss"
	if _, err := m.SetLead(ctx, "lab", terms); err != nil {
		t.Fatal(err)
	}
	return boss, team
}

func teamAs(t *testing.T, m *Manager, from, method, path, body string) (int, string) {
	t.Helper()
	return mailAs(t, m, testIsoID, from, method, "/team"+path, body)
}

// makeAgent has from make an agent of its own, and says which.
func makeAgent(t *testing.T, m *Manager, from string, req api.TeamAgentNew) api.TeamAgent {
	t.Helper()
	b, _ := json.Marshal(req)
	code, out := teamAs(t, m, from, "POST", "/agents", string(b))
	if code != http.StatusCreated {
		t.Fatalf("made no agent: %d %s", code, out)
	}
	var a api.TeamAgent
	json.Unmarshal([]byte(out), &a)
	return a
}

// free is whether agent id is idle with nothing on its way to it, as a
// lead whose turn on its news has ended.
func free(m *Manager, id string) bool {
	r, err := m.runtime(id)
	return err == nil && r.free()
}

// news is what boss was told of its team, message by message.
func news(e *testEnv, boss string) []string {
	var out []string
	for _, u := range users(e, boss) {
		if strings.HasPrefix(u, "[psps team news for you as lead of team ") {
			out = append(out, u)
		}
	}
	return out
}

func TestALeadMakesAgentsOfItsOwnAndHearsOfTheirTurns(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	boss, _ := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), MaxLive: ptr(1)})
	other, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "other"})

	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "Prof iler!", Kind: driver.Codex, Prompt: "hello"})
	if !strings.HasPrefix(a.ID, "prof-iler-") || a.Role != "Prof iler!" {
		t.Fatalf("%+v", a)
	}
	made, err := m.storedAgent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if made.MadeBy != boss.ID || made.TurnMinutes != leadTurnMinutes || made.Options.Approvals != driver.ApprovalsAuto || made.Workdir != boss.Workdir {
		t.Fatalf("%+v", made)
	}
	if v, _ := m.Agent(made.ID); v.Team != "lab" {
		t.Fatalf("made in team %q", v.Team)
	}
	// Its turn's end is the lead's news, with its answer.
	e.waitFor("the lead's news", func() bool { return len(news(e, boss.ID)) == 1 })
	n := news(e, boss.ID)[0]
	for _, want := range []string{`team "lab": 1 item(s)`, a.ID + ` ("Prof iler!", codex) ended its turn: idle, after `, "It has used 1.1k tokens.",
		"Its last answer:\n\necho: [psps team: you are \"Prof iler!\" in team \"lab\", an agent its lead boss made.", "]\n\nhello"} {
		if !strings.Contains(n, want) {
			t.Fatalf("news lacks %q:\n%s", want, n)
		}
	}

	// Only what it made is its own; only one at a time is live.
	if code, _ := teamAs(t, m, boss.ID, "GET", "/agents/other", ""); code != http.StatusNotFound {
		t.Fatalf("another's agent: %d", code)
	}
	if code, out := teamAs(t, m, other.ID, "GET", "", ""); code != http.StatusForbidden {
		t.Fatalf("not a lead: %d %s", code, out)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents", `{"role":"two","kind":"codex","prompt":"hi"}`); code != http.StatusTooManyRequests || !strings.Contains(out, "1 agents of yours are live") {
		t.Fatalf("past max live: %d %s", code, out)
	}
	code, out := teamAs(t, m, boss.ID, "GET", "/agents/"+a.ID, "")
	var one api.TeamAgent
	json.Unmarshal([]byte(out), &one)
	if code != 200 || one.Status != event.StatusIdle || one.Spent.New() != 1100 || !strings.HasSuffix(one.Answer, "]\n\nhello") {
		t.Fatalf("%d %s", code, out)
	}

	// What it writes reaches the agent as its lead's.
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+a.ID+"/send", "again"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the lead's message", func() bool { return e.hasAssistant(made.ID, "echo: [psps team: from your lead boss]\n\nagain") })
	e.waitFor("the second news", func() bool { return len(news(e, boss.ID)) == 2 })
	e.waitFor("the lead's turn", func() bool { return status(m, boss.ID) == event.StatusIdle && len(e.events(boss.ID)) > 0 })
	time.Sleep(100 * time.Millisecond)

	// The lead's own turns count toward the budget too.
	var run api.TeamRun
	_, out = teamAs(t, m, boss.ID, "GET", "", "")
	json.Unmarshal([]byte(out), &run)
	if run.Lead != "boss" || len(run.Agents) != 1 || run.Agents[0].ID != a.ID || run.Spent.New() != 2*1100+2*1100 || run.Settings.MaxLive != 1 {
		t.Fatalf("%s", out)
	}

	// Archived, it is live no more.
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+a.ID+"/archive", ""); code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	b := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "two", Kind: driver.Codex, Prompt: "hi", TurnMinutes: 5})
	e.waitFor("its news", func() bool { return len(news(e, boss.ID)) == 3 })

	// The budget used, psps stops the run until the operator resumes it.
	if _, err := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss", Budget: ptr(int64(3000))}); err != nil {
		t.Fatal(err)
	}
	m.teamCheck(run.Team.ID)
	if r, _ := m.TeamRun("lab"); !strings.HasPrefix(r.Stopped, "the budget is used: ") || !strings.HasSuffix(r.Stopped, " of 3.0k tokens") {
		t.Fatalf("%+v", r)
	}
	e.waitFor("the stop's news", func() bool { return len(news(e, boss.ID)) == 4 })
	if n := news(e, boss.ID)[3]; !strings.Contains(n, "psps stopped the run: the budget is used") {
		t.Fatal(n)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+b.ID+"/send", "more"); code != http.StatusConflict {
		t.Fatalf("a stopped run wrote: %d %s", code, out)
	}
	// Resumed on the same budget, it would stop again at once.
	if _, err := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss"}); err == nil ||
		!strings.Contains(err.Error(), "the run would stop again at once: the team has used ") || !strings.Contains(err.Error(), " of its 3.0k tokens; give it a larger budget") {
		t.Fatalf("resumed with its budget used: %v", err)
	}
	if r, _ := m.TeamRun("lab"); r.Stopped == "" {
		t.Fatal("a refused resume resumed the run")
	}
	if _, err := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss", Budget: ptr(int64(1_000_000))}); err != nil {
		t.Fatal(err)
	}
	// The lead hears it goes on, and is asked for the next team.
	e.waitFor("the resume's news", func() bool {
		return strings.Contains(strings.Join(news(e, boss.ID), "\n"), "Your operator resumed the run")
	})
	for _, n := range news(e, boss.ID)[4:] {
		if i := strings.Index(n, "Your operator resumed the run"); i >= 0 {
			n = n[i:]
			for _, want := range []string{"Your operator resumed the run, which psps had stopped (the budget is used: ", " of 3.0k tokens). Nothing of your team is running. The team has used ",
				" of 1.0M tokens. Design the next team, or say why you stop."} {
				if !strings.Contains(n, want) {
					t.Fatalf("news lacks %q:\n%s", want, n)
				}
			}
			if strings.Contains(n, "patience") {
				t.Fatalf("a run without an eval told of patience:\n%s", n)
			}
		}
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+b.ID+"/send", "more"); code != http.StatusAccepted {
		t.Fatalf("resumed: %d %s", code, out)
	}

	// Without a lead, its agents are the operator's: their turns are no
	// one's news.
	if err := m.ClearLead("lab"); err != nil {
		t.Fatal(err)
	}
	if code, _ := teamAs(t, m, boss.ID, "GET", "", ""); code != http.StatusForbidden {
		t.Fatalf("a former lead: %d", code)
	}
}

// fakeTimeout puts a timeout(1) on PATH that runs its command as it is,
// as the isolation's does within its limit.
func fakeTimeout(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "timeout"), []byte("#!/bin/sh\nshift 2\nexec \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// A lead without a budget is held back only by the plan ceiling, so it
// leads, and makes agents, only where their CLI runs on a subscription.
func TestALeadWithoutABudgetRunsOnlyOnASubscription(t *testing.T) {
	e, m, home := cliEnv(t)
	ctx := context.Background()
	loggedInCodex(t, home, "ChatGPT")
	boss, team := withTeams(t, e, m, api.PutLead{})
	run, err := m.TeamRun("lab")
	if err != nil || run.Settings.Budget != 0 {
		t.Fatalf("%+v %v", run, err)
	}
	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("its news", func() bool { return len(news(e, boss.ID)) == 1 })
	m.teamCheck(team.ID)
	if run, _ = m.TeamRun("lab"); run.Stopped != "" || run.Spent.New() == 0 {
		t.Fatalf("a run without a budget stopped, or spent nothing: %+v", run)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+a.ID+"/send", "more"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}

	// Claude on an API key: no claude agent without a budget.
	key := "sk-ant-api03-test"
	if err := m.SetEnv("dev", api.SetEnv{"ANTHROPIC_API_KEY": &key}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(api.TeamAgentNew{Role: "two", Kind: driver.Claude, Prompt: "hello"})
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents", string(b)); code != http.StatusForbidden || !strings.Contains(out, "claude is logged in there with API key (ANTHROPIC_API_KEY), billed per use") {
		t.Fatalf("a claude agent on an API key: %d %s", code, out)
	}

	// The lead's own Codex on an API key: it leads only with a budget.
	loggedInCodex(t, home, "an API key - sk-proj-***")
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss"}); err == nil || !strings.Contains(err.Error(), "codex is logged in there with API key, billed per use") {
		t.Fatalf("a lead on an API key without a budget: %v", err)
	}
	if run, err = m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Budget: ptr(int64(1_000_000))}); err != nil || run.Settings.Budget != 1_000_000 {
		t.Fatalf("%+v %v", run, err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Budget: ptr(int64(0))}); err == nil {
		t.Fatal("a budget taken off a lead on an API key")
	}
}

func TestARoundEndsWithTheEvalAndARunWithoutProgressStops(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	dir := t.TempDir()
	score := func(s string) { os.WriteFile(filepath.Join(dir, "score"), []byte("measuring\nSCORE: "+s+"\n"), 0o644) }
	score("1.5")
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr("cat score"), EvalDir: ptr(dir), Patience: ptr(2)})
	told := func() string { return strings.Join(news(e, boss.ID), "\n\n") }

	// The lead hears of its agents' turns before any eval: it may go on
	// with the round.
	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	b := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "two", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("both turns' news", func() bool {
		return strings.Contains(told(), a.ID+` ("one", codex) ended its turn`) && strings.Contains(told(), b.ID+` ("two", codex) ended its turn`)
	})
	if strings.Contains(told(), "Eval #") {
		t.Fatalf("an eval before the lead heard of the round:\n%s", told())
	}
	// Until its turn on them ends, the round goes on.
	e.waitFor("the lead at work", func() bool { return status(m, boss.ID) != event.StatusIdle || len(e.events(boss.ID)) > 0 })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	before := len(news(e, boss.ID))

	// Its turn ended with nothing started: the round is over, and the
	// eval's news says nothing runs.
	m.teamCheck(team.ID)
	e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == before+1 })
	n := news(e, boss.ID)[before]
	for _, want := range []string{"1 item(s)", "Eval #1 (the round ended) took ", "SCORE 1.5, a new best.",
		"Nothing of your team is running. The team has used ", "which signs of a stuck team yours shows", "Then design the next team, have the eval run on a change of your own (POST $PSPS_TEAM_URL/eval), or say why you stop."} {
		if !strings.Contains(n, want) {
			t.Fatalf("news lacks %q:\n%s", want, n)
		}
	}
	// Told so once.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if len(news(e, boss.ID)) != before+1 {
		t.Fatalf("nudged twice: %q", news(e, boss.ID)[before:])
	}

	// Evals without a new best, as many as its patience, stop the run.
	score("1.2")
	for i, want := range []string{"SCORE 1.2; the best is 1.5, at eval #1. 1 of 2 evals without a new best.", "2 of 2 evals without a new best."} {
		if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusAccepted {
			t.Fatalf("%d %s", code, out)
		}
		e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == before+2+i })
		if n := news(e, boss.ID)[before+1+i]; !strings.Contains(n, want) || strings.Contains(n, "Nothing of your team is running") {
			t.Fatalf("news lacks %q, or asks for a team after an eval asked for:\n%s", want, n)
		}
		e.waitFor("the lead idle", func() bool { return status(m, boss.ID) == event.StatusIdle })
	}
	if n := news(e, boss.ID)[before+2]; !strings.Contains(n, "psps stopped the run: 2 evals in a row came without a new best") {
		t.Fatal(n)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents", `{"role":"three","kind":"codex","prompt":"hi"}`); code != http.StatusConflict {
		t.Fatalf("a stopped run made an agent: %d %s", code, out)
	}
	run, _ := m.TeamRun("lab")
	if len(run.Evals) != 3 || run.Best == nil || run.Best.ID != 1 || run.Stale != 2 || !strings.HasPrefix(run.Status, "stopped: ") {
		t.Fatalf("%+v", run)
	}
	// A target its best reaches would stop it again at its next eval.
	if _, err := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss", Target: ptr(1.5)}); err == nil ||
		!strings.Contains(err.Error(), "the run would stop again at its next eval: eval #1 scored 1.5, which reaches the target 1.5; raise the target") {
		t.Fatalf("resumed past its target: %v", err)
	}
	// Resumed, the evals so far no longer count, and the lead hears so.
	if run, _ = m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss"}); run.Stopped != "" || run.Stale != 0 || run.Settings.Target != nil {
		t.Fatalf("%+v", run)
	}
	e.waitFor("the resume's news", func() bool { return len(news(e, boss.ID)) == before+4 })
	n = news(e, boss.ID)[before+3]
	for _, want := range []string{"1 item(s)", "Your operator resumed the run, which psps had stopped (2 evals in a row came without a new best). The evals so far count no more toward its patience. The best is 1.5, at eval #1. Nothing of your team is running. The team has used ",
		"which signs of a stuck team yours shows", "Then design the next team, have the eval run on a change of your own (POST $PSPS_TEAM_URL/eval), or say why you stop."} {
		if !strings.Contains(n, want) {
			t.Fatalf("news lacks %q:\n%s", want, n)
		}
	}

	// The operator resets the best: the evals so far count for none, a
	// target the old best reached is set, and the next scored eval is the
	// best. The lead hears so.
	if err := m.ResetBest("lab"); err != nil {
		t.Fatal(err)
	}
	if run, _ = m.TeamRun("lab"); run.Best != nil || run.Stale != 0 {
		t.Fatalf("%+v", run)
	}
	e.waitFor("the reset's news", func() bool {
		return strings.Contains(told(), "Your operator reset the best score: evals up to #3 count no more, for the best, the target or the patience; the next scored eval is the best. The best was 1.5, at eval #1.")
	})
	if _, err := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss", Target: ptr(1.5)}); err != nil {
		t.Fatalf("the reset best still stops the run: %v", err)
	}
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the eval after the reset", func() bool { r, _ := m.TeamRun("lab"); return len(r.Evals) == 4 })
	if run, _ = m.TeamRun("lab"); run.Best == nil || run.Best.ID != 4 || *run.Best.Score != 1.2 || run.Stopped != "" {
		t.Fatalf("%+v", run)
	}
}

// A lead whose run compacts first has /compact sent once a round's eval
// ended, and hears the round's score after it; an eval it asks for comes
// as it is.
func TestALeadThatCompactsFirstHearsTheRoundAfterCompact(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 2\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr("cat score"), EvalDir: ptr(dir), CompactFirst: ptr(true)})
	if run, _ := m.TeamRun("lab"); !run.Settings.CompactFirst {
		t.Fatalf("%+v", run.Settings)
	}
	makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("the turn's news", func() bool { return len(news(e, boss.ID)) == 1 })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if slices.Contains(users(e, boss.ID), store.CompactCommand) {
		t.Fatal("compacted before the round ended")
	}
	m.teamCheck(team.ID)
	e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == 2 })
	us := users(e, boss.ID)
	i := slices.Index(us, store.CompactCommand)
	if i < 0 || i != len(us)-2 || !strings.Contains(us[i+1], "Eval #1 (the round ended)") || !strings.Contains(us[i+1], "psps compacted your conversation (/compact) just before this") {
		t.Fatalf("no /compact just before the round's score: %q", us)
	}
	if st, _ := m.store.LeadCompact(team.ID); st != "" {
		t.Fatalf("compaction left %q", st)
	}

	// An eval the lead asks for is no round's end.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the asked eval's news", func() bool { return len(news(e, boss.ID)) == 3 })
	n := 0
	for _, u := range users(e, boss.ID) {
		if u == store.CompactCommand {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("compacted %d times", n)
	}

	// Off, a round's score comes as it is.
	if run, _ := m.SetLead(context.Background(), "lab", api.PutLead{Agent: "boss", CompactFirst: ptr(false)}); run.Settings.CompactFirst {
		t.Fatal("still compacts first")
	}
}

// A lead with no runtime is archived only when its line has no live
// session: while psps starts it is not back yet, and one psps could not
// start has no session.
func TestALeadWithoutARuntimeIsArchivedOnlyWhenItIs(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	boss, _ := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000))})
	status := func() string { r, _ := m.TeamRun("lab"); return r.Status }
	if got := status(); got != "the lead is idle, and nothing of its runs" {
		t.Fatal(got)
	}
	m.mu.Lock()
	r := m.runtimes[boss.ID]
	delete(m.runtimes, boss.ID)
	m.mu.Unlock()
	m.reconciled.Store(false)
	if got := status(); got != "psps is starting, and has not brought the team back yet" {
		t.Fatal(got)
	}
	m.reconciled.Store(true)
	if got := status(); !strings.HasPrefix(got, "the lead has no session") {
		t.Fatal(got)
	}
	m.mu.Lock()
	m.runtimes[boss.ID] = r
	m.mu.Unlock()
	if err := m.Archive(context.Background(), boss.ID); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != "the lead is archived" {
		t.Fatal(got)
	}
}

// An agent a lead made that has not read its log since psps started (or
// its shim came back) counts as at work: no round ends on what its driver
// last knew, though its lead is back and idle.
func TestAnAgentNotBackYetHoldsTheRound(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 1\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr("cat score"), EvalDir: ptr(dir)})
	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("the turn's news", func() bool { return len(news(e, boss.ID)) == 1 })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) && m.childrenBusy(boss.ID) == 0 })

	r, err := m.runtime(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.caughtUp = false
	r.noteBusyLocked()
	r.mu.Unlock()
	if n := m.childrenBusy(boss.ID); n != 1 {
		t.Fatalf("%d at work", n)
	}
	if run, _ := m.TeamRun("lab"); run.Status != "1 of the lead's agents at work" {
		t.Fatal(run.Status)
	}
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if run, _ := m.TeamRun("lab"); len(run.Evals) != 0 || m.evalRunning(team.ID) {
		t.Fatalf("a round ended on an agent not back: %+v", run.Evals)
	}

	// A problem keeps it from coming back: it holds the round no more.
	r.setProblem(errors.New("its isolation is down"))
	if n := m.childrenBusy(boss.ID); n != 0 {
		t.Fatalf("%d at work with a problem", n)
	}
	r.setProblem(nil)
	// Back, it is what its log says.
	r.mu.Lock()
	r.catchUpLocked()
	r.mu.Unlock()
	if n := m.childrenBusy(boss.ID); n != 0 {
		t.Fatalf("%d at work once back", n)
	}
	m.teamCheck(team.ID)
	e.waitFor("the round's eval", func() bool { run, _ := m.TeamRun("lab"); return len(run.Evals) == 1 })
}

// A round's next step, which its lead starts once it hears the last one
// ended, is the same round: no eval scores the work between the two.
func TestARoundGoesOnWhileItsLeadStartsTheNextStep(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 1\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Eval: ptr("cat score"), EvalDir: ptr(dir)})
	evals := func() int { es, _ := m.store.Evals(team.ID); return len(es) }

	// The first step ends while the lead is at work: its news waits for the
	// lead, and so does the eval.
	if err := m.Send(boss.ID, "hang"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the lead at work", func() bool { return status(m, boss.ID) == event.StatusWorking })
	makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "prep", Kind: driver.Codex, Prompt: "prepare the data"})
	e.waitFor("the first step's end", func() bool {
		n, _ := m.store.TeamNewsWaiting(team.ID)
		return n == 1 && m.childrenBusy(boss.ID) == 0
	})
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if evals() != 0 {
		t.Fatal("an eval before the lead heard the step ended")
	}

	// It hears, and starts the next step in its turn.
	if err := m.Interrupt(boss.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the lead's turn to stop", func() bool { s := status(m, boss.ID); return s == event.StatusIdle || s == event.StatusInterrupted })
	if err := m.Send(boss.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the first step's news", func() bool { return len(news(e, boss.ID)) == 1 })
	train := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "train", Kind: driver.Codex, Prompt: "hang"})
	e.waitFor("the lead's turn on it", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if evals() != 0 {
		t.Fatal("an eval while the round's next step ran")
	}

	// The last step ends, and the lead's turn on it starts nothing: one
	// eval, of the round.
	if err := m.Interrupt(train.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the last step's news", func() bool { return len(news(e, boss.ID)) == 2 })
	e.waitFor("the lead's turn on it", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	e.waitFor("the round's eval", func() bool { return len(news(e, boss.ID)) == 3 })
	if n := news(e, boss.ID)[2]; !strings.Contains(n, "Eval #1 (the round ended)") || !strings.Contains(n, "SCORE 1, a new best.") {
		t.Fatal(n)
	}
	if evals() != 1 {
		t.Fatalf("%d evals of one round", evals())
	}
}

// While the lead's isolation has no teams the run is paused: the lead could
// do nothing, so it hears nothing and is not told to make a team; its
// agents' turns end and the eval runs, and once teams are on again what
// came meanwhile reaches it at once.
func TestARunIsPausedWhileTheLeadsIsolationHasNoTeams(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 2\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Eval: ptr("cat score"), EvalDir: ptr(dir)})

	makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("its turn's news", func() bool { return len(news(e, boss.ID)) == 1 })
	e.waitFor("the lead's turn on it", func() bool { return free(m, boss.ID) })
	if err := m.Unexpose(ctx, "dev", "team"); err != nil {
		t.Fatal(err)
	}
	if code, out := teamAs(t, m, boss.ID, "GET", "", ""); code != http.StatusForbidden || !strings.Contains(out, "isolation dev has no teams") {
		t.Fatalf("a lead without teams: %d %s", code, out)
	}
	// Paused, the lead can start nothing: the round is over, and its eval
	// runs; what it scored waits.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	e.waitFor("the eval", func() bool { evals, _ := m.store.Evals(team.ID); return len(evals) == 1 && !m.evalRunning(team.ID) })
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if n := news(e, boss.ID); len(n) != 1 {
		t.Fatalf("a paused lead was told: %q", n[1:])
	}
	if n, _ := m.store.TeamNewsWaiting(team.ID); n != 1 {
		t.Fatalf("%d items wait, not the eval", n)
	}
	run, _ := m.TeamRun("lab")
	if run.TeamsOff != "dev" || !strings.HasPrefix(run.Status, "paused, as teams are off in dev; ") || run.Stopped != "" {
		t.Fatalf("%+v", run)
	}

	if _, err := m.Expose(ctx, "dev", "team", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the news that waited", func() bool { return len(news(e, boss.ID)) == 2 })
	if n := news(e, boss.ID)[1]; !strings.Contains(n, "SCORE 2, a new best.") {
		t.Fatal(n)
	}
	if run, _ = m.TeamRun("lab"); run.TeamsOff != "" || strings.HasPrefix(run.Status, "paused") {
		t.Fatalf("%+v", run)
	}

	// Nor is a paused lead told to make the next team, after an eval its
	// operator asked for.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if err := m.EvalTeam("lab"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the asked eval's news", func() bool { return len(news(e, boss.ID)) == 3 })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if err := m.Unexpose(ctx, "dev", "team"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	time.Sleep(200 * time.Millisecond)
	m.teamCheck(team.ID)
	if n, _ := m.store.TeamNewsWaiting(team.ID); n != 0 {
		t.Fatalf("a paused lead was nudged: %d items wait", n)
	}
	if _, err := m.Expose(ctx, "dev", "team", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the nudge", func() bool { m.teamCheck(team.ID); return len(news(e, boss.ID)) == 4 })
	if n := news(e, boss.ID)[3]; !strings.Contains(n, "Nothing of your team is running.") {
		t.Fatal(n)
	}
}

// An eval set after a round ended does not score that round: its first
// eval comes after the next round, or when asked.
func TestAnEvalSetAfterARoundScoresNoneBefore(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 1\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{})
	makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("the round's end", func() bool {
		l, _ := m.store.Lead(team.ID)
		return l.Dirty && m.childrenBusy(boss.ID) == 0
	})
	e.waitFor("its news", func() bool { return len(news(e, boss.ID)) == 1 })
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Eval: ptr("cat score"), EvalDir: ptr(dir)}); err != nil {
		t.Fatal(err)
	}
	if l, _ := m.store.Lead(team.ID); l.Dirty {
		t.Fatal("a round that ended before the eval is still to score")
	}
	m.teamCheck(team.ID)
	time.Sleep(200 * time.Millisecond)
	if evals, _ := m.store.Evals(team.ID); len(evals) != 0 || m.evalRunning(team.ID) {
		t.Fatalf("an eval ran on a round before it: %+v", evals)
	}
	// Unchanged, setting it keeps a round that ended for it.
	m.store.SetLeadDirty(team.ID, true)
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", MaxLive: ptr(2)}); err != nil {
		t.Fatal(err)
	}
	if l, _ := m.store.Lead(team.ID); !l.Dirty {
		t.Fatal("a term other than the eval forgot the round")
	}
}

// An eval isolation scores a copy of the work, made for each eval, where
// no service is and the team's agents are not: they cannot reach what
// scores them.
func TestAnEvalRunsInItsIsolationOnACopyOfTheWork(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	ev, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "ev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 2.5\n"), 0o644)
	boss, _ := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr("cat score; touch made"), EvalDir: ptr(dir), EvalIsolation: ptr("ev")})
	if run, _ := m.TeamRun("lab"); run.EvalIsolation != "ev" || run.Settings.EvalIsolation != ev.ID {
		t.Fatalf("%+v", run)
	}
	if err := m.EvalTeam("lab"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == 1 })
	run, _ := m.TeamRun("lab")
	if len(run.Evals) != 1 || run.Evals[0].Score == nil || *run.Evals[0].Score != 2.5 {
		t.Fatalf("%+v", run.Evals)
	}
	work := filepath.Join(e.fc.evalRoot(ev.Container), "work")
	if _, err := os.Stat(filepath.Join(work, "made")); err != nil {
		t.Fatal("the eval did not run on the copy:", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "made")); err == nil {
		t.Fatal("the eval ran on the work itself")
	}
	e.fc.mu.Lock()
	resets := strings.Join(e.fc.evalResets, ", ")
	e.fc.mu.Unlock()
	if resets != ev.Container+" true, "+ev.Container+" false" {
		t.Fatalf("resets %s", resets)
	}

	// What it prints is the operator's alone: what is scored could print
	// the eval, or what it reads, for the lead.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Eval: ptr("printf 'the-%s-says' grader; exit 3")}); err != nil {
		t.Fatal(err)
	}
	m.EvalTeam("lab")
	e.waitFor("the unscored eval's news", func() bool { return len(news(e, boss.ID)) == 2 })
	if n := news(e, boss.ID)[1]; strings.Contains(n, "the-grader-says") || !strings.Contains(n, "Its output is your operator's alone, as it ran in an isolation of its own. psps says: the eval: exit status 3") {
		t.Fatal(n)
	}
	if _, out := teamAs(t, m, boss.ID, "GET", "", ""); strings.Contains(out, "the-grader-says") || !strings.Contains(out, `"private":true`) {
		t.Fatalf("the lead's view: %s", out)
	}
	if run, _ := m.TeamRun("lab"); !strings.Contains(run.Evals[1].Output, "the-grader-says") || !run.Evals[1].Private {
		t.Fatalf("the operator's view: %+v", run.Evals)
	}

	// Not the lead's own, and none with a service in it but control.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalIsolation: ptr("dev")}); err == nil || !strings.Contains(err.Error(), "it is the lead's own") {
		t.Fatalf("the lead's own: %v", err)
	}
	if _, err := m.Expose(ctx, "ev", "team", api.Expose{}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss"}); err == nil || !strings.Contains(err.Error(), "it has the team service") {
		t.Fatalf("with a service: %v", err)
	}
	if err := m.Unexpose(ctx, "ev", "team"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutEnv(ctx, "control", "reads", proxy.Patch{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "ev", "control", api.Expose{Envs: []string{"reads"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalIsolation: ptr("ev")}); err != nil {
		t.Fatal("with control:", err)
	}
	// A service the operator turns on there anyway says what it costs, and
	// the eval it keeps from running counts for no patience.
	x, err := m.Expose(ctx, "ev", "team", api.Expose{})
	if err != nil || !strings.Contains(x.Warning, `isolation ev runs the eval of team "lab": while it has team, psps refuses to run it`) {
		t.Fatalf("the warning: %q %v", x.Warning, err)
	}
	before, _ := m.TeamRun("lab")
	m.EvalTeam("lab")
	e.waitFor("the refused eval's news", func() bool { return len(news(e, boss.ID)) == 3 })
	if n := news(e, boss.ID)[2]; !strings.Contains(n, "psps could not run it, so the work was not scored, and it counts for no patience") ||
		!strings.Contains(n, "psps says: isolation ev cannot run the eval: it has the team service") {
		t.Fatal(n)
	}
	if run, _ := m.TeamRun("lab"); !run.Evals[len(run.Evals)-1].NotRun || run.Stale != before.Stale {
		t.Fatalf("stale %d, was %d: %+v", run.Stale, before.Stale, run.Evals)
	}
	if x, err := m.Expose(ctx, "ev", "control", api.Expose{}); err != nil || x.Warning != "" {
		t.Fatalf("control warns: %q %v", x.Warning, err)
	}
	if err := m.Unexpose(ctx, "ev", "team"); err != nil {
		t.Fatal(err)
	}
	// An agent of the operator's may work there: the eval runs as ever.
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "ev", Kind: driver.Codex, Handle: "helper"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalIsolation: ptr("ev")}); err != nil {
		t.Fatal("with an agent in it:", err)
	}
	m.EvalTeam("lab")
	e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == 4 })
	if n := news(e, boss.ID)[3]; strings.Contains(n, "cannot run the eval") || !strings.Contains(n, "psps says: the eval: exit status 3") {
		t.Fatal(n)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalIsolation: ptr("")}); err != nil {
		t.Fatal("back to the lead's own:", err)
	}
}

// An eval runs in its isolation apart from the daemon: one that stops
// meanwhile does not end it, and the next follows it to its score. One
// whose VM started again is gone, and counts for no patience; one stopped
// before it ran is started again.
func TestAnEvalRunsOnWhileTheDaemonStopsAndTheNextFollowsIt(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	ctx := context.Background()
	ev, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "ev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "go")
	eval := fmt.Sprintf(`while [ ! -f %s ]; do sleep 0.02; done; echo SCORE: 7`, gate)
	_, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr(eval), EvalDir: ptr(t.TempDir()), EvalIsolation: ptr("ev")})
	started := func() store.EvalRun {
		var run store.EvalRun
		e.waitFor("the eval to start", func() bool {
			runs, _ := e.st.EvalRuns()
			if len(runs) == 1 && runs[0].Isolation == ev.ID {
				run = runs[0]
				return true
			}
			return false
		})
		return run
	}
	evals := func() []store.Eval {
		es, _ := e.st.Evals(team.ID)
		return es
	}

	if err := m.EvalTeam("lab"); err != nil {
		t.Fatal(err)
	}
	run := started()
	stop()
	os.WriteFile(gate, nil, 0o644)
	fr := e.fc.evalRun(ev.Container, run.Run)
	<-fr.done
	if es := evals(); len(es) != 0 {
		t.Fatalf("a stopping daemon told of the eval: %+v", es)
	}
	m, stop = e.daemon()
	e.waitFor("the score", func() bool { return len(evals()) == 1 })
	if es := evals(); es[0].Score == nil || *es[0].Score != 7 || es[0].NotRun || !es[0].At.Equal(run.At) {
		t.Fatalf("%+v", es[0])
	}
	e.waitFor("the eval run to go", func() bool {
		runs, _ := e.st.EvalRuns()
		return len(runs) == 0 && !m.evalRunning(team.ID)
	})

	// Its VM started again while no daemon looked: nothing is left of it.
	os.Remove(gate)
	if err := m.EvalTeam("lab"); err != nil {
		t.Fatal(err)
	}
	started()
	stop()
	e.fc.reboot(ev.Container, true)
	m, stop = e.daemon()
	e.waitFor("the lost eval", func() bool { return len(evals()) == 2 })
	if es := evals(); !es[1].NotRun || es[1].Score != nil || !strings.Contains(es[1].Output, "isolation ev started again while the eval ran there") {
		t.Fatalf("%+v", es[1])
	}
	e.waitFor("the eval run to go", func() bool { return !m.evalRunning(team.ID) })

	// One the daemon stopped before it ran is run again.
	stop()
	os.WriteFile(gate, nil, 0o644)
	if err := e.st.PutEvalRun(store.EvalRun{Team: team.ID, At: time.Now(), Why: "asked", Run: "before"}); err != nil {
		t.Fatal(err)
	}
	m, stop = e.daemon()
	defer stop()
	e.waitFor("the eval run again", func() bool { return len(evals()) == 3 })
	if es := evals(); es[2].Score == nil || *es[2].Score != 7 || es[2].Why != "asked" {
		t.Fatalf("%+v", es[2])
	}
}

func TestATurnPastItsLimitIsStoppedForAFinalReport(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	boss, _ := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000))})
	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "slow", Kind: driver.Codex, Prompt: "hang", TurnMinutes: 1})
	e.waitFor("its turn", func() bool { return status(m, a.ID) == event.StatusWorking })
	m.turnLimits(time.Now().Add(30 * time.Second))
	time.Sleep(200 * time.Millisecond)
	if status(m, a.ID) != event.StatusWorking {
		t.Fatal("stopped within its limit")
	}
	m.turnLimits(time.Now().Add(2 * time.Minute))
	e.waitFor("its report's news", func() bool { return len(news(e, boss.ID)) == 1 })
	n := news(e, boss.ID)[0]
	for _, want := range []string{"ended its turn: idle", "psps stopped its turn at its 1-minute limit and asked for a final report; this is it.",
		"echo: [psps team: your turn ran past its 1 minutes, and psps stopped it.]"} {
		if !strings.Contains(n, want) {
			t.Fatalf("news lacks %q:\n%s", want, n)
		}
	}
}
