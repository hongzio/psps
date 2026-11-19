package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// An Ask is something an agent leaves for the operator (internal/proxy/
// askproxy): a question, a go-ahead to give, or a thing to do by hand. It
// waits, open, for as long as it takes; the agent goes on meanwhile. The
// operator closes it with an answer, or without one, and that reaches the
// agent once, in its outbox, as receipts do. The agent may withdraw one
// still open.
type Ask struct {
	ID      int64       `json:"id"`
	Agent   string      `json:"agent"`
	Kind    string      `json:"kind"`
	Title   string      `json:"title"`
	Body    string      `json:"body,omitempty"`
	Options []AskOption `json:"options,omitempty"`
	// State is AskOpen, AskAnswered, AskDismissed or AskWithdrawn.
	State string `json:"state"`
	// Outcome is the answer to a confirm (yes, no) or a task (done,
	// cant); a question's is empty.
	Outcome string `json:"outcome,omitempty"`
	// Pick is the option chosen, from 1; 0 is none.
	Pick int `json:"pick,omitempty"`
	// Text is what the operator wrote, with the answer or without one.
	Text        string     `json:"text,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

// An AskOption is one answer a question offers.
type AskOption struct {
	Label string `json:"label"`
	Note  string `json:"note,omitempty"`
}

const (
	AskOpen      = "open"
	AskAnswered  = "answered"
	AskDismissed = "dismissed"
	AskWithdrawn = "withdrawn"
)

const askSchema = `
CREATE TABLE IF NOT EXISTS asks (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	agent        TEXT NOT NULL REFERENCES agents(id),
	kind         TEXT NOT NULL,
	title        TEXT NOT NULL,
	body         TEXT NOT NULL DEFAULT '',
	options      TEXT NOT NULL DEFAULT '[]',
	state        TEXT NOT NULL DEFAULT 'open',
	outcome      TEXT NOT NULL DEFAULT '',
	pick         INTEGER NOT NULL DEFAULT 0,
	answer       TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	closed_at    TEXT,
	delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS asks_agent ON asks(agent, id);
CREATE INDEX IF NOT EXISTS asks_open ON asks(state, id);
`

const askCols = `id, agent, kind, title, body, options, state, outcome, pick, answer, created_at, closed_at, delivered_at`

// ErrTooManyAsks refuses an ask from an agent with as many open as it may
// have.
var ErrTooManyAsks = errors.New("too many open asks")

// ErrAskClosed refuses closing or withdrawing an ask that is no longer
// open.
var ErrAskClosed = errors.New("ask already closed")

func scanAsk(row interface{ Scan(...any) error }) (Ask, error) {
	var a Ask
	var opts, created string
	var closed, delivered sql.NullString
	err := row.Scan(&a.ID, &a.Agent, &a.Kind, &a.Title, &a.Body, &opts, &a.State, &a.Outcome, &a.Pick, &a.Text, &created, &closed, &delivered)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	json.Unmarshal([]byte(opts), &a.Options)
	a.CreatedAt = parseTS(created)
	if closed.Valid {
		t := parseTS(closed.String)
		a.ClosedAt = &t
	}
	if delivered.Valid {
		t := parseTS(delivered.String)
		a.DeliveredAt = &t
	}
	return a, nil
}

func (s *Store) asks(q string, args ...any) ([]Ask, error) {
	rows, err := s.db.Query(`SELECT `+askCols+` FROM asks `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Ask{}
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AddAsk keeps a, open, unless its agent has max open already. An ask of
// the agent's still open with the same title is not made twice: that one
// comes back, with made false.
func (s *Store) AddAsk(a Ask, max int) (Ask, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	same, err := scanAsk(tx.QueryRow(`SELECT `+askCols+` FROM asks WHERE agent = ? AND state = ? AND title = ? ORDER BY id LIMIT 1`, a.Agent, AskOpen, a.Title))
	if err == nil {
		return same, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return a, false, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM asks WHERE agent = ? AND state = ?`, a.Agent, AskOpen).Scan(&n); err != nil {
		return a, false, err
	}
	if n >= max {
		return a, false, fmt.Errorf("%d asks of its are open: %w", n, ErrTooManyAsks)
	}
	if a.Options == nil {
		a.Options = []AskOption{}
	}
	opts, _ := json.Marshal(a.Options)
	a.State = AskOpen
	a.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := tx.Exec(`INSERT INTO asks (agent, kind, title, body, options, state, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.Agent, a.Kind, a.Title, a.Body, string(opts), a.State, ts(a.CreatedAt))
	if err != nil {
		return a, false, err
	}
	if a.ID, err = res.LastInsertId(); err != nil {
		return a, false, err
	}
	return a, true, tx.Commit()
}

// GetAsk is ask id.
func (s *Store) GetAsk(id int64) (Ask, error) {
	a, err := scanAsk(s.db.QueryRow(`SELECT `+askCols+` FROM asks WHERE id = ?`, id))
	if errors.Is(err, ErrNotFound) {
		return a, fmt.Errorf("there is no ask #%d: %w", id, ErrNotFound)
	}
	return a, err
}

// AsksOf are agent's asks, newest first, at most limit of them.
func (s *Store) AsksOf(agent string, limit int) ([]Ask, error) {
	return s.asks(`WHERE agent = ? ORDER BY id DESC LIMIT ?`, agent, limit)
}

// OpenAsks are every agent's open asks, oldest first.
func (s *Store) OpenAsks() ([]Ask, error) {
	return s.asks(`WHERE state = ? ORDER BY id`, AskOpen)
}

// OpenAskCounts is how many asks of each agent's are open, for those with
// any.
func (s *Store) OpenAskCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT agent, COUNT(*) FROM asks WHERE state = ? GROUP BY agent`, AskOpen)
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

// CloseAsk closes open ask id as state (AskAnswered or AskDismissed), with
// what the operator answered; its agent hears of it next (AnswersWaiting).
func (s *Store) CloseAsk(id int64, state, outcome string, pick int, text string) (Ask, error) {
	a, err := scanAsk(s.db.QueryRow(`UPDATE asks SET state = ?, outcome = ?, pick = ?, answer = ?, closed_at = ?
		WHERE id = ? AND state = ? RETURNING `+askCols, state, outcome, pick, text, ts(time.Now()), id, AskOpen))
	if errors.Is(err, ErrNotFound) {
		if a, err := s.GetAsk(id); err != nil {
			return a, err
		} else {
			return a, fmt.Errorf("ask #%d is %s: %w", id, a.State, ErrAskClosed)
		}
	}
	return a, err
}

// WithdrawAsk closes agent's open ask id at its own word: there is
// nothing to tell it.
func (s *Store) WithdrawAsk(agent string, id int64) (Ask, error) {
	now := ts(time.Now())
	a, err := scanAsk(s.db.QueryRow(`UPDATE asks SET state = ?, closed_at = ?, delivered_at = ?
		WHERE id = ? AND agent = ? AND state = ? RETURNING `+askCols, AskWithdrawn, now, now, id, agent, AskOpen))
	if errors.Is(err, ErrNotFound) {
		if a, err := s.GetAsk(id); err != nil || a.Agent != agent {
			return a, fmt.Errorf("you have no ask #%d: %w", id, ErrNotFound)
		} else {
			return a, fmt.Errorf("ask #%d is %s: %w", id, a.State, ErrAskClosed)
		}
	}
	return a, err
}

// answersWaiting is the condition for an ask closed by the operator whose
// agent has not heard of it yet.
const answersWaiting = `state IN ('answered', 'dismissed') AND delivered_at IS NULL`

// AnswersWaiting is how many of agent's asks the operator closed that it
// has not heard of yet.
func (s *Store) AnswersWaiting(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM asks WHERE agent = ? AND `+answersWaiting, agent).Scan(&n)
	return n, err
}

// DeliverAnswers moves every answer waiting for agent into its outbox, as
// one message frame makes of them, in one transaction.
func (s *Store) DeliverAnswers(agent string, after int64, frame func([]Ask) string) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT `+askCols+` FROM asks WHERE agent = ? AND `+answersWaiting+` ORDER BY closed_at, id`, agent)
	if err != nil {
		return Pending{}, err
	}
	var as []Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			rows.Close()
			return Pending{}, err
		}
		as = append(as, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Pending{}, err
	}
	if len(as) == 0 {
		return Pending{}, fmt.Errorf("answers for %s: %w", agent, ErrNotFound)
	}
	now := ts(time.Now())
	ids := make([]any, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	if _, err := tx.Exec(`UPDATE asks SET delivered_at = ? WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, append([]any{now}, ids...)...); err != nil {
		return Pending{}, err
	}
	text := frame(as)
	res, err := tx.Exec(`INSERT INTO outbox (agent, text, after, created_at) VALUES (?, ?, ?, ?)`, agent, text, after, now)
	if err != nil {
		return Pending{}, err
	}
	p := Pending{Text: text, After: after}
	if p.ID, err = res.LastInsertId(); err != nil {
		return Pending{}, err
	}
	return p, tx.Commit()
}
