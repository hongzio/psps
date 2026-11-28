package store

import (
	"time"
)

// A Route is a direction files may be transferred in, from one isolation
// to another, which the operator allowed: an agent may ask for a transfer
// along it, and the operator still approves each. None is allowed until
// the operator says so, and one does not run back.
type Route struct {
	// From and To are the isolations' IDs.
	From      string    `json:"from"`
	To        string    `json:"to"`
	CreatedAt time.Time `json:"created_at"`
}

const routeSchema = `
CREATE TABLE IF NOT EXISTS transfer_routes (
	from_iso   TEXT NOT NULL,
	to_iso     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (from_iso, to_iso)
);
`

// AllowRoute allows transfers from isolation from to isolation to. It is
// whether it was not allowed already.
func (s *Store) AllowRoute(from, to string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO transfer_routes (from_iso, to_iso, created_at) VALUES (?, ?, ?)`, from, to, ts(time.Now()))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DenyRoute takes the route from from to to away. It is whether there was
// one.
func (s *Store) DenyRoute(from, to string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM transfer_routes WHERE from_iso = ? AND to_iso = ?`, from, to)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RouteAllowed is whether files may be transferred from from to to.
func (s *Store) RouteAllowed(from, to string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM transfer_routes WHERE from_iso = ? AND to_iso = ?`, from, to).Scan(&n)
	return n > 0, err
}

// Routes are the routes allowed, oldest first.
func (s *Store) Routes() ([]Route, error) {
	rows, err := s.db.Query(`SELECT from_iso, to_iso, created_at FROM transfer_routes ORDER BY created_at, from_iso, to_iso`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Route{}
	for rows.Next() {
		var r Route
		var created string
		if err := rows.Scan(&r.From, &r.To, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}
