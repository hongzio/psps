package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/awsproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// Strict SSM, the daemon's half (awsproxy has the AWS half): an agent of
// an isolation in strict mode asks, at its own address, for a command on
// an instance; the request is an approval in its conversation, held by
// the daemon rather than by the agent's CLI; accepted, psps runs it and
// the agent's request answers with what it printed.

const (
	awsService = "aws"
	// ssmWait is how long one request waits for its outcome: under the
	// ten minutes an agent's Bash tool allows a command.
	ssmWait = 9 * time.Minute
	// ssmKeep is how long an outcome stays to be fetched again.
	ssmKeep = time.Hour
	// ssmMaxCommand bounds what a person is asked to read.
	ssmMaxCommand = 32 << 10
)

// ssmApprovalKind is the kind its approvals carry; the web and the TUI
// leave these open when the agent's own status moves on, as the agent's
// CLI is not what waits on them.
const ssmApprovalKind = "ssm"

// ssmRun is one request, from asking to outcome.
type ssmRun struct {
	ID      string
	Agent   string // the agent's key
	Iso     string // the isolation's ID
	IsoName string
	Req     awsproxy.RunRequest
	Asked   time.Time

	done   chan struct{} // closed once State is final
	mu     sync.Mutex
	State  string // pending, running, declined, done, failed
	Result awsproxy.RunResult
	Err    string
	ended  time.Time
}

// ssmAnswer is what the agent's request answers with.
type ssmAnswer struct {
	ID    string `json:"id"`
	State string `json:"state"`
	*awsproxy.RunResult
	Error string `json:"error,omitempty"`
	Note  string `json:"note,omitempty"`
}

func (r *ssmRun) answer() ssmAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := ssmAnswer{ID: r.ID, State: r.State, Error: r.Err}
	switch r.State {
	case "done":
		res := r.Result
		a.RunResult = &res
	case "pending", "running":
		a.Note = fmt.Sprintf(`no outcome yet; fetch it with curl -sS "$PSPS_AWS_URL/ssm/%s", which waits again`, r.ID)
	case "declined":
		a.Note = "the operator declined this command; do not send it again, and say in your reply what you needed it for"
	}
	return a
}

func (r *ssmRun) finish(state string, res awsproxy.RunResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.State == "declined" || r.State == "done" || r.State == "failed" {
		return
	}
	r.State, r.Result, r.ended = state, res, time.Now()
	if err != nil {
		r.Err = err.Error()
	}
	close(r.done)
}

// ssmBook holds the requests the daemon knows, by ID.
type ssmBook struct {
	mu   sync.Mutex
	runs map[string]*ssmRun
}

func newSSMBook() *ssmBook { return &ssmBook{runs: map[string]*ssmRun{}} }

func (b *ssmBook) add(r *ssmRun) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, x := range b.runs {
		x.mu.Lock()
		old := !x.ended.IsZero() && time.Since(x.ended) > ssmKeep
		x.mu.Unlock()
		if old {
			delete(b.runs, id)
		}
	}
	b.runs[r.ID] = r
}

func (b *ssmBook) get(id string) *ssmRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runs[id]
}

// awsStrict is isolation iso's aws exposure, if it is in strict mode.
func (m *Manager) awsStrict(iso string) (store.Exposure, bool) {
	x, err := m.store.Exposure(iso, awsService)
	if err != nil {
		return store.Exposure{}, false
	}
	var st awsproxy.Settings
	if len(x.Settings) > 0 {
		json.Unmarshal(x.Settings, &st)
	}
	return x, st.Strict()
}

// awsEnv is an agent process's address for asking psps to run SSM
// commands, while its isolation is in strict mode.
func (m *Manager) awsEnv(a store.Agent) []string {
	if _, strict := m.awsStrict(a.IsolationID); !strict {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: aws token: %v", a.ID, err)
		return nil
	}
	return proxy.AgentEnv("AWS_URL", mailproxy.URL(tok)+"/aws", m.fromGero(a.IsolationID))
}

