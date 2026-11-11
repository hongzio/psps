package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hongzio/psps/internal/driver"
)

// An agent CLI's models are the ones it said it runs, last time one of its
// processes did, in an isolation: what a new agent's model is offered from,
// before it has a process to ask. They differ by the CLI's version and the
// login it runs on, so they are kept per isolation.
const modelSchema = `
CREATE TABLE IF NOT EXISTS cli_models (
	kind      TEXT NOT NULL,
	isolation TEXT NOT NULL,
	models    TEXT NOT NULL,
	seen_at   INTEGER NOT NULL, -- unix nanoseconds
	PRIMARY KEY (kind, isolation)
);
`

// SetCLIModels keeps models as the ones kind's CLI said it runs in iso at
// at, unless it was already said to run others since.
func (s *Store) SetCLIModels(kind driver.Kind, iso string, models []driver.Choice, at time.Time) error {
	b, err := json.Marshal(models)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO cli_models (kind, isolation, models, seen_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (kind, isolation) DO UPDATE SET models = excluded.models, seen_at = excluded.seen_at
		WHERE excluded.seen_at >= cli_models.seen_at`,
		string(kind), iso, string(b), at.UnixNano())
	return err
}

// CLIModels are the models kind's CLI last said it runs in iso, and from
// which isolation they are: iso's own, or else those last said in any; none
// when it never said.
func (s *Store) CLIModels(kind driver.Kind, iso string) (models []driver.Choice, from string, err error) {
	var raw string
	err = s.db.QueryRow(`SELECT models, isolation FROM cli_models WHERE kind = ?
		ORDER BY isolation = ? DESC, seen_at DESC LIMIT 1`, string(kind), iso).Scan(&raw, &from)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	return models, from, json.Unmarshal([]byte(raw), &models)
}
