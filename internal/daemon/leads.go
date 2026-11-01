package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/store"
)

// Leads (store.Lead): a team's line that makes agents of its own in the
// team, through the team service (leadapi.go). gero keeps the lead's run
// going and within its operator's terms:
//
//   - a turn of an agent the lead made ends, or waits for an approval:
//     that is news for the lead, with the agent's last answer;
//   - a turn runs past its limit: gero stops it and asks for a final
//     report, whose end is the news;
//   - the lead's agents have all ended their turns: gero runs the team's
//     eval, and the lead hears of the round once, with the score;
//   - nothing of the team runs after that: the lead is told so, once per
//     eval;
//   - the budget (if it has one) is used, the target reached, or no new
//     best has come for Patience evals: gero stops the run, and makes no
//     more agents of the lead's until the operator resumes it.
//
// News waits in the store and goes to the lead's live session in one
// message whenever it is free, as mail does. RunTeams makes the decisions,
// one team at a time, whenever something of a team changed and every
// teamStep besides (turn limits, plan windows that started over).

const (
	leadMaxLive     = 3
	leadPlanCeiling = 90
	leadTurnMinutes = 60
	leadEvalMinutes = 20
	leadPatience    = 3
	// reportMinutes is how long an agent stopped at its turn limit has for
	// its final report.
	reportMinutes = 10
	// newsAnswer is how much of the end of an answer news carries, and
	// evalOutput how much of an eval's output is kept.
	newsAnswer = 6 << 10
	evalOutput = 4 << 10
	teamStep   = 15 * time.Second
	// leadPromptMax caps what a lead writes to an agent of its own.
	leadPromptMax = 64 << 10
)

// --- the operator's side ---------------------------------------------------

// SetLead makes an agent its team's lead, or sets its terms. The agent
// joins the team if it is in another, unless it leads that one. A new lead
// counts its spending from now, and the run, stopped or not, starts over:
// evals so far count no more toward its patience.
func (m *Manager) SetLead(ctx context.Context, teamRef string, req api.PutLead) (api.TeamRun, error) {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return api.TeamRun{}, err
	}
	a, err := m.storedAgent(req.Agent)
	if err != nil {
		return api.TeamRun{}, err
	}
	if a.ArchivedAt != nil {
		return api.TeamRun{}, httpError{http.StatusConflict, fmt.Errorf("agent %s is archived; a lead is a live agent", shownID(a))}
	}
	if a.MadeBy != "" {
		return api.TeamRun{}, badRequest(fmt.Errorf("agent %s was made by a lead; it leads nothing", shownID(a)))
	}
	line := cmp.Or(a.Line, a.ID)
	// Without a budget only the plan ceiling holds the run back, and a CLI
	// billed per use has no plan. Asked before the lock: it takes a moment.
	perUse := ""
	if req.Budget == nil || *req.Budget == 0 {
		if was, err := m.store.Lead(t.ID); req.Budget != nil || err != nil || was.Settings.Budget == 0 {
			perUse = m.perUse(ctx, a.IsolationID, a.Kind)
		}
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	if other, err := m.store.LeadOfLine(line); err == nil && other.Team != t.ID {
		o, _ := m.store.Team(other.Team)
		return api.TeamRun{}, httpError{http.StatusConflict, fmt.Errorf("agent %s leads team %q; take it off that first (`gero team lead %q --clear`)", shownID(a), o.Name, o.Name)}
	}
	l, err := m.store.Lead(t.ID)
	had := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return api.TeamRun{}, err
	}
	if !had || l.Line != line {
		agents, spent, err := m.spentNow()
		if err != nil {
			return api.TeamRun{}, err
		}
		l = store.Lead{Team: t.ID, Line: line, Settings: l.Settings, Since: time.Now().UTC(), Base: lineSpent(line, agents, spent)}
	}
	if l.Settings, err = m.leadTerms(l.Settings, req, a); err != nil {
		return api.TeamRun{}, err
	}
	if l.Settings.Budget == 0 && perUse != "" {
		return api.TeamRun{}, badRequest(fmt.Errorf("without a budget only the plan ceiling holds a lead back, and %s; give it a budget (`--budget 5M`)", perUse))
	}
	evals, err := m.store.Evals(t.ID)
	if err != nil {
		return api.TeamRun{}, err
	}
	var last int64
	if n := len(evals); n > 0 {
		last = evals[n-1].ID
	}
	l.Stopped, l.StaleFrom, l.Nudged = "", last, last
	if team, err := m.store.LineTeam(line); err != nil {
		return api.TeamRun{}, err
	} else if team != t.ID {
		if err := m.store.SetTeam(line, t.ID); err != nil {
			return api.TeamRun{}, err
		}
	}
	if err := m.store.PutLead(l); err != nil {
		return api.TeamRun{}, err
	}
	budget := "none"
	if l.Settings.Budget > 0 {
		budget = tokens(l.Settings.Budget) + " tokens"
	}
	m.log.Printf("team %s: %s leads it (budget %s)", t.ID, line, budget)
	go m.teamNewsChanged(t.ID)
	m.teamChanged(t.ID)
	return m.teamRun(t, l)
}

