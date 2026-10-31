package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
)

// Auto todos are sent one at a time, oldest first, each when a turn ends in
// idle: not when one is added to an idle agent, and not those left to the
// operator.
func TestAutoTodosGoOneAtEachTurnsEnd(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send(a.ID, "start"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the first turn", func() bool { return e.hasAssistant(a.ID, "echo: start") && status(m, a.ID) == event.StatusIdle })

	m.AddTodo(a.ID, "by hand", false)
	first, err := m.AddTodo(a.ID, "first", true)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := m.AddTodo(a.ID, "second", false)
	if _, err := m.SetTodoAuto(a.ID, second.ID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := users(e, a.ID); len(got) != 1 {
		t.Fatalf("a todo added to an idle agent was sent: %q", got)
	}

	if err := m.Send(a.ID, "go"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("both todos", func() bool { return e.hasAssistant(a.ID, "echo: second") && status(m, a.ID) == event.StatusIdle })
	time.Sleep(300 * time.Millisecond)
	if got := strings.Join(users(e, a.ID), ","); got != "start,go,first,second" {
		t.Fatalf("turns %s", got)
	}
	ts, _ := m.Todos(a.ID)
	if len(ts) != 1 || ts[0].Text != "by hand" || ts[0].ID == first.ID {
		t.Fatalf("left %+v", ts)
	}
}
