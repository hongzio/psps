package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// A Cron is a message the operator set to be sent to an agent on a
// schedule. Each time it comes due the agent is sent it if it is free;
// if not, IfBusy says whether that run is skipped or waits for it.
type Cron struct {
	ID       int64  `json:"id"`
	Agent    string `json:"agent"`
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt"`
	// IfBusy is CronSkip or CronWait.
	IfBusy    string    `json:"if_busy"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	// NextAt is when it next comes due; nil while it is off.
	NextAt *time.Time `json:"next_at,omitempty"`
	// WaitingSince: a run found the agent busy and waits for it to be
	// free. Runs that come due meanwhile are folded into it.
	WaitingSince *time.Time `json:"waiting_since,omitempty"`
	// LastAt is when it last came due, and LastResult what came of it;
	// SentAt when it was last sent.
	LastAt     *time.Time `json:"last_at,omitempty"`
	LastResult string     `json:"last_result,omitempty"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
	// Self: the agent set it itself, through the cron service. It may
	// change and remove only those; the operator any.
	Self bool `json:"self,omitempty"`
}

const (
	CronSkip = "skip"
	CronWait = "wait"
)

const cronSchema = `
CREATE TABLE IF NOT EXISTS crons (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	agent         TEXT NOT NULL REFERENCES agents(id),
	schedule      TEXT NOT NULL,
	prompt        TEXT NOT NULL,
	if_busy       TEXT NOT NULL,
	enabled       INTEGER NOT NULL DEFAULT 1,
	created_at    TEXT NOT NULL,
	next_at       TEXT,
	waiting_since TEXT,
	last_at       TEXT,
	last_result   TEXT NOT NULL DEFAULT '',
	sent_at       TEXT,
	self          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS crons_agent ON crons(agent, id);
`

const cronColumns = `id, agent, schedule, prompt, if_busy, enabled, created_at, next_at, waiting_since, last_at, last_result, sent_at, self`

// cronLive is the condition for a cron that runs: on, of an agent that is not
// archived.
const cronLive = `c.enabled = 1 AND c.agent IN (SELECT id FROM agents WHERE archived_at IS NULL)`

// cts is a cron's time, in whole seconds: written the same width each
// time, so its text sorts as the time does.
func cts(t time.Time) string { return ts(t.Truncate(time.Second)) }

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return cts(*t)
}

func scanCron(row interface{ Scan(...any) error }) (Cron, error) {
	var c Cron
	var created string
	var next, waiting, last, sent sql.NullString
	err := row.Scan(&c.ID, &c.Agent, &c.Schedule, &c.Prompt, &c.IfBusy, &c.Enabled, &created, &next, &waiting, &last, &c.LastResult, &sent, &c.Self)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt = parseTS(created)
	for _, f := range []struct {
		s  sql.NullString
		to **time.Time
	}{{next, &c.NextAt}, {waiting, &c.WaitingSince}, {last, &c.LastAt}, {sent, &c.SentAt}} {
		if f.s.Valid {
			t := parseTS(f.s.String)
			*f.to = &t
		}
	}
	return c, nil
}

func (s *Store) crons(where string, args ...any) ([]Cron, error) {
	rows, err := s.db.Query(`SELECT `+cronColumns+` FROM crons c WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Cron
	for rows.Next() {
		c, err := scanCron(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddCron stores c and returns it with its ID.
func (s *Store) AddCron(c Cron) (Cron, error) {
	c.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.db.Exec(`INSERT INTO crons (agent, schedule, prompt, if_busy, enabled, created_at, next_at, self) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Agent, c.Schedule, c.Prompt, c.IfBusy, c.Enabled, cts(c.CreatedAt), nullTS(c.NextAt), c.Self)
	if err != nil {
		return c, err
	}
	c.ID, err = res.LastInsertId()
	return c, err
}

// Crons are agent's, oldest first.
func (s *Store) Crons(agent string) ([]Cron, error) {
	return s.crons(`agent = ? ORDER BY id`, agent)
}

func (s *Store) Cron(agent string, id int64) (Cron, error) {
	c, err := scanCron(s.db.QueryRow(`SELECT `+cronColumns+` FROM crons c WHERE id = ? AND agent = ?`, id, agent))
	if errors.Is(err, ErrNotFound) {
		return c, fmt.Errorf("agent %s has no cron %d: %w", agent, id, ErrNotFound)
	}
	return c, err
}