func (m *Manager) awsRoutes(mux *http.ServeMux, iso string) {
	caller := func(tok string) (store.Agent, store.Exposure, error) {
		name := m.isoName(iso)
		a, err := m.store.AgentByMailToken(tok)
		if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
			return a, store.Exposure{}, httpError{http.StatusForbidden, fmt.Errorf("aws: this is not the address of an agent in isolation %s; use $PSPS_AWS_URL as your process was started with it", name)}
		}
		x, strict := m.awsStrict(iso)
		if !strict {
			return a, x, httpError{http.StatusConflict, fmt.Errorf("aws: isolation %s is not in strict SSM mode; its AWS credentials open SSM themselves (the psps-aws skill)", name)}
		}
		return a, x, nil
	}
	mux.HandleFunc("POST /a/{token}/aws/ssm", func(w http.ResponseWriter, r *http.Request) {
		a, x, err := caller(r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, ssmMaxCommand+4096))
		if err != nil {
			reply(w, nil, badRequest(errors.New("aws: the body could not be read")))
			return
		}
		var req awsproxy.RunRequest
		if err := json.Unmarshal(body, &req); err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`aws: the body is JSON: {"env": "...", "instance": "i-...", "command": "...", "timeout": 120}: %v`, err)))
			return
		}
		run, err := m.askSSM(a, x, req)
		if err != nil {
			reply(w, nil, err)
			return
		}
		m.waitSSM(w, r, run)
	})
	mux.HandleFunc("GET /a/{token}/aws/ssm/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, _, err := caller(r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		run := m.ssm.get(r.PathValue("id"))
		if run == nil || run.Agent != a.ID {
			reply(w, nil, httpError{http.StatusNotFound, fmt.Errorf("aws: no request %s of yours is known (the daemon keeps them an hour, and forgets them when it restarts)", r.PathValue("id"))})
			return
		}
		m.waitSSM(w, r, run)
	})
}

// askSSM checks a request and puts it to the person, in a's conversation.
func (m *Manager) askSSM(a store.Agent, x store.Exposure, req awsproxy.RunRequest) (*ssmRun, error) {
	req.Env = strings.TrimSpace(req.Env)
	if req.Env == "" {
		req.Env = x.Default
	}
	if !containsString(x.Envs, req.Env) {
		return nil, badRequest(fmt.Errorf("aws: environment %q is not exposed here (exposed: %s)", req.Env, orNone(strings.Join(x.Envs, ", "))))
	}
	switch {
	case strings.TrimSpace(req.Command) == "":
		return nil, badRequest(errors.New(`aws: "command" is the script to run, and it is empty`))
	case len(req.Command) > ssmMaxCommand:
		return nil, badRequest(fmt.Errorf("aws: a command is at most %d bytes; put a longer script in a file the instance can fetch", ssmMaxCommand))
	case req.Timeout < 0 || req.Timeout > awsproxy.MaxRunTimeout:
		return nil, badRequest(fmt.Errorf("aws: timeout is 1 to %d seconds", awsproxy.MaxRunTimeout))
	}
	e, err := m.awsEnvConfig(req.Env)
	if err != nil {
		return nil, err
	}
	if err := e.CanRun(req.Instance); err != nil {
		return nil, badRequest(err)
	}
	var b [6]byte
	rand.Read(b[:])
	run := &ssmRun{ID: "ssm-" + hex.EncodeToString(b[:]), Agent: a.ID, Iso: a.IsolationID, IsoName: m.isoName(a.IsolationID),
		Req: req, Asked: time.Now(), done: make(chan struct{}), State: "pending"}
	m.ssm.add(run)
	first, _, _ := strings.Cut(strings.TrimSpace(req.Command), "\n")
	if len(first) > 120 {
		first = first[:120] + "…"
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = awsproxy.DefaultRunTimeout
	}
	m.addEvents(a.ID, event.Event{Kind: event.KindApproval, Approval: &event.Approval{
		ID:      run.ID,
		Kind:    ssmApprovalKind,
		Summary: fmt.Sprintf("run on %s (aws env %s) through SSM: %s", req.Instance, req.Env, first),
		Reason:  fmt.Sprintf("strict SSM: psps runs it as ssm-user if you accept, for up to %ds, and gives the agent what it prints", timeout),
		Detail:  req.Command,
	}})
	m.log.Printf("aws: %s of isolation %s asks to run on %s (env %s): %s", shownID(a), run.IsoName, req.Instance, req.Env, run.ID)
	return run, nil
}

func (m *Manager) awsEnvConfig(name string) (awsproxy.Env, error) {
	var e awsproxy.Env
	se, err := m.store.Env(awsService, name)
	if err != nil {
		return e, badRequest(fmt.Errorf("aws: environment %q: %w", name, err))
	}
	if err := json.Unmarshal(se.Config, &e); err != nil {
		return e, err
	}
	return e, nil
}