// leadTerms are was with req's terms, each checked; a term never set is
// its default.
func (m *Manager) leadTerms(was store.LeadSettings, req api.PutLead, lead store.Agent) (store.LeadSettings, error) {
	s := was
	s.MaxLive = cmp.Or(s.MaxLive, leadMaxLive)
	s.PlanCeiling = cmp.Or(s.PlanCeiling, leadPlanCeiling)
	s.TurnMinutes = cmp.Or(s.TurnMinutes, leadTurnMinutes)
	s.EvalMinutes = cmp.Or(s.EvalMinutes, leadEvalMinutes)
	s.Patience = cmp.Or(s.Patience, leadPatience)
	s.EvalDir = cmp.Or(s.EvalDir, lead.Workdir)
	setInt := func(to *int, from *int) {
		if from != nil {
			*to = *from
		}
	}
	if req.Budget != nil {
		s.Budget = *req.Budget
	}
	setInt(&s.MaxLive, req.MaxLive)
	setInt(&s.PlanCeiling, req.PlanCeiling)
	setInt(&s.TurnMinutes, req.TurnMinutes)
	setInt(&s.EvalMinutes, req.EvalMinutes)
	setInt(&s.Patience, req.Patience)
	if req.Eval != nil {
		s.Eval = strings.TrimSpace(*req.Eval)
	}
	if req.EvalDir != nil {
		s.EvalDir = *req.EvalDir
	}
	if req.Target != nil {
		s.Target = req.Target
	}
	if req.NoTarget {
		s.Target = nil
	}
	in := func(what string, v, lo, hi int) error {
		if v < lo || v > hi {
			return badRequest(fmt.Errorf("%s is %d to %d, not %d", what, lo, hi, v))
		}
		return nil
	}
	if s.Budget < 0 {
		return s, badRequest(fmt.Errorf("a budget is a count of tokens, or 0 for none, not %d", s.Budget))
	}
	for _, c := range []struct {
		what      string
		v, lo, hi int
	}{
		{"the number of live agents", s.MaxLive, 1, 20},
		{"the plan ceiling (percent)", s.PlanCeiling, 1, 100},
		{"a turn's limit (minutes)", s.TurnMinutes, 1, 24 * 60},
		{"the eval's limit (minutes)", s.EvalMinutes, 1, 4 * 60},
		{"the patience (evals)", s.Patience, 1, 100},
	} {
		if err := in(c.what, c.v, c.lo, c.hi); err != nil {
			return s, err
		}
	}
	if strings.ContainsRune(s.Eval, 0) {
		return s, badRequest(errors.New("the eval is a shell command, without NUL"))
	}
	dir, err := m.isoPath(s.EvalDir)
	if err != nil {
		return s, badRequest(fmt.Errorf("the eval's directory: %w", err))
	}
	s.EvalDir = dir
	if s.Target != nil && (math.IsNaN(*s.Target) || math.IsInf(*s.Target, 0)) {
		return s, badRequest(errors.New("the target is a number"))
	}
	return s, nil
}

// ClearLead leaves a team without a lead. The agents it made stay, the
// operator's now.
func (m *Manager) ClearLead(teamRef string) error {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return err
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	return m.store.DeleteLead(t.ID)
}

// TeamRun is a team's run, for its operator.
func (m *Manager) TeamRun(teamRef string) (api.TeamRun, error) {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return api.TeamRun{}, err
	}
	l, err := m.store.Lead(t.ID)
	if errors.Is(err, store.ErrNotFound) {
		return api.TeamRun{}, fmt.Errorf("team %q has no lead (`gero team lead %q AGENT`): %w", t.Name, t.Name, store.ErrNotFound)
	}
	if err != nil {
		return api.TeamRun{}, err
	}
	return m.teamRun(t, l)
}

// EvalTeam has a team's eval run now, for its operator.
func (m *Manager) EvalTeam(teamRef string) error {
	t, err := m.store.Team(teamRef)
	if err != nil {
		return err
	}
	l, err := m.store.Lead(t.ID)
	if err != nil {
		return err
	}
	return m.startEval(l, "your operator asked")
}

// teamsWithRuns are teams as the sidebar draws them.
func (m *Manager) teamsWithRuns(teams []store.Team) ([]api.Team, error) {
	runs, err := m.runBriefs()
	if err != nil {
		return nil, err
	}
	out := make([]api.Team, 0, len(teams))
	for _, t := range teams {
		out = append(out, api.Team{Team: t, Run: runs[t.ID]})
	}
	return out, nil
}

