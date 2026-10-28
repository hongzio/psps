package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// An Env is one named thing a credential proxy can serve: "the prod Datadog
// org", "the readonly role on the prod database", "the work GitHub
// account". Envs are defined once, for every isolation; an Exposure decides
// which of them one isolation sees. Nothing here is secret — the env's
// credential lives in the credentials file under the same (service, name).
type Env struct {
	Service   string          `json:"service"`
	Name      string          `json:"name"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// An Exposure turns a service on for one isolation and names the envs it
// serves there, in order. Envs may be empty: git then serves public
// repositories only, and the other proxies refuse with the fix.
type Exposure struct {
	IsolationID string   `json:"isolation_id"`
	Service     string   `json:"service"`
	Envs        []string `json:"envs"`
	Default     string   `json:"default_env,omitempty"`
	// Settings are the service's per-isolation switches (git's public
	// reads and API side, for instance).
	Settings json.RawMessage `json:"settings,omitempty"`
}

const envSchema = `
CREATE TABLE IF NOT EXISTS envs (
	service    TEXT NOT NULL,
	name       TEXT NOT NULL,
	config     TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (service, name)
);
CREATE TABLE IF NOT EXISTS exposures (
	isolation   TEXT NOT NULL REFERENCES isolations(id) ON DELETE CASCADE,
	service     TEXT NOT NULL,
	envs        TEXT NOT NULL DEFAULT '[]',
	default_env TEXT NOT NULL DEFAULT '',
	settings    TEXT NOT NULL DEFAULT '{}',
	PRIMARY KEY (isolation, service)
);
CREATE TABLE IF NOT EXISTS tokens (
	isolation TEXT NOT NULL REFERENCES isolations(id) ON DELETE CASCADE,
	service   TEXT NOT NULL,
	env       TEXT NOT NULL,
	token     TEXT NOT NULL,
	PRIMARY KEY (isolation, service, env)
);
`

func (s *Store) PutEnv(e Env) error {
	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if len(e.Config) == 0 {
		e.Config = json.RawMessage("{}")
	}
	_, err := s.db.Exec(`INSERT INTO envs (service, name, config, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(service, name) DO UPDATE SET config=excluded.config, updated_at=excluded.updated_at`,
		e.Service, e.Name, string(e.Config), ts(e.CreatedAt), ts(now))
	return err
}

func scanEnv(row interface{ Scan(...any) error }) (Env, error) {
	var e Env
	var cfg, created, updated string
	err := row.Scan(&e.Service, &e.Name, &cfg, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	e.Config = json.RawMessage(cfg)
	e.CreatedAt, e.UpdatedAt = parseTS(created), parseTS(updated)
	return e, err
}

func (s *Store) Env(service, name string) (Env, error) {
	e, err := scanEnv(s.db.QueryRow(`SELECT service, name, config, created_at, updated_at FROM envs WHERE service = ? AND name = ?`, service, name))
	if errors.Is(err, ErrNotFound) {
		return e, fmt.Errorf("no %s env named %s: %w", service, name, ErrNotFound)
	}
	return e, err
}

// Envs lists one service's envs, or every env when service is "".
func (s *Store) Envs(service string) ([]Env, error) {
	rows, err := s.db.Query(`SELECT service, name, config, created_at, updated_at FROM envs
		WHERE ? = '' OR service = ? ORDER BY service, name`, service, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Exposing lists the isolations whose exposure of service names env, by ID.
func (s *Store) Exposing(service, env string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT x.isolation FROM exposures x, json_each(x.envs) j
		WHERE x.service = ? AND j.value = ? ORDER BY x.isolation`, service, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ErrExposed refuses deleting an env an isolation still serves. Isolations
// are their names.
type ErrExposed struct {
	Service, Env string
	Isolations   []string
}

func (e *ErrExposed) Error() string {
	return fmt.Sprintf("%s env %s is exposed to %s; unexpose it there first, or pass --force to drop it everywhere",
		e.Service, e.Env, strings.Join(e.Isolations, ", "))
}

// DeleteEnv removes an env. One still exposed is refused unless force, which
// drops it from every exposure in the same transaction, so no isolation is
// left naming an env that no longer exists.
func (s *Store) DeleteEnv(service, name string, force bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT x.isolation, i.name, x.envs, x.default_env FROM exposures x JOIN isolations i ON i.id = x.isolation,
		json_each(x.envs) j WHERE x.service = ? AND j.value = ? ORDER BY i.name`, service, name)
	if err != nil {
		return err
	}
	type hit struct {
		iso, name, def string
		envs           []string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		var envs string
		rows.Scan(&h.iso, &h.name, &envs, &h.def)
		json.Unmarshal([]byte(envs), &h.envs)
		hits = append(hits, h)
	}
	rows.Close()
	if len(hits) > 0 && !force {
		e := &ErrExposed{Service: service, Env: name}
		for _, h := range hits {
			e.Isolations = append(e.Isolations, h.name)
		}
		return e
	}
	for _, h := range hits {
		var kept []string
		for _, n := range h.envs {
			if n != name {
				kept = append(kept, n)
			}
		}
		if h.def == name {
			h.def = ""
			if len(kept) > 0 {
				h.def = kept[0]
			}
		}
		b, _ := json.Marshal(nonNil(kept))
		if _, err := tx.Exec(`UPDATE exposures SET envs = ?, default_env = ? WHERE isolation = ? AND service = ?`,
			string(b), h.def, h.iso, service); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM tokens WHERE isolation = ? AND service = ? AND env = ?`, h.iso, service, name); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`DELETE FROM envs WHERE service = ? AND name = ?`, service, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no %s env named %s: %w", service, name, ErrNotFound)
	}
	return tx.Commit()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Store) PutExposure(x Exposure) error {
	envs, _ := json.Marshal(nonNil(x.Envs))
	settings := x.Settings
	if len(settings) == 0 {
		settings = json.RawMessage("{}")
	}
	_, err := s.db.Exec(`INSERT INTO exposures (isolation, service, envs, default_env, settings) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(isolation, service) DO UPDATE SET envs=excluded.envs, default_env=excluded.default_env, settings=excluded.settings`,
		x.IsolationID, x.Service, string(envs), x.Default, string(settings))
	return err
}

