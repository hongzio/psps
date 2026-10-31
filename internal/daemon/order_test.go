package daemon

import (
	"slices"
	"testing"
	"time"

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
	})
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
