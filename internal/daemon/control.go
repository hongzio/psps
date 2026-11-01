package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/cron"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy/controlproxy"
	"github.com/hongzio/gero/internal/proxy/cronproxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

// The control service (internal/proxy/controlproxy): some of what the
// operator does with the gero command, opened to an isolation's agents by
// its env, on the agents of the isolations it reaches. The daemon answers
// it on mail's port, at each agent's own address, so what is done is done
// as that agent: what it makes, writes to or sets a cron on reads so, and
// the log says so. Nothing it logs carries a path: the path carries the
// agent's token.

// controlMax caps a body: a prompt or a message, as a lead's.
const controlMax = leadPromptMax

// controlScope is what an isolation's agents may do through the control
// service: what its env opens, on the agents of the isolations it reaches.
type controlScope struct {
	iso, name, env string
	controlproxy.Env
	// reach are the isolations' IDs, its own among them.
	reach map[string]bool
}

// controlOf is isolation iso's scope; false while it has no control
// service, or its env is gone.
func (m *Manager) controlOf(iso string) (controlScope, bool, error) {
	if _, err := m.registry().Get(controlproxy.Name); err != nil {
		return controlScope{}, false, nil
	}
	x, err := m.store.Exposure(iso, controlproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return controlScope{}, false, nil
	}
	if err != nil {
		return controlScope{}, false, err
	}
	e, err := m.store.Env(controlproxy.Name, x.Default)
	if errors.Is(err, store.ErrNotFound) {
		return controlScope{}, false, nil
	}
	if err != nil {
		return controlScope{}, false, err
	}
	s := controlScope{iso: iso, name: m.isoName(iso), env: x.Default, reach: map[string]bool{iso: true}}
	if err := json.Unmarshal(e.Config, &s.Env); err != nil {
		return controlScope{}, false, fmt.Errorf("control env %s: %w", x.Default, err)
	}
	var st controlproxy.Settings
	if len(x.Settings) > 0 {
		json.Unmarshal(x.Settings, &st)
	}
	for _, id := range st.Reach {
		s.reach[id] = true
	}
	return s, true, nil
}

// controlEnv is an agent process's control address, while its isolation
// has the control service.
func (m *Manager) controlEnv(a store.Agent) []string {
	if _, on, err := m.controlOf(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: control token: %v", a.ID, err)
		return nil
	}
	return []string{"GERO_CONTROL_URL=" + controlproxy.URL(tok)}
}

// controlCall is one request: the agent that makes it, and its scope.
type controlCall struct {
	me store.Agent
	s  controlScope
}

// need refuses what c's env does not open.
func (c controlCall) need(action string) error {
	if c.s.Opens(action) {
		return nil
	}
	return httpError{http.StatusForbidden, fmt.Errorf("control: %s is not open to the agents of isolation %s (its env %s opens %s); only your operator opens more",
		action, c.s.name, c.s.env, orNone(strings.Join(c.s.Allow, ", ")))}
}

// did logs what c did, to what.
func (m *Manager) did(c controlCall, action, what string) {
	m.log.Printf("control: %s of isolation %s: %s %s", shownID(c.me), c.s.name, action, what)
}

// by is how what c sets reads, saying whose it is.
func (c controlCall) by() string {
	return fmt.Sprintf("%s in isolation %s", shownID(c.me), c.s.name)
}

