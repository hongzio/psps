package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/store"
)

func TestTheOperatorsOrderComesFirstInEachList(t *testing.T) {
	now := time.Now()
	agent := func(id, line string, archived bool) agentView {
		var a agentView
		a.ID, a.Line, a.CreatedAt = id, line, now
		if archived {
			a.ArchivedAt = &now
		}
		return a
	}
	as := []agentView{agent("a", "A", false), agent("a0", "A", true), agent("b", "B", false), agent("c", "C", false),
		agent("d0", "D", true), agent("new", "N", false), agent("e", "", false)}
	orderAgents(as, map[string]map[string]int{
		store.LiveList:     {"C": 0, "e": 1, "A": 2, "B": 3},
		store.ArchivedList: {"D": 0, "A": 1},
	}, nil)
	var live, archived []string
	for _, a := range as {
		if a.ArchivedAt != nil {
			archived = append(archived, a.ID)
		} else {
			live = append(live, a.ID)
		}
	}
	if got, want := live, []string{"c", "e", "a", "b", "new"}; !slices.Equal(got, want) {
		t.Errorf("live %v, want %v", got, want)
	}
	if got, want := archived, []string{"d0", "a0"}; !slices.Equal(got, want) {
		t.Errorf("archived %v, want %v", got, want)
	}
}

func TestTheOrderKeepsTheSidebarsDividers(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	if err := m.SetOrder(store.LiveList, []string{"a", "divider:1f", "b"}); err != nil {
		t.Fatal(err)
	}
	o, err := m.Order()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(o.Live, []string{"a", "divider:1f", "b"}) || o.Archived == nil || len(o.Archived) != 0 {
		t.Fatalf("%+v", o)
	}
}

// A team's agents are together where the team is in the live list, in its
// order; those it has not ordered yet at its bottom. A team not placed is
// where its first agent was, among the lines not placed.
func TestATeamsAgentsAreTogetherWhereTheTeamIs(t *testing.T) {
	now := time.Now()
	agent := func(id string, archived bool) agentView {
		var a agentView
		a.ID, a.Line, a.CreatedAt = id, id, now
		if archived {
			a.ArchivedAt = &now
		}
		return a
	}
	as := []agentView{agent("a", false), agent("t1", false), agent("b", false), agent("t2", false), agent("t3", false),
		agent("u1", false), agent("c", false), agent("u2", false), agent("t4", true)}
	orderAgents(as, map[string]map[string]int{
		store.LiveList:     {"b": 0, "team:T": 1, "a": 2, "t1": 3},
		"team:T":           {"t3": 0, "t1": 1},
		store.ArchivedList: {"t4": 0},
	}, map[string]string{"t1": "T", "t2": "T", "t3": "T", "t4": "T", "u1": "U", "u2": "U"})
	var live []string
	for _, a := range as {
		if a.ArchivedAt == nil {
			live = append(live, a.ID)
		}
	}
	if want := []string{"b", "t3", "t1", "t2", "a", "u1", "u2", "c"}; !slices.Equal(live, want) {
		t.Errorf("live %v, want %v", live, want)
	}
}

// A team holds a line: the agent's next session is in it, a fork joins
// it, and once it is deleted its agents are where it was.
func TestAnAgentsTeamFollowsItsLine(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()

	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id1", Team: " backend "})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id2"})
	e.waitFor("the agents", func() bool { return status(m, a.ID) == event.StatusIdle && status(m, b.ID) == event.StatusIdle })
	if v, _ := m.Agent("id1"); v.Team != "backend" || v.TeamID == "" {
		t.Fatalf("team %q %q", v.Team, v.TeamID)
	}
	if _, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Team: "two\nlines"}); err == nil {
		t.Fatal("a team of two lines")
	}
	if _, err := m.UpdateAgent("id2", api.UpdateAgent{Team: ptr("backend")}); err != nil {
		t.Fatal(err)
	}
	o, _ := m.Order()
	if len(o.Teams) != 1 || o.Teams[0].Name != "backend" || !slices.Equal(o.Live, []string{store.TeamLine(o.Teams[0].ID)}) {
		t.Fatalf("%+v", o)
	}
	team := store.TeamLine(o.Teams[0].ID)
	if err := m.SetOrder(team, []string{b.ID, a.ID}); err != nil {
		t.Fatal(err)
	}

	sent, err := m.Message("id1", "/new")
	if err != nil {
		t.Fatal(err)
	}
	f, err := m.Fork(ctx, "id2")
	if err != nil {
		t.Fatal(err)
	}
	if f.Team != "backend" {
		t.Fatalf("the fork's team %q", f.Team)
	}
	c, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id3"})
	if got, want := liveIDs(m), []string{b.ID, sent.Agent, f.ID, c.ID}; !slices.Equal(got, want) {
		t.Fatalf("live %v, want %v", got, want)
	}
	if v, _ := m.Agent(sent.Agent); v.Team != "backend" {
		t.Fatalf("the next session's team %q", v.Team)
	}

	if _, err := m.UpdateAgent(f.ID, api.UpdateAgent{Team: ptr("")}); err != nil {
		t.Fatal(err)
	}
	if o, _ := m.Order(); !slices.Equal(o.Live, []string{team, f.ID}) {
		t.Fatalf("out of its team, it is under it: %v", o.Live)
	}
	if _, err := m.RenameTeam("backend", "api"); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.Agent(b.ID); v.Team != "api" {
		t.Fatalf("renamed, the team is %q", v.Team)
	}
	if err := m.DeleteTeam("api"); err != nil {
		t.Fatal(err)
	}
	if got, want := liveIDs(m), []string{b.ID, sent.Agent, f.ID, c.ID}; !slices.Equal(got, want) {
		t.Fatalf("live %v, want %v", got, want)
	}
	if v, _ := m.Agent(b.ID); v.Team != "" || v.TeamID != "" {
		t.Fatalf("the team outlived its deletion: %q", v.Team)
	}
	if o, _ := m.Order(); len(o.Teams) != 0 || !slices.Equal(o.Live, []string{b.ID, a.ID, f.ID}) {
		t.Fatalf("%+v", o)
	}
}
