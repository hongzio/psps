package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
)

// The team page: a team's lead and the lead's terms, set in a form, over
// how its run is going.

type teamPage struct {
	team, name string
	run        *api.TeamRun
	form       *form
	// leads are the agents the Lead field's options stand for, "" for
	// none; isos their isolations. evalIsos are the isolations the eval's
	// isolation field's options name, "" for the lead's own.
	leads, isos, evalIsos []string
	busy                  bool
	err                   string
}

type teamRunMsg struct {
	team string
	run  api.TeamRun
	err  error
}

// openTeam opens team g's page, its run fetched first if it has a lead.
func (m *Model) openTeam(g liveGroup) tea.Cmd {
	m.teamPage = &teamPage{team: g.team, name: g.name}
	m.view = viewTeam
	m.setFocus(focusMain)
	for _, a := range g.agents {
		if a.Leads != nil {
			return m.fetchTeamRun(g.team)
		}
	}
	m.teamPage.form = m.leadForm(g, nil)
	return nil
}

func (m *Model) fetchTeamRun(team string) tea.Cmd {
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		run, err := be.TeamRun(ctx, team)
		return teamRunMsg{team, run, err}
	}
}

func (m *Model) teamRunArrived(msg teamRunMsg) {
	p := m.teamPage
	if p == nil || p.team != msg.team {
		return
	}
	g, _ := m.team(p.team)
	if msg.err != nil {
		p.err = msg.err.Error()
		p.form = m.leadForm(g, nil)
		return
	}
	p.run = &msg.run
	if p.form == nil {
		p.form = m.leadForm(g, p.run)
	}
}

// leadForm is the form for team g's lead and its terms, filled in from
// run, the team's run if it has a lead.
func (m *Model) leadForm(g liveGroup, run *api.TeamRun) *form {
	p := m.teamPage
	p.leads, p.isos = []string{""}, []string{""}
	options := []string{"none"}
	pick := 0
	for _, a := range g.agents {
		if a.MadeBy != "" {
			continue // a lead's agents lead nothing
		}
		if run != nil && (a.Line == run.LeadLine || a.ID == run.LeadLine) {
			pick = len(options)
		}
		p.leads, p.isos = append(p.leads, a.ID), append(p.isos, a.Isolation)
		options = append(options, fmt.Sprintf("%s (%s, %s)", shownID(a), title(a), a.Isolation))
	}
	lead := selectField("Lead", options, "makes agents of its own in the team, in its isolation, and hears when their turns end")
	lead.idx = pick
	// An eval isolation is another than the lead's: the one it has as the
	// form opens.
	p.evalIsos = []string{""}
	evalIn := []string{"the lead's own"}
	evalPick := 0
	for _, iso := range m.isolations {
		if iso.Name == p.isos[pick] {
			continue
		}
		if run != nil && iso.Name == run.EvalIsolation {
			evalPick = len(evalIn)
		}
		p.evalIsos, evalIn = append(p.evalIsos, iso.Name), append(evalIn, iso.Name)
	}
	evalIso := selectField("Eval isolation", evalIn, "another, with no services but control, scores a copy of the eval directory as a user who can change nothing else there; keep the grader there (its path in full: ~ is an empty home), take only the work from the copy; the lead hears only the score; an agent of yours there runs as the grader's owner")
	evalIso.idx = evalPick
	text := func(label, placeholder, hint, value string) *field {
		f := textField(label, placeholder, hint)
		f.text.SetValue(value)
		return f
	}
	num := func(n int) string {
		if run == nil {
			return ""
		}
		return strconv.Itoa(n)
	}
	var s struct {
		budget, eval, dir, target              string
		live, turn, ceiling, evalMin, patience int
	}
	if run != nil {
		t := run.Settings
		s.budget, s.eval, s.dir = budgetText(t.Budget), t.Eval, t.EvalDir
		s.live, s.turn, s.ceiling, s.evalMin, s.patience = t.MaxLive, t.TurnMinutes, t.PlanCeiling, t.EvalMinutes, t.Patience
		if t.Target != nil {
			s.target = strconv.FormatFloat(*t.Target, 'g', -1, 64)
		}
	}
	fields := []*field{
		lead,
		text("Budget", "none", "tokens sent anew and written by the lead and its agents, as 5M or 800k; none leaves only the plan ceiling", s.budget),
		text("Live at most", "3", "agents of the lead's live at once", num(s.live)),
		text("Turn limit", "60", "minutes a turn of one of them runs before gero stops it and asks for a report", num(s.turn)),
		text("Plan ceiling", "90", "percent of a plan window, the account's whole use (yours too), past which nothing new starts on it", num(s.ceiling)),
		text("Eval", "none", `a shell command; its last "SCORE: x" line is the score, higher better`, s.eval),
		text("Eval directory", "the lead's directory", "where in the lead's isolation the eval runs, or what is copied to the eval isolation", s.dir),
		evalIso,
		text("Eval limit", "20", "minutes the eval may run", num(s.evalMin)),
		text("Target", "none", "a score that ends the run once reached", s.target),
		text("Patience", "3", "evals in a row without a new best that end the run", num(s.patience)),
		selectField("Teams in its isolation", []string{"turn on if off", "leave as they are"}, "the lead acts only where its isolation has the team service"),
	}
	fields[0].focus(true)
	return &form{fields: fields}
}

