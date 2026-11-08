// Package teamtext writes a team's run as people, and agents, read it.
package teamtext

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hongzio/psps/internal/api"
)

// errWriter keeps the first error of its writes.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return len(p), nil
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, nil
}

// Run writes a team's run as `psps team show` prints it, and as an agent
// reads it through control with ?format=text; now is when it is read.
func Run(out io.Writer, run api.TeamRun, now time.Time) error {
	w := &errWriter{w: out}
	s := run.Settings
	fmt.Fprintf(w, "team %q, led by %s since %s: %s\n", run.Team.Name, run.Lead, run.Since.Local().Format("Jan 2 15:04"), run.Status)
	fmt.Fprintf(w, "  spent  %s (%s more read from cache)\n", Spent(run.Spent.New(), s.Budget), Tokens(run.Spent.Cached))
	fmt.Fprintf(w, "  terms  %d live at most, turns of %dm, nothing new past %d%% of a plan window\n", s.MaxLive, s.TurnMinutes, s.PlanCeiling)
	for _, p := range run.Plan {
		fmt.Fprintf(w, "  plan   %s %s %.0f%%\n", p.Kind, p.Window, p.Percent)
	}
	if s.EvalJob != "" {
		fmt.Fprintf(w, "  eval   a job of runner %s, on a copy of %s, patience %d", s.EvalJob, s.EvalDir, s.Patience)
		if s.Target != nil {
			fmt.Fprintf(w, ", target %g", *s.Target)
		}
		fmt.Fprintln(w)
	} else if s.Eval == "" {
		fmt.Fprintln(w, "  eval   none")
	} else {
		where := "in " + s.EvalDir
		if run.EvalIsolation != "" {
			where = "in isolation " + run.EvalIsolation + ", on a copy of " + s.EvalDir
		}
		fmt.Fprintf(w, "  eval   %s (%s, %dm at most), patience %d", s.Eval, where, s.EvalMinutes, s.Patience)
		if s.Target != nil {
			fmt.Fprintf(w, ", target %g", *s.Target)
		}
		if s.EvalSetBy != "" {
			fmt.Fprintf(w, "; set by agent %s, through control", s.EvalSetBy)
		}
		fmt.Fprintln(w)
	}
	if s.CompactFirst {
		fmt.Fprintln(w, "  news   the lead's conversation is compacted before it hears a round's score")
	}
	if line := EvalNow(run.Eval, run.Team.Name, now); line != "" {
		fmt.Fprintln(w, line)
	}
	for _, e := range run.Evals {
		score := "no score"
		if e.Score != nil {
			score = Score(*e.Score)
		} else if e.NotRun {
			score = "not run"
		}
		fmt.Fprintf(w, "  #%-4d %s  %-10s %s, %s\n", e.ID, e.At.Local().Format("Jan 2 15:04"), score, e.Why, e.Took.Round(time.Second))
	}
	if n := len(run.Evals); n > 0 && run.Evals[n-1].Score == nil && run.Evals[n-1].Output != "" {
		e := run.Evals[n-1]
		fmt.Fprintf(w, "  the end of #%d's output:\n", e.ID)
		for _, l := range strings.Split(e.Output, "\n") {
			fmt.Fprintln(w, "    "+l)
		}
	}
	if run.Best != nil {
		fmt.Fprintf(w, "  best   %s at #%d; %d since without a new one\n", Score(*run.Best.Score), run.Best.ID, run.Stale)
	}
	if len(run.Agents) == 0 {
		fmt.Fprintf(w, "  agents none live (%d archived)\n", run.Archived)
		return w.err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tROLE\tKIND\tSTATUS\tTURN\tSPENT")
	for _, a := range run.Agents {
		turn := "-"
		if a.TurnSince != nil {
			turn = fmt.Sprintf("%s of %dm", now.Sub(*a.TurnSince).Round(time.Minute), a.TurnMinutes)
		}
		status := string(a.Status)
		if a.Waits != "" {
			status += ": " + a.Waits
		}
		switch {
		case a.Stalled > 0:
			status += fmt.Sprintf(", stalled (its process ended with %d in the background; the operator resumes it)", a.Stalled)
		case a.InBackground:
			status += fmt.Sprintf(", at work on %d in the background", a.Background)
		case a.Background > 0 && a.Released:
			status += fmt.Sprintf(", released, %d in the background", a.Background)
		case a.Background > 0:
			status += fmt.Sprintf(", %d in the background", a.Background)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.Role, a.Kind, status, turn, Tokens(a.Spent.New()))
	}
	if run.Archived > 0 {
		fmt.Fprintf(tw, "  (%d archived)\n", run.Archived)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return w.err
}

// Spent is spent tokens, of budget if there is one.
func Spent(spent, budget int64) string {
	if budget <= 0 {
		return Tokens(spent) + " tokens, no budget"
	}
	return fmt.Sprintf("%s of %s tokens", Tokens(spent), Tokens(budget))
}

// Tokens is n as people read it: 950, 12.3k, 4.1M.
func Tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// Score is a score as a person reads it: two decimals at most.
func Score(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

// EvalNow is what the run's eval is doing now, for team show; "" for
// a team without one.
func EvalNow(ev *api.EvalNow, team string, now time.Time) string {
	if ev == nil {
		return ""
	}
	took := func(d time.Duration) string { return d.Round(time.Second).String() }
	usual := ""
	if ev.Usual > 0 {
		usual = " (usually ~" + took(ev.Usual) + ")"
	}
	on := ""
	switch {
	case ev.Runner != "":
		on = " on runner " + ev.Runner
	case ev.Isolation != "":
		on = " in isolation " + ev.Isolation
	}
	job := ""
	if ev.Job != "" {
		job = "\n         job " + ev.Job + " (psps job show " + ev.Job + "; psps team stop-eval stops it)"
	}
	switch ev.State {
	case "running":
		return fmt.Sprintf("  now    #%d running%s for %s%s, as %s%s", ev.Number, on, took(now.Sub(ev.Since)), usual, ev.Why, job)
	case "queued":
		place := ""
		if ev.Place > 0 {
			place = fmt.Sprintf(", %s in line", ordinal(ev.Place))
		}
		return fmt.Sprintf("  now    #%d queued%s%s, as %s%s", ev.Number, on, place, ev.Why, job)
	case "waiting":
		return fmt.Sprintf("  now    no eval; the next when the round ends (%s)", ev.Waiting)
	case "due":
		d := now.Sub(ev.Since)
		if d < time.Minute {
			return "  now    the round ended; the eval is starting"
		}
		return fmt.Sprintf("  now    no eval, though the round ended %s ago: psps team eval %q runs one", took(d), team)
	case "idle":
		return "  now    no eval; nothing changed since the last"
	}
	return ""
}

func ordinal(n int) string {
	switch {
	case n%100 >= 11 && n%100 <= 13:
		return fmt.Sprintf("%dth", n)
	case n%10 == 1:
		return fmt.Sprintf("%dst", n)
	case n%10 == 2:
		return fmt.Sprintf("%dnd", n)
	case n%10 == 3:
		return fmt.Sprintf("%drd", n)
	}
	return fmt.Sprintf("%dth", n)
}
