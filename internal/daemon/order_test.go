package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

func TestTheOperatorsOrderComesFirstInTheLiveList(t *testing.T) {
	now := time.Now()
	agent := func(id, line string, archived time.Duration) agentView {
		var a agentView
		a.ID, a.Line, a.CreatedAt = id, line, now
		if archived != 0 {
			at := now.Add(archived)
			a.ArchivedAt = &at
		}
		return a
	}
	as := []agentView{agent("a", "A", 0), agent("a0", "A", -3*time.Hour), agent("b", "B", 0), agent("c", "C", 0),
		agent("d0", "D", -2*time.Hour), agent("new", "N", 0), agent("e", "", 0),
		agent("f0", "F", -4*time.Hour), agent("f1", "F", -time.Hour)}
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
	// The archived list ignores the operator's order: a line archived last
	// is first, its sessions together.
	if got, want := archived, []string{"f0", "f1", "d0", "a0"}; !slices.Equal(got, want) {
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
	// The fork is right under the one it forks, in their team.
	if got, want := liveIDs(m), []string{b.ID, f.ID, sent.Agent, c.ID}; !slices.Equal(got, want) {
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

// A new agent goes right after the last of its isolation's in the live
// list, a team's agents aside; with none of its isolation, at the bottom.
func TestANewAgentGoesAfterTheLastOfItsIsolation(t *testing.T) {
	agent := func(id, iso string) agentView {
		var a agentView
		a.ID, a.Line, a.IsolationID = id, id, iso
		return a
	}
	teams := map[string]string{"t1": "T"}
	cases := []struct {
		name string
		live []string
		as   []agentView
		want []string
	}{
		{"placed", []string{"a", "divider:1", "b", "team:T", "c"},
			[]agentView{agent("a", "x"), agent("b", "y"), agent("t1", "x"), agent("c", "y"), agent("new", "x")},
			[]string{"a", "new", "divider:1", "b", "team:T", "c"}},
		{"after the last", []string{"a", "b", "c"},
			[]agentView{agent("a", "x"), agent("b", "y"), agent("c", "x"), agent("new", "x")},
			[]string{"a", "b", "c", "new"}},
		{"not placed yet", []string{"a"},
			[]agentView{agent("a", "y"), agent("b", "x"), agent("t1", "x"), agent("c", "y"), agent("new", "x")},
			[]string{"a", "b", "new"}},
		{"none of it", []string{"a"},
			[]agentView{agent("a", "y"), agent("t1", "x"), agent("new", "x")}, nil},
	}
	for _, c := range cases {
		got, ok := placedAfter(c.live, c.as, teams, "new", false, func(a agentView) bool { return a.IsolationID == "x" })
		if ok != (c.want != nil) || !slices.Equal(got, c.want) {
			t.Errorf("%s: %v %v, want %v", c.name, got, ok, c.want)
		}
	}
}

func TestAForkIsRightUnderTheAgentItForks(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()

	a, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id1"})
	b, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id2"})
	c, _ := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "id3"})
	e.waitFor("the agents", func() bool {
		return status(m, a.ID) == event.StatusIdle && status(m, b.ID) == event.StatusIdle && status(m, c.ID) == event.StatusIdle
	})
	f, err := m.Fork(ctx, "id1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := liveIDs(m), []string{a.ID, f.ID, b.ID, c.ID}; !slices.Equal(got, want) {
		t.Fatalf("live %v, want %v", got, want)
	}
	// A fork of the fork is right under it, and one of an archived agent
	// goes where a new one does.
	g, _ := m.Fork(ctx, f.ID)
	if err := m.Archive(ctx, "id2"); err != nil {
		t.Fatal(err)
	}
	h, _ := m.Fork(ctx, "id2")
	if got, want := liveIDs(m), []string{a.ID, f.ID, g.ID, c.ID, h.ID}; !slices.Equal(got, want) {
		t.Fatalf("live %v, want %v", got, want)
	}
}
