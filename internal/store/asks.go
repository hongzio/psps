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
// waits, open, for as long as it takes; the agent goes on meanwhile. While
// it is open the two may add to it (its Notes): the operator a reply that
// keeps it open, the agent what it found since, and Turn says whose word
// it waits on. The operator closes it with an answer, or without one, and
// that reaches the agent once, in its outbox, as receipts do, as do the
// operator's replies. The agent may withdraw one still open, or close it
// as resolved.
type Ask struct {
	ID      int64       `json:"id"`
	Agent   string      `json:"agent"`
	Kind    string      `json:"kind"`
	Title   string      `json:"title"`
	Body    string      `json:"body,omitempty"`
	Options []AskOption `json:"options,omitempty"`
	// State is AskOpen, AskAnswered, AskDismissed, AskWithdrawn or
	// AskResolved.
	State string `json:"state"`
	// Turn is whose word an open ask waits on: TurnOperator or TurnAgent.
	Turn string `json:"turn"`
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
	// Notes are what the two added while it was open, oldest first.
	Notes []AskNote `json:"notes,omitempty"`
	// Transfer is the file a transfer moves, for that kind alone.
	Transfer *AskTransfer `json:"transfer,omitempty"`
}

// An AskTransfer is the file a transfer ask moves from one isolation to
// another's inbox once the operator approves: the size and sha256 it had
// when it was asked for, which it must still have.
type AskTransfer struct {
	// From and To are the isolations' IDs.
	From   string `json:"from"`
	Path   string `json:"path"`
	To     string `json:"to"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// CopiedAt is when it landed, once it has.
	CopiedAt *time.Time `json:"copied_at,omitempty"`
}

// An AskNote is what the operator or the agent added to an open ask.
type AskNote struct {
	ID int64 `json:"id"`
	// Author is TurnOperator or TurnAgent.
	Author string `json:"author"`
	Text   string `json:"text,omitempty"`
	// Options, the agent's alone, are the question's options from then on.
	Options     []AskOption `json:"options,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	DeliveredAt *time.Time  `json:"delivered_at,omitempty"`
}

// Current is the options the question offers now: those the agent added
// last, or those it asked with.
func (a Ask) Current() []AskOption {
	for i := len(a.Notes) - 1; i >= 0; i-- {
		if len(a.Notes[i].Options) > 0 {
			return a.Notes[i].Options
		}
	}
	return a.Options
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
	AskResolved  = "resolved"
)

// Whose word an open ask waits on, and who wrote a note.
const (
	TurnOperator = "operator"
	TurnAgent    = "agent"
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
	delivered_at TEXT,
	turn         TEXT NOT NULL DEFAULT 'operator',
	transfer     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS asks_agent ON asks(agent, id);
CREATE INDEX IF NOT EXISTS asks_open ON asks(state, id);
CREATE TABLE IF NOT EXISTS ask_notes (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	ask          INTEGER NOT NULL REFERENCES asks(id),
	author       TEXT NOT NULL,
	text         TEXT NOT NULL DEFAULT '',
	options      TEXT NOT NULL DEFAULT '[]',
	created_at   TEXT NOT NULL,
	delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS ask_notes_ask ON ask_notes(ask, id);
`

const askCols = `id, agent, kind, title, body, options, state, outcome, pick, answer, created_at, closed_at, delivered_at, turn, transfer`

// ErrTooManyAsks refuses an ask from an agent with as many open as it may
// have.
var ErrTooManyAsks = errors.New("too many open asks")

// ErrTooManyNotes refuses a note to an ask with as many as it may have.
var ErrTooManyNotes = errors.New("too many notes")

// ErrAskClosed refuses closing or withdrawing an ask that is no longer
// open.
var ErrAskClosed = errors.New("ask already closed")

func scanAsk(row interface{ Scan(...any) error }) (Ask, error) {
	var a Ask
	var opts, created string
	var closed, delivered sql.NullString
	var transfer string
	err := row.Scan(&a.ID, &a.Agent, &a.Kind, &a.Title, &a.Body, &opts, &a.State, &a.Outcome, &a.Pick, &a.Text, &created, &closed, &delivered, &a.Turn, &transfer)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	json.Unmarshal([]byte(opts), &a.Options)
	if transfer != "" {
		a.Transfer = &AskTransfer{}
		json.Unmarshal([]byte(transfer), a.Transfer)
	}
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, withNotes(s.db, out)
}

// withNotes reads each ask's notes into it.
func withNotes(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, as []Ask) error {
	if len(as) == 0 {
		return nil
	}
	at := make(map[int64]int, len(as))
	ids := make([]any, len(as))
	for i, a := range as {
		at[a.ID] = i
		ids[i] = a.ID
	}
	rows, err := db.Query(`SELECT ask, id, author, text, options, created_at, delivered_at FROM ask_notes WHERE ask IN (?`+strings.Repeat(", ?", len(ids)-1)+`) ORDER BY id`, ids...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ask int64
		var n AskNote
		var opts, created string
		var delivered sql.NullString
		if err := rows.Scan(&ask, &n.ID, &n.Author, &n.Text, &opts, &created, &delivered); err != nil {
			return err
		}
		json.Unmarshal([]byte(opts), &n.Options)
		n.CreatedAt = parseTS(created)
		if delivered.Valid {
			t := parseTS(delivered.String)
			n.DeliveredAt = &t
		}
		i := at[ask]
		as[i].Notes = append(as[i].Notes, n)
	}
	return rows.Err()
}

// one is a with its notes.
func (s *Store) one(a Ask, err error) (Ask, error) {
	if err != nil {
		return a, err
	}
	as := []Ask{a}
	err = withNotes(s.db, as)
	return as[0], err
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
		tx.Rollback()
		same, err = s.one(same, nil)
		return same, false, err
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
	a.State, a.Turn = AskOpen, TurnOperator
	a.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := tx.Exec(`INSERT INTO asks (agent, kind, title, body, options, state, created_at, transfer) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Agent, a.Kind, a.Title, a.Body, string(opts), a.State, ts(a.CreatedAt), transferJSON(a.Transfer))
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
	return s.one(a, err)
}

