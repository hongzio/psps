package store

import (
	"database/sql"
	"errors"
	"time"

	"github.com/hongzio/gero/internal/driver"
)

// An Account is an agent CLI credential defined once and used by any
// number of isolations, one per CLI in each. Nothing here is secret: the
// credential is in the credentials file under [account <name>].
type Account struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
}

const accountSchema = `
CREATE TABLE IF NOT EXISTS accounts (
	name       TEXT PRIMARY KEY,
	kind       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS account_links (
	isolation TEXT NOT NULL REFERENCES isolations(id) ON DELETE CASCADE,
	tool      TEXT NOT NULL,
	account   TEXT NOT NULL REFERENCES accounts(name),
	PRIMARY KEY (isolation, tool)
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func (s *Store) PutAccount(a Account) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(`INSERT INTO accounts (name, kind, created_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET kind=excluded.kind`, a.Name, a.Kind, ts(a.CreatedAt))
	return err
}

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var created string
	err := row.Scan(&a.Name, &a.Kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.CreatedAt = parseTS(created)
	return a, err
}

func (s *Store) Account(name string) (Account, error) {
	return scanAccount(s.db.QueryRow(`SELECT name, kind, created_at FROM accounts WHERE name = ?`, name))
}

func (s *Store) Accounts() ([]Account, error) {
	rows, err := s.db.Query(`SELECT name, kind, created_at FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteAccount removes the account and every isolation's use of it.
func (s *Store) DeleteAccount(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM account_links WHERE account = ?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM accounts WHERE name = ?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// UseAccount sets the account iso's tool uses; "" removes it.
func (s *Store) UseAccount(iso string, tool driver.Kind, account string) error {
	if account == "" {
		_, err := s.db.Exec(`DELETE FROM account_links WHERE isolation = ? AND tool = ?`, iso, string(tool))
		return err
	}
	_, err := s.db.Exec(`INSERT INTO account_links (isolation, tool, account) VALUES (?, ?, ?)
		ON CONFLICT(isolation, tool) DO UPDATE SET account=excluded.account`, iso, string(tool), account)
	return err
}

// AccountsUsed is the account each of iso's tools uses.
func (s *Store) AccountsUsed(iso string) (map[driver.Kind]string, error) {
	rows, err := s.db.Query(`SELECT tool, account FROM account_links WHERE isolation = ?`, iso)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[driver.Kind]string{}
	for rows.Next() {
		var tool, account string
		if err := rows.Scan(&tool, &account); err != nil {
			return nil, err
		}
		out[driver.Kind(tool)] = account
	}
	return out, rows.Err()
}

// AccountUsers are the isolations using the account, by ID.
func (s *Store) AccountUsers(name string) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT isolation FROM account_links WHERE account = ? ORDER BY isolation`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var iso string
		if err := rows.Scan(&iso); err != nil {
			return nil, err
		}
		out = append(out, iso)
	}
	return out, rows.Err()
}

// Meta and SetMeta keep small facts about the store itself, such as which
// one-time moves of data have been made.
func (s *Store) Meta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}