// withRuns gives each lead the brief of its run.
func (m *Manager) withRuns(as []api.Agent) error {
	runs, err := m.runBriefs()
	if err != nil || len(runs) == 0 {
		return err
	}
	byLine := map[string]*api.TeamBrief{}
	for _, b := range runs {
		byLine[b.Lead] = b
	}
	for i, a := range as {
		if a.ArchivedAt == nil {
			as[i].Leads = byLine[cmp.Or(a.Line, a.ID)]
		}
	}
	return nil
}

// runBriefs are the runs of the teams with a lead, by team.
func (m *Manager) runBriefs() (map[string]*api.TeamBrief, error) {
	leads, err := m.store.Leads()
	if err != nil || len(leads) == 0 {
		return nil, err
	}
	agents, spent, err := m.spentNow()
	if err != nil {
		return nil, err
	}
	out := map[string]*api.TeamBrief{}
	for _, l := range leads {
		evals, err := m.store.Evals(l.Team)
		if err != nil {
			return nil, err
		}
		best, _ := bestOf(evals, l.StaleFrom)
		b := &api.TeamBrief{Lead: l.Line, Status: m.runStatus(l), Spent: teamSpent(l, agents, spent).New(), Budget: l.Settings.Budget}
		if best != nil {
			b.Best = best.Score
		}
		out[l.Team] = b
	}
	return out, nil
}

// teamRun is t's run as l is now.
func (m *Manager) teamRun(t store.Team, l store.Lead) (api.TeamRun, error) {
	agents, spent, err := m.spentNow()
	if err != nil {
		return api.TeamRun{}, err
	}
	evals, err := m.store.Evals(t.ID)
	if err != nil {
		return api.TeamRun{}, err
	}
	best, stale := bestOf(evals, l.StaleFrom)
	run := api.TeamRun{Team: t, Lead: l.Line, LeadLine: l.Line, Settings: l.Settings, Since: l.Since, Stopped: l.Stopped,
		Status: m.runStatus(l), Spent: teamSpent(l, agents, spent), Evaluating: m.evalRunning(t.ID), Stale: stale, Evals: []store.Eval{}, Agents: []api.TeamAgent{}}
	if best != nil {
		b := cutEval(*best)
		run.Best = &b
	}
	for _, e := range evals[max(len(evals)-10, 0):] {
		run.Evals = append(run.Evals, cutEval(e))
	}
	kinds := map[driver.Kind]bool{}
	iso := ""
	for _, a := range agents {
		switch {
		case cmp.Or(a.Line, a.ID) == l.Line && a.ArchivedAt == nil:
			run.Lead, iso = shownID(a), a.IsolationID
			kinds[a.Kind] = true
		case a.MadeBy != l.Line:
		case a.ArchivedAt != nil:
			run.Archived++
		default:
			kinds[a.Kind] = true
			run.Agents = append(run.Agents, m.teamAgent(a, spent[a.ID], false))
		}
	}
	for _, k := range []driver.Kind{driver.Claude, driver.Codex} {
		if kinds[k] && iso != "" {
			for _, lim := range planOf(iso, k, agents, spent) {
				run.Plan = append(run.Plan, api.PlanWindow{Kind: k, Limit: lim})
			}
		}
	}
	return run, nil
}

// teamAgent is a, an agent a lead made, as its run shows it; with its
// whole last answer if answer.
func (m *Manager) teamAgent(a store.Agent, sp store.Spent, answer bool) api.TeamAgent {
	v := api.TeamAgent{ID: shownID(a), Role: a.Name, Kind: a.Kind, Model: a.Options.Model, Status: event.StatusIdle,
		TurnMinutes: a.TurnMinutes, Spent: sp.Tokens}
	m.mu.Lock()
	r := m.runtimes[a.ID]
	m.mu.Unlock()
	if r == nil {
		return v
	}
	_, st, _ := r.shown()
	r.mu.Lock()
	since, said, waits := r.turnSince, r.answer, r.waits
	r.mu.Unlock()
	v.Status = st.Status
	if st.Usage != nil {
		v.Model = cmp.Or(st.Usage.Model, v.Model)
		v.Spent = st.Usage.Spent
	}
	if st.Status == event.StatusWorking || st.Status == event.StatusBlocked {
		v.TurnSince = &since
	}
	if st.Status == event.StatusBlocked {
		v.Waits = waits
	}
	if answer {
		v.Answer = said
	}
	return v
}

func cutEval(e store.Eval) store.Eval {
	e.Output = tail(e.Output, 1000)
	return e
}

