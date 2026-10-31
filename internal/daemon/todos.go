package daemon

import (
	"errors"
	"strings"

	"github.com/hongzio/gero/internal/store"
)

// maxTodo is the longest prompt put aside, in bytes.
const maxTodo = 64 << 10

// Todos are the prompts put aside for an agent, oldest first.
func (m *Manager) Todos(agent string) ([]store.Todo, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return nil, err
	}
	return m.store.Todos(a.ID)
}

// AddTodo puts a prompt aside for a live agent.
func (m *Manager) AddTodo(agent, text string, auto bool) (store.Todo, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Todo{}, err
	}
	if a.ArchivedAt != nil {
		return store.Todo{}, badRequest(errors.New("an archived agent is sent nothing"))
	}
	if strings.TrimSpace(text) == "" {
		return store.Todo{}, badRequest(errors.New("a todo needs a prompt"))
	}
	if len(text) > maxTodo {
		return store.Todo{}, badRequest(errors.New("a todo is at most 64 KiB"))
	}
	return m.store.AddTodo(store.Todo{Agent: a.ID, Text: text, Auto: auto})
}

// SetTodoAuto sets whether a todo is sent by itself when the agent's next
// turn ends.
func (m *Manager) SetTodoAuto(agent string, id int64, auto bool) (store.Todo, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Todo{}, err
	}
	return m.store.SetTodoAuto(a.ID, id, auto)
}

// sendTodoLocked sends the agent's oldest auto todo through the outbox:
// from then on it is a message like the operator's. One a turn.
func (r *runtime) sendTodoLocked() {
	r.todoDue = false
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.SendTodo(r.agent.ID, after)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.m.log.Printf("agent %s: todo: %v", r.agent.ID, err)
		}
		return
	}
	r.outbox = append(r.outbox, p)
	r.nameLocked(p.Text)
	r.drv.Send(p.Text)
	r.m.log.Printf("agent %s: todo sent", r.agent.ID)
}

// TakeTodo removes a todo and returns it, to be written into a message.
func (m *Manager) TakeTodo(agent string, id int64) (store.Todo, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Todo{}, err
	}
	return m.store.TakeTodo(a.ID, id)
}
