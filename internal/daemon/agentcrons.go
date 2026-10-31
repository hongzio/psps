package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/cron"
	"github.com/hongzio/gero/internal/proxy/cronproxy"
	"github.com/hongzio/gero/internal/store"
)

// Crons agents set themselves (internal/proxy/cronproxy). The daemon
// answers them on mail's port, at each agent's own address: an agent
// reaches its own crons only, and changes those it set.

// cronOn is whether isolation iso's agents may set crons.
func (m *Manager) cronOn(iso string) (bool, error) {
	if _, err := m.registry().Get(cronproxy.Name); err != nil {
		return false, nil
	}
	_, err := m.store.Exposure(iso, cronproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// cronEnv is an agent process's crons' address, while its isolation lets
// agents set crons.
func (m *Manager) cronEnv(a store.Agent) []string {
	if on, err := m.cronOn(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: cron token: %v", a.ID, err)
		return nil
	}
	return []string{"GERO_CRON_URL=" + cronproxy.URL(tok)}
}

// cronRoutes adds the agents' crons to isolation iso's mail port. Nothing
// it logs carries a path: the path carries the agent's token.
func (m *Manager) cronRoutes(mux *http.ServeMux, iso string) {
	mux.HandleFunc("GET /a/{token}/crons", func(w http.ResponseWriter, r *http.Request) {
		a, err := m.cronAgent(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.Crons(a.ID)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /a/{token}/crons", func(w http.ResponseWriter, r *http.Request) {
		a, err := m.cronAgent(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		var req api.PutCron
		if !cronRequest(w, r, &req) {
			return
		}
		c, err := m.addCron(a, req, true)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
		}
		reply(w, c, err)
	})
	mux.HandleFunc("PATCH /a/{token}/crons/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, id, err := m.cronOfAgent(iso, r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		var req api.PutCron
		if !cronRequest(w, r, &req) {
			return
		}
		c, err := m.updateCron(a, id, req, true)
		reply(w, c, err)
	})
	mux.HandleFunc("DELETE /a/{token}/crons/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, id, err := m.cronOfAgent(iso, r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		reply(w, nil, m.deleteCron(a, id, true))
	})
}

// cronAgent is the live agent of isolation iso whose address carries tok,
// while the isolation lets agents set crons.
func (m *Manager) cronAgent(iso, tok string) (store.Agent, error) {
	on, err := m.cronOn(iso)
	if err != nil {
		return store.Agent{}, err
	}
	name := m.isoName(iso)
	if !on {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("cron: isolation %s's agents set no crons; the operator turns that on with `gero iso expose %s cron`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("cron: this is not the address of an agent in isolation %s; use $GERO_CRON_URL as your process was started with it", name)}
	}
	return a, nil
}

func (m *Manager) cronOfAgent(iso string, r *http.Request) (store.Agent, int64, error) {
	a, err := m.cronAgent(iso, r.PathValue("token"))
	if err != nil {
		return a, 0, err
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return a, 0, badRequest(fmt.Errorf("cron: %q is not a cron's id", r.PathValue("id")))
	}
	return a, id, nil
}

// cronRequest reads a cron as an agent writes it, refusing what is not.
func cronRequest(w http.ResponseWriter, r *http.Request, req *api.PutCron) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, cronMaxPrompt+4096))
	if err == nil {
		err = json.Unmarshal(body, req)
	}
	if err != nil {
		reply(w, nil, badRequest(fmt.Errorf(`cron: the body is JSON, as {"schedule": "@every 1h", "prompt": "...", "if_busy": "skip"}: %v`, err)))
		return false
	}
	return true
}

// selfCron checks what an agent sets of a cron of its own: a schedule it
// may keep, and no more of them than it may have.
func (m *Manager) selfCron(c store.Cron, adding bool) error {
	if adding {
		n := 0
		cs, err := m.store.Crons(c.Agent)
		if err != nil {
			return err
		}
		for _, x := range cs {
			if x.Self {
				n++
			}
		}
		if n >= cronproxy.MaxCrons {
			return httpError{http.StatusTooManyRequests, fmt.Errorf("cron: you have %d crons of your own, as many as you may; remove one you no longer need", n)}
		}
	}
	s, err := cron.Parse(c.Schedule)
	if err != nil {
		return badRequest(err)
	}
	if tooOften(s, time.Now()) {
		return badRequest(fmt.Errorf("cron: %q comes more often than every %v, more than a cron of your own may", c.Schedule, cronproxy.MinGap))
	}
	return nil
}

// tooOften is whether s ever leaves less than cronproxy.MinGap between two
// runs, over its next few hundred.
func tooOften(s cron.Schedule, now time.Time) bool {
	if e := s.Every(); e > 0 {
		return e < cronproxy.MinGap
	}
	t := s.Next(now)
	for range 500 {
		next := s.Next(t)
		if t.IsZero() || next.IsZero() {
			return false
		}
		if next.Sub(t) < cronproxy.MinGap {
			return true
		}
		t = next
	}
	return false
}