// runStatus says what l's run is doing now.
func (m *Manager) runStatus(l store.Lead) string {
	switch {
	case l.Stopped != "":
		return "stopped: " + l.Stopped
	case m.evalRunning(l.Team):
		return "running the eval"
	}
	if n := m.childrenBusy(l.Line); n > 0 {
		return fmt.Sprintf("%d of the lead's agents at work", n)
	}
	if r := m.lineRuntime(l.Line); r != nil {
		if _, st, _ := r.shown(); st.Status == event.StatusWorking || st.Status == event.StatusBlocked {
			return "the lead is at work"
		}
		return "the lead is idle, and nothing of its runs"
	}
	return "the lead is archived"
}

// --- spending and plan windows ----------------------------------------------

// spentNow is every agent with what it last said it spent.
func (m *Manager) spentNow() ([]store.Agent, map[string]store.Spent, error) {
	agents, err := m.store.Agents()
	if err != nil {
		return nil, nil, err
	}
	spent, err := m.store.SpentOf()
	return agents, spent, err
}

func lineSpent(line string, agents []store.Agent, spent map[string]store.Spent) driver.Tokens {
	var t driver.Tokens
	for _, a := range agents {
		if cmp.Or(a.Line, a.ID) == line {
			t = t.Plus(spent[a.ID].Tokens)
		}
	}
	return t
}

// teamSpent is what l and the agents it made have used since it became
// the lead.
func teamSpent(l store.Lead, agents []store.Agent, spent map[string]store.Spent) driver.Tokens {
	own := lineSpent(l.Line, agents, spent).Minus(l.Base)
	own = driver.Tokens{Input: max(own.Input, 0), Cached: max(own.Cached, 0), Output: max(own.Output, 0)}
	for _, a := range agents {
		if a.MadeBy == l.Line {
			own = own.Plus(spent[a.ID].Tokens)
		}
	}
	return own
}

// planOf is the plan windows of the account kind runs on in isolation iso,
// as its agents last said them: per window, the one that resets last and,
// of those, the most used. A window that has started over since is gone.
func planOf(iso string, kind driver.Kind, agents []store.Agent, spent map[string]store.Spent) []driver.Limit {
	latest := map[string]driver.Limit{}
	for _, a := range agents {
		if a.IsolationID != iso || a.Kind != kind {
			continue
		}
		for _, l := range spent[a.ID].Limits {
			k, ok := latest[l.Window]
			if !ok || k.ResetsAt < l.ResetsAt || k.ResetsAt == l.ResetsAt && k.Percent < l.Percent {
				latest[l.Window] = l
			}
		}
	}
	now := time.Now().Unix()
	var out []driver.Limit
	for _, l := range latest {
		if l.ResetsAt == 0 || l.ResetsAt > now {
			out = append(out, l)
		}
	}
	slices.SortFunc(out, func(a, b driver.Limit) int { return strings.Compare(a.Window, b.Window) })
	return out
}

// overBudget is whether l has a budget and used has reached it.
func overBudget(l store.Lead, used int64) bool {
	return l.Settings.Budget > 0 && used >= l.Settings.Budget
}

// spentText is used tokens, against budget if there is one.
func spentText(used, budget int64) string {
	if budget <= 0 {
		return tokens(used) + " tokens, with no budget"
	}
	return fmt.Sprintf("%s of %s tokens", tokens(used), tokens(budget))
}

// perUse says why kind's CLI in the isolation (by ID) needs a budget: it
// is logged in with something billed per use, which no plan window holds
// back, or gero cannot tell how it is. "" when it runs on a subscription,
// or is logged in to nothing and runs nothing.
func (m *Manager) perUse(ctx context.Context, isoID string, kind driver.Kind) string {
	iso, err := m.store.Isolation(isoID)
	if err != nil {
		return fmt.Sprintf("gero cannot tell how %s is logged in there (%v)", kind, err)
	}
	c := m.cliStatus(ctx, iso, kind)
	switch {
	case c.Error != "":
		return fmt.Sprintf("gero cannot tell how %s is logged in there (%s)", kind, c.Error)
	case !c.LoggedIn, c.Method == "ChatGPT", c.Method == "CLAUDE_CODE_OAUTH_TOKEN", strings.HasPrefix(c.Method, "Claude subscription"):
		return ""
	}
	return fmt.Sprintf("%s is logged in there with %s, billed per use", kind, c.Method)
}

// planFull says why nothing new starts on kind's plan in iso: a window
// used past ceiling percent; "" if none is.
func (m *Manager) planFull(iso string, kind driver.Kind, ceiling int) string {
	agents, spent, err := m.spentNow()
	if err != nil {
		return ""
	}
	for _, l := range planOf(iso, kind, agents, spent) {
		if l.Percent >= float64(ceiling) {
			why := fmt.Sprintf("%s's %s plan window is %.0f%% used, past the %d%% your operator allows", kind, windowName(l.Window), l.Percent, ceiling)
			if l.ResetsAt > 0 {
				why += fmt.Sprintf("; it starts over in %s", time.Until(time.Unix(l.ResetsAt, 0)).Round(time.Minute))
			}
			return why
		}
	}
	return ""
}

