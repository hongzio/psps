package api

import (
	"encoding/json"
	"time"

	"github.com/hongzio/psps/internal/store"
)

// Job is a job for the operator: all psps keeps of it, with names beside
// the IDs. Result is the command's result.json whole.
type Job struct {
	store.Job
	Result          json.RawMessage `json:"result,omitempty"`
	Options         json.RawMessage `json:"options"`
	RunnerIsolation string          `json:"runner_isolation"`
	FromIsolation   string          `json:"from_isolation,omitempty"`
	AgentID         string          `json:"agent_id,omitempty"`
	TeamName        string          `json:"team_name,omitempty"`
	// Queued is its place among the runner's waiting jobs, from 1.
	Queued int `json:"queued,omitempty"`
}

// CallerJob is a job as the agent that asked for it sees it: Result is
// what the command left for its caller (result.json's "caller"), and
// nothing else of what it did or printed.
type CallerJob struct {
	ID          string          `json:"id"`
	Runner      string          `json:"runner"`
	Dir         string          `json:"dir"`
	Agent       string          `json:"agent,omitempty"`
	Status      string          `json:"status"`
	Error       string          `json:"error,omitempty"`
	Note        string          `json:"note,omitempty"`
	Queued      int             `json:"queued,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	StartedAt   *time.Time      `json:"started_at,omitempty"`
	EndedAt     *time.Time      `json:"ended_at,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`
	Exit        *int            `json:"exit,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
}

// PostJob asks for a job. Isolation is where Dir is, for the operator's
// own; an agent's is its own isolation.
type PostJob struct {
	Runner    string          `json:"runner,omitempty"`
	Dir       string          `json:"dir"`
	Options   json.RawMessage `json:"options,omitempty"`
	Isolation string          `json:"isolation,omitempty"`
}

// Runner is a runner as the agents it is open to see it.
type Runner struct {
	Name      string   `json:"name"`
	Default   bool     `json:"default,omitempty"`
	Minutes   int      `json:"minutes"`
	Memory    string   `json:"memory"`
	Pids      int      `json:"pids"`
	MaxLive   int      `json:"max_live"`
	MaxQueued int      `json:"max_queued"`
	MaxMB     int      `json:"max_mb"`
	MaxFiles  int      `json:"max_files"`
	Exclude   []string `json:"exclude"`
	Running   int      `json:"running"`
	Waiting   int      `json:"waiting"`
}

// JobHome is what an agent's job address answers: the runners open to its
// isolation and the jobs asked for from there.
type JobHome struct {
	Runners []Runner    `json:"runners"`
	Jobs    []CallerJob `json:"jobs"`
}
