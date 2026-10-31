package store

import (
	"errors"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/driver"
)

func TestATodoIsTakenOnceAndGoesOnWithTheAgentsID(t *testing.T) {
	s := open(t)
	now := time.Now()
	if err := s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	agent := func(id string) Agent {
		return Agent{ID: id, Name: id, Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}
	}
	if err := s.CreateAgent(agent("a1")); err != nil {
		t.Fatal(err)
	}
	first, err := s.AddTodo(Todo{Agent: "a1", Text: "write the tests\nthen ship"})
	if err != nil {
		t.Fatal(err)
	}
	s.AddTodo(Todo{Agent: "a1", Text: "update the README"})
	if n, _ := s.TodoCount("a1"); n != 2 {
		t.Fatalf("%d todos, want 2", n)
	}

	got, err := s.TakeTodo("a1", first.ID)
	if err != nil || got.Text != "write the tests\nthen ship" {
		t.Fatalf("took %+v, %v", got, err)
	}
	if _, err := s.TakeTodo("a1", first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a todo was taken twice: %v", err)
	}

	// /new: the next session has them.
	if err := s.Renew("a1", agent("a2"), now); err != nil {
		t.Fatal(err)
	}
	ts, _ := s.Todos("a2")
	if len(ts) != 1 || ts[0].Text != "update the README" {
		t.Fatalf("a2 has %+v", ts)
	}
	if c, _ := s.TodoCounts(); c["a1"] != 0 || c["a2"] != 1 {
		t.Fatalf("counts %v", c)
	}
	if err := s.DeleteAgent("a2"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.TodoCount("a2"); n != 0 {
		t.Fatal("todos outlived their agent")
	}
}

func TestAutoTodosGoToTheOutboxOldestFirst(t *testing.T) {
	s := open(t)
	now := time.Now()
	s.PutIsolation(Isolation{ID: "i1", Name: "dev", Container: "gero-i1", Image: "img", CreatedAt: now})
	if err := s.CreateAgent(Agent{ID: "a1", Name: "a1", Kind: driver.Claude, IsolationID: "i1", Workdir: "/w", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	s.AddTodo(Todo{Agent: "a1", Text: "by hand"})
	later, _ := s.AddTodo(Todo{Agent: "a1", Text: "second"})
	first, _ := s.AddTodo(Todo{Agent: "a1", Text: "first", Auto: true})
	if _, err := s.SetTodoAuto("a1", later.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTodoAuto("a1", first.ID+1, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set a missing todo: %v", err)
	}
	for _, want := range []string{"second", "first"} {
		p, err := s.SendTodo("a1", 7)
		if err != nil || p.Text != want || p.After != 7 {
			t.Fatalf("sent %+v, %v; want %q", p, err, want)
		}
	}
	if _, err := s.SendTodo("a1", 7); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a todo not auto was sent: %v", err)
	}
	if ps, _ := s.Pending("a1"); len(ps) != 2 || ps[0].Text != "second" {
		t.Fatalf("outbox %+v", ps)
	}
	if ts, _ := s.Todos("a1"); len(ts) != 1 || ts[0].Text != "by hand" || ts[0].Auto {
		t.Fatalf("left %+v", ts)
	}
}
