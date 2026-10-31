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
func (m *Manager) AddTodo(agent, text string) (store.Todo, error) {
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
	return m.store.AddTodo(store.Todo{Agent: a.ID, Text: text})
}

// TakeTodo removes a todo and returns it, to be written into a message.
func (m *Manager) TakeTodo(agent string, id int64) (store.Todo, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Todo{}, err
	}
	return m.store.TakeTodo(a.ID, id)
}
