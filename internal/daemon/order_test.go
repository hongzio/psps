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
		agent("d0", "D", true), agent("new", "N", false)}
	orderAgents(as, map[string]map[string]int{
		store.LiveList:     {"C": 0, "A": 1, "B": 2},
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
	if got, want := live, []string{"c", "a", "b", "new"}; !slices.Equal(got, want) {
		t.Errorf("live %v, want %v", got, want)
	}
	if got, want := archived, []string{"d0", "a0"}; !slices.Equal(got, want) {
		t.Errorf("archived %v, want %v", got, want)
	}
}
