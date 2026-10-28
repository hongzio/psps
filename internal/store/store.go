// Package store is the daemon's durable state: what isolations and agents
// should exist, and every event the agents have produced.
//
// It is the desired state the daemon reconciles toward, not a cache of the
// world. Agent processes and their native session files live in the
// isolation; the store remembers which ones gero owns and how far into each
// agent's shim log it has read.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
)

var ErrNotFound = errors.New("not found")

type Isolation struct {
	Name string `json:"name"`
	// Base is what its container, network and volumes are named after,
	// when that is not Name: the name it had when they were made, which a
	// rename leaves them with, or one a new isolation took because its own
	// was still in use that way.
	Base      string `json:"base,omitempty"`
	Container string `json:"container"`
	Image     string `json:"image"`
	CPUs      int    `json:"cpus,omitempty"`
	Memory    string `json:"memory,omitempty"`
	// Network is the isolation's own container network; "" is the shared
	// default network an isolation created before the fence existed is on.
	Network string `json:"network,omitempty"`
	// Fence is "on" (the default: agents start only behind a verified
	// fence) or "off" (the user chose to let this isolation reach the Mac
	// and the LAN).
	Fence string `json:"fence"`
	// Env reaches every agent process in the isolation. It holds whatever
	// credentials the user put there (API keys), so the API reports names
	// only.
	Env       map[string]string `json:"-"`
	CreatedAt time.Time         `json:"created_at"`
}

type Agent struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Kind       driver.Kind    `json:"kind"`
	Isolation  string         `json:"isolation"`
	Workdir    string         `json:"workdir"`
	Repo       string         `json:"repo,omitempty"`
	Options    driver.Options `json:"options"`
	SessionID  string         `json:"session_id,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	ArchivedAt *time.Time     `json:"archived_at,omitempty"`
	// LastSrc is the newest shim log Seq whose events are committed.
	LastSrc int64 `json:"-"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS isolations (
	name       TEXT PRIMARY KEY,
	container  TEXT NOT NULL,
	image      TEXT NOT NULL,
	cpus       INTEGER NOT NULL DEFAULT 0,
	memory     TEXT NOT NULL DEFAULT '',
	env        TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agents (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	kind        TEXT NOT NULL,
	isolation   TEXT NOT NULL REFERENCES isolations(name),
	workdir     TEXT NOT NULL,
	repo        TEXT NOT NULL DEFAULT '',
	options     TEXT NOT NULL DEFAULT '{}',
	session_id  TEXT NOT NULL DEFAULT '',
	last_src    INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	archived_at TEXT
);
CREATE TABLE IF NOT EXISTS outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	agent      TEXT NOT NULL REFERENCES agents(id),
	text       TEXT NOT NULL,
	after      INTEGER NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	agent TEXT NOT NULL REFERENCES agents(id),
	id    INTEGER NOT NULL,
	src   INTEGER NOT NULL,
	body  TEXT NOT NULL,
	PRIMARY KEY (agent, id)
);
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One writer is all SQLite allows anyway; a single connection turns
	// contention into queueing instead of SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema + envSchema + accountSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	// Isolation environments can hold credentials: the store is the
	// owner's alone, whatever umask created it.
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(f, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			db.Close()
			return nil, fmt.Errorf("store: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Vacuum rewrites the database, so what was deleted from it is gone from
// the file too, not left in free pages; the checkpoint carries the rewrite
// from the write-ahead log into the file itself.
func (s *Store) Vacuum() error {
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return err
	}
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// columns added after a table first shipped, with their definitions.
var added = []struct{ table, column, def string }{
	{"isolations", "network", "TEXT NOT NULL DEFAULT ''"},
	{"isolations", "fence", "TEXT NOT NULL DEFAULT 'on'"},
	{"isolations", "base", "TEXT NOT NULL DEFAULT ''"},
}

func migrate(db *sql.DB) error {
	for _, a := range added {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, a.table)
		if err != nil {
			return err
		}
		have := false
		for rows.Next() {
			var name string
			rows.Scan(&name)
			have = have || name == a.column
		}
		rows.Close()
		if !have {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, a.table, a.column, a.def)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func (s *Store) PutIsolation(iso Isolation) error {
	env, _ := json.Marshal(iso.Env)
	if iso.Fence == "" {
		iso.Fence = "on"
	}
	_, err := s.db.Exec(`INSERT INTO isolations (name, container, image, cpus, memory, env, created_at, network, fence, base)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET container=excluded.container, image=excluded.image,
			cpus=excluded.cpus, memory=excluded.memory, env=excluded.env,
			network=excluded.network, fence=excluded.fence, base=excluded.base`,
		iso.Name, iso.Container, iso.Image, iso.CPUs, iso.Memory, string(env), ts(iso.CreatedAt), iso.Network, iso.Fence, iso.Base)
	return err
}

// BaseName is what the isolation's container, network and volumes are
// named after.
func (iso Isolation) BaseName() string {
	if iso.Base != "" {
		return iso.Base
	}
	return iso.Name
}

// ErrExists refuses a name another isolation has.
var ErrExists = errors.New("already exists")

// RenameIsolation moves an isolation, and everything that names it, to a
// new name in one transaction. Its container, network and volumes keep
// theirs: Base remembers it.
func (s *Store) RenameIsolation(from, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The rows that point at the isolation follow it within this
	// transaction; checked at commit, the references hold again.
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM isolations WHERE name = ?`, to).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("isolation %s %w", to, ErrExists)
	}
	res, err := tx.Exec(`UPDATE isolations SET name = ?, base = CASE WHEN base = '' THEN name ELSE base END WHERE name = ?`, to, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("isolation %s: %w", from, ErrNotFound)
	}
	for _, table := range []string{"agents", "exposures", "tokens", "account_links"} {
		if _, err := tx.Exec(`UPDATE `+table+` SET isolation = ? WHERE isolation = ?`, to, from); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanIsolation(row interface{ Scan(...any) error }) (Isolation, error) {
	var iso Isolation
	var env, created string
	err := row.Scan(&iso.Name, &iso.Container, &iso.Image, &iso.CPUs, &iso.Memory, &env, &created, &iso.Network, &iso.Fence, &iso.Base)
	if errors.Is(err, sql.ErrNoRows) {
		return iso, ErrNotFound
	}
	json.Unmarshal([]byte(env), &iso.Env)
	iso.CreatedAt = parseTS(created)
	return iso, err
}

const isolationCols = `name, container, image, cpus, memory, env, created_at, network, fence, base`

func (s *Store) Isolation(name string) (Isolation, error) {
	return scanIsolation(s.db.QueryRow(`SELECT `+isolationCols+` FROM isolations WHERE name = ?`, name))
}

func (s *Store) Isolations() ([]Isolation, error) {
	rows, err := s.db.Query(`SELECT ` + isolationCols + ` FROM isolations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Isolation
	for rows.Next() {
		iso, err := scanIsolation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, iso)
	}
	return out, rows.Err()
}

func (s *Store) DeleteIsolation(name string) error {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE isolation = ?`, name).Scan(&n)
	if n > 0 {
		return fmt.Errorf("isolation %s still has %d agent(s)", name, n)
	}
	_, err := s.db.Exec(`DELETE FROM isolations WHERE name = ?`, name)
	return err
}

func (s *Store) CreateAgent(a Agent) error {
	opts, _ := json.Marshal(a.Options)
	_, err := s.db.Exec(`INSERT INTO agents (id, name, kind, isolation, workdir, repo, options, session_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, string(a.Kind), a.Isolation, a.Workdir, a.Repo, string(opts), a.SessionID, ts(a.CreatedAt))
	return err
}

const agentCols = `id, name, kind, isolation, workdir, repo, options, session_id, last_src, created_at, archived_at`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var kind, opts, created string
	var archived sql.NullString
	err := row.Scan(&a.ID, &a.Name, &kind, &a.Isolation, &a.Workdir, &a.Repo, &opts, &a.SessionID,
		&a.LastSrc, &created, &archived)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.Kind = driver.Kind(kind)
	json.Unmarshal([]byte(opts), &a.Options)
	a.CreatedAt = parseTS(created)
	if archived.Valid {
		t := parseTS(archived.String)
		a.ArchivedAt = &t
	}
	return a, err
}

func (s *Store) Agent(id string) (Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, id))
}