func (m *Model) handleTeamKey(msg tea.KeyMsg) tea.Cmd {
	p, k := m.teamPage, m.keys
	if p.busy {
		return nil
	}
	switch {
	case k.Is(msg, "back"):
		m.closeMainPage()
		return nil
	case p.form == nil:
		return nil
	case k.Is(msg, "submit"), msg.String() == "enter" && p.form.cur == len(p.form.fields)-1:
		return m.saveLead()
	}
	cmd, _ := p.form.handle(k, msg)
	return cmd
}

type leadSavedMsg struct {
	flash string
	err   error
}

// saveLead sets the team's lead and its terms from the form, or takes its
// lead away. Setting a lead resumes a run gero stopped.
func (m *Model) saveLead() tea.Cmd {
	p := m.teamPage
	fs := p.form.fields
	lead, iso := p.leads[fs[0].idx], p.isos[fs[0].idx]
	be, team, name := m.be, p.team, p.name
	if lead == "" {
		if p.run == nil {
			p.err = "choose a lead, or close the page (" + m.keys.Label("back") + ")"
			return nil
		}
		p.busy, p.err = true, ""
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			return leadSavedMsg{fmt.Sprintf("team %q has no lead; the agents it made are yours", name), be.ClearLead(ctx, team)}
		}
	}
	req := api.PutLead{Agent: lead}
	n, err := parseBudget(fs[1].value())
	if err != nil {
		p.err = err.Error()
		return nil
	}
	req.Budget = &n
	for i, to := range []**int{&req.MaxLive, &req.TurnMinutes, &req.PlanCeiling} {
		if err := setNumber(fs[2+i], to); err != nil {
			p.err = err.Error()
			return nil
		}
	}
	eval := fs[5].value()
	req.Eval = &eval
	if d := fs[6].value(); d != "" {
		req.EvalDir = &d
	}
	evalIn := p.evalIsos[fs[7].idx]
	req.EvalIsolation = &evalIn
	if err := setNumber(fs[8], &req.EvalMinutes); err != nil {
		p.err = err.Error()
		return nil
	}
	if t := fs[9].value(); t == "" {
		req.NoTarget = true
	} else if v, err := strconv.ParseFloat(t, 64); err != nil {
		p.err = fmt.Sprintf("the target %q is not a number", t)
		return nil
	} else {
		req.Target = &v
	}
	if err := setNumber(fs[10], &req.Patience); err != nil {
		p.err = err.Error()
		return nil
	}
	turnOn := fs[11].idx == 0
	p.busy, p.err = true, ""
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		run, err := be.SetLead(ctx, team, req)
		if err != nil {
			return leadSavedMsg{err: err}
		}
		flash := fmt.Sprintf("%s leads team %q", run.Lead, name)
		if turnOn {
			xs, err := be.Exposures(ctx, iso)
			if err != nil {
				return leadSavedMsg{err: err}
			}
			on := false
			for _, x := range xs {
				on = on || x.Service == "team" && x.Enabled
			}
			if !on {
				if _, err := be.Expose(ctx, iso, "team", api.Expose{}); err != nil {
					return leadSavedMsg{err: fmt.Errorf("%s leads team %q, but teams could not be turned on in %s: %w", run.Lead, name, iso, err)}
				}
				flash += "; teams are on in " + iso
			}
		}
		return leadSavedMsg{flash: flash}
	}
}

func (m *Model) leadSaved(msg leadSavedMsg) tea.Cmd {
	p := m.teamPage
	if p == nil {
		return nil
	}
	p.busy = false
	if msg.err != nil {
		p.err = msg.err.Error()
		return nil
	}
	m.setFlash(msg.flash, false)
	m.closeMainPage()
	return m.fetchAgents()
}