func windowName(w string) string {
	switch w {
	case "five_hour":
		return "5-hour"
	case "seven_day":
		return "7-day"
	}
	return w
}

// --- what goes on in a team ----------------------------------------------

// teamChanged has team looked at again soon.
func (m *Manager) teamChanged(team string) {
	m.mu.Lock()
	m.teamsDue[team] = true
	m.mu.Unlock()
	select {
	case m.teamWake <- struct{}{}:
	default:
	}
}

// RunTeams keeps the leads' runs going, until the daemon stops.
func (m *Manager) RunTeams() {
	tick := time.NewTicker(teamStep)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			m.turnLimits(time.Now())
			if leads, err := m.store.Leads(); err == nil {
				m.mu.Lock()
				for _, l := range leads {
					m.teamsDue[l.Team] = true
				}
				m.mu.Unlock()
			}
		case <-m.teamWake:
		case <-m.ctx.Done():
			return
		}
		m.mu.Lock()
		due := m.teamsDue
		m.teamsDue = map[string]bool{}
		m.mu.Unlock()
		for team := range due {
			m.teamCheck(team)
		}
	}
}

// teamCheck decides what a team's run does next: stop it for its budget,
// run its eval when a round has ended, or tell its lead nothing runs.
func (m *Manager) teamCheck(team string) {
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	l, err := m.store.Lead(team)
	if err != nil || l.Stopped != "" {
		return
	}
	agents, spent, err := m.spentNow()
	if err != nil {
		m.log.Printf("team %s: %v", team, err)
		return
	}
	if used := teamSpent(l, agents, spent).New(); overBudget(l, used) {
		m.stopRunLocked(l, fmt.Sprintf("the budget is used: %s of %s tokens", tokens(used), tokens(l.Settings.Budget)))
		return
	}
	if l.Settings.Eval == "" || m.evalRunning(team) || m.childrenBusy(l.Line) > 0 {
		return
	}
	if l.Dirty {
		if err := m.startEval(l, "the turns of your agents ended"); err != nil {
			m.log.Printf("team %s: eval: %v", team, err)
		}
		return
	}
	evals, err := m.store.Evals(team)
	if err != nil || len(evals) == 0 || evals[len(evals)-1].ID <= l.Nudged {
		return
	}
	if n, err := m.store.TeamNewsWaiting(team); err != nil || n > 0 {
		return
	}
	r := m.lineRuntime(l.Line)
	if r == nil || !r.free() {
		return
	}
	lead, _, _ := r.view()
	if why := m.planFull(lead.IsolationID, lead.Kind, l.Settings.PlanCeiling); why != "" {
		return // the next step looks again, the window may have started over
	}
	last := evals[len(evals)-1]
	best, stale := bestOf(evals, l.StaleFrom)
	msg := "Nothing of your team is running. " + scoreLine(last, best, stale, l.Settings.Patience) +
		" The team has used " + spentText(teamSpent(l, agents, spent).New(), l.Settings.Budget) + "." +
		" Design the next team, or say why you stop."
	if err := m.store.AddTeamNews(team, msg); err != nil {
		m.log.Printf("team %s: %v", team, err)
		return
	}
	m.store.SetLeadNudged(team, last.ID)
	go m.teamNewsChanged(team)
}

// stopRunLocked stops l's run, saying why to its lead.
func (m *Manager) stopRunLocked(l store.Lead, why string) {
	if err := m.store.SetLeadStopped(l.Team, why); err != nil {
		m.log.Printf("team %s: %v", l.Team, err)
		return
	}
	m.log.Printf("team %s: run stopped: %s", l.Team, why)
	m.store.AddTeamNews(l.Team, "gero stopped the run: "+why+". Until your operator resumes it, no agent of yours can be made or written to; the ones at work finish their turns.")
	go m.teamNewsChanged(l.Team)
}

// childrenBusy is how many agents line made are at work, or about to be.
func (m *Manager) childrenBusy(line string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.busy[line])
}

// setChildBusy keeps whether an agent lead made is at work.
func (m *Manager) setChildBusy(lead, agent string, busy bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !busy {
		delete(m.busy[lead], agent)
		return
	}
	if m.busy[lead] == nil {
		m.busy[lead] = map[string]bool{}
	}
	m.busy[lead][agent] = true
}

// lineRuntime is the live session of line, if it has one.
func (m *Manager) lineRuntime(line string) *runtime {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.runtimes {
		if cmp.Or(r.agent.Line, r.agent.ID) == line {
			return r
		}
	}
	return nil
}

