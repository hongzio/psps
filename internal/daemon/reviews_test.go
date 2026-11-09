package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

// asks is what judge was told as an approver, message by message.
func asks(e *testEnv, judge string) []string {
	var out []string
	for _, u := range users(e, judge) {
		if strings.HasPrefix(u, reviewsHead) {
			out = append(out, u)
		}
	}
	return out
}

// A round put to its approver waits for its verdict, with no deadline: a
// rejection runs no eval and tells the lead why, and the round goes to the
// approver again only after more work; an approval, the approver's or the
// operator's, runs the eval. An eval the lead asks for waits the same way,
// the operator's does not. Another approver withdraws what waited; under
// notify the eval runs and the approver is told.
func TestARoundWaitsForItsApprover(t *testing.T) {
	fakeTimeout(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "score"), []byte("SCORE: 3\n"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), Eval: ptr("cat score"), EvalDir: ptr(dir)})
	told := func() string { return strings.Join(news(e, boss.ID), "\n\n") }
	judge, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "judge", Options: driver.Options{Approvals: driver.ApprovalsAuto}})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the approver", func() bool { return status(m, judge.ID) == event.StatusIdle })

	// Neither the lead nor one of its own approves.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr("boss")}); err == nil || !strings.Contains(err.Error(), "does not approve its own rounds") {
		t.Fatalf("the lead approves itself: %v", err)
	}
	own := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr(own.ID)}); err == nil || !strings.Contains(err.Error(), "was made by the lead") {
		t.Fatalf("the lead's own approves: %v", err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr("judge"), ApproveMode: ptr("maybe")}); err == nil {
		t.Fatal("a mode of none")
	}
	run, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr("judge")})
	if err != nil || run.Settings.Approver != judge.ID || run.Settings.ApproveMode != store.ApproveGate || run.Approver != "judge" || !run.ApproverLive {
		t.Fatalf("%v %+v", err, run)
	}

	// The round ends: no eval, the approver is asked, the lead told to wait.
	e.waitFor("the turn's news", func() bool { return strings.Contains(told(), own.ID+` ("one", codex) ended its turn`) })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	e.waitFor("the ask", func() bool { return len(asks(e, judge.ID)) == 1 })
	ask := asks(e, judge.ID)[0]
	for _, want := range []string{"Review #1: the eval waits for your verdict.", `Team "lab", led by boss. Its round ended`, "No eval has run yet.",
		own.ID + ` ("one", codex) ended its turn`, `"$PSPS_TEAM_URL/approvals/ID"`} {
		if !strings.Contains(ask, want) {
			t.Fatalf("the ask lacks %q:\n%s", want, ask)
		}
	}
	e.waitFor("the lead told to wait", func() bool {
		return strings.Contains(told(), "psps put it to judge, and the eval runs once judge approves")
	})
	run, _ = m.TeamRun("lab")
	if len(run.Evals) != 0 || run.Review == nil || run.Review.ID != 1 || run.Eval.State != "approval" || run.Eval.Waiting != "judge" ||
		!strings.HasPrefix(run.Status, "waiting for judge to approve the round's eval (review #1") {
		t.Fatalf("%+v %+v", run, run.Eval)
	}
	// No deadline, and asked once.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	m.watchDue(time.Now().Add(time.Hour))
	if rs, _ := m.store.Reviews(team.ID, 10); len(rs) != 1 {
		t.Fatalf("asked again: %+v", rs)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusConflict || !strings.Contains(out, "waits for judge's verdict already") {
		t.Fatalf("%d %s", code, out)
	}
	code, out := mailAs(t, m, testIsoID, judge.ID, "GET", "/team/approvals", "")
	var pending []store.Review
	json.Unmarshal([]byte(out), &pending)
	if code != http.StatusOK || len(pending) != 1 || pending[0].ID != 1 {
		t.Fatalf("%d %s", code, out)
	}

	// Rejected, with why; the lead alone can not.
	if code, out := mailAs(t, m, testIsoID, judge.ID, "POST", "/team/approvals/1", `{"verdict":"reject"}`); code != http.StatusBadRequest {
		t.Fatalf("rejected without a note: %d %s", code, out)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/approvals/1", `{"verdict":"approve"}`); code != http.StatusNotFound {
		t.Fatalf("the lead approved its own round: %d %s", code, out)
	}
	if code, out := mailAs(t, m, testIsoID, judge.ID, "POST", "/team/approvals/1", `{"verdict":"reject","note":"the merge leans on a sim-only lever"}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the rejection's news", func() bool {
		return strings.Contains(told(), "judge rejected the round (review #1): no eval ran, and none counts toward the patience.\nIts note: the merge leans on a sim-only lever")
	})
	if code, _ := mailAs(t, m, testIsoID, judge.ID, "POST", "/team/approvals/1", `{"verdict":"approve"}`); code != http.StatusConflict {
		t.Fatalf("decided twice: %d", code)
	}
	// Nothing new: no new ask.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	if rs, _ := m.store.Reviews(team.ID, 10); len(rs) != 1 {
		t.Fatalf("asked again with nothing new: %+v", rs)
	}
	if run, _ = m.TeamRun("lab"); len(run.Evals) != 0 {
		t.Fatalf("an eval of a rejected round: %+v", run.Evals)
	}

	// The lead asks for an eval: it goes to the approver; the operator
	// approves in its place, and the eval runs.
	if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusAccepted || !strings.Contains(out, "put to judge, your operator's approver, first (review #2)") {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := m.DecideTeamReview("lab", api.PostReview{Verdict: "approve", Note: "fine"}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the approved eval's news", func() bool {
		return strings.Contains(told(), "your operator approved the round's eval (review #2).\nIts note: fine") && strings.Contains(told(), "Eval #1 (you asked)")
	})
	if _, err := m.DecideTeamReview("lab", api.PostReview{Verdict: "approve"}); err == nil {
		t.Fatal("approved with nothing waiting")
	}

	// Another round; the approver taken off, what waited is scored.
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+own.ID+"/send", "again"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the round's ask", func() bool {
		if free(m, boss.ID) {
			m.teamCheck(team.ID)
		}
		r, _ := m.TeamRun("lab")
		return r.Review != nil && r.Review.ID == 3
	})
	if run, err = m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr("-")}); err != nil || run.Settings.Approver != "" || run.Review != nil {
		t.Fatalf("%v %+v", err, run)
	}
	if r, _ := m.store.Review(3); r.Verdict != store.VerdictWithdrawn {
		t.Fatalf("%+v", r)
	}
	e.waitFor("the withdrawn round scored", func() bool {
		if free(m, boss.ID) {
			m.teamCheck(team.ID)
		}
		r, _ := m.TeamRun("lab")
		return len(r.Evals) == 2 && r.Evals[1].Why == whyRound
	})

	// Under notify the eval runs as ever, and the approver is told.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Approver: ptr("judge"), ApproveMode: ptr("notify")}); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	if code, out := teamAs(t, m, boss.ID, "POST", "/eval", ""); code != http.StatusAccepted || !strings.Contains(out, "running") {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the approver told", func() bool {
		as := asks(e, judge.ID)
		return len(as) >= 2 && strings.Contains(as[len(as)-1], "for you to look at; the eval runs as ever") && !strings.Contains(as[len(as)-1], "curl")
	})
	e.waitFor("the eval", func() bool { r, _ := m.TeamRun("lab"); return len(r.Evals) == 3 })
}
