package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

// An icon is its line's: a new session wears it, as does the archived one,
// another line does not, and one with no name takes it away.
func TestAnIconIsItsLines(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	c, _ := serveAPI(t, m)

	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one", Handle: "id1"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.UpdateAgent(ctx, "id1", api.UpdateAgent{Icon: &store.Icon{Name: "flask-conical", Color: "teal"}})
	if err != nil || v.Icon == nil || *v.Icon != (store.Icon{Name: "flask-conical", Color: "teal"}) {
		t.Fatalf("set: %+v, %v", v.Icon, err)
	}
	for _, bad := range []store.Icon{{Name: "Flask"}, {Name: "a b"}, {Name: "-x"}, {Name: "star", Color: "chartreuse"}} {
		var he httpError
		if _, err := m.UpdateAgent("id1", api.UpdateAgent{Icon: &bad}); !errors.As(err, &he) || he.code != http.StatusBadRequest {
			t.Errorf("icon %+v: %v", bad, err)
		}
	}

	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })
	sent, err := m.Message("id1", "/new")
	if err != nil {
		t.Fatal(err)
	}
	all, err := c.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		want := x.ID == a.ID || x.ID == sent.Agent
		if got := x.Icon != nil && x.Icon.Name == "flask-conical"; got != want {
			t.Errorf("agent %s (line %s): icon %+v", x.ID, x.Line, x.Icon)
		}
	}
	if x, _ := m.Agent(other.ID); x.Icon != nil {
		t.Fatalf("another line has %+v", x.Icon)
	}

	// From the archived session: the line's, so the live one changes too.
	if _, err := m.UpdateAgent(a.ID, api.UpdateAgent{Icon: &store.Icon{Name: "star"}}); err != nil {
		t.Fatal(err)
	}
	if x, _ := m.Agent("id1"); x.Icon == nil || *x.Icon != (store.Icon{Name: "star"}) {
		t.Fatalf("the live session has %+v", x.Icon)
	}
	if _, err := m.UpdateAgent("id1", api.UpdateAgent{Icon: &store.Icon{}}); err != nil {
		t.Fatal(err)
	}
	if x, _ := m.Agent(a.ID); x.Icon != nil {
		t.Fatalf("taken away, still %+v", x.Icon)
	}
}