// DeleteExposure turns a service off for an isolation, and with it the
// workspace tokens that routed to it.
func (s *Store) DeleteExposure(iso, service string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM exposures WHERE isolation = ? AND service = ?`, iso, service); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tokens WHERE isolation = ? AND service = ?`, iso, service); err != nil {
		return err
	}
	return tx.Commit()
}

func scanExposure(row interface{ Scan(...any) error }) (Exposure, error) {
	var x Exposure
	var envs, settings string
	err := row.Scan(&x.IsolationID, &x.Service, &envs, &x.Default, &settings)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	json.Unmarshal([]byte(envs), &x.Envs)
	x.Envs = nonNil(x.Envs)
	x.Settings = json.RawMessage(settings)
	return x, err
}

func (s *Store) Exposure(iso, service string) (Exposure, error) {
	return scanExposure(s.db.QueryRow(`SELECT isolation, service, envs, default_env, settings FROM exposures
		WHERE isolation = ? AND service = ?`, iso, service))
}

func (s *Store) Exposures(iso string) ([]Exposure, error) {
	rows, err := s.db.Query(`SELECT isolation, service, envs, default_env, settings FROM exposures
		WHERE isolation = ? ORDER BY service`, iso)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Exposure
	for rows.Next() {
		x, err := scanExposure(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Tokens returns the workspace tokens for one isolation's service, keyed by
// env ("" for a service with one token for the whole isolation).
func (s *Store) Tokens(iso, service string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT env, token FROM tokens WHERE isolation = ? AND service = ?`, iso, service)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var env, tok string
		rows.Scan(&env, &tok)
		out[env] = tok
	}
	return out, rows.Err()
}

// EnsureTokens makes the isolation's tokens for service cover exactly keys:
// existing ones survive (a planted workspace variable keeps working),
// missing ones are minted, and the rest are dropped so a revoked exposure
// stops routing.
func (s *Store) EnsureTokens(iso, service string, keys []string) (map[string]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	have := map[string]string{}
	rows, err := tx.Query(`SELECT env, token FROM tokens WHERE isolation = ? AND service = ?`, iso, service)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var env, tok string
		rows.Scan(&env, &tok)
		have[env] = tok
	}
	rows.Close()
	want := map[string]bool{}
	out := map[string]string{}
	for _, k := range keys {
		want[k] = true
		if tok, ok := have[k]; ok {
			out[k] = tok
			continue
		}
		tok := newToken()
		if _, err := tx.Exec(`INSERT INTO tokens (isolation, service, env, token) VALUES (?, ?, ?, ?)`, iso, service, k, tok); err != nil {
			return nil, err
		}
		out[k] = tok
	}
	for k := range have {
		if !want[k] {
			if _, err := tx.Exec(`DELETE FROM tokens WHERE isolation = ? AND service = ? AND env = ?`, iso, service, k); err != nil {
				return nil, err
			}
		}
	}
	return out, tx.Commit()
}

func newToken() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