// controlCaller is the live agent of isolation iso whose address carries
// tok, and its scope.
func (m *Manager) controlCaller(iso, tok string) (controlCall, error) {
	s, on, err := m.controlOf(iso)
	if err != nil {
		return controlCall{}, err
	}
	name := m.isoName(iso)
	if !on {
		return controlCall{}, httpError{http.StatusForbidden, fmt.Errorf("control: isolation %s has no control; the operator turns it on with `gero iso expose %s control ENV`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return controlCall{}, httpError{http.StatusForbidden, fmt.Errorf("control: this is not the address of an agent in isolation %s; use $GERO_CONTROL_URL as your process was started with it", name)}
	}
	return controlCall{me: a, s: s}, nil
}

// reachAgent is the agent ref names among those c reaches, by its ID, its
// own ID, or a prefix of its key; the others are not there for it.
func (m *Manager) reachAgent(c controlCall, ref string) (store.Agent, error) {
	all, err := m.store.Agents()
	if err != nil {
		return store.Agent{}, err
	}
	var found []store.Agent
	for _, a := range all {
		if !c.s.reach[a.IsolationID] {
			continue
		}
		if a.ID == ref || (a.Handle != "" && a.Handle == ref) {
			return a, nil
		}
		if ref != "" && strings.HasPrefix(a.ID, ref) {
			found = append(found, a)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return store.Agent{}, httpError{http.StatusNotFound, fmt.Errorf("control: no agent you reach is %s (GET $GERO_CONTROL_URL/agents lists them)", ref)}
	}
	return store.Agent{}, badRequest(fmt.Errorf("control: %q is the start of more than one agent's ID; say more of it", ref))
}

// reachIsolation is the isolation named name, if c reaches it.
func (m *Manager) reachIsolation(c controlCall, name string) (store.Isolation, error) {
	iso, err := m.store.IsolationNamed(name)
	if err != nil || !c.s.reach[iso.ID] {
		return store.Isolation{}, httpError{http.StatusNotFound, fmt.Errorf("control: no isolation you reach is named %q (GET $GERO_CONTROL_URL lists them)", name)}
	}
	return iso, nil
}

// controlLive is how many agents made through the control service by the
// agents of isolation iso are live.
func (m *Manager) controlLive(iso string) (int, error) {
	all, err := m.store.Agents()
	if err != nil {
		return 0, err
	}
	in := map[string]string{}
	for _, a := range all {
		in[a.ID] = a.IsolationID
	}
	n := 0
	for _, a := range all {
		if a.MadeVia != "" && a.ArchivedAt == nil && in[a.MadeVia] == iso {
			n++
		}
	}
	return n, nil
}

// roomForOne refuses one more agent made through c's isolation's control
// past its env's limit. The caller holds controlMu.
func (m *Manager) roomForOne(c controlCall) error {
	n, err := m.controlLive(c.s.iso)
	if err != nil {
		return err
	}
	if n >= c.s.MaxLive {
		return httpError{http.StatusTooManyRequests, fmt.Errorf("control: %d agents made through it by the agents of isolation %s are live, as many as your operator allows; archive one that is done", n, c.s.name)}
	}
	return nil
}

// controlJSON reads r's body into v, refusing a key v does not have.
func controlJSON(r *http.Request, v any, example string) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, controlMax+4096+1))
	if err != nil {
		return badRequest(errors.New("control: the body could not be read"))
	}
	if len(body) > controlMax+4096 {
		return httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("control: a body is at most %d bytes; put the rest in a file and say where", controlMax)}
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return badRequest(fmt.Errorf("control: the body is JSON, as %s: %v", example, err))
	}
	return nil
}

// controlNew is an agent as an agent makes it. Approvals is there to be
// refused: an agent's approvals are the operator's.
type controlNew struct {
	Kind      driver.Kind `json:"kind"`
	Prompt    string      `json:"prompt"`
	Name      string      `json:"name"`
	Isolation string      `json:"isolation"`
	Workdir   string      `json:"workdir"`
	Repo      string      `json:"repo"`
	Model     string      `json:"model"`
	Effort    string      `json:"effort"`
	Team      string      `json:"team"`
	Approvals *string     `json:"approvals"`
}

var errApprovals = httpError{http.StatusForbidden, errors.New("control: an agent's approvals are your operator's: one you make takes yours in your own isolation, and asks them in another")}

