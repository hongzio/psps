package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/teamproxy"
	"github.com/hongzio/psps/internal/store"
)

// The team service (internal/proxy/teamproxy): a lead's own address for
// its team, on mail's port. The daemon answers it itself, since it keeps
// the agents. A lead reaches the agents it made, and no other.

// teamOn is whether isolation iso has the team service.
func (m *Manager) teamOn(iso string) (bool, error) {
	if _, err := m.registry().Get(teamproxy.Name); err != nil {
		return false, nil
	}
	_, err := m.store.Exposure(iso, teamproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// teamEnv is an agent process's team address, while its isolation has the
// team service. Every agent gets it, so a lead made later needs no new
// process; only a lead's answers.
func (m *Manager) teamEnv(a store.Agent) []string {
	if on, err := m.teamOn(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: team token: %v", a.ID, err)
		return nil
	}
	return proxy.AgentEnv("TEAM_URL", teamproxy.URL(tok), m.fromGero(a.IsolationID))
}

// teamRoutes adds the leads' teams to isolation iso's mail port. Nothing
// it logs carries a path: the path carries the agent's token.
func (m *Manager) teamRoutes(mux *http.ServeMux, iso string) {
	lead := func(w http.ResponseWriter, r *http.Request) (store.Agent, store.Lead, bool) {
		a, l, err := m.teamLead(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return a, l, false
		}
		return a, l, true
	}
	mux.HandleFunc("GET /a/{token}/team", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		t, err := m.store.Team(l.Team)
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.teamRun(t, l)
		reply(w, leadsView(v), err)
	})
	mux.HandleFunc("POST /a/{token}/team/agents", func(w http.ResponseWriter, r *http.Request) {
		a, l, ok := lead(w, r)
		if !ok {
			return
		}
		var req api.TeamAgentNew
		body, err := io.ReadAll(io.LimitReader(r.Body, leadPromptMax+4096))
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`team: the body is JSON, as {"role": "profiler", "kind": "claude", "prompt": "..."}: %v`, err)))
			return
		}
		mode, err := teamWatchQuery(r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.leadMake(r.Context(), a, l, req, mode)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		reply(w, v, err)
	})
	mux.HandleFunc("GET /a/{token}/team/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		a, err := m.leadsAgent(l, r.PathValue("id"), false)
		if err != nil {
			reply(w, nil, err)
			return
		}
		spent, err := m.store.SpentOf()
		reply(w, m.teamAgent(a, spent[a.ID], true), err)
	})
	mux.HandleFunc("POST /a/{token}/team/agents/{id}/send", func(w http.ResponseWriter, r *http.Request) {
		lead, l, ok := lead(w, r)
		if !ok {
			return
		}
		mode, err := teamWatchQuery(r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, leadPromptMax+1))
		switch {
		case err != nil:
			err = badRequest(errors.New("team: the body could not be read"))
		case len(body) > leadPromptMax:
			err = httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("team: a message is at most %d bytes; put the rest in a file and say where", leadPromptMax)}
		case !utf8.Valid(body) || strings.TrimSpace(string(body)) == "":
			err = badRequest(errors.New("team: the body is the message, in UTF-8, and it is empty"))
		default:
			err = m.leadSend(lead, l, r.PathValue("id"), string(body), mode)
		}
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			reply(w, map[string]string{"sent": "it reads it as your message: at once if it is idle, otherwise within or after its turn"}, nil)
			return
		}
		reply(w, nil, err)
	})
	mux.HandleFunc("POST /a/{token}/team/agents/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		a, err := m.leadsAgent(l, r.PathValue("id"), true)
		var v api.TeamRelease
		if err == nil {
			var rt *runtime
			if rt, err = m.runtime(a.ID); err == nil {
				v, err = rt.teamRelease()
			}
		}
		if err == nil {
			m.teamChanged(l.Team)
		}
		reply(w, v, err)
	})
	mux.HandleFunc("POST /a/{token}/team/agents/{id}/interrupt", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		a, err := m.leadsAgent(l, r.PathValue("id"), true)
		if err == nil {
			err = m.Interrupt(a.ID)
		}
		reply(w, map[string]string{"interrupted": shownID(a)}, err)
	})
	mux.HandleFunc("POST /a/{token}/team/agents/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		a, err := m.leadsAgent(l, r.PathValue("id"), true)
		if err == nil {
			err = m.Archive(r.Context(), a.ID)
		}
		reply(w, map[string]string{"archived": shownID(a)}, err)
	})
	mux.HandleFunc("POST /a/{token}/team/eval", func(w http.ResponseWriter, r *http.Request) {
		_, l, ok := lead(w, r)
		if !ok {
			return
		}
		said, err := m.leadEval(l)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
		}
		reply(w, map[string]string{"eval": said}, err)
	})
}