func (s *Store) Agents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SetOptions(id string, o driver.Options) error {
	b, _ := json.Marshal(o)
	_, err := s.db.Exec(`UPDATE agents SET options = ? WHERE id = ?`, string(b), id)
	return err
}

func (s *Store) SetSessionID(id, sessionID string) error {
	_, err := s.db.Exec(`UPDATE agents SET session_id = ? WHERE id = ?`, sessionID, id)
	return err
}

func (s *Store) SetArchived(id string, at *time.Time) error {
	var v any
	if at != nil {
		v = ts(*at)
	}
	_, err := s.db.Exec(`UPDATE agents SET archived_at = ? WHERE id = ?`, v, id)
	return err
}

// Commit stores the events derived from shim record src and advances the
// agent's read position, atomically: after a crash the daemon re-derives
// exactly the events that were not committed. It returns evs with IDs.
func (s *Store) Commit(agentID string, src int64, evs []event.Event) ([]event.Event, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var last int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events WHERE agent = ?`, agentID).Scan(&last); err != nil {
		return nil, err
	}
	out := make([]event.Event, 0, len(evs))
	for _, e := range evs {
		if e.Ephemeral {
			continue
		}
		last++
		e.ID = last
		b, _ := json.Marshal(e)
		if _, err := tx.Exec(`INSERT INTO events (agent, id, src, body) VALUES (?, ?, ?, ?)`, agentID, e.ID, e.Src, string(b)); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if _, err := tx.Exec(`UPDATE agents SET last_src = ? WHERE id = ? AND last_src < ?`, src, agentID, src); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// Events returns stored events with ID > after, oldest first.
func (s *Store) Events(agentID string, after int64, limit int) ([]event.Event, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := s.db.Query(`SELECT body FROM events WHERE agent = ? AND id > ? ORDER BY id LIMIT ?`, agentID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var e event.Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Pending is a user message the daemon accepted and the agent's log does not
// show yet. After is the newest log Seq known when it was accepted; only a
// later delivery can be this one.
type Pending struct {
	ID    int64
	Text  string
	After int64
}

// AddPending records a message before anything tries to deliver it, so a
// daemon that dies in between still delivers it after restart.
func (s *Store) AddPending(agentID, text string, after int64) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO outbox (agent, text, after, created_at) VALUES (?, ?, ?, ?)`,
		agentID, text, after, ts(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Pending(agentID string) ([]Pending, error) {
	rows, err := s.db.Query(`SELECT id, text, after FROM outbox WHERE agent = ? ORDER BY id`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.Text, &p.After); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePending(id int64) error {
	_, err := s.db.Exec(`DELETE FROM outbox WHERE id = ?`, id)
	return err
}