// leadNewsWaiting is how much news waits for a as its team's lead.
func (m *Manager) leadNewsWaiting(a store.Agent) (int, error) {
	l, err := m.store.LeadOfLine(cmp.Or(a.Line, a.ID))
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return m.store.TeamNewsWaiting(l.Team)
}

// teamNewsChanged tells team's lead its news changed.
func (m *Manager) teamNewsChanged(team string) {
	l, err := m.store.Lead(team)
	if err != nil {
		return
	}
	if r := m.lineRuntime(l.Line); r != nil {
		r.newsChanged()
	}
}

// --- the eval -------------------------------------------------------------

func (m *Manager) evalRunning(team string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evaluating[team]
}

// startEval runs l's eval, unless it runs already; its result is news.
func (m *Manager) startEval(l store.Lead, why string) error {
	if l.Settings.Eval == "" {
		return httpError{http.StatusConflict, errors.New("the team has no eval; its operator sets one with `gero team lead TEAM AGENT --eval CMD`")}
	}
	m.mu.Lock()
	if m.evaluating[l.Team] {
		m.mu.Unlock()
		return httpError{http.StatusConflict, errors.New("the eval is running already; its result comes as news")}
	}
	m.evaluating[l.Team] = true
	m.mu.Unlock()
	m.store.SetLeadDirty(l.Team, false)
	m.loops.Add(1)
	go func() {
		defer m.loops.Done()
		m.runEval(l, why)
		m.mu.Lock()
		delete(m.evaluating, l.Team)
		m.mu.Unlock()
		m.teamNewsChanged(l.Team)
		m.teamChanged(l.Team)
	}()
	return nil
}

var scoreLineRe = regexp.MustCompile(`(?m)^SCORE:[ \t]*(\S+)[ \t]*$`)

// runEval runs l's eval in the lead's isolation, as its agents, and tells
// the lead what came of it; a target reached or patience run out stops
// the run.
func (m *Manager) runEval(l store.Lead, why string) {
	e := store.Eval{Team: l.Team, At: time.Now().UTC(), Why: why}
	out, err := m.evalOutput(l)
	e.Took = time.Since(e.At).Round(time.Second)
	e.Output = tail(out, evalOutput)
	if ms := scoreLineRe.FindAllStringSubmatch(out, -1); len(ms) > 0 {
		if v, perr := strconv.ParseFloat(ms[len(ms)-1][1], 64); perr == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			e.Score = &v
		}
	}
	if err != nil && e.Score == nil {
		e.Output = tail(strings.TrimSpace(e.Output+"\n"+err.Error()), evalOutput)
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	e, aerr := m.store.AddEval(e)
	if aerr != nil {
		m.log.Printf("team %s: eval: %v", l.Team, aerr)
		return
	}
	if l, aerr = m.store.Lead(l.Team); aerr != nil {
		return
	}
	evals, _ := m.store.Evals(l.Team)
	best, stale := bestOf(evals, l.StaleFrom)
	msg := fmt.Sprintf("Eval #%d (%s) took %s. %s", e.ID, why, e.Took, scoreLine(e, best, stale, l.Settings.Patience))
	if e.Score == nil {
		msg += "\nThe end of its output:\n\n" + tail(e.Output, 1500)
	}
	m.log.Printf("team %s: eval #%d: %v", l.Team, e.ID, e.Score)
	if err := m.store.AddTeamNews(l.Team, msg); err != nil {
		m.log.Printf("team %s: %v", l.Team, err)
	}
	if l.Stopped != "" {
		return
	}
	switch {
	case best != nil && l.Settings.Target != nil && *best.Score >= *l.Settings.Target:
		m.stopRunLocked(l, fmt.Sprintf("the target %g is reached, with %g at eval #%d", *l.Settings.Target, *best.Score, best.ID))
	case stale >= l.Settings.Patience:
		m.stopRunLocked(l, fmt.Sprintf("%d evals in a row came without a new best", stale))
	}
}

// evalOutput runs l's eval and says what it printed.
func (m *Manager) evalOutput(l store.Lead) (string, error) {
	var lead store.Agent
	if r := m.lineRuntime(l.Line); r != nil {
		lead, _, _ = r.view()
	} else if a, err := m.storedAgent(l.Line); err == nil {
		lead = a
	} else {
		return "", err
	}
	limit := time.Duration(l.Settings.EvalMinutes) * time.Minute
	ctx, cancel := context.WithTimeout(m.ctx, limit+time.Minute)
	defer cancel()
	iso, err := m.store.Isolation(lead.IsolationID)
	if err != nil {
		return "", err
	}
	if _, err := m.ensureIsolation(ctx, iso.ID); err != nil {
		return "", err
	}
	if ok, why := m.fenceOK(iso.ID); !ok {
		return "", fmt.Errorf("isolation %s has no verified network fence: %s", iso.Name, why)
	}
	// timeout(1) signals the eval's whole process group; ending `container
	// exec` would leave it running.
	return m.cli.RunAsAgent(ctx, iso.Container, "timeout", "--kill-after=30", fmt.Sprintf("%ds", int(limit.Seconds())),
		"sh", "-c", `cd -- "$1" && exec sh -c "$2"`, "gero-eval", l.Settings.EvalDir, l.Settings.Eval)
}