// setNumber sets to from a whole number typed in f, if one is.
func setNumber(f *field, to **int) error {
	s := f.value()
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fmt.Errorf("%s: %q is not a whole number", strings.ToLower(f.label), s)
	}
	*to = &n
	return nil
}

// parseBudget reads a count of tokens: 5000000, 5M, 800k; nothing or
// none is 0, no budget.
func parseBudget(s string) (int64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "" || t == "none" {
		return 0, nil
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "m"):
		mult, t = 1e6, strings.TrimSuffix(t, "m")
	case strings.HasSuffix(t, "k"):
		mult, t = 1e3, strings.TrimSuffix(t, "k")
	}
	v, err := strconv.ParseFloat(t, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("the budget %q is not a count of tokens (5M, 800k, 2000000, none)", s)
	}
	return int64(v * mult), nil
}

// budgetText is a budget as it is typed back: 5M, 800k, the number, or
// nothing for none.
func budgetText(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n > 0 && n%1_000_000 == 0:
		return strconv.FormatInt(n/1_000_000, 10) + "M"
	case n > 0 && n%1_000 == 0:
		return strconv.FormatInt(n/1_000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

// spentOf is spent tokens, of budget if there is one.
func spentOf(spent, budget int64) string {
	if budget <= 0 {
		return tokenCount(spent) + " tokens, no budget"
	}
	return fmt.Sprintf("%s of %s tokens", tokenCount(spent), tokenCount(budget))
}

func (m *Model) renderTeam(w int) string {
	th, p, k := m.th, m.teamPage, m.keys
	var b strings.Builder
	b.WriteString(th.title.Render("Team "+p.name) + "\n\n")
	if r := p.run; r != nil {
		line := func(s string) { b.WriteString("  " + truncate(s, w-4) + "\n") }
		line(fmt.Sprintf("%s leads it: %s", r.Lead, r.Status))
		line(fmt.Sprintf("spent %s (%s more read from cache)", spentOf(r.Spent.New(), r.Settings.Budget), tokenCount(r.Spent.Cached)))
		for _, pw := range r.Plan {
			line(fmt.Sprintf("plan  %s %s %.0f%%", pw.Kind, pw.Window, pw.Percent))
		}
		if r.Settings.EvalSetBy != "" {
			line(fmt.Sprintf("the eval was set by agent %s, through control; saving it changed makes it yours", r.Settings.EvalSetBy))
		}
		for _, e := range r.Evals[max(len(r.Evals)-5, 0):] {
			score := "no score"
			if e.Score != nil {
				score = strconv.FormatFloat(*e.Score, 'g', -1, 64)
			}
			line(fmt.Sprintf("eval #%d %s  %s  (%s)", e.ID, e.At.Local().Format("Jan 2 15:04"), score, e.Why))
		}
		if n := len(r.Evals); n > 0 && r.Evals[n-1].Score == nil && r.Evals[n-1].Output != "" {
			out := strings.Split(r.Evals[n-1].Output, "\n")
			for _, l := range out[max(len(out)-4, 0):] {
				b.WriteString(th.dim.Render("    "+truncate(l, w-6)) + "\n")
			}
		}
		if r.Best != nil {
			line(fmt.Sprintf("best %g at #%d; %d since without a new one", *r.Best.Score, r.Best.ID, r.Stale))
		}
		for _, a := range r.Agents {
			s := fmt.Sprintf("%s %s (%s, %s) %s", "↳", a.ID, a.Role, a.Kind, a.Status)
			if a.Waits != "" {
				s += ": " + a.Waits
			}
			line(s)
		}
		if r.Archived > 0 {
			line(fmt.Sprintf("%d of its agents archived", r.Archived))
		}
		b.WriteString("\n")
	}
	switch {
	case p.form == nil:
		b.WriteString(th.working.Render("  "+spinner[m.frame%len(spinner)]+" reading the team's run…") + "\n")
		return b.String()
	default:
		b.WriteString(p.form.render(th, w))
	}
	hint := fmt.Sprintf("  %s save · %s next field · ←/→ change · %s close", k.Label("submit"), k.Label("focus_next"), k.Label("back"))
	if p.run != nil && p.run.Stopped != "" {
		hint += " · saving resumes the stopped run"
	}
	b.WriteString("\n" + th.dim.Render(hint) + "\n")
	switch {
	case p.busy:
		b.WriteString("\n" + th.working.Render(spinner[m.frame%len(spinner)]+" saving…") + "\n")
	case p.err != "":
		b.WriteString("\n" + th.err.Render("  "+p.err) + "\n")
	}
	return b.String()
}