// controlMake makes the agent req says, as c's.
func (m *Manager) controlMake(ctx context.Context, c controlCall, req controlNew) (api.Agent, error) {
	if req.Approvals != nil {
		return api.Agent{}, errApprovals
	}
	if req.Kind != driver.Claude && req.Kind != driver.Codex {
		return api.Agent{}, badRequest(fmt.Errorf(`control: "kind" is "claude" or "codex", not %q`, req.Kind))
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return api.Agent{}, badRequest(errors.New(`control: an agent starts on its "prompt", and it is empty`))
	}
	iso := store.Isolation{ID: c.s.iso, Name: c.s.name}
	if strings.TrimSpace(req.Isolation) != "" {
		var err error
		if iso, err = m.reachIsolation(c, strings.TrimSpace(req.Isolation)); err != nil {
			return api.Agent{}, err
		}
	}
	// Where the maker works it may already do what the agent would; anywhere
	// else, what the agent does is the operator's to approve.
	approvals := driver.ApprovalsAsk
	if iso.ID == c.me.IsolationID {
		approvals = c.me.Options.Approvals
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	if err := m.roomForOne(c); err != nil {
		return api.Agent{}, err
	}
	header := fmt.Sprintf("[gero control: agent %s made you, through gero's control service. Your operator may not be watching.]\n\n", c.by())
	a, err := m.createAgent(ctx, api.CreateAgent{Isolation: iso.Name, Kind: req.Kind, Prompt: header + prompt, Name: req.Name,
		Workdir: req.Workdir, Repo: req.Repo, Team: req.Team,
		Options: driver.Options{Model: strings.TrimSpace(req.Model), Effort: strings.TrimSpace(req.Effort), Approvals: approvals}},
		store.Agent{MadeVia: c.me.ID})
	if err != nil {
		return api.Agent{}, err
	}
	m.did(c, "agent.new", fmt.Sprintf("%s in isolation %s", shownID(a), iso.Name))
	return m.viewOf(a)
}

// controlCron is a cron as an agent sets one through the control service:
// one of at most cronproxy.MaxCrons of the agent's set by agents, none
// coming more often than cronproxy.MinGap.
func (m *Manager) controlCron(a store.Agent, c store.Cron, adding bool) error {
	if adding {
		cs, err := m.store.Crons(a.ID)
		if err != nil {
			return err
		}
		n := 0
		for _, x := range cs {
			if x.Self || x.SetBy != "" {
				n++
			}
		}
		if n >= cronproxy.MaxCrons {
			return httpError{http.StatusTooManyRequests, fmt.Errorf("control: %s has %d crons set by agents, as many as it may; remove one that is no longer needed", shownID(a), n)}
		}
	}
	s, err := cron.Parse(c.Schedule)
	if err != nil {
		return badRequest(err)
	}
	if tooOften(s, time.Now()) {
		return badRequest(fmt.Errorf("control: %q comes more often than every %v, more than a cron agents set may", c.Schedule, cronproxy.MinGap))
	}
	return nil
}

// agentsCron is a's cron id, if an agent set it: the operator's are theirs.
func (m *Manager) agentsCron(a store.Agent, id int64) (store.Cron, error) {
	c, err := m.store.Cron(a.ID, id)
	if err != nil {
		return c, err
	}
	if !c.Self && c.SetBy == "" {
		return c, httpError{http.StatusForbidden, fmt.Errorf("control: cron #%d of %s is your operator's; you change and remove those agents set", id, shownID(a))}
	}
	return c, nil
}

// controlTeam is the team ref names, whether c reaches every agent in it,
// and whether it reaches its lead, if it has one.
func (m *Manager) controlTeam(c controlCall, ref string) (store.Team, bool, bool, error) {
	t, err := m.store.Team(ref)
	if err != nil {
		return t, false, false, err
	}
	lines, err := m.store.LineTeams()
	if err != nil {
		return t, false, false, err
	}
	all, err := m.store.Agents()
	if err != nil {
		return t, false, false, err
	}
	l, err := m.store.Lead(t.ID)
	hasLead := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return t, false, false, err
	}
	every, lead := true, false
	for _, a := range all {
		line := cmp.Or(a.Line, a.ID)
		if lines[line] == t.ID && !c.s.reach[a.IsolationID] {
			every = false
		}
		if hasLead && line == l.Line && a.ArchivedAt == nil && c.s.reach[a.IsolationID] {
			lead = true
		}
	}
	return t, every, lead, nil
}