// bestOf is the best scored of evals, and how many evals since it came
// without a new best, those up to from not counted.
func bestOf(evals []store.Eval, from int64) (*store.Eval, int) {
	var best *store.Eval
	stale := 0
	for i := range evals {
		e := &evals[i]
		if e.Score != nil && (best == nil || *e.Score > *best.Score) {
			best, stale = e, 0
			continue
		}
		if e.ID > from {
			stale++
		}
	}
	return best, stale
}

// scoreLine says what e scored against the best.
func scoreLine(e store.Eval, best *store.Eval, stale, patience int) string {
	if e.Score == nil {
		s := `It printed no "SCORE:" line, so it scored nothing`
		if best != nil {
			s += fmt.Sprintf("; the best is %g, at eval #%d", *best.Score, best.ID)
		}
		return s + fmt.Sprintf(". %d of %d evals without a new best.", stale, patience)
	}
	if best != nil && best.ID == e.ID {
		return fmt.Sprintf("SCORE %g, a new best.", *e.Score)
	}
	s := fmt.Sprintf("SCORE %g", *e.Score)
	if best != nil {
		s += fmt.Sprintf("; the best is %g, at eval #%d", *best.Score, best.ID)
	}
	return s + fmt.Sprintf(". %d of %d evals without a new best.", stale, patience)
}

// --- the runtime's side -----------------------------------------------------

// teamTurnLocked tells the agent's team what a turn of its came to, as it
// happens: it ended (ended), or waits for an approval (waited).
func (r *runtime) teamTurnLocked(ended event.Status, waited string, at time.Time) {
	if r.agent.MadeBy == "" {
		// A lead's turn that ends may leave its team with nothing to do.
		if l, err := r.m.store.LeadOfLine(cmp.Or(r.agent.Line, r.agent.ID)); err == nil && ended != "" {
			r.m.teamChanged(l.Team)
		}
		return
	}
	l, err := r.m.store.LeadOfLine(r.agent.MadeBy)
	if err != nil {
		return // its lead leads no team now
	}
	who := fmt.Sprintf("%s (%q, %s)", shownID(r.agent), r.agent.Name, r.agent.Kind)
	if waited != "" {
		r.teamNewsLocked(l, fmt.Sprintf("%s waits for your operator to approve: %s. It goes on once they decide; you cannot decide for it.", who, waited))
	}
	if ended == "" {
		return
	}
	note := ""
	switch r.overtime {
	case overStopping:
		// Its end is the report's.
		r.overtime = overReporting
		if err := r.sendLocked(fmt.Sprintf("[gero team: your turn ran past its %d minutes, and gero stopped it.] Start no new work, edits or measurements. Reply now with your final report: what you changed, what you measured and how, what is unfinished, and where your notes and files are.", r.agent.TurnMinutes)); err != nil {
			r.m.log.Printf("agent %s: report: %v", r.agent.ID, err)
		}
		return
	case overReporting, overStopped:
		note = fmt.Sprintf(" gero stopped its turn at its %d-minute limit and asked for a final report; this is it.", r.agent.TurnMinutes)
	}
	r.overtime = overNone
	st := r.drv.State()
	how := string(ended)
	if ended == event.StatusError && st.Error != "" {
		how += " (" + st.Error + ")"
	}
	took := ""
	if !r.turnSince.IsZero() {
		took = ", after " + at.Sub(r.turnSince).Round(time.Second).String()
	}
	var spent driver.Tokens
	if st.Usage != nil {
		spent = st.Usage.Spent
	}
	body := fmt.Sprintf("%s ended its turn: %s%s. It has used %s tokens.%s", who, how, took, tokens(spent.New()), note)
	if r.answer != "" {
		body += "\nIts last answer:\n\n" + tail(r.answer, newsAnswer)
	} else {
		body += " It said nothing."
	}
	// The round is not evaluated before its news is there to be held.
	if err := r.m.store.SetLeadDirty(l.Team, true); err != nil {
		r.m.log.Printf("team %s: %v", l.Team, err)
	}
	r.teamNewsLocked(l, body)
	r.m.teamChanged(l.Team)
}

func (r *runtime) teamNewsLocked(l store.Lead, body string) {
	if err := r.m.store.AddTeamNews(l.Team, body); err != nil {
		r.m.log.Printf("team %s: news: %v", l.Team, err)
		return
	}
	go r.m.teamNewsChanged(l.Team)
}

