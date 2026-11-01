package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/driver"
)

// A team may have a lead: one of its lines, which makes agents of its own
// in the team (their MadeBy is its line), hears from gero when a turn of
// theirs ends, and works toward the team's goal within its budget. What
// gero tells it waits as the team's news until the lead's live session is
// free, then goes to it in one message.
const leadSchema = `
CREATE TABLE IF NOT EXISTS team_leads (
	team       TEXT PRIMARY KEY REFERENCES teams(id) ON DELETE CASCADE,
	line       TEXT NOT NULL UNIQUE,
	settings   TEXT NOT NULL DEFAULT '{}',
	since      TEXT NOT NULL,
	base       TEXT NOT NULL DEFAULT '{}',
	stopped    TEXT NOT NULL DEFAULT '',
	stale_from INTEGER NOT NULL DEFAULT 0,
	nudged     INTEGER NOT NULL DEFAULT 0,
	dirty      INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS team_news (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	team         TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	body         TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS team_news_team ON team_news(team, id);
CREATE TABLE IF NOT EXISTS team_evals (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	team    TEXT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
	at      TEXT NOT NULL,
	took_ms INTEGER NOT NULL,
	score   REAL,
	why     TEXT NOT NULL,
	output  TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS spent (
	agent  TEXT PRIMARY KEY REFERENCES agents(id),
	input  INTEGER NOT NULL,
	cached INTEGER NOT NULL,
	output INTEGER NOT NULL,
	limits TEXT NOT NULL DEFAULT '[]',
	at     TEXT NOT NULL
);
`

// LeadSettings are the operator's terms for a lead.
type LeadSettings struct {
	// Budget is the tokens (driver.Tokens.New) the lead and the agents it
	// makes may use, counted from when it became the lead; 0 is none, and
	// only the plan ceiling holds them back.
	Budget int64 `json:"budget"`
	// MaxLive is how many agents of its own may be live at once.
	MaxLive int `json:"max_live"`
	// PlanCeiling is the percent of a plan window past which nothing new
	// starts on that plan.
	PlanCeiling int `json:"plan_ceiling"`
	// TurnMinutes is the turn limit of an agent it makes that sets none.
	TurnMinutes int `json:"turn_minutes"`
	// Eval is the shell command gero runs in the isolation, in EvalDir, to
	// score the team's work: its last line starting "SCORE:" is the score,
	// higher better. EvalMinutes is how long it may run.
	Eval        string `json:"eval,omitempty"`
	EvalDir     string `json:"eval_dir,omitempty"`
	EvalMinutes int    `json:"eval_minutes"`
	// Target is a score that ends the run once reached; Patience how many
	// evals in a row without a new best end it.
	Target   *float64 `json:"target,omitempty"`
	Patience int      `json:"patience"`
}

type Lead struct {
	Team     string       `json:"team"`
	Line     string       `json:"line"`
	Settings LeadSettings `json:"settings"`
	Since    time.Time    `json:"since"`
	// Base is what the lead's line had spent when it became the lead.
	Base driver.Tokens `json:"base"`
	// Stopped says why gero stopped the run, "" while it runs: the lead
	// makes no agent until the operator resumes it.
	Stopped string `json:"stopped,omitempty"`
	// StaleFrom: evals up to it do not count toward Patience. Nudged is
	// the eval after which the lead was last told nothing runs. Dirty: a
	// turn of one of its agents ended since the last eval.
	StaleFrom int64 `json:"-"`
	Nudged    int64 `json:"-"`
	Dirty     bool  `json:"-"`
}

const leadCols = `team, line, settings, since, base, stopped, stale_from, nudged, dirty`

func scanLead(row interface{ Scan(...any) error }) (Lead, error) {
	var l Lead
	var settings, since, base string
	err := row.Scan(&l.Team, &l.Line, &settings, &since, &base, &l.Stopped, &l.StaleFrom, &l.Nudged, &l.Dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	json.Unmarshal([]byte(settings), &l.Settings)
	json.Unmarshal([]byte(base), &l.Base)
	l.Since = parseTS(since)
	return l, err
}

// PutLead makes l its team's lead, or sets its terms and state anew. Its
// line is in its team: a line in another team, or leading one, is not
// made a lead.
func (s *Store) PutLead(l Lead) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var team string
	err = tx.QueryRow(`SELECT team FROM team_lines WHERE line = ?`, l.Line).Scan(&team)
	if errors.Is(err, sql.ErrNoRows) || team != l.Team {
		return fmt.Errorf("line %s is not in team %s: %w", l.Line, l.Team, ErrNotFound)
	}
	if err != nil {
		return err
	}
	settings, _ := json.Marshal(l.Settings)
	base, _ := json.Marshal(l.Base)
	if _, err := tx.Exec(`DELETE FROM team_leads WHERE line = ? AND team != ?`, l.Line, l.Team); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO team_leads (`+leadCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (team) DO UPDATE SET line = excluded.line, settings = excluded.settings, since = excluded.since, base = excluded.base,
			stopped = excluded.stopped, stale_from = excluded.stale_from, nudged = excluded.nudged, dirty = excluded.dirty`,
		l.Team, l.Line, string(settings), ts(l.Since), string(base), l.Stopped, l.StaleFrom, l.Nudged, l.Dirty); err != nil {
		return err
	}
	return tx.Commit()
}

// Lead is team's lead.
func (s *Store) Lead(team string) (Lead, error) {
	return scanLead(s.db.QueryRow(`SELECT `+leadCols+` FROM team_leads WHERE team = ?`, team))
}

