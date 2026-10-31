package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"sort"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/store"
)

// SetOrder is the operator's order of a sidebar list, live or archived, as
// the lines in it from the top.
func (m *Manager) SetOrder(list string, lines []string) error {
	if list != store.LiveList && list != store.ArchivedList {
		return badRequest(fmt.Errorf("the list is %q or %q, not %q", store.LiveList, store.ArchivedList, list))
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

// Order is every sidebar list from the top, dividers included, for each
// UI to draw the one sidebar.
func (m *Manager) Order() (api.AgentOrders, error) {
	ls, err := m.store.Lists()
	if err != nil {
		return api.AgentOrders{}, err
	}
	return api.AgentOrders{Live: ls[store.LiveList], Archived: ls[store.ArchivedList]}, nil
}

// orderAgents puts the lines the operator ordered first in each list, in
// that order, and the rest after them as they were: a line not moved yet,
// a new one, stays at the bottom.
func orderAgents(as []agentView, order map[string]map[string]int) {
	place := func(a agentView) (int, bool) {
		list := store.LiveList
		if a.ArchivedAt != nil {
			list = store.ArchivedList
		}
		p, ok := order[list][cmp.Or(a.Line, a.ID)]
		return p, ok
	}
	sort.SliceStable(as, func(i, j int) bool {
		pi, oki := place(as[i])
		pj, okj := place(as[j])
		if oki != okj {
			return oki
		}
		return oki && pi < pj
	})
}
