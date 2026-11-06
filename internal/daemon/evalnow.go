package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// A team's eval as the operator sees it now: under way (queued on its
// runner or running), waiting on the round, or due. Due is a round that
// ended with no eval started: teamCheck starts one at once, so a minute of
// it is something wrong, which the sidebar and the log say. It is watched
// apart from teamCheck, from what can be seen of the run, so a teamCheck
// that never gets there shows.

// dueAfter is how long a round may have ended with no eval before it is
// something wrong.
const dueAfter = time.Minute

// roundOver is whether l's round has ended with no eval under way: an
// agent's turn ended since the last eval, nothing of the team is at work
// and the lead has heard everything and is idle.
func (m *Manager) roundOver(l store.Lead) bool {
	if l.Stopped != "" || !l.Settings.HasEval() || !l.Dirty || m.evalRunning(l.Team) || m.childrenBusy(l.Line) > 0 {
		return false
	}
	r := m.lineRuntime(l.Line)
	if r == nil || m.teamsOff(l) != "" {
		return true
	}
	if n, err := m.store.TeamNewsWaiting(l.Team); err != nil || n > 0 {
		return false
	}
	return r.free()
}

// watchDue keeps since when each team's round has been over with no
// eval, and says once in the log when that has gone on past dueAfter.
func (m *Manager) watchDue(now time.Time) {
	leads, err := m.store.Leads()
	if err != nil {
		return
	}
	over := map[string]bool{}
	for _, l := range leads {
		over[l.Team] = m.roundOver(l)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for team, since := range m.dueSince {
		if !over[team] {
			delete(m.dueSince, team)
			delete(m.dueTold, team)
		} else if now.Sub(since) >= dueAfter && !m.dueTold[team] {
			m.dueTold[team] = true
			m.log.Printf("team %s: the round ended %s ago and no eval has started", team, now.Sub(since).Round(time.Second))
		}
	}
	for team, o := range over {
		if _, ok := m.dueSince[team]; o && !ok {
			m.dueSince[team] = now
		}
	}
}

// evalNow is l's eval as it is now; evals are the team's so far.
func (m *Manager) evalNow(l store.Lead, evals []store.Eval) *api.EvalNow {
	v := &api.EvalNow{State: "none", Usual: usualTook(evals), Limit: time.Duration(l.Settings.EvalMinutes) * time.Minute}
	if len(evals) > 0 {
		v.Number = evals[len(evals)-1].ID + 1
	} else {
		v.Number = 1
	}
	if l.Settings.EvalJob != "" {
		v.Runner = l.Settings.EvalJob
	}
	if m.evalRunning(l.Team) {
		v.State = "running"
		runs, _ := m.store.EvalRuns()
		i := slices.IndexFunc(runs, func(r store.EvalRun) bool { return r.Team == l.Team })
		if i < 0 {
			return v
		}
		run := runs[i]
		v.Why, v.Since, v.Started = run.Why, run.At, run.At
		if run.Limit > 0 {
			v.Limit = run.Limit
		}
		if run.Isolation != "" {
			v.Isolation = m.isoName(run.Isolation)
		}
		if l.Settings.EvalJob != "" && run.Job == "" {
			v.State = "queued"
			return v
		}
		if run.Job == "" {
			return v
		}
		v.Job, v.Stoppable = run.Job, true
		j, err := m.store.Job(run.Job)
		if err != nil {
			return v
		}
		v.Runner, v.Limit = j.Runner, j.Limit
		v.Note, _, _ = strings.Cut(j.Note, ";")
		switch {
		case j.Status == store.JobQueued:
			v.State, v.Started = "queued", time.Time{}
			v.Place = m.queuePlaces()[j.ID]
		case j.StartedAt != nil:
			v.Started = *j.StartedAt
		}
		return v
	}
	if l.Stopped != "" || !l.Settings.HasEval() {
		return v
	}
	m.mu.Lock()
	since, due := m.dueSince[l.Team]
	m.mu.Unlock()
	switch {
	case due:
		v.State, v.Since = "due", since
	case l.Dirty:
		v.State, v.Waiting = "waiting", m.runDoing(l)
	default:
		v.State = "idle"
	}
	return v
}

// usualTook is the median of what the last five evals that ran took.
func usualTook(evals []store.Eval) time.Duration {
	var ts []time.Duration
	for i := len(evals) - 1; i >= 0 && len(ts) < 5; i-- {
		if !evals[i].NotRun && evals[i].Took > 0 {
			ts = append(ts, evals[i].Took)
		}
	}
	if len(ts) == 0 {
		return 0
	}
	slices.Sort(ts)
	return ts[len(ts)/2]
}

// lastEval is the latest of evals, as a team's head shows it.
func lastEval(evals []store.Eval, l store.Lead) *api.EvalBrief {
	if len(evals) == 0 {
		return nil
	}
	e := evals[len(evals)-1]
	best, _ := bestOf(evals, l)
	b := &api.EvalBrief{ID: e.ID, At: e.At, Took: e.Took, Score: e.Score, NotRun: e.NotRun, NewBest: best != nil && best.ID == e.ID}
	if e.Score == nil {
		lines := strings.Split(strings.TrimSpace(e.Output), "\n")
		b.Note = tail(lines[len(lines)-1], 160)
	}
	return b
}

// StopEval stops a team's eval under way, for its operator: one that is a
// job of a runner, which is cancelled; the eval then counts as not run.
func (m *Manager) StopEval(teamRef string) error {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return err
	}
	if !m.evalRunning(t.ID) {
		return httpError{http.StatusConflict, fmt.Errorf("team %q has no eval under way", t.Name)}
	}
	runs, err := m.store.EvalRuns()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(runs, func(r store.EvalRun) bool { return r.Team == t.ID })
	switch {
	case i < 0:
		return httpError{http.StatusConflict, fmt.Errorf("team %q has no eval under way", t.Name)}
	case runs[i].Job == "":
		return httpError{http.StatusConflict, errors.New("this eval is a command, not a runner's job: it stops at its limit")}
	}
	_, err = m.CancelJob(runs[i].Job)
	if err == nil {
		m.log.Printf("team %s: its operator stopped eval job %s", t.ID, runs[i].Job)
	}
	return err
}
