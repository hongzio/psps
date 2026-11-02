package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/store"
)

// SetOrder is the operator's order of a sidebar list, live, archived or a
// team's, as the lines in it from the top. A team's agents are drawn in
// its own list only: in the live list it is one line, so they move within
// it alone.
func (m *Manager) SetOrder(list string, lines []string) error {
	if _, team := store.TeamOf(list); !team && list != store.LiveList && list != store.ArchivedList {
		return badRequest(fmt.Errorf("the list is %q, %q or a team's, not %q", store.LiveList, store.ArchivedList, list))
	}
	if len(lines) > 10000 {
		return badRequest(errors.New("too many lines"))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		if l == "" || seen[l] {
			return badRequest(fmt.Errorf("line %q is empty or given twice", l))
		}
		seen[l] = true
	}
	return m.store.SetOrder(list, lines)
}

// Order is every sidebar list from the top, dividers and teams included,
// with the teams, for each UI to draw the one sidebar.
func (m *Manager) Order() (api.AgentOrders, error) {
	ls, err := m.store.Lists()
	if err != nil {
		return api.AgentOrders{}, err
	}
	teams, err := m.store.Teams()
	if err != nil {
		return api.AgentOrders{}, err
	}
	runs, err := m.teamsWithRuns(teams)
	if err != nil {
		return api.AgentOrders{}, err
	}
	return api.AgentOrders{Live: ls[store.LiveList], Archived: ls[store.ArchivedList], Teams: runs}, nil
}

// orderAgents puts the lines the operator ordered first in the live list,
// in that order, and the rest after them as they were: a line not moved
// yet, a new one, stays at the bottom. The archived list goes by when its
// lines were archived, the latest first. A live agent whose line is in a team
// (teams, by line) is where the team is in the live list, in the team's
// order, its teammates all together.
func orderAgents(as []agentView, order map[string]map[string]int, teams map[string]string) {
	// key is where an agent goes, compared in turn: placed groups (its
	// line, or its team, in its list) first, by their place, the others by
	// where each first was; then within the group, by its place in the team.
	type key struct {
		unplaced, at, group, unplacedIn, in int
	}
	keys := make(map[string]key, len(as))
	groups := map[string]int{}
	archived := archivedRanks(as)
	for i, a := range as {
		line := cmp.Or(a.Line, a.ID)
		list, at := store.LiveList, line
		if a.ArchivedAt != nil {
			list = store.ArchivedList
		} else if t, ok := teams[line]; ok {
			at = store.TeamLine(t)
		}
		g := list + "\x00" + at
		if _, ok := groups[g]; !ok {
			groups[g] = i
		}
		k := key{group: groups[g], unplacedIn: 1}
		if list == store.ArchivedList {
			k.at = archived[line]
		} else if p, ok := order[list][at]; ok {
			k.at = p
		} else {
			k.unplaced, k.at = 1, k.group
		}
		if at != line {
			if p, ok := order[at][line]; ok {
				k.unplacedIn, k.in = 0, p
			}
		}
		keys[a.ID] = k
	}
	sort.SliceStable(as, func(i, j int) bool {
		ki, kj := keys[as[i].ID], keys[as[j].ID]
		return cmp.Or(
			cmp.Compare(ki.unplaced, kj.unplaced),
			cmp.Compare(ki.at, kj.at),
			cmp.Compare(ki.group, kj.group),
			cmp.Compare(ki.unplacedIn, kj.unplacedIn),
			cmp.Compare(ki.in, kj.in),
		) < 0
	})
}

// archivedRanks are the archived lines' places in their list: the one with
// the session archived last first.
func archivedRanks(as []agentView) map[string]int {
	last := map[string]time.Time{}
	var lines []string
	for _, a := range as {
		if a.ArchivedAt == nil {
			continue
		}
		line := cmp.Or(a.Line, a.ID)
		t, ok := last[line]
		if !ok {
			lines = append(lines, line)
		}
		if !ok || a.ArchivedAt.After(t) {
			last[line] = *a.ArchivedAt
		}
	}
	sort.SliceStable(lines, func(i, j int) bool { return last[lines[i]].After(last[lines[j]]) })
	out := make(map[string]int, len(lines))
	for i, l := range lines {
		out[l] = i
	}
	return out
}
