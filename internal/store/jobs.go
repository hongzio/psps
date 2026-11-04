package store

import (
	"database/sql"
	"errors"
	"time"
)

// A job is a runner's command run in the runner's isolation on a copy of a
// directory, for a caller (internal/proxy/jobproxy). It runs apart from
// the daemon, so one that outlives a daemon is followed by the next.
const jobSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id           TEXT PRIMARY KEY,
	runner       TEXT NOT NULL,
	isolation    TEXT NOT NULL,
	caller       TEXT NOT NULL,
	agent        TEXT NOT NULL DEFAULT '',
	from_iso     TEXT NOT NULL DEFAULT '',
	team         TEXT NOT NULL DEFAULT '',
	dir          TEXT NOT NULL,
	why          TEXT NOT NULL DEFAULT '',
	options      TEXT NOT NULL,
	status       TEXT NOT NULL,
	error        TEXT NOT NULL DEFAULT '',
	note         TEXT NOT NULL DEFAULT '',
	requested_at TEXT NOT NULL,
	started_at   TEXT,
	ended_at     TEXT,
	limit_s      INTEGER NOT NULL DEFAULT 0,
	cancel_at    TEXT,
	exit         INTEGER,
	result       TEXT,
	output       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status ON jobs(status, requested_at);
`

// Job statuses: queued until the runner has room, starting while the copy
// is made, running, and then one of the ends.
const (
	JobQueued    = "queued"
	JobStarting  = "starting"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
	JobLost      = "lost"
)

// JobEnded is whether status is one a job ends in.
func JobEnded(status string) bool {
	switch status {
	case JobDone, JobFailed, JobCancelled, JobLost:
		return true
	}
	return false
}

// Job callers.
const (
	CallerAgent    = "agent"
	CallerSystem   = "system"
	CallerOperator = "operator"
)

// A Job is one, as kept. Isolation is the runner's isolation, by ID, as it
// was asked for; From the isolation the copy was made from. Result is the
// command's result.json as it was, Output the end of what it printed.
type Job struct {
	ID        string `json:"id"`
	Runner    string `json:"runner"`
	Isolation string `json:"isolation"`
	Caller    string `json:"caller"`
	Agent     string `json:"agent,omitempty"`
	From      string `json:"from,omitempty"`
	Team      string `json:"team,omitempty"`
	Dir       string `json:"dir"`
	// Why is what psps asked for it for (a team's eval), for its own.
	Why         string        `json:"why,omitempty"`
	Options     string        `json:"options"`
	Status      string        `json:"status"`
	Error       string        `json:"error,omitempty"`
	Note        string        `json:"note,omitempty"`
	RequestedAt time.Time     `json:"requested_at"`
	StartedAt   *time.Time    `json:"started_at,omitempty"`
	EndedAt     *time.Time    `json:"ended_at,omitempty"`
	Limit       time.Duration `json:"limit"`
	CancelAt    *time.Time    `json:"cancel_at,omitempty"`
	Exit        *int          `json:"exit,omitempty"`
	Result      string        `json:"result,omitempty"`
	Output      string        `json:"output,omitempty"`
}

const jobCols = `id, runner, isolation, caller, agent, from_iso, team, dir, why, options, status, error, note, requested_at, started_at, ended_at, limit_s, cancel_at, exit, result, output`

func optTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// PutJob keeps j as it is now.
func (s *Store) PutJob(j Job) error {
	var exit, result any
	if j.Exit != nil {
		exit = *j.Exit
	}
	if j.Result != "" {
		result = j.Result
	}
	_, err := s.db.Exec(`INSERT INTO jobs (`+jobCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET isolation=excluded.isolation, status=excluded.status, error=excluded.error, note=excluded.note, started_at=excluded.started_at,
			ended_at=excluded.ended_at, limit_s=excluded.limit_s, cancel_at=excluded.cancel_at, exit=excluded.exit, result=excluded.result, output=excluded.output`,
		j.ID, j.Runner, j.Isolation, j.Caller, j.Agent, j.From, j.Team, j.Dir, j.Why, j.Options, j.Status, j.Error, j.Note,
		ts(j.RequestedAt), optTS(j.StartedAt), optTS(j.EndedAt), int64(j.Limit/time.Second), optTS(j.CancelAt), exit, result, j.Output)
	return err
}

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (Job, error) {
	var j Job
	var req string
	var started, ended, cancel, result sql.NullString
	var exit sql.NullInt64
	var limit int64
	if err := r.Scan(&j.ID, &j.Runner, &j.Isolation, &j.Caller, &j.Agent, &j.From, &j.Team, &j.Dir, &j.Why, &j.Options, &j.Status, &j.Error, &j.Note,
		&req, &started, &ended, &limit, &cancel, &exit, &result, &j.Output); err != nil {
		return j, err
	}
	j.RequestedAt, j.Limit = parseTS(req), time.Duration(limit)*time.Second
	for _, p := range []struct {
		s  sql.NullString
		to **time.Time
	}{{started, &j.StartedAt}, {ended, &j.EndedAt}, {cancel, &j.CancelAt}} {
		if p.s.Valid {
			t := parseTS(p.s.String)
			*p.to = &t
		}
	}
	if exit.Valid {
		n := int(exit.Int64)
		j.Exit = &n
	}
	j.Result = result.String
	return j, nil
}

// Job is the job id names.
func (s *Store) Job(id string) (Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	return j, err
}

// Jobs are the jobs kept, newest first: all of them, or with live those
// that have not ended.
func (s *Store) Jobs(live bool) ([]Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs`
	if live {
		q += ` WHERE status IN ('queued', 'starting', 'running')`
	}
	rows, err := s.db.Query(q + ` ORDER BY requested_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// PruneJobs removes the jobs that ended before t.
func (s *Store) PruneJobs(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM jobs WHERE ended_at IS NOT NULL AND ended_at < ?`, ts(t))
	return err
}
