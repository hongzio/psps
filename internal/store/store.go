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
	// ID is what an isolation is to gero, for good: every row that belongs
	// to it names it by ID, and its container, network and volumes are
	// named after it. Name is what people call it, and can change.
	ID        string `json:"id"`
	Name      string `json:"name"`
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
	// ID is the key gero gave it: its workspace, its log and every row
	// about it are named after it.
	ID string `json:"id"`
	// Handle is the ID the operator gave it, if any: unique among all
	// agents, archived ones included, as theirs or as their key.
	Handle string      `json:"handle,omitempty"`
	Name   string      `json:"name"`
	Kind   driver.Kind `json:"kind"`
	// IsolationID is the ID of the isolation it runs in.
	IsolationID string         `json:"isolation_id"`
	Workdir     string         `json:"workdir"`
	Repo        string         `json:"repo,omitempty"`
	Options     driver.Options `json:"options"`
	SessionID   string         `json:"session_id,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	ArchivedAt  *time.Time     `json:"archived_at,omitempty"`
	// LastSrc is the newest shim log Seq whose events are committed.
	LastSrc int64 `json:"-"`
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS isolations (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	container  TEXT NOT NULL,
	image      TEXT NOT NULL,
	cpus       INTEGER NOT NULL DEFAULT 0,
	memory     TEXT NOT NULL DEFAULT '',
	env        TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agents (
	id          TEXT PRIMARY KEY,
	handle      TEXT,
	name        TEXT NOT NULL,
	kind        TEXT NOT NULL,
	isolation   TEXT NOT NULL REFERENCES isolations(id),
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
	{"agents", "handle", "TEXT"},
}

func hasColumn(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		rows.Scan(&name)
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func migrate(db *sql.DB) error {
	for _, a := range added {
		have, err := hasColumn(db, a.table, a.column)
		if err != nil {
			return err
		}
		if !have {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, a.table, a.column, a.def)); err != nil {
				return err
			}
		}
	}
	// NULL is no handle; any number of agents have none.
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS agents_handle ON agents(handle)`); err != nil {
		return err
	}
	return migrateIsolationIDs(db)
}

