package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

// Handler is the daemon's HTTP API. The TUI reaches it over the unix socket
// and the web UI will over TCP; both see the same routes.
func (m *Manager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		reply(w, api.Health{OK: true, Version: api.Version, PID: os.Getpid(), Web: m.webURL, WebError: m.webErr}, nil)
	})

	mux.HandleFunc("GET /v1/isolations", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Isolations(r.Context())
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/isolations/{name}", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Isolation(r.Context(), r.PathValue("name"))
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/isolations", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateIsolation
		if !decode(w, r, &req) {
			return
		}
		v, err := m.CreateIsolation(r.Context(), req, nil)
		reply(w, v, err)
	})
	mux.HandleFunc("PATCH /v1/isolations/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateIsolation
		if !decode(w, r, &req) {
			return
		}
		v, err := m.UpdateIsolation(r.Context(), r.PathValue("name"), req)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/isolations/{name}/upgrade", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.UpgradeIsolation(r.Context(), r.PathValue("name"), nil)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/isolations/{name}/verify", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Verify(r.Context(), r.PathValue("name"))
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /v1/isolations/{name}", func(w http.ResponseWriter, r *http.Request) {
		err := m.DeleteIsolation(r.Context(), r.PathValue("name"), r.URL.Query().Get("purge") == "1")
		reply(w, nil, err)
	})
	mux.HandleFunc("PATCH /v1/isolations/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		var req api.SetEnv
		if !decode(w, r, &req) {
			return
		}
		reply(w, nil, m.SetEnv(r.PathValue("name"), req))
	})

	mux.HandleFunc("GET /v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Accounts()
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/account-kinds", func(w http.ResponseWriter, r *http.Request) {
		reply(w, api.AccountKinds, nil)
	})
	putAccount := func(create bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var req api.PutAccount
			if !decode(w, r, &req) {
				return
			}
			v, err := m.PutAccount(r.PathValue("name"), req, create)
			reply(w, v, err)
		}
	}
	mux.HandleFunc("POST /v1/accounts/{name}", putAccount(true))
	mux.HandleFunc("PATCH /v1/accounts/{name}", putAccount(false))
	mux.HandleFunc("DELETE /v1/accounts/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.DeleteAccount(r.PathValue("name"), r.URL.Query().Get("force") == "1"))
	})
	mux.HandleFunc("PUT /v1/isolations/{name}/clis/{tool}/account", func(w http.ResponseWriter, r *http.Request) {
		var req api.UseAccount
		if !decode(w, r, &req) {
			return
		}
		reply(w, nil, m.UseAccount(r.PathValue("name"), r.PathValue("tool"), req.Account))
	})

	mux.HandleFunc("GET /v1/isolations/{name}/clis", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.CLIs(r.Context(), r.PathValue("name"))
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/isolations/{name}/clis/{tool}/login", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.StartCLILogin(r.Context(), r.PathValue("name"), r.PathValue("tool"))
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/isolations/{name}/clis/{tool}/login", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.CLILogin(r.PathValue("name"), r.PathValue("tool"))
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/isolations/{name}/clis/{tool}/login/code", func(w http.ResponseWriter, r *http.Request) {
		var req api.CLICode
		if !decode(w, r, &req) {
			return
		}
		v, err := m.SendCLICode(r.Context(), r.PathValue("name"), r.PathValue("tool"), req.Code)
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /v1/isolations/{name}/clis/{tool}/login", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.CancelCLILogin(r.PathValue("name"), r.PathValue("tool")))
	})
	mux.HandleFunc("POST /v1/isolations/{name}/clis/{tool}/logout", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.CLILogout(r.Context(), r.PathValue("name"), r.PathValue("tool")))
	})

	mux.HandleFunc("GET /v1/services", func(w http.ResponseWriter, r *http.Request) {
		reply(w, m.Services(), nil)
	})
	mux.HandleFunc("GET /v1/envs", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Envs(r.URL.Query().Get("service"))
		reply(w, v, err)
	})
	putEnv := func(create bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var p proxy.Patch
			if !decode(w, r, &p) {
				return
			}
			v, err := m.PutEnv(r.Context(), r.PathValue("service"), r.PathValue("name"), p, create)
			reply(w, v, err)
		}
	}
	mux.HandleFunc("POST /v1/envs/{service}/{name}", putEnv(true))
	mux.HandleFunc("PATCH /v1/envs/{service}/{name}", putEnv(false))
	mux.HandleFunc("DELETE /v1/envs/{service}/{name}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.DeleteEnv(r.Context(), r.PathValue("service"), r.PathValue("name"), r.URL.Query().Get("force") == "1"))
	})
	mux.HandleFunc("PUT /v1/envs/{service}/{name}/secret", func(w http.ResponseWriter, r *http.Request) {
		var v map[string]string
		if !decode(w, r, &v) {
			return
		}
		if v == nil {
			v = map[string]string{}
		}
		reply(w, nil, m.SetSecret(r.PathValue("service"), r.PathValue("name"), v))
	})
	mux.HandleFunc("DELETE /v1/envs/{service}/{name}/secret", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.SetSecret(r.PathValue("service"), r.PathValue("name"), nil))
	})
	mux.HandleFunc("POST /v1/envs/{service}/{name}/login", func(w http.ResponseWriter, r *http.Request) {
		var req api.LoginStart
		if !decode(w, r, &req) {
			return
		}
		v, err := m.StartLogin(r.Context(), r.PathValue("service"), r.PathValue("name"), req.Redirect)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/envs/{service}/{name}/login/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req api.LoginFinish
		if !decode(w, r, &req) {
			return
		}
		reply(w, nil, m.FinishLogin(r.Context(), r.PathValue("service"), r.PathValue("name"), r.PathValue("id"), req.Callback))
	})
	mux.HandleFunc("POST /v1/services/{service}/choices/{field}", func(w http.ResponseWriter, r *http.Request) {
		var req api.ChoicesRequest
		if !decode(w, r, &req) {
			return
		}
		v, err := m.Choices(r.Context(), r.PathValue("service"), r.PathValue("field"), req)
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/envs/{service}/{name}/check", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.CheckEnv(r.Context(), r.PathValue("service"), r.PathValue("name"))
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/isolations/{name}/services", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Exposures(r.PathValue("name"))
		reply(w, v, err)
	})
	mux.HandleFunc("PUT /v1/isolations/{name}/services/{service}", func(w http.ResponseWriter, r *http.Request) {
		var req api.Expose
		if !decode(w, r, &req) {
			return
		}
		v, err := m.Expose(r.Context(), r.PathValue("name"), r.PathValue("service"), req)
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /v1/isolations/{name}/services/{service}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Unexpose(r.Context(), r.PathValue("name"), r.PathValue("service")))
	})

	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Agents()
		reply(w, v, err)
	})
	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		var req api.CreateAgent
		if !decode(w, r, &req) {
			return
		}
		a, err := m.CreateAgent(r.Context(), req)
		if err != nil {
			reply(w, nil, err)
			return
		}
		v, err := m.Agent(a.ID)
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, err := m.Agent(r.PathValue("id"))
		reply(w, v, err)
	})
	mux.HandleFunc("PATCH /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req api.UpdateAgent
		if !decode(w, r, &req) {
			return
		}
		v, err := m.UpdateAgent(r.PathValue("id"), req)
		reply(w, v, err)
	})
	mux.HandleFunc("GET /v1/agents/{id}/events", m.handleEvents)
	mux.HandleFunc("GET /v1/events", m.handleEvents)
	mux.HandleFunc("POST /v1/agents/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var req api.Message
		if !decode(w, r, &req) {
			return
		}
		reply(w, nil, m.Send(r.PathValue("id"), req.Text))
	})
	mux.HandleFunc("POST /v1/agents/{id}/approvals/{approval}", func(w http.ResponseWriter, r *http.Request) {
		var req api.Decision
		if !decode(w, r, &req) {
			return
		}
		d, err := driver.ParseDecision(string(req.Decision))
		if err != nil {
			reply(w, nil, badRequest(err))
			return
		}
		reply(w, nil, m.Approve(r.PathValue("id"), r.PathValue("approval"), d))
	})
	mux.HandleFunc("POST /v1/agents/{id}/interrupt", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Interrupt(r.PathValue("id")))
	})
	mux.HandleFunc("PUT /v1/agents/{id}/options", func(w http.ResponseWriter, r *http.Request) {
		var req driver.Options
		if !decode(w, r, &req) {
			return
		}
		reply(w, nil, m.SetOptions(r.PathValue("id"), req))
	})
	mux.HandleFunc("POST /v1/agents/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.Archive(r.Context(), r.PathValue("id")))
	})
	mux.HandleFunc("DELETE /v1/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, nil, m.DeleteAgent(r.Context(), r.PathValue("id"), r.URL.Query().Get("purge") == "1"))
	})
	return mux
}

