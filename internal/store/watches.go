package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A Watch is an agent waiting on another to finish with what it was told:
// psps tells the sender, as a receipt, what came of it (internal/daemon,
// watches.go). It is open until it is done: the receipt that ends it is
// written, or the sender drops it or goes away.
type Watch struct {
	ID int64 `json:"id"`
	// Sender waits; Target is waited on.
	Sender string `json:"sender"`
	Target string `json:"target"`
	// Pending is the outbox message the watch is on, until the target
	// reads it; 0 for a watch on no message, or once it is read.
	Pending int64 `json:"pending,omitempty"`
	// Mode is WatchSettle or WatchTurn.
	Mode string `json:"mode"`
	// Excerpt is the start of what was sent, for the receipt to say which.
	Excerpt string `json:"excerpt"`
	// Reading: the target has the message (or, for a watch on no message,
	// has started a turn since), and its turns' ends are judged; ReadSeq
	// is its log's record where it read it.
	Reading bool  `json:"reading"`
	ReadSeq int64 `json:"read_seq,omitempty"`
	// Minutes, if not 0, is when the sender asked to hear that it is still
	// at work; Elapsed counts the seconds the daemon has run since, so
	// time the Mac was off does not count.
	Minutes int   `json:"minutes,omitempty"`
	Elapsed int64 `json:"elapsed,omitempty"`
	// Noted are the receipts of the watch's way already written, each said
	// once: "background", "deadline", "approval:<summary>".
	Noted []string `json:"noted,omitempty"`
	// Stalled is how many background tasks the target's process took with
	// it as it ended: the work is not done, and only the operator resumes
	// it. 0 when it is not stalled.
	Stalled int `json:"stalled,omitempty"`
	// Others: something besides the watched message reached the target
	// since it read it, so its last answer may be about that.
	Others bool `json:"others,omitempty"`
	// Answers are the target's last answers of the turns it ended since it
	// read the message that no receipt carried yet, oldest first: the
	// final receipt says them too, as its last turn may only say that
	// nothing new came of a task in the background.
	Answers   []string   `json:"answers,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

const (
	WatchSettle = "settle"
	WatchTurn   = "turn"
)

// Noting is whether what is among w's receipts already written.
func (w Watch) Noting(what string) bool {
	for _, n := range w.Noted {
		if n == what {
			return true
		}
	}
	return false
}

const watchSchema = `
CREATE TABLE IF NOT EXISTS watches (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	sender     TEXT NOT NULL,
	target     TEXT NOT NULL,
	pending    INTEGER NOT NULL DEFAULT 0,
	mode       TEXT NOT NULL,
	excerpt    TEXT NOT NULL,
	reading    INTEGER NOT NULL DEFAULT 0,
	read_seq   INTEGER NOT NULL DEFAULT 0,
	minutes    INTEGER NOT NULL DEFAULT 0,
	elapsed    INTEGER NOT NULL DEFAULT 0,
	noted      TEXT NOT NULL DEFAULT '',
	stalled    INTEGER NOT NULL DEFAULT 0,
	others     INTEGER NOT NULL DEFAULT 0,
	answers    TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL,
	done_at    TEXT
);
CREATE INDEX IF NOT EXISTS watches_target ON watches(target) WHERE done_at IS NULL;
CREATE INDEX IF NOT EXISTS watches_sender ON watches(sender) WHERE done_at IS NULL;
CREATE TABLE IF NOT EXISTS receipts (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	agent        TEXT NOT NULL REFERENCES agents(id),
	watch        INTEGER NOT NULL,
	body         TEXT NOT NULL,
	created_at   TEXT NOT NULL,
	delivered_at TEXT
);
CREATE INDEX IF NOT EXISTS receipts_agent ON receipts(agent, id);
`

const watchCols = `id, sender, target, pending, mode, excerpt, reading, read_seq, minutes, elapsed, noted, stalled, others, answers, created_at, done_at`

func scanWatch(row interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var noted, answers, created string
	var done sql.NullString
	if err := row.Scan(&w.ID, &w.Sender, &w.Target, &w.Pending, &w.Mode, &w.Excerpt, &w.Reading, &w.ReadSeq, &w.Minutes, &w.Elapsed,
		&noted, &w.Stalled, &w.Others, &answers, &created, &done); err != nil {
		return Watch{}, err
	}
	json.Unmarshal([]byte(answers), &w.Answers)
	if noted != "" {
		w.Noted = strings.Split(noted, "\n")
	}
	w.CreatedAt = parseTS(created)
	if done.Valid {
		t := parseTS(done.String)
		w.DoneAt = &t
	}
	return w, nil
}

func (s *Store) watches(where string, args ...any) ([]Watch, error) {
	rows, err := s.db.Query(`SELECT `+watchCols+` FROM watches WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ErrTooMany refuses one more of something an agent has as many of as it
// may.
var ErrTooMany = errors.New("too many")

// insertWatch adds w, unless its sender has max open already.
func insertWatch(tx *sql.Tx, w Watch, max int) (int64, error) {
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM watches WHERE sender = ? AND done_at IS NULL`, w.Sender).Scan(&n); err != nil {
		return 0, err
	}
	if n >= max {
		return 0, fmt.Errorf("agent %s has %d watches open: %w", w.Sender, n, ErrTooMany)
	}
	res, err := tx.Exec(`INSERT INTO watches (sender, target, pending, mode, excerpt, reading, read_seq, minutes, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.Sender, w.Target, w.Pending, w.Mode, w.Excerpt, w.Reading, w.ReadSeq, w.Minutes, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddPendingWatched records a message for agent, as AddPending does, and
// a watch on it, in one transaction: the two are there or gone together
// whenever the daemon dies. A sender with max watches open already gets
// the message without one: watch is 0 and werr ErrTooMany.
func (s *Store) AddPendingWatched(agent, text string, after int64, w Watch, max int) (pending, watch int64, werr, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, nil, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO outbox (agent, text, after, rewind, created_at) VALUES (?, ?, ?, 0, ?)`, agent, text, after, ts(time.Now()))
	if err != nil {
		return 0, 0, nil, err
	}
	if pending, err = res.LastInsertId(); err != nil {
		return 0, 0, nil, err
	}
	w.Target, w.Pending = agent, pending
	watch, werr = insertWatch(tx, w, max)
	if werr != nil && !errors.Is(werr, ErrTooMany) {
		return 0, 0, nil, werr
	}
	return pending, watch, werr, tx.Commit()
}

// AddWatch adds a watch on no message, unless its sender has max open.
func (s *Store) AddWatch(w Watch, max int) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	w.Pending = 0
	id, err := insertWatch(tx, w, max)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Watch is the watch id.
func (s *Store) Watch(id int64) (Watch, error) {
	w, err := scanWatch(s.db.QueryRow(`SELECT `+watchCols+` FROM watches WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Watch{}, fmt.Errorf("watch %d: %w", id, ErrNotFound)
	}
	return w, err
}

// WatchesOn are the open watches on agent, oldest first.
func (s *Store) WatchesOn(agent string) ([]Watch, error) {
	return s.watches(`target = ? AND done_at IS NULL`, agent)
}

// WatchesBy are agent's open watches, oldest first.
func (s *Store) WatchesBy(agent string) ([]Watch, error) {
	return s.watches(`sender = ? AND done_at IS NULL`, agent)
}

// OpenWatches are all the open watches, oldest first.
func (s *Store) OpenWatches() ([]Watch, error) {
	return s.watches(`done_at IS NULL`)
}

// PutWatch keeps what changed of an open watch on its way: whether it is
// read and where, what it has said, its elapsed time, whether it stalled.
func (s *Store) PutWatch(w Watch) error {
	_, err := s.db.Exec(`UPDATE watches SET pending = ?, reading = ?, read_seq = ?, elapsed = ?, noted = ?, stalled = ?, others = ?, answers = ? WHERE id = ? AND done_at IS NULL`,
		w.Pending, w.Reading, w.ReadSeq, w.Elapsed, strings.Join(w.Noted, "\n"), w.Stalled, w.Others, answersJSON(w.Answers), w.ID)
	return err
}

// Receipt writes a receipt of w's for its sender and keeps w as it is
// now, in one transaction; done ends w with it. A watch ended already
// writes nothing: ok is false.
func (s *Store) Receipt(w Watch, body string, done bool) (ok bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := ts(time.Now())
	var doneAt any
	if done {
		doneAt = now
	}
	res, err := tx.Exec(`UPDATE watches SET pending = ?, reading = ?, read_seq = ?, elapsed = ?, noted = ?, stalled = ?, others = ?, answers = ?, done_at = ? WHERE id = ? AND done_at IS NULL`,
		w.Pending, w.Reading, w.ReadSeq, w.Elapsed, strings.Join(w.Noted, "\n"), w.Stalled, w.Others, answersJSON(w.Answers), doneAt, w.ID)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if body != "" {
		if _, err := tx.Exec(`INSERT INTO receipts (agent, watch, body, created_at) VALUES (?, ?, ?, ?)`, w.Sender, w.ID, body, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// EndWatch ends an open watch with no receipt; false if it was not open.
func (s *Store) EndWatch(id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE watches SET done_at = ? WHERE id = ? AND done_at IS NULL`, ts(time.Now()), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// EndWatchesBy ends agent's open watches with no receipt, as it goes away.
func (s *Store) EndWatchesBy(agent string) error {
	_, err := s.db.Exec(`UPDATE watches SET done_at = ? WHERE sender = ? AND done_at IS NULL`, ts(time.Now()), agent)
	return err
}

// ReceiptsWaiting is how many receipts wait for agent.
func (s *Store) ReceiptsWaiting(agent string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE agent = ? AND delivered_at IS NULL`, agent).Scan(&n)
	return n, err
}

// DeliverReceipts moves all the receipts waiting for agent into its
// outbox as one message, frame's of them, oldest first, in one
// transaction, as DeliverTeamNews does. With none waiting it is
// ErrNotFound.
func (s *Store) DeliverReceipts(agent string, after int64, frame func(receipts []string) string) (Pending, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Pending{}, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, body FROM receipts WHERE agent = ? AND delivered_at IS NULL ORDER BY id`, agent)
	if err != nil {
		return Pending{}, err
	}
	var ids []any
	var bodies []string
	for rows.Next() {
		var id int64
		var body string
		if err := rows.Scan(&id, &body); err != nil {
			rows.Close()
			return Pending{}, err
		}
		ids, bodies = append(ids, id), append(bodies, body)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Pending{}, err
	}
	if len(bodies) == 0 {
		return Pending{}, fmt.Errorf("receipts for %s: %w", agent, ErrNotFound)
	}
	now := ts(time.Now())
	if _, err := tx.Exec(`UPDATE receipts SET delivered_at = ? WHERE id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, append([]any{now}, ids...)...); err != nil {
		return Pending{}, err
	}
	text := frame(bodies)
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

func answersJSON(a []string) string {
	if len(a) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(a)
	return string(b)
}

// handOverWatches gives what next takes over from old as old goes (Renew,
// Restore): its open watches and the receipts not yet delivered to it.
func handOverWatches(tx *sql.Tx, next, old string) error {
	for _, q := range []string{`UPDATE watches SET sender = ? WHERE sender = ? AND done_at IS NULL`, `UPDATE receipts SET agent = ? WHERE agent = ? AND delivered_at IS NULL`} {
		if _, err := tx.Exec(q, next, old); err != nil {
			return err
		}
	}
	return nil
}

// A TeamWait is how psps holds a lead's agent's turns that end with tasks
// still in the background (internal/daemon, leads.go): under WatchSettle,
// unless its lead released it, it counts as at work until a turn of it
// ends with none, and its lead hears of the first such turn alone, then of
// the one that ends it, with what it said between. Under WatchTurn each
// turn's end is news, whatever runs in the background.
type TeamWait struct {
	Agent string `json:"agent"`
	Mode  string `json:"mode"`
	// Noted: its lead heard that a turn ended with tasks in the background.
	Noted bool `json:"noted,omitempty"`
	// Released: it counts as done for now, though its tasks run on, as its
	// lead asked or its turn's limit passed, until a turn of it ends with
	// nothing in the background.
	Released bool `json:"released,omitempty"`
	// Stalled is how many tasks its process took with it as it ended out
	// of a turn: the work is not done, and the operator resumes it.
	Stalled int `json:"stalled,omitempty"`
	// Elapsed is the seconds the daemon has run while it waited on its
	// background tasks, against its turn's limit.
	Elapsed int64 `json:"elapsed,omitempty"`
	// Answers are its last answers of the turns its lead did not hear of.
	Answers []string `json:"answers,omitempty"`
}

const teamWaitSchema = `
CREATE TABLE IF NOT EXISTS team_waits (
	agent    TEXT PRIMARY KEY REFERENCES agents(id),
	mode     TEXT NOT NULL DEFAULT 'settle',
	noted    INTEGER NOT NULL DEFAULT 0,
	released INTEGER NOT NULL DEFAULT 0,
	stalled  INTEGER NOT NULL DEFAULT 0,
	elapsed  INTEGER NOT NULL DEFAULT 0,
	answers  TEXT NOT NULL DEFAULT '[]'
);
`

// TeamWaitOf is agent's, WatchSettle and nothing else if it has none yet.
func (s *Store) TeamWaitOf(agent string) (TeamWait, error) {
	w := TeamWait{Agent: agent, Mode: WatchSettle}
	var answers string
	err := s.db.QueryRow(`SELECT mode, noted, released, stalled, elapsed, answers FROM team_waits WHERE agent = ?`, agent).
		Scan(&w.Mode, &w.Noted, &w.Released, &w.Stalled, &w.Elapsed, &answers)
	if errors.Is(err, sql.ErrNoRows) {
		return w, nil
	}
	json.Unmarshal([]byte(answers), &w.Answers)
	return w, err
}

// PutTeamWait keeps w.
func (s *Store) PutTeamWait(w TeamWait) error {
	_, err := s.db.Exec(`INSERT INTO team_waits (agent, mode, noted, released, stalled, elapsed, answers) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET mode = excluded.mode, noted = excluded.noted, released = excluded.released, stalled = excluded.stalled,
		elapsed = excluded.elapsed, answers = excluded.answers`,
		w.Agent, w.Mode, w.Noted, w.Released, w.Stalled, w.Elapsed, answersJSON(w.Answers))
	return err
}
