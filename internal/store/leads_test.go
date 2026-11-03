package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/driver"
)

func TestALeadLeadsTheTeamItIsIn(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	for _, id := range []string{"a1", "b1"} {
		if err := s.CreateAgent(Agent{ID: id, Name: id, Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	s.CreateTeam(Team{ID: "t1", Name: "lab", CreatedAt: now})
	s.CreateTeam(Team{ID: "t2", Name: "ops", CreatedAt: now})
	l := Lead{Team: "t1", Line: "a1", Settings: LeadSettings{Budget: 1000, MaxLive: 3}, Since: now, Base: driver.Tokens{Input: 5}}
	if err := s.PutLead(l); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a line in no team was made a lead: %v", err)
	}
	s.SetTeam("a1", "t1")
	if err := s.PutLead(l); err != nil {
		t.Fatal(err)
	}
	got, err := s.LeadOfLine("a1")
	if err != nil || got.Team != "t1" || got.Settings.Budget != 1000 || got.Base.Input != 5 {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.SetLeadStopped("t1", "budget used"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Lead("t1"); got.Stopped != "budget used" {
		t.Fatalf("%+v", got)
	}

	// News waits, then goes in one message, once.
	for _, b := range []string{"one", "two"} {
		if err := s.AddTeamNews("t1", b); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := s.TeamNewsWaiting("t1"); n != 2 {
		t.Fatalf("%d waiting", n)
	}
	p, err := s.DeliverTeamNews("t1", "a1", 7, func(news []string) string { return strings.Join(news, "+") })
	if err != nil || p.Text != "one+two" || p.After != 7 {
		t.Fatalf("%+v %v", p, err)
	}
	if out, _ := s.Pending("a1"); len(out) != 1 || out[0].Text != "one+two" {
		t.Fatalf("outbox %+v", out)
	}
	if _, err := s.DeliverTeamNews("t1", "a1", 7, func([]string) string { return "" }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delivered twice: %v", err)
	}

	score := 1.5
	if e, err := s.AddEval(Eval{Team: "t1", At: now, Took: 2 * time.Second, Score: &score, Why: "the lead", Output: "SCORE: 1.5"}); err != nil || e.ID == 0 {
		t.Fatalf("%+v %v", e, err)
	}
	s.AddEval(Eval{Team: "t1", At: now, Why: "the operator", Output: "boom"})
	if es, _ := s.Evals("t1"); len(es) != 2 || *es[0].Score != 1.5 || es[0].Took != 2*time.Second || es[1].Score != nil {
		t.Fatalf("%+v", es)
	}

	if err := s.PutSpent("a1", Spent{Tokens: driver.Tokens{Input: 10, Cached: 90, Output: 3}, At: now}); err != nil {
		t.Fatal(err)
	}
	s.PutSpent("a1", Spent{Tokens: driver.Tokens{Input: 20, Cached: 180, Output: 6}, At: now, Limits: []driver.Limit{{Window: "five_hour", Percent: 12}}})
	if sp, _ := s.SpentOf(); sp["a1"].Tokens.New() != 26 || len(sp["a1"].Limits) != 1 {
		t.Fatalf("%+v", sp)
	}

	// Out of its team, it leads none.
	s.SetTeam("a1", "t2")
	if _, err := s.Lead("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a lead outside its team: %v", err)
	}
	// A deleted team takes its lead, news and evals with it.
	s.SetTeam("b1", "t2")
	if err := s.PutLead(Lead{Team: "t2", Line: "b1", Since: now}); err != nil {
		t.Fatal(err)
	}
	s.AddTeamNews("t2", "x")
	if err := s.DeleteTeam("t2"); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.Leads(); len(ls) != 0 {
		t.Fatalf("leads of a deleted team: %+v", ls)
	}
	if n, _ := s.TeamNewsWaiting("t2"); n != 0 {
		t.Fatalf("news of a deleted team: %d", n)
	}
}