// newsChanged follows its team's news, as its lead.
func (r *runtime) newsChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := r.m.leadNewsWaiting(r.agent); err == nil {
		r.news = n
	}
	r.kickLocked()
}

// deliverNewsLocked moves all its team's waiting news into its outbox, as
// one message. With an eval to run, news waits for the round to end: until
// the lead's agents have ended their turns and the eval has run, so the
// lead hears of a round once, with its score.
func (r *runtime) deliverNewsLocked() {
	l, err := r.m.store.LeadOfLine(cmp.Or(r.agent.Line, r.agent.ID))
	if err != nil {
		r.news = 0
		return
	}
	if l.Settings.Eval != "" && l.Stopped == "" && (l.Dirty || r.m.evalRunning(l.Team)) {
		return
	}
	t, err := r.m.store.Team(l.Team)
	if err != nil {
		return
	}
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.DeliverTeamNews(l.Team, r.agent.ID, after, func(news []string) string {
		head := fmt.Sprintf("[gero team news for you as lead of team %q: %d item(s). Your operator may not be watching.]", t.Name, len(news))
		if len(news) == 1 {
			return head + "\n\n" + news[0]
		}
		var b strings.Builder
		b.WriteString(head)
		for i, n := range news {
			fmt.Fprintf(&b, "\n\n%d. %s", i+1, n)
		}
		return b.String()
	})
	if err == nil {
		r.outbox = append(r.outbox, p)
		r.drv.Send(p.Text)
		r.m.log.Printf("agent %s: team %s's news delivered", r.agent.ID, l.Team)
	} else if !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: team news: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.TeamNewsWaiting(l.Team); err == nil {
		r.news = n
	}
}

// keepSpentLocked keeps what the agent has spent as its log says it now,
// for its team's budget once it is archived too, and tells its team.
func (r *runtime) keepSpentLocked() {
	st := r.drv.State()
	if st.Usage == nil {
		return
	}
	sp := store.Spent{Tokens: st.Usage.Spent, Limits: st.Usage.Limits, At: time.Now().UTC()}
	if sp.Tokens == r.spent.Tokens && slices.Equal(sp.Limits, r.spent.Limits) {
		return
	}
	if err := r.m.store.PutSpent(r.agent.ID, sp); err != nil {
		r.m.log.Printf("agent %s: spent: %v", r.agent.ID, err)
		return
	}
	r.spent = sp
	line := cmp.Or(r.agent.MadeBy, r.agent.Line, r.agent.ID)
	if l, err := r.m.store.LeadOfLine(line); err == nil {
		r.m.teamChanged(l.Team)
	}
}

// noteBusyLocked tells the agent's lead whether it is at work, or about
// to be.
func (r *runtime) noteBusyLocked() {
	if r.agent.MadeBy == "" {
		return
	}
	st := r.drv.State()
	busy := st.Status == event.StatusWorking || st.Status == event.StatusBlocked || st.Queued > 0 || len(r.outbox) > 0 || r.starting
	r.m.setChildBusy(r.agent.MadeBy, r.agent.ID, busy)
}

// free is freeLocked, taking the lock.
func (r *runtime) free() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caughtUp && r.freeLocked(r.drv.State())
}

// turnLimits stops the turns of agents leads made that ran past their
// limits.
func (m *Manager) turnLimits(now time.Time) {
	m.mu.Lock()
	var rs []*runtime
	for _, r := range m.runtimes {
		if r.agent.MadeBy != "" {
			rs = append(rs, r)
		}
	}
	m.mu.Unlock()
	for _, r := range rs {
		r.turnLimit(now)
	}
}

// turnLimit stops the turn the agent is in if it ran past its limit, or
// its final report past reportMinutes.
func (r *runtime) turnLimit(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp || r.agent.TurnMinutes <= 0 || r.turnSince.IsZero() {
		return
	}
	if st := r.drv.State().Status; st != event.StatusWorking && st != event.StatusBlocked {
		return
	}
	limit := time.Duration(r.agent.TurnMinutes) * time.Minute
	if r.overtime == overReporting {
		limit = reportMinutes * time.Minute
	}
	if now.Sub(r.turnSince) < limit {
		return
	}
	switch r.overtime {
	case overNone:
		r.overtime = overStopping
	case overReporting:
		r.overtime = overStopped
	default:
		return // stopped already: its turn is ending
	}
	r.m.log.Printf("agent %s: its turn ran past its limit; stopping it", r.agent.ID)
	r.drv.Interrupt()
	r.kickLocked()
}

// tail is the end of s, at most n bytes of it, from a line's start where
// one is near.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < n/4 {
		s = s[i+1:]
	}
	for len(s) > 0 && !utf8RuneStart(s[0]) {
		s = s[1:]
	}
	return "…" + s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// tokens is n as people read it: 950, 12.3k, 4.1M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	}
	return strconv.FormatInt(n, 10)
}