// AsksOf are agent's asks, newest first, at most limit of them.
func (s *Store) AsksOf(agent string, limit int) ([]Ask, error) {
	return s.asks(`WHERE agent = ? ORDER BY id DESC LIMIT ?`, agent, limit)
}

// OpenAsks are every agent's open asks, those waiting on the operator
// first, oldest first.
func (s *Store) OpenAsks() ([]Ask, error) {
	return s.asks(`WHERE state = ? ORDER BY turn = ? DESC, id`, AskOpen, TurnOperator)
}

// AskCounts is how many of an agent's open asks wait on the operator, how
// many on the agent, and how many notes they hold.
type AskCounts struct{ Operator, Agent, Notes int }

// OpenAskCounts counts each agent's open asks, for those with any.
func (s *Store) OpenAskCounts() (map[string]AskCounts, error) {
	rows, err := s.db.Query(`SELECT agent, SUM(turn = ?), SUM(turn != ?), SUM((SELECT COUNT(*) FROM ask_notes n WHERE n.ask = asks.id)) FROM asks WHERE state = ? GROUP BY agent`, TurnOperator, TurnOperator, AskOpen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AskCounts{}
	for rows.Next() {
		var a string
		var n AskCounts
		if err := rows.Scan(&a, &n.Operator, &n.Agent, &n.Notes); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

// AddNote adds what author (TurnOperator or TurnAgent) says to open ask
// id, the agent's own if agent is given, at most max notes to an ask. The
// operator's passes the turn to the agent, the agent's to the operator.
// With resolve, the agent closes the ask after it, as resolved.
func (s *Store) AddNote(id int64, agent, author, text string, opts []AskOption, max int, resolve bool) (Ask, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Ask{}, err
	}
	defer tx.Rollback()
	a, err := scanAsk(tx.QueryRow(`SELECT `+askCols+` FROM asks WHERE id = ?`, id))
	if err == nil && agent != "" && a.Agent != agent {
		err = ErrNotFound
	}
	if errors.Is(err, ErrNotFound) {
		if agent != "" {
			return a, fmt.Errorf("you have no ask #%d: %w", id, ErrNotFound)
		}
		return a, fmt.Errorf("there is no ask #%d: %w", id, ErrNotFound)
	}
	if err != nil {
		return a, err
	}
	if a.State != AskOpen {
		return a, fmt.Errorf("ask #%d is %s: %w", id, a.State, ErrAskClosed)
	}
	now := ts(time.Now())
	if text != "" || len(opts) > 0 {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM ask_notes WHERE ask = ?`, id).Scan(&n); err != nil {
			return a, err
		}
		if n >= max {
			return a, fmt.Errorf("ask #%d has %d notes: %w", id, n, ErrTooManyNotes)
		}
		if opts == nil {
			opts = []AskOption{}
		}
		o, _ := json.Marshal(opts)
		if _, err := tx.Exec(`INSERT INTO ask_notes (ask, author, text, options, created_at) VALUES (?, ?, ?, ?, ?)`, id, author, text, string(o), now); err != nil {
			return a, err
		}
	}
	turn := TurnAgent
	if author == TurnAgent {
		turn = TurnOperator
	}
	if resolve {
		_, err = tx.Exec(`UPDATE asks SET state = ?, closed_at = ?, delivered_at = ? WHERE id = ?`, AskResolved, now, now, id)
	} else {
		_, err = tx.Exec(`UPDATE asks SET turn = ? WHERE id = ?`, turn, id)
	}
	if err != nil {
		return a, err
	}
	if err := tx.Commit(); err != nil {
		return a, err
	}
	return s.GetAsk(id)
}

func transferJSON(t *AskTransfer) string {
	if t == nil {
		return ""
	}
	b, _ := json.Marshal(t)
	return string(b)
}

// TransferDone keeps that open ask id's file landed, at copied.
func (s *Store) TransferDone(id int64, copied time.Time) error {
	a, err := s.GetAsk(id)
	if err != nil {
		return err
	}
	if a.Transfer == nil {
		return fmt.Errorf("ask #%d moves no file", id)
	}
	t := *a.Transfer
	c := copied.UTC().Truncate(time.Second)
	t.CopiedAt = &c
	_, err = s.db.Exec(`UPDATE asks SET transfer = ? WHERE id = ?`, transferJSON(&t), id)
	return err
}

// CloseAsk closes open ask id as state (AskAnswered or AskDismissed), with
// what the operator answered; its agent hears of it next (AnswersWaiting).
func (s *Store) CloseAsk(id int64, state, outcome string, pick int, text string) (Ask, error) {
	a, err := s.one(scanAsk(s.db.QueryRow(`UPDATE asks SET state = ?, outcome = ?, pick = ?, answer = ?, closed_at = ?
		WHERE id = ? AND state = ? RETURNING `+askCols, state, outcome, pick, text, ts(time.Now()), id, AskOpen)))
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
	a, err := s.one(scanAsk(s.db.QueryRow(`UPDATE asks SET state = ?, closed_at = ?, delivered_at = ?
		WHERE id = ? AND agent = ? AND state = ? RETURNING `+askCols, AskWithdrawn, now, now, id, agent, AskOpen)))
	if errors.Is(err, ErrNotFound) {
		if a, err := s.GetAsk(id); err != nil || a.Agent != agent {
			return a, fmt.Errorf("you have no ask #%d: %w", id, ErrNotFound)
		} else {
			return a, fmt.Errorf("ask #%d is %s: %w", id, a.State, ErrAskClosed)
		}
	}
	return a, err
}

// answersWaiting is the condition for an ask the operator closed, or
// replied to, whose agent has not heard of it yet.
const answersWaiting = `((state IN ('answered', 'dismissed') AND delivered_at IS NULL)
	OR EXISTS (SELECT 1 FROM ask_notes n WHERE n.ask = asks.id AND n.author = 'operator' AND n.delivered_at IS NULL))`

// AnswersWaiting is how many of agent's asks the operator closed, or
// replied to, that it has not heard of yet.
func (s *Store) AnswersWaiting(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM asks WHERE agent = ? AND `+answersWaiting, agent).Scan(&n)
	return n, err
}

// DeliverAnswers moves every answer and reply waiting for agent into its
// outbox, as one message frame makes of them, in one transaction. frame
// reads the replies new to the agent as the operator's notes not yet
// delivered.
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
	if err := withNotes(tx, as); err != nil {
		return Pending{}, err
	}
	now := ts(time.Now())
	ids := make([]any, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	in := `(?` + strings.Repeat(", ?", len(ids)-1) + `)`
	if _, err := tx.Exec(`UPDATE asks SET delivered_at = ? WHERE state IN ('answered', 'dismissed') AND delivered_at IS NULL AND id IN `+in, append([]any{now}, ids...)...); err != nil {
		return Pending{}, err
	}
	if _, err := tx.Exec(`UPDATE ask_notes SET delivered_at = ? WHERE author = 'operator' AND delivered_at IS NULL AND ask IN `+in, append([]any{now}, ids...)...); err != nil {
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