type httpError struct {
	code int
	err  error
}

func (e httpError) Error() string { return e.err.Error() }

func badRequest(err error) error { return httpError{http.StatusBadRequest, err} }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		reply(w, nil, badRequest(fmt.Errorf("bad request body: %w", err)))
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		code := http.StatusInternalServerError
		var he httpError
		switch {
		case errors.As(err, &he):
			code = he.code
		case errors.Is(err, store.ErrNotFound):
			code = http.StatusNotFound
		case errors.Is(err, store.ErrExists):
			code = http.StatusConflict
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(api.Error{Error: err.Error()})
		return
	}
	if v == nil {
		v = map[string]bool{"ok": true}
	}
	json.NewEncoder(w).Encode(v)
}

// handleEvents serves an agent's stored history after ?after=N, as JSON, or
// as a server-sent event stream that continues live with ?follow=1. Without
// an agent in the path it follows every agent and has no history.
func (m *Manager) handleEvents(w http.ResponseWriter, r *http.Request) {
	agentID := r.PathValue("id")
	if agentID != "" {
		a, err := m.Agent(agentID)
		if err != nil {
			reply(w, nil, err)
			return
		}
		agentID = a.ID
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	follow := r.URL.Query().Get("follow") == "1"

	if !follow {
		if agentID == "" {
			reply(w, nil, badRequest(errors.New("history needs an agent")))
			return
		}
		evs, err := m.store.Events(agentID, after, 0)
		if evs == nil {
			evs = []event.Event{}
		}
		reply(w, evs, err)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		reply(w, nil, errors.New("streaming unsupported"))
		return
	}
	// Subscribe before reading history: an event committed in between is
	// then delivered twice and dropped by ID, never lost.
	sub := m.hub.subscribe(agentID)
	defer m.hub.unsubscribe(sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(e event.Event) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	last := after
	if agentID != "" {
		evs, err := m.store.Events(agentID, after, 0)
		if err != nil {
			return
		}
		for _, e := range evs {
			if !send(e) {
				return
			}
			last = e.ID
		}
		// The history is all there: an agent with none has none, rather
		// than one still on its way.
		if _, err := fmt.Fprint(w, "event: ready\ndata: {}\n\n"); err != nil {
			return
		}
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case e := <-sub.ch:
			if e.ID != 0 && e.ID <= last && agentID != "" {
				continue
			}
			if !send(e) {
				return
			}
			if e.ID > last {
				last = e.ID
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-sub.gone:
			return
		case <-r.Context().Done():
			return
		}
	}
}
