package store

import (
	"database/sql"
	"errors"
)

// An agent's snapshot is its driver's fold of its shim log up to seq, with
// what the daemon keeps beside it: attaching again, the daemon goes on
// from there instead of reading the log from its start. It is the
// daemon's to read; the store keeps it as it is given.
const snapshotSchema = `
CREATE TABLE IF NOT EXISTS snapshots (
	agent TEXT PRIMARY KEY,
	seq   INTEGER NOT NULL,
	data  BLOB NOT NULL
);
`

// PutSnapshot keeps data as agent's snapshot at seq, in place of the one
// it had.
func (s *Store) PutSnapshot(agent string, seq int64, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO snapshots (agent, seq, data) VALUES (?, ?, ?)
		ON CONFLICT (agent) DO UPDATE SET seq = excluded.seq, data = excluded.data`, agent, seq, data)
	return err
}

// Snapshot is agent's snapshot and its seq; none (nil) when it has none.
func (s *Store) Snapshot(agent string) (seq int64, data []byte, err error) {
	err = s.db.QueryRow(`SELECT seq, data FROM snapshots WHERE agent = ?`, agent).Scan(&seq, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return seq, data, err
}

// DropSnapshot forgets agent's snapshot: its log is read from the start.
func (s *Store) DropSnapshot(agent string) error {
	_, err := s.db.Exec(`DELETE FROM snapshots WHERE agent = ?`, agent)
	return err
}