// teamLead is the live agent of isolation iso whose address carries tok,
// and the team it leads, while the isolation has the team service.
func (m *Manager) teamLead(iso, tok string) (store.Agent, store.Lead, error) {
	on, err := m.teamOn(iso)
	if err != nil {
		return store.Agent{}, store.Lead{}, err
	}
	name := m.isoName(iso)
	if !on {
		return store.Agent{}, store.Lead{}, httpError{http.StatusForbidden, fmt.Errorf("team: isolation %s has no teams; the operator turns them on with `psps iso expose %s team`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return store.Agent{}, store.Lead{}, httpError{http.StatusForbidden, fmt.Errorf("team: this is not the address of an agent in isolation %s; use $PSPS_TEAM_URL as your process was started with it", name)}
	}
	l, err := m.store.LeadOfLine(cmp.Or(a.Line, a.ID))
	if errors.Is(err, store.ErrNotFound) {
		return a, l, httpError{http.StatusForbidden, errors.New("team: you lead no team; the operator makes an agent a team's lead with `psps team lead TEAM AGENT`")}
	}
	return a, l, err
}

// leadsAgent is the agent l made that id names, live if live.
func (m *Manager) leadsAgent(l store.Lead, id string, live bool) (store.Agent, error) {
	a, err := m.storedAgent(id)
	if err != nil || a.MadeBy != l.Line {
		return store.Agent{}, fmt.Errorf("team: no agent of yours is %s (GET $PSPS_TEAM_URL lists them): %w", id, store.ErrNotFound)
	}
	if live && a.ArchivedAt != nil {
		return a, httpError{http.StatusConflict, fmt.Errorf("team: %s is archived", shownID(a))}
	}
	return a, nil
}

// leadMake makes an agent of lead's own in its team, within its
// operator's terms.
func (m *Manager) leadMake(ctx context.Context, lead store.Agent, l store.Lead, req api.TeamAgentNew, mode string) (api.TeamAgent, error) {
	role, err := agentName(req.Role)
	if err != nil {
		return api.TeamAgent{}, badRequest(errors.New(`team: say what the agent is for, as its "role"`))
	}
	if len([]rune(role)) > 40 {
		return api.TeamAgent{}, badRequest(errors.New("team: a role is at most 40 characters"))
	}
	if req.Kind != driver.Claude && req.Kind != driver.Codex {
		return api.TeamAgent{}, badRequest(fmt.Errorf(`team: "kind" is "claude" or "codex", not %q`, req.Kind))
	}
	prompt := strings.TrimSpace(req.Prompt)
	switch {
	case prompt == "":
		return api.TeamAgent{}, badRequest(errors.New(`team: an agent starts on its "prompt", and it is empty`))
	case len(prompt) > leadPromptMax:
		return api.TeamAgent{}, httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("team: a prompt is at most %d bytes; put the rest in a file and say where", leadPromptMax)}
	}
	// As SetLead asks it of the lead's own CLI, before the lock.
	perUse := ""
	if l.Settings.Budget == 0 {
		perUse = m.perUse(ctx, lead.IsolationID, req.Kind)
	}
	m.teamMu.Lock()
	defer m.teamMu.Unlock()
	if l, err = m.store.Lead(l.Team); err != nil {
		return api.TeamAgent{}, err
	}
	turn := cmp.Or(req.TurnMinutes, l.Settings.TurnMinutes)
	if turn < 1 || turn > 24*60 {
		return api.TeamAgent{}, badRequest(fmt.Errorf("team: a turn's limit is 1 to %d minutes, not %d", 24*60, turn))
	}
	if l.Stopped != "" {
		return api.TeamAgent{}, httpError{http.StatusConflict, fmt.Errorf("team: psps stopped the run (%s); only your operator resumes it", l.Stopped)}
	}
	agents, spent, err := m.spentNow()
	if err != nil {
		return api.TeamAgent{}, err
	}
	live := 0
	for _, a := range agents {
		if a.MadeBy == l.Line && a.ArchivedAt == nil {
			live++
		}
	}
	if live >= l.Settings.MaxLive {
		return api.TeamAgent{}, httpError{http.StatusTooManyRequests, fmt.Errorf("team: %d agents of yours are live, as many as your operator allows; archive one you are done with", live)}
	}
	if used := teamSpent(l, agents, spent).New(); overBudget(l, used) {
		return api.TeamAgent{}, httpError{http.StatusTooManyRequests, fmt.Errorf("team: the team has used its budget, %s of %s tokens", tokens(used), tokens(l.Settings.Budget))}
	}
	if l.Settings.Budget == 0 && perUse != "" {
		return api.TeamAgent{}, httpError{http.StatusForbidden, fmt.Errorf("team: your team has no budget, so only the plan ceiling holds it back, and %s; make the other kind, or ask your operator for a budget", perUse)}
	}
	if why := m.planFull(lead.IsolationID, req.Kind, l.Settings.PlanCeiling); why != "" {
		return api.TeamAgent{}, httpError{http.StatusTooManyRequests, errors.New("team: " + why)}
	}
	t, err := m.store.Team(l.Team)
	if err != nil {
		return api.TeamAgent{}, err
	}
	header := fmt.Sprintf("[psps team: you are %q in team %q, an agent its lead %s made. Your operator may not be watching. When a turn of yours ends, psps gives your lead your last answer, so end each turn with what it needs: what you did, what you measured and how, what is unfinished, and where your notes are. A turn of yours may run %d minutes.]\n\n",
		role, t.Name, shownID(lead), turn)
	opts := driver.Options{Model: strings.TrimSpace(req.Model), Effort: strings.TrimSpace(req.Effort), Approvals: lead.Options.Approvals}
	base := roleSlug(role)
	var a store.Agent
	for try := 0; ; try++ {
		a, err = m.createAgent(ctx, api.CreateAgent{Isolation: m.isoName(lead.IsolationID), Kind: req.Kind, Prompt: header + prompt, Name: role,
			Handle: forkHandle(base), Workdir: cmp.Or(strings.TrimSpace(req.Workdir), lead.Workdir), Options: opts, Team: t.Name},
			store.Agent{MadeBy: l.Line, TurnMinutes: turn})
		if !errors.Is(err, store.ErrExists) || try == 4 {
			break
		}
	}
	if err != nil {
		return api.TeamAgent{}, err
	}
	m.log.Printf("team %s: %s made %s (%s)", l.Team, l.Line, a.ID, role)
	if rt, err := m.runtime(a.ID); err == nil {
		rt.teamMode(mode)
	}
	return m.teamAgent(a, store.Spent{}, false), nil
}

// leadSend writes text to the agent of l's that id names, as l's message.
func (m *Manager) leadSend(lead store.Agent, l store.Lead, id, text, mode string) error {
	a, err := m.leadsAgent(l, id, true)
	if err != nil {
		return err
	}
	if l.Stopped != "" {
		return httpError{http.StatusConflict, fmt.Errorf("team: psps stopped the run (%s); only your operator resumes it", l.Stopped)}
	}
	agents, spent, err := m.spentNow()
	if err != nil {
		return err
	}
	if used := teamSpent(l, agents, spent).New(); overBudget(l, used) {
		return httpError{http.StatusTooManyRequests, fmt.Errorf("team: the team has used its budget, %s of %s tokens", tokens(used), tokens(l.Settings.Budget))}
	}
	rt, err := m.runtime(a.ID)
	if err != nil {
		return err
	}
	rt.teamMode(mode)
	return rt.send(fmt.Sprintf("[psps team: from your lead %s]\n\n%s", shownID(lead), text))
}

// teamWatchQuery is how a lead asks its agent's turns that end with tasks
// in the background to be held, with ?watch=: settle unless it says turn.
func teamWatchQuery(r *http.Request) (string, error) {
	switch mode := strings.TrimSpace(r.URL.Query().Get("watch")); mode {
	case "", store.WatchSettle:
		return store.WatchSettle, nil
	case store.WatchTurn:
		return mode, nil
	default:
		return "", badRequest(fmt.Errorf(`team: watch is "settle" (the default: it is at work until a turn of it ends with nothing in the background) or "turn" (each turn's end is news), not %q`, mode))
	}
}

// roleSlug is a role as the start of an agent's ID.
func roleSlug(role string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(role) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 20 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "agent"
	}
	return s
}
