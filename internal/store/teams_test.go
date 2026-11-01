package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/driver"
)

func TestATeamIsALineOfTheLiveListAndAListOfItsOwn(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.SetOrder(LiveList, []string{"a", "divider:1f", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTeam(Team{ID: "t1", Name: "backend", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTeam(Team{ID: "t2", Name: "backend", CreatedAt: now}); !errors.Is(err, ErrExists) {
		t.Fatalf("a second team called backend: %v", err)
	}
	ls, _ := s.Lists()
	if want := []string{"a", "divider:1f", "b", "team:t1"}; !slices.Equal(ls[LiveList], want) {
		t.Fatalf("live %v, want %v", ls[LiveList], want)
	}

	// Into a team: out of the live list, at the team's bottom.
	for _, l := range []string{"b", "a", "c"} {
		if err := s.SetTeam(l, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	ls, _ = s.Lists()
	if want := []string{"divider:1f", "team:t1"}; !slices.Equal(ls[LiveList], want) {
		t.Fatalf("live %v, want %v", ls[LiveList], want)
	}
	if want := []string{"b", "a", "c"}; !slices.Equal(ls["team:t1"], want) {
		t.Fatalf("team %v, want %v", ls["team:t1"], want)
	}
	if err := s.SetOrder("team:t1", []string{"c", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOrder("team:nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a team that is none was ordered: %v", err)
	}
	if err := s.SetTeam("d", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a line went into a team that is none: %v", err)
	}

	// Out of it: just under it.
	if err := s.SetTeam("b", ""); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.Lists()
	if want := []string{"divider:1f", "team:t1", "b"}; !slices.Equal(ls[LiveList], want) {
		t.Fatalf("live %v, want %v", ls[LiveList], want)
	}
	if want := []string{"c", "a"}; !slices.Equal(ls["team:t1"], want) {
		t.Fatalf("team %v, want %v", ls["team:t1"], want)
	}
	teams, _ := s.LineTeams()
	if len(teams) != 2 || teams["a"] != "t1" || teams["c"] != "t1" {
		t.Fatalf("%v", teams)
	}

	if err := s.RenameTeam("t1", "api"); err != nil {
		t.Fatal(err)
	}
	if tm, err := s.Team("api"); err != nil || tm.ID != "t1" {
		t.Fatalf("%+v %v", tm, err)
	}
	if tm, err := s.Team("t1"); err != nil || tm.Name != "api" {
		t.Fatalf("%+v %v", tm, err)
	}

	// Deleted, its lines are where it was, in its order.
	if err := s.DeleteTeam("t1"); err != nil {
		t.Fatal(err)
	}
	ls, _ = s.Lists()
	if want := []string{"divider:1f", "c", "a", "b"}; !slices.Equal(ls[LiveList], want) {
		t.Fatalf("live %v, want %v", ls[LiveList], want)
	}
	if _, ok := ls["team:t1"]; ok {
		t.Fatalf("the team's order outlived it: %v", ls)
	}
	if teams, _ := s.LineTeams(); len(teams) != 0 {
		t.Fatalf("lines still in a deleted team: %v", teams)
	}
	if err := s.DeleteTeam("t1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

func TestADeletedLineLeavesItsTeamAndItsLists(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	for _, id := range []string{"a1", "b1"} {
		if err := s.CreateAgent(Agent{ID: id, Name: id, Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	// b1's line goes on in b2, a new session of it.
	if err := s.Renew("b1", Agent{ID: "b2", Name: "b", Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTeam(Team{ID: "t1", Name: "backend", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"a1", "b1"} {
		if err := s.SetTeam(l, "t1"); err != nil {
			t.Fatal(err)
		}
	}
	s.SetArchived("a1", &now)
	if err := s.SetOrder(ArchivedList, []string{"a1"}); err != nil {
		t.Fatal(err)
	}

	// A session of a line that goes on keeps it in its team.
	if err := s.DeleteAgent("b1"); err != nil {
		t.Fatal(err)
	}
	if teams, _ := s.LineTeams(); teams["b1"] != "t1" {
		t.Fatalf("b1's line left its team with b2 on it: %v", teams)
	}
	// The last one takes it out of every list.
	if err := s.DeleteAgent("a1"); err != nil {
		t.Fatal(err)
	}
	if teams, _ := s.LineTeams(); len(teams) != 1 {
		t.Fatalf("a1's line outlived it in its team: %v", teams)
	}
	ls, _ := s.Lists()
	if slices.Contains(ls[ArchivedList], "a1") || slices.Contains(ls["team:t1"], "a1") {
		t.Fatalf("a1's line outlived it in a list: %v", ls)
	}
}