// waitSSM answers with run's outcome, or with pending if it takes longer
// than a request may wait.
func (m *Manager) waitSSM(w http.ResponseWriter, r *http.Request, run *ssmRun) {
	t := time.NewTimer(ssmWait)
	defer t.Stop()
	select {
	case <-run.done:
	case <-t.C:
	case <-r.Context().Done():
		return
	}
	reply(w, run.answer(), nil)
}

// addEvents stores events of the daemon's own in agent's conversation and
// shows them. They come from no shim record, so they have none (Src 0)
// and no rewind takes them out.
func (m *Manager) addEvents(agent string, evs ...event.Event) {
	now := time.Now().UTC()
	for i := range evs {
		evs[i].Agent, evs[i].Time = agent, now
	}
	committed, err := m.store.Commit(agent, 0, evs)
	if err != nil {
		m.log.Printf("agent %s: commit: %v", agent, err)
		return
	}
	for _, e := range committed {
		m.hub.publish(e)
	}
}

// decideSSM is a person's decision on one of these approvals. One the
// daemon no longer holds (it restarted since) is closed as void, so it
// stops asking.
func (m *Manager) decideSSM(agent, approvalID string, d driver.Decision) error {
	run := m.ssm.get(approvalID)
	if run == nil || run.Agent != agent {
		m.addEvents(agent, event.Event{Kind: event.KindApprovalDone, Approval: &event.Approval{ID: approvalID, Kind: ssmApprovalKind, Decision: "void"}})
		return badRequest(errors.New("that SSM request is no longer waiting (the daemon restarted since it was made); the agent can ask again"))
	}
	run.mu.Lock()
	if run.State != "pending" {
		run.mu.Unlock()
		return badRequest(fmt.Errorf("that SSM request was already decided (%s)", run.State))
	}
	if d == driver.Accept {
		run.State = "running"
	} else {
		run.State, run.ended = "declined", time.Now()
		close(run.done)
	}
	run.mu.Unlock()
	m.addEvents(agent, event.Event{Kind: event.KindApprovalDone, Approval: &event.Approval{ID: approvalID, Kind: ssmApprovalKind, Decision: string(d)}})
	m.log.Printf("aws: %s on %s for isolation %s: %s", run.ID, run.Req.Instance, run.IsoName, d)
	switch d {
	case driver.Accept:
		go m.runSSM(run)
	case driver.Cancel:
		// Decline & stop: the agent's turn ends too.
		if err := m.Interrupt(agent); err != nil {
			m.log.Printf("aws: %s: stopping the agent: %v", run.ID, err)
		}
	}
	return nil
}

func (m *Manager) runSSM(run *ssmRun) {
	timeout := run.Req.Timeout
	if timeout == 0 {
		timeout = awsproxy.DefaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second+5*time.Minute)
	defer cancel()
	e, err := m.awsEnvConfig(run.Req.Env)
	var res awsproxy.RunResult
	if err == nil {
		res, err = awsproxy.Run(ctx, e, run.IsoName, run.Req)
	}
	if err != nil {
		run.finish("failed", res, err)
		m.log.Printf("aws: %s on %s for isolation %s failed: %v", run.ID, run.Req.Instance, run.IsoName, err)
		m.addEvents(run.Agent, event.Event{Kind: event.KindNotice, Text: fmt.Sprintf("SSM %s on %s failed: %v", run.ID, run.Req.Instance, err)})
		return
	}
	run.finish("done", res, nil)
	m.log.Printf("aws: %s on %s for isolation %s: %s, exit %d, as %s (command %s)", run.ID, run.Req.Instance, run.IsoName, res.Status, res.ExitCode, res.User, res.CommandID)
	m.addEvents(run.Agent, event.Event{Kind: event.KindNotice, Text: fmt.Sprintf("SSM %s on %s: %s, exit %d, as %s", run.ID, run.Req.Instance, res.Status, res.ExitCode, cmpOr(res.User, "?"))})
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// endSSMSessions ends the SSM sessions isolation iso's credentials still
// have open, as strict mode starts: its credentials from before stay valid
// until they expire, but what they opened does not stay open.
func (m *Manager) endSSMSessions(iso string, x store.Exposure) {
	name := m.isoName(iso)
	for _, env := range x.Envs {
		e, err := m.awsEnvConfig(env)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		n, err := awsproxy.EndSessions(ctx, e, name)
		cancel()
		switch {
		case err != nil:
			m.log.Printf("aws: isolation %s is strict now; ending its open SSM sessions (env %s): %d ended, and: %v", name, env, n, err)
		default:
			m.log.Printf("aws: isolation %s is strict now; %d open SSM session(s) of env %s ended", name, n, env)
		}
	}
}
