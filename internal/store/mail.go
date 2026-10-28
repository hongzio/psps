package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A Mail is one agent's message to another. It waits in the recipient's
// inbox until it is delivered (moved to the recipient's outbox, in one
// transaction, so it reaches the agent exactly once whenever the daemon
// dies) or the operator discards it unread; either way it stays, as the
// two agents' mail history.
type Mail struct {
	ID int64 `json:"id"`
	// To is the recipient's key, From the sender's. FromID is what the
	// sender was called (its own ID, or its key) when it wrote, and
	// FromIsolation the isolation it wrote from, by ID: what the mail says
	// of a sender deleted since.
	To            string     `json:"to"`
	From          string     `json:"from"`
	FromID        string     `json:"from_id"`
	FromIsolation string     `json:"from_isolation"`
	Body          string     `json:"body"`
	CreatedAt     time.Time  `json:"created_at"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	DiscardedAt   *time.Time `json:"discarded_at,omitempty"`
}

// Waiting is whether m is still in its recipient's inbox.
func (m Mail) Waiting() bool { return m.DeliveredAt == nil && m.DiscardedAt == nil }

const mailSchema = `
CREATE TABLE IF NOT EXISTS mail (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	agent        TEXT NOT NULL REFERENCES agents(id),
	sender       TEXT NOT NULL,
	sender_id    TEXT NOT NULL,
	sender_iso   TEXT NOT NULL,
	body         TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	delivered_at TEXT,
	discarded_at TEXT
);
CREATE INDEX IF NOT EXISTS mail_agent ON mail(agent, id);
CREATE INDEX IF NOT EXISTS mail_sender ON mail(sender, id);
`

// waiting is the condition for mail still in an inbox.
const waiting = `delivered_at IS NULL AND discarded_at IS NULL`

// ErrInboxFull refuses mail to an agent that has as much waiting as it
// may.
var ErrInboxFull = errors.New("inbox full")

// AddMail puts m in its recipient's inbox, unless max mails already wait
// there. It returns m with its ID, and how many wait now, m included.
func (s *Store) AddMail(m Mail, max int) (Mail, int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return m, 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM mail WHERE agent = ? AND `+waiting, m.To).Scan(&n); err != nil {
		return m, 0, err
	}
	if n >= max {
		return m, n, fmt.Errorf("%d mails wait for it already: %w", n, ErrInboxFull)
	}
	m.CreatedAt = time.Now().UTC()
	res, err := tx.Exec(`INSERT INTO mail (agent, sender, sender_id, sender_iso, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		m.To, m.From, m.FromID, m.FromIsolation, m.Body, ts(m.CreatedAt))
	if err != nil {
		return m, 0, err
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return m, 0, err
	}
	return m, n + 1, tx.Commit()
}

const mailCols = `id, agent, sender, sender_id, sender_iso, body, created_at, delivered_at, discarded_at`

func scanMail(row interface{ Scan(...any) error }) (Mail, error) {
	var m Mail
	var created string
	var delivered, discarded sql.NullString
	err := row.Scan(&m.ID, &m.To, &m.From, &m.FromID, &m.FromIsolation, &m.Body, &created, &delivered, &discarded)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	m.CreatedAt = parseTS(created)
	for _, t := range []struct {
		v  sql.NullString
		to **time.Time
	}{{delivered, &m.DeliveredAt}, {discarded, &m.DiscardedAt}} {
		if t.v.Valid {
			at := parseTS(t.v.String)
			*t.to = &at
		}
	}
	return m, err
}

func (s *Store) mails(q string, args ...any) ([]Mail, error) {
	rows, err := s.db.Query(`SELECT `+mailCols+` FROM mail WHERE `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Mail{}
	for rows.Next() {
		m, err := scanMail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Inbox is the mail waiting for an agent, oldest first: the order it will
// read it in.
func (s *Store) Inbox(agent string) ([]Mail, error) {
	return s.mails(`agent = ? AND `+waiting+` ORDER BY id`, agent)
}

// MailOf is an agent's mail history, newest first: what it was sent,
// waiting or not, and what it wrote. At most limit mails.
func (s *Store) MailOf(agent string, limit int) ([]Mail, error) {
	return s.mails(`agent = ? OR sender = ? ORDER BY id DESC LIMIT ?`, agent, agent, limit)
}

// NextMail is the oldest mail waiting for an agent.
func (s *Store) NextMail(agent string) (Mail, error) {
	return scanMail(s.db.QueryRow(`SELECT `+mailCols+` FROM mail WHERE agent = ? AND `+waiting+` ORDER BY id LIMIT 1`, agent))
}

// InboxCounts is how much mail waits, by agent; an agent with none is not
// in it.
func (s *Store) InboxCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT agent, COUNT(*) FROM mail WHERE ` + waiting + ` GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var agent string
		var n int
		if err := rows.Scan(&agent, &n); err != nil {
			return nil, err
		}
		out[agent] = n
	}
	return out, rows.Err()
}

func (s *Store) InboxCount(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mail WHERE agent = ? AND `+waiting, agent).Scan(&n)
	return n, err
}

// DeliverMail takes mail id out of its recipient's inbox and puts it in
// its outbox as text, accepted after log position after, as AddPending
// would. A mail no longer waiting (delivered, or discarded meanwhile) is
// ErrNotFound, and nothing is added.
func (s *Store) DeliverMail(id int64, text string, after int64) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	var agent string
	err = tx.QueryRow(`SELECT agent FROM mail WHERE id = ? AND `+waiting, id).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) {
		return Pending{}, fmt.Errorf("mail %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Pending{}, err
	}
	now := ts(time.Now())
	if _, err := tx.Exec(`UPDATE mail SET delivered_at = ? WHERE id = ?`, now, id); err != nil {
		return Pending{}, err
	}
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

// DiscardMail takes mail id out of agent's inbox unread; it stays in the
// history as discarded.
func (s *Store) DiscardMail(agent string, id int64) error {
	res, err := s.db.Exec(`UPDATE mail SET discarded_at = ? WHERE id = ? AND agent = ? AND `+waiting, ts(time.Now()), id, agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no mail %d waits for agent %s: %w", id, agent, ErrNotFound)
	}
	return nil
}

// ClearInbox discards every mail waiting for agent, and says how many.
func (s *Store) ClearInbox(agent string) (int, error) {
	res, err := s.db.Exec(`UPDATE mail SET discarded_at = ? WHERE agent = ? AND `+waiting, ts(time.Now()), agent)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// MailToken is the token in an agent's mail address, made the first time
// it is asked for and the agent's for good after. It is no part of Agent:
// nothing that shows an agent shows it.
func (s *Store) MailToken(agent string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var tok sql.NullString
	err = tx.QueryRow(`SELECT mail_token FROM agents WHERE id = ?`, agent).Scan(&tok)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("agent %s: %w", agent, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	if tok.Valid && tok.String != "" {
		return tok.String, nil
	}
	t := newToken()
	if _, err := tx.Exec(`UPDATE agents SET mail_token = ? WHERE id = ?`, t, agent); err != nil {
		return "", err
	}
	return t, tx.Commit()
}

// AgentByMailToken is the agent whose mail address carries tok.
func (s *Store) AgentByMailToken(tok string) (Agent, error) {
	if tok == "" {
		return Agent{}, ErrNotFound
	}
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE mail_token = ?`, tok))
}