// PutCron writes what the operator sets of c, with when it next comes due
// and whether a run of it waits.
func (s *Store) PutCron(c Cron) error {
	res, err := s.db.Exec(`UPDATE crons SET schedule = ?, prompt = ?, if_busy = ?, enabled = ?, next_at = ?, waiting_since = ? WHERE id = ? AND agent = ?`,
		c.Schedule, c.Prompt, c.IfBusy, c.Enabled, nullTS(c.NextAt), nullTS(c.WaitingSince), c.ID, c.Agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent %s has no cron %d: %w", c.Agent, c.ID, ErrNotFound)
	}
	return nil
}

func (s *Store) DeleteCron(agent string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM crons WHERE id = ? AND agent = ?`, id, agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent %s has no cron %d: %w", agent, id, ErrNotFound)
	}
	return nil
}

// DueCrons are the live crons due at now, soonest first.
func (s *Store) DueCrons(now time.Time) ([]Cron, error) {
	return s.crons(cronLive+` AND c.next_at <= ? ORDER BY c.next_at, c.id`, cts(now))
}

// NextCronAt is when the next live cron comes due after now; nil if none
// will.
func (s *Store) NextCronAt(now time.Time) (*time.Time, error) {
	var next sql.NullString
	if err := s.db.QueryRow(`SELECT MIN(c.next_at) FROM crons c WHERE `+cronLive+` AND c.next_at > ?`, cts(now)).Scan(&next); err != nil || !next.Valid {
		return nil, err
	}
	t := parseTS(next.String)
	return &t, nil
}

// CronCounts are how many crons are on, by agent.
func (s *Store) CronCounts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT agent, COUNT(*) FROM crons WHERE enabled = 1 GROUP BY agent`)
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

func (s *Store) CronCount(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM crons WHERE agent = ? AND enabled = 1`, agent).Scan(&n)
	return n, err
}

// CronCame records cron id coming due at at without being sent: it next
// comes at next, and result is what came of this run. wait: the run waits
// for the agent (the first one that did, if one already waits).
func (s *Store) CronCame(id int64, at, next time.Time, result string, wait bool) error {
	q := `UPDATE crons SET next_at = ?, last_at = ?, last_result = ? WHERE id = ?`
	if wait {
		q = `UPDATE crons SET next_at = ?, last_at = ?, last_result = ?, waiting_since = COALESCE(waiting_since, ?) WHERE id = ?`
		_, err := s.db.Exec(q, cts(next), cts(at), result, cts(at), id)
		return err
	}
	_, err := s.db.Exec(q, cts(next), cts(at), result, id)
	return err
}

// WaitingCron is the live cron of agent's that has waited longest for it.
func (s *Store) WaitingCron(agent string) (Cron, error) {
	cs, err := s.crons(cronLive+` AND c.agent = ? AND c.waiting_since IS NOT NULL ORDER BY c.waiting_since, c.id LIMIT 1`, agent)
	if err != nil {
		return Cron{}, err
	}
	if len(cs) == 0 {
		return Cron{}, ErrNotFound
	}
	return cs[0], nil
}

func (s *Store) WaitingCrons(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM crons c WHERE `+cronLive+` AND c.agent = ? AND c.waiting_since IS NOT NULL`, agent).Scan(&n)
	return n, err
}

// SendCron moves cron id's message, text, to its agent's outbox in one
// transaction with recording it sent at at, so it reaches the agent exactly
// once whenever the daemon dies. next, if given, is that it came due at at
// and next comes then; without it, it is a run that waited being sent.
func (s *Store) SendCron(id int64, text string, after int64, at time.Time, next *time.Time) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	var agent string
	err = tx.QueryRow(`SELECT agent FROM crons WHERE id = ? AND enabled = 1`, id).Scan(&agent)
	if errors.Is(err, sql.ErrNoRows) {
		return Pending{}, fmt.Errorf("cron %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Pending{}, err
	}
	now := cts(at)
	q, args := `UPDATE crons SET waiting_since = NULL, last_result = 'sent', sent_at = ? WHERE id = ?`, []any{now, id}
	if next != nil {
		q, args = `UPDATE crons SET waiting_since = NULL, last_result = 'sent', sent_at = ?, last_at = ?, next_at = ? WHERE id = ?`, []any{now, now, cts(*next), id}
	}
	if _, err := tx.Exec(q, args...); err != nil {
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