// LeadOfLine is the lead line is, if it is one.
func (s *Store) LeadOfLine(line string) (Lead, error) {
	return scanLead(s.db.QueryRow(`SELECT `+leadCols+` FROM team_leads WHERE line = ?`, line))
}

func (s *Store) Leads() ([]Lead, error) {
	rows, err := s.db.Query(`SELECT ` + leadCols + ` FROM team_leads ORDER BY team`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lead
	for rows.Next() {
		l, err := scanLead(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLead leaves team without a lead. The agents it made stay, in the
// team, the operator's.
func (s *Store) DeleteLead(team string) error {
	res, err := s.db.Exec(`DELETE FROM team_leads WHERE team = ?`, team)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team %s has no lead: %w", team, ErrNotFound)
	}
	return nil
}

// SetLeadStopped stops team's run, saying why, or with "" resumes it.
func (s *Store) SetLeadStopped(team, why string) error {
	_, err := s.db.Exec(`UPDATE team_leads SET stopped = ? WHERE team = ?`, why, team)
	return err
}

func (s *Store) SetLeadNudged(team string, eval int64) error {
	_, err := s.db.Exec(`UPDATE team_leads SET nudged = ? WHERE team = ?`, eval, team)
	return err
}

func (s *Store) SetLeadDirty(team string, dirty bool) error {
	_, err := s.db.Exec(`UPDATE team_leads SET dirty = ? WHERE team = ?`, dirty, team)
	return err
}

// AddTeamNews puts something gero tells team's lead in its waiting news.
func (s *Store) AddTeamNews(team, body string) error {
	_, err := s.db.Exec(`INSERT INTO team_news (team, body, created_at) VALUES (?, ?, ?)`, team, body, ts(time.Now()))
	return err
}

// TeamNewsWaiting is how much news waits for team's lead.
func (s *Store) TeamNewsWaiting(team string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM team_news WHERE team = ? AND delivered_at IS NULL`, team).Scan(&n)
	return n, err
}

// DeliverTeamNews moves all the news waiting for team's lead into agent's
// outbox as one message, frame's of the news, oldest first, in one
// transaction, as DeliverMail moves a mail: it reaches the lead exactly
// once. With none waiting it is ErrNotFound.
func (s *Store) DeliverTeamNews(team, agent string, after int64, frame func(news []string) string) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, body FROM team_news WHERE team = ? AND delivered_at IS NULL ORDER BY id`, team)
	if err != nil {
		return Pending{}, err
	}
	var ids []any
	var news []string
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return Pending{}, err
		}
		ids, news = append(ids, id), append(news, body)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Pending{}, err
	}
	if len(news) == 0 {
		return Pending{}, fmt.Errorf("news for team %s: %w", team, ErrNotFound)
	}
	now := ts(time.Now())
	if _, err := tx.Exec(`UPDATE team_news SET delivered_at = ? WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, append([]any{now}, ids...)...); err != nil {
		return Pending{}, err
	}
	text := frame(news)
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

// An Eval is one run of a team's eval. Score is nil when it printed none.
type Eval struct {
	ID     int64         `json:"id"`
	Team   string        `json:"team"`
	At     time.Time     `json:"at"`
	Took   time.Duration `json:"took"`
	Score  *float64      `json:"score,omitempty"`
	Why    string        `json:"why"`
	Output string        `json:"output"`
}

func (s *Store) AddEval(e Eval) (Eval, error) {
	res, err := s.db.Exec(`INSERT INTO team_evals (team, at, took_ms, score, why, output) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Team, ts(e.At), e.Took.Milliseconds(), e.Score, e.Why, e.Output)
	if err != nil {
		return e, err
	}
	e.ID, err = res.LastInsertId()
	return e, err
}

// Evals are team's evals, oldest first.
func (s *Store) Evals(team string) ([]Eval, error) {
	rows, err := s.db.Query(`SELECT id, team, at, took_ms, score, why, output FROM team_evals WHERE team = ? ORDER BY id`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Eval{}
	for rows.Next() {
		var e Eval
		var at string
		var took int64
		var score sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.Team, &at, &took, &score, &e.Why, &e.Output); err != nil {
			return nil, err
		}
		e.At, e.Took = parseTS(at), time.Duration(took)*time.Millisecond
		if score.Valid {
			e.Score = &score.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Spent is what an agent's CLI last said it used, and its plan windows.
type Spent struct {
	Tokens driver.Tokens  `json:"tokens"`
	Limits []driver.Limit `json:"limits,omitempty"`
	At     time.Time      `json:"at"`
}

// PutSpent keeps what agent has spent, as its log says now.
func (s *Store) PutSpent(agent string, sp Spent) error {
	limits, _ := json.Marshal(sp.Limits)
	_, err := s.db.Exec(`INSERT INTO spent (agent, input, cached, output, limits, at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET input = excluded.input, cached = excluded.cached, output = excluded.output, limits = excluded.limits, at = excluded.at`,
		agent, sp.Tokens.Input, sp.Tokens.Cached, sp.Tokens.Output, string(limits), ts(sp.At))
	return err
}

// SpentOf is what each agent has spent, by key; one that said nothing is
// not in it.
func (s *Store) SpentOf() (map[string]Spent, error) {
	rows, err := s.db.Query(`SELECT agent, input, cached, output, limits, at FROM spent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Spent{}
	for rows.Next() {
		var agent, limits, at string
		var sp Spent
		if err := rows.Scan(&agent, &sp.Tokens.Input, &sp.Tokens.Cached, &sp.Tokens.Output, &limits, &at); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(limits), &sp.Limits)
		sp.At = parseTS(at)
		out[agent] = sp
	}
	return out, rows.Err()
}