// controlRoutes adds the control service to isolation iso's mail port: the
// paths of the daemon's own API, under the agent's address.
func (m *Manager) controlRoutes(mux *http.ServeMux, iso string) {
	route := func(pattern, action string, f func(w http.ResponseWriter, r *http.Request, c controlCall)) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /a/{token}/control"+path, func(w http.ResponseWriter, r *http.Request) {
			c, err := m.controlCaller(iso, r.PathValue("token"))
			if err == nil && action != "" {
				err = c.need(action)
			}
			if err != nil {
				reply(w, nil, err)
				return
			}
			f(w, r, c)
		})
	}
	// agent is the agent the path names, among those c reaches.
	agent := func(w http.ResponseWriter, r *http.Request, c controlCall) (store.Agent, bool) {
		a, err := m.reachAgent(c, r.PathValue("id"))
		if err != nil {
			reply(w, nil, err)
			return a, false
		}
		return a, true
	}
	created := func(w http.ResponseWriter, v any, err error) {
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		reply(w, v, err)
	}

	route("GET ", "", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		v := api.ControlView{Env: c.s.env, Allow: c.s.Allow, Reach: []string{c.s.name}, MaxLive: c.s.MaxLive}
		isos, err := m.store.Isolations()
		if err != nil {
			reply(w, nil, err)
			return
		}
		for _, i := range isos {
			if c.s.reach[i.ID] && i.ID != c.s.iso {
				v.Reach = append(v.Reach, i.Name)
			}
		}
		v.Live, err = m.controlLive(c.s.iso)
		reply(w, v, err)
	})

	route("GET /agents", "agent.ls", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		all, err := m.Agents()
		out := []api.Agent{}
		for _, a := range all {
			if c.s.reach[a.IsolationID] {
				out = append(out, a)
			}
		}
		reply(w, out, err)
	})
	route("GET /agents/{id}", "agent.ls", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			v, err := m.Agent(a.ID)
			reply(w, v, err)
		}
	})
	route("GET /agents/{id}/events", "agent.log", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		last, _ := strconv.Atoi(r.URL.Query().Get("last"))
		evs, err := m.store.Events(a.ID, after, 0)
		if last > 0 && len(evs) > last {
			evs = evs[len(evs)-last:]
		}
		if evs == nil {
			evs = []event.Event{}
		}
		reply(w, evs, err)
	})
	route("POST /agents", "agent.new", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		var req controlNew
		if err := controlJSON(r, &req, `{"kind": "claude", "prompt": "..."}`); err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.controlMake(r.Context(), c, req)
		created(w, v, err)
	})
	route("POST /agents/{id}/send", "agent.send", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, controlMax+1))
		switch {
		case err != nil:
			err = badRequest(errors.New("control: the body could not be read"))
		case len(body) > controlMax:
			err = httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("control: a message is at most %d bytes; put the rest in a file and say where", controlMax)}
		case !utf8.Valid(body) || strings.TrimSpace(string(body)) == "":
			err = badRequest(errors.New("control: the body is the message, in UTF-8, and it is empty"))
		case cmp.Or(a.Line, a.ID) == cmp.Or(c.me.Line, c.me.ID):
			err = badRequest(errors.New("control: that is you; say it in your own turn"))
		case a.ArchivedAt != nil:
			err = httpError{http.StatusConflict, fmt.Errorf("control: %s is archived", shownID(a))}
		default:
			// Its isolation's sending budget, mail's: two agents writing to
			// each other spend it, whichever way they write.
			if ok, retry := m.mailBucket(c.s.iso).Allow(); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
				err = httpError{http.StatusTooManyRequests, fmt.Errorf("control: isolation %s's agents send at most %d messages a minute, mail included; wait and send again", c.s.name, mailproxy.PerMinute)}
				break
			}
			err = m.Send(a.ID, fmt.Sprintf("[gero control: from agent %s, through gero's control service]\n\n%s", c.by(), body))
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		m.did(c, "agent.send", shownID(a))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		reply(w, map[string]string{"sent": "it reads it as yours: at once if it is idle, otherwise within or after its turn"}, nil)
	})
	route("POST /agents/{id}/interrupt", "agent.interrupt", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			err := m.Interrupt(a.ID)
			if err == nil {
				m.did(c, "agent.interrupt", shownID(a))
			}
			reply(w, map[string]string{"interrupted": shownID(a)}, err)
		}
	})
	route("POST /agents/{id}/archive", "agent.archive", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			err := m.Archive(r.Context(), a.ID)
			if err == nil {
				m.did(c, "agent.archive", shownID(a))
			}
			reply(w, map[string]string{"archived": shownID(a)}, err)
		}
	})
	route("POST /agents/{id}/restore", "agent.restore", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		m.controlMu.Lock()
		defer m.controlMu.Unlock()
		if a.MadeVia != "" {
			if maker, err := m.store.Agent(a.MadeVia); err == nil && maker.IsolationID == c.s.iso {
				if err := m.roomForOne(c); err != nil {
					reply(w, nil, err)
					return
				}
			}
		}
		v, err := m.Restore(r.Context(), a.ID)
		if err == nil {
			m.did(c, "agent.restore", shownID(a))
		}
		reply(w, v, err)
	})
	route("POST /agents/{id}/fork", "agent.fork", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		m.controlMu.Lock()
		defer m.controlMu.Unlock()
		if err := m.roomForOne(c); err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.fork(r.Context(), a.ID, c.me.ID)
		if err == nil {
			m.did(c, "agent.fork", fmt.Sprintf("%s as %s", shownID(a), v.ID))
		}
		created(w, v, err)
	})
	route("PATCH /agents/{id}", "", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		var req struct {
			Name *string `json:"name"`
			Team *string `json:"team"`
		}
		err := controlJSON(r, &req, `{"name": "profiler"} or {"team": "lab"}`)
		switch {
		case err != nil:
		case req.Name == nil && req.Team == nil:
			err = badRequest(errors.New(`control: say a "name" or a "team"`))
		case req.Name != nil && c.need("agent.rename") != nil:
			err = c.need("agent.rename")
		case req.Team != nil && c.need("agent.team") != nil:
			err = c.need("agent.team")
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.UpdateAgent(a.ID, api.UpdateAgent{Name: req.Name, Team: req.Team})
		if err == nil {
			if req.Name != nil {
				m.did(c, "agent.rename", fmt.Sprintf("%s to %q", shownID(a), *req.Name))
			}
			if req.Team != nil {
				m.did(c, "agent.team", fmt.Sprintf("%s to %q", shownID(a), *req.Team))
			}
		}
		reply(w, v, err)
	})
	route("PUT /agents/{id}/options", "agent.set", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		var req struct {
			Model     *string `json:"model"`
			Effort    *string `json:"effort"`
			Approvals *string `json:"approvals"`
		}
		err := controlJSON(r, &req, `{"model": "...", "effort": "high"}`)
		if err == nil && req.Approvals != nil {
			err = errApprovals
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		o := a.Options
		if req.Model != nil {
			o.Model = strings.TrimSpace(*req.Model)
		}
		if req.Effort != nil {
			o.Effort = strings.TrimSpace(*req.Effort)
		}
		err = m.SetOptions(a.ID, o)
		if err == nil {
			m.did(c, "agent.set", fmt.Sprintf("%s to model %q, effort %q", shownID(a), o.Model, o.Effort))
		}
		reply(w, nil, err)
	})

	route("GET /agents/{id}/crons", "agent.cron", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			v, err := m.Crons(a.ID)
			reply(w, v, err)
		}
	})
	route("POST /agents/{id}/crons", "agent.cron", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		var req api.PutCron
		if err := controlJSON(r, &req, `{"schedule": "@every 2h", "prompt": "...", "if_busy": "skip"}`); err != nil {
			reply(w, nil, err)
			return
		}
		if a.ArchivedAt != nil {
			reply(w, nil, badRequest(errors.New("control: an archived agent is sent nothing")))
			return
		}
		cr := store.Cron{Agent: a.ID, IfBusy: store.CronSkip, Enabled: true, SetBy: c.by()}
		if req.Schedule == nil || req.Prompt == nil {
			reply(w, nil, badRequest(errors.New("control: a cron needs a schedule and a prompt")))
			return
		}
		err := setCron(&cr, req, time.Now())
		if err == nil {
			err = m.controlCron(a, cr, true)
		}
		if err == nil {
			cr, err = m.store.AddCron(cr)
		}
		if err == nil {
			m.cronsChanged(a.ID)
			m.did(c, "agent.cron", fmt.Sprintf("%s: added #%d, %q", shownID(a), cr.ID, cr.Schedule))
		}
		created(w, cr, err)
	})
	cronOf := func(w http.ResponseWriter, r *http.Request, c controlCall) (store.Agent, store.Cron, bool) {
		a, ok := agent(w, r, c)
		if !ok {
			return a, store.Cron{}, false
		}
		id, err := strconv.ParseInt(r.PathValue("cron"), 10, 64)
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf("control: %q is not a cron's number", r.PathValue("cron"))))
			return a, store.Cron{}, false
		}
		cr, err := m.agentsCron(a, id)
		if err != nil {
			reply(w, nil, err)
			return a, cr, false
		}
		return a, cr, true
	}
	route("PATCH /agents/{id}/crons/{cron}", "agent.cron", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, cr, ok := cronOf(w, r, c)
		if !ok {
			return
		}
		var req api.PutCron
		err := controlJSON(r, &req, `{"enabled": false}`)
		if err == nil {
			next := cr
			if err = setCron(&next, req, time.Now()); err == nil {
				err = m.controlCron(a, next, false)
			}
		}
		if err == nil {
			cr, err = m.updateCron(a, cr.ID, req, false)
		}
		if err == nil {
			m.did(c, "agent.cron", fmt.Sprintf("%s: changed #%d", shownID(a), cr.ID))
		}
		reply(w, cr, err)
	})
	route("DELETE /agents/{id}/crons/{cron}", "agent.cron", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, cr, ok := cronOf(w, r, c)
		if !ok {
			return
		}
		err := m.deleteCron(a, cr.ID, false)
		if err == nil {
			m.did(c, "agent.cron", fmt.Sprintf("%s: removed #%d", shownID(a), cr.ID))
		}
		reply(w, nil, err)
	})

	route("GET /agents/{id}/inbox", "agent.inbox", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			v, err := m.Inbox(a.ID)
			reply(w, v, err)
		}
	})
	route("DELETE /agents/{id}/inbox", "agent.inbox", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		if a, ok := agent(w, r, c); ok {
			n, err := m.ClearInbox(a.ID)
			if err == nil {
				m.did(c, "agent.inbox", fmt.Sprintf("%s: cleared %d", shownID(a), n))
			}
			reply(w, api.Cleared{Removed: n}, err)
		}
	})
	route("DELETE /agents/{id}/inbox/{mail}", "agent.inbox", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		a, ok := agent(w, r, c)
		if !ok {
			return
		}
		id, err := strconv.ParseInt(r.PathValue("mail"), 10, 64)
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf("control: %q is not a mail's number", r.PathValue("mail"))))
			return
		}
		err = m.DiscardMail(a.ID, id)
		if err == nil {
			m.did(c, "agent.inbox", fmt.Sprintf("%s: discarded #%d", shownID(a), id))
		}
		reply(w, nil, err)
	})

	route("GET /isolations", "iso.ls", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		all, err := m.Isolations(r.Context())
		out := []api.Isolation{}
		for _, i := range all {
			if c.s.reach[i.ID] {
				out = append(out, i)
			}
		}
		reply(w, out, err)
	})
	route("GET /isolations/{name}/services", "iso.services", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		iso, err := m.reachIsolation(c, r.PathValue("name"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.Exposures(iso.Name)
		reply(w, v, err)
	})

	route("GET /teams", "team.ls", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		teams, err := m.store.Teams()
		if err != nil {
			reply(w, nil, err)
			return
		}
		lines, err := m.store.LineTeams()
		if err != nil {
			reply(w, nil, err)
			return
		}
		all, err := m.store.Agents()
		if err != nil {
			reply(w, nil, err)
			return
		}
		has, seen := map[string]bool{}, map[string]bool{}
		for _, a := range all {
			t := lines[cmp.Or(a.Line, a.ID)]
			seen[t] = true
			if c.s.reach[a.IsolationID] {
				has[t] = true
			}
		}
		out := []store.Team{}
		for _, t := range teams {
			if has[t.ID] || !seen[t.ID] {
				out = append(out, t)
			}
		}
		reply(w, out, nil)
	})
	route("GET /teams/{team}/run", "team.show", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		t, _, lead, err := m.controlTeam(c, r.PathValue("team"))
		if err == nil && !lead {
			err = httpError{http.StatusNotFound, fmt.Errorf("control: team %q has no lead you reach", t.Name)}
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.TeamRun(t.ID)
		reply(w, leadsView(v), err)
	})
	route("POST /teams", "team.new", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		var req api.PutTeam
		if err := controlJSON(r, &req, `{"name": "lab"}`); err != nil {
			reply(w, nil, err)
			return
		}
		t, err := m.CreateTeam(req.Name)
		if err == nil {
			m.did(c, "team.new", strconv.Quote(t.Name))
		}
		created(w, t, err)
	})
	// whole is the team the path names, where c reaches all its agents.
	whole := func(w http.ResponseWriter, r *http.Request, c controlCall) (store.Team, bool) {
		t, every, _, err := m.controlTeam(c, r.PathValue("team"))
		if err == nil && !every {
			err = httpError{http.StatusForbidden, fmt.Errorf("control: team %q has agents you do not reach; it is your operator's to change", t.Name)}
		}
		if err != nil {
			reply(w, nil, err)
			return t, false
		}
		return t, true
	}
	route("PATCH /teams/{team}", "team.rename", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		t, ok := whole(w, r, c)
		if !ok {
			return
		}
		var req api.PutTeam
		if err := controlJSON(r, &req, `{"name": "lab 2"}`); err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.RenameTeam(t.ID, req.Name)
		if err == nil {
			m.did(c, "team.rename", fmt.Sprintf("%q to %q", t.Name, v.Name))
		}
		reply(w, v, err)
	})
	route("DELETE /teams/{team}", "team.rm", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		t, ok := whole(w, r, c)
		if !ok {
			return
		}
		err := m.DeleteTeam(t.ID)
		if err == nil {
			m.did(c, "team.rm", strconv.Quote(t.Name))
		}
		reply(w, nil, err)
	})
	route("POST /teams/{team}/eval", "team.eval", func(w http.ResponseWriter, r *http.Request, c controlCall) {
		t, _, lead, err := m.controlTeam(c, r.PathValue("team"))
		if err == nil && !lead {
			err = httpError{http.StatusNotFound, fmt.Errorf("control: team %q has no lead you reach", t.Name)}
		}
		if err == nil {
			err = m.EvalTeam(t.ID)
		}
		if err != nil {
			reply(w, nil, err)
			return
		}
		m.did(c, "team.eval", strconv.Quote(t.Name))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		reply(w, map[string]string{"eval": "running; its result goes to the team's lead"}, nil)
	})
}