// migrateIsolationIDs gives isolations from before IDs one: the name their
// container, network and volumes are named after, which is their name
// unless they were renamed (a base column remembered it then). The column
// that was the name becomes the ID, so every row pointing at an isolation,
// foreign keys included, keeps pointing at it; the name is a column of its
// own from then on.
func migrateIsolationIDs(db *sql.DB) error {
	if have, err := hasColumn(db, "isolations", "id"); err != nil || have {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}
	hasBase, err := hasColumn(tx, "isolations", "base")
	if err != nil {
		return err
	}
	moves := map[string]string{} // name -> base, for renamed isolations
	if hasBase {
		rows, err := tx.Query(`SELECT name, base FROM isolations WHERE base != '' AND base != name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name, base string
			rows.Scan(&name, &base)
			moves[name] = base
		}
		rows.Close()
	}
	for _, q := range []string{
		`ALTER TABLE isolations RENAME COLUMN name TO id`,
		`ALTER TABLE isolations ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`UPDATE isolations SET name = id`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	// A renamed isolation's ID is its base. Moved in two steps, through a
	// value no name can be: a base can be another isolation's name, and so
	// for now its ID.
	move := func(from, to string) error {
		if _, err := tx.Exec(`UPDATE isolations SET id = ? WHERE id = ?`, to, from); err != nil {
			return err
		}
		for _, table := range []string{"agents", "exposures", "tokens", "account_links"} {
			if _, err := tx.Exec(`UPDATE `+table+` SET isolation = ? WHERE isolation = ?`, to, from); err != nil {
				return err
			}
		}
		return nil
	}
	for name := range moves {
		if err := move(name, "#"+name); err != nil {
			return err
		}
	}
	for name, base := range moves {
		if err := move("#"+name, base); err != nil {
			return err
		}
	}
	if hasBase {
		if _, err := tx.Exec(`ALTER TABLE isolations DROP COLUMN base`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX isolations_name ON isolations(name)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// ErrExists refuses a name another isolation has, or an ID another agent
// has.
var ErrExists = errors.New("already exists")

func (s *Store) PutIsolation(iso Isolation) error {
	if iso.ID == "" || iso.Name == "" {
		return errors.New("store: an isolation needs an ID and a name")
	}
	env, _ := json.Marshal(iso.Env)
	if iso.Fence == "" {
		iso.Fence = "on"
	}
	_, err := s.db.Exec(`INSERT INTO isolations (id, name, container, image, cpus, memory, env, created_at, network, fence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, container=excluded.container, image=excluded.image,
			cpus=excluded.cpus, memory=excluded.memory, env=excluded.env,
			network=excluded.network, fence=excluded.fence`,
		iso.ID, iso.Name, iso.Container, iso.Image, iso.CPUs, iso.Memory, string(env), ts(iso.CreatedAt), iso.Network, iso.Fence)
	return err
}

func scanIsolation(row interface{ Scan(...any) error }) (Isolation, error) {
	var iso Isolation
	var env, created string
	err := row.Scan(&iso.ID, &iso.Name, &iso.Container, &iso.Image, &iso.CPUs, &iso.Memory, &env, &created, &iso.Network, &iso.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return iso, ErrNotFound
	}
	json.Unmarshal([]byte(env), &iso.Env)
	iso.CreatedAt = parseTS(created)
	return iso, err
}

const isolationCols = `id, name, container, image, cpus, memory, env, created_at, network, fence`

// Isolation is the isolation with that ID.
func (s *Store) Isolation(id string) (Isolation, error) {
	return scanIsolation(s.db.QueryRow(`SELECT `+isolationCols+` FROM isolations WHERE id = ?`, id))
}

// IsolationNamed is the isolation people call name.
func (s *Store) IsolationNamed(name string) (Isolation, error) {
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

func (s *Store) DeleteIsolation(id string) error {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM agents WHERE isolation = ?`, id).Scan(&n)
	if n > 0 {
		return fmt.Errorf("%d agent(s) still belong to it", n)
	}
	_, err := s.db.Exec(`DELETE FROM isolations WHERE id = ?`, id)
	return err
}

// RenameIsolation gives an isolation a new name. Nothing else names it by
// name, so nothing else changes.
func (s *Store) RenameIsolation(id, to string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM isolations WHERE name = ? AND id != ?`, to, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("isolation %s %w", to, ErrExists)
	}
	res, err := tx.Exec(`UPDATE isolations SET name = ? WHERE id = ?`, to, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("isolation %s: %w", id, ErrNotFound)
	}
	return tx.Commit()
}

// CreateAgent stores a new agent; neither its key nor its handle may be
// any agent's key or handle.
func (s *Store) CreateAgent(a Agent) error {
	opts, _ := json.Marshal(a.Options)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, ref := range []string{a.ID, a.Handle} {
		if err := refFree(tx, ref, ""); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO agents (id, handle, name, kind, isolation, workdir, repo, options, session_id, created_at)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Handle, a.Name, string(a.Kind), a.IsolationID, a.Workdir, a.Repo, string(opts), a.SessionID, ts(a.CreatedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// refFree refuses ref if an agent other than except has it as its key or
// its handle. "" is no ref.
func refFree(q interface {
	QueryRow(string, ...any) *sql.Row
}, ref, except string) error {
	if ref == "" {
		return nil
	}
	var other string
	err := q.QueryRow(`SELECT id FROM agents WHERE (id = ? OR handle = ?) AND id != ?`, ref, ref, except).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("agent ID %s %w (agent %s)", ref, ErrExists, other)
}

// RefFree refuses ref, with ErrExists, if an agent has it as its key or its
// handle.
func (s *Store) RefFree(ref string) error { return refFree(s.db, ref, "") }

// AgentByHandle is the agent the operator gave the ID handle.
func (s *Store) AgentByHandle(handle string) (Agent, error) {
	return scanAgent(s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE handle = ?`, handle))
}

const agentCols = `id, COALESCE(handle, ''), name, kind, isolation, workdir, repo, options, session_id, last_src, created_at, archived_at`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var kind, opts, created string
	var archived sql.NullString
	err := row.Scan(&a.ID, &a.Handle, &a.Name, &kind, &a.IsolationID, &a.Workdir, &a.Repo, &opts, &a.SessionID,
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

// DeleteAgent removes an agent and everything stored for it.
func (s *Store) DeleteAgent(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM events WHERE agent = ?`, `DELETE FROM outbox WHERE agent = ?`, `DELETE FROM agents WHERE id = ?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) SetOptions(id string, o driver.Options) error {
	b, _ := json.Marshal(o)
	_, err := s.db.Exec(`UPDATE agents SET options = ? WHERE id = ?`, string(b), id)
	return err
}

// RenameAgent changes what is set of an agent's name and handle at once:
// both, or neither if its handle is taken. A handle of "" takes it away.
func (s *Store) RenameAgent(id string, name, handle *string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if handle != nil {
		if err := refFree(tx, *handle, id); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`UPDATE agents SET name = COALESCE(?, name), handle = CASE WHEN ? THEN NULLIF(?, '') ELSE handle END WHERE id = ?`,
		name, handle != nil, deref(handle), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent %s: %w", id, ErrNotFound)
	}
	return tx.Commit()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (s *Store) SetAgentName(id, name string) error {
	res, err := s.db.Exec(`UPDATE agents SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("agent %s: %w", id, ErrNotFound)
	}
	return nil
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
