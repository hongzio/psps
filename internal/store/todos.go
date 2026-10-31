package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A Todo is a prompt the operator put aside for an agent, to send it later:
// taken, it goes into the message being written and is gone from the list.
type Todo struct {
	ID        int64     `json:"id"`
	Agent     string    `json:"agent"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

const todoSchema = `
CREATE TABLE IF NOT EXISTS todos (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	agent      TEXT NOT NULL REFERENCES agents(id),
	text       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS todos_agent ON todos(agent, id);
`

// AddTodo stores t and returns it with its ID.
func (s *Store) AddTodo(t Todo) (Todo, error) {
	t.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.db.Exec(`INSERT INTO todos (agent, text, created_at) VALUES (?, ?, ?)`, t.Agent, t.Text, ts(t.CreatedAt))
	if err != nil {
		return t, err
	}
	t.ID, err = res.LastInsertId()
	return t, err
}

// Todos are agent's, oldest first.
func (s *Store) Todos(agent string) ([]Todo, error) {
	rows, err := s.db.Query(`SELECT id, agent, text, created_at FROM todos WHERE agent = ? ORDER BY id`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Todo{}
	for rows.Next() {
		var t Todo
		var at string
		if err := rows.Scan(&t.ID, &t.Agent, &t.Text, &at); err != nil {
			return nil, err
		}
		t.CreatedAt = parseTS(at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// TakeTodo removes one of agent's todos and returns it: only one taker of
// it gets it.
func (s *Store) TakeTodo(agent string, id int64) (Todo, error) {
	t := Todo{ID: id, Agent: agent}
	var at string
	err := s.db.QueryRow(`DELETE FROM todos WHERE id = ? AND agent = ? RETURNING text, created_at`, id, agent).Scan(&t.Text, &at)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, fmt.Errorf("agent %s has no todo %d: %w", agent, id, ErrNotFound)
		}
		return t, err
	}
	t.CreatedAt = parseTS(at)
	return t, nil
}

// TodoCounts are how many todos each agent has.
func (s *Store) TodoCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT agent, COUNT(*) FROM todos GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

func (s *Store) TodoCount(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM todos WHERE agent = ?`, agent).Scan(&n)
	return n, err
}
