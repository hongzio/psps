package daemon

import (
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

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/askproxy"
	"github.com/hongzio/psps/internal/store"
)

// Asks agents leave the operator (internal/proxy/askproxy). The daemon
// answers them on mail's port, at each agent's own address; the operator
// answers them through the API, and the answers reach each agent through
// its runtime, all that wait in one message whenever it is free.

// askOn is whether isolation iso's agents may leave asks.
func (m *Manager) askOn(iso string) (bool, error) {
	if _, err := m.registry().Get(askproxy.Name); err != nil {
		return false, nil
	}
	_, err := m.store.Exposure(iso, askproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// askEnv is an agent process's asks' address, while its isolation lets
// agents leave asks.
func (m *Manager) askEnv(a store.Agent) []string {
	if on, err := m.askOn(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: ask token: %v", a.ID, err)
		return nil
	}
	return proxy.AgentEnv("ASK_URL", askproxy.URL(tok), m.fromGero(a.IsolationID))
}

// --- what agents ask, through their isolation's tunnel ----------------------

// agentAsk is an ask as its agent reads it.
type agentAsk struct {
	ID        int64             `json:"id"`
	Kind      string            `json:"kind"`
	Title     string            `json:"title"`
	Body      string            `json:"body,omitempty"`
	Options   []store.AskOption `json:"options,omitempty"`
	State     string            `json:"state"`
	Turn      string            `json:"turn,omitempty"`
	Verdict   string            `json:"verdict"`
	Picked    string            `json:"picked,omitempty"`
	Text      string            `json:"text,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	ClosedAt  *time.Time        `json:"closed_at,omitempty"`
	Notes     []agentNote       `json:"notes,omitempty"`
	Transfer  *api.Transfer     `json:"transfer,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// agentNote is what was added to an ask, as its agent reads it: by "you"
// or "operator".
type agentNote struct {
	By        string            `json:"by"`
	Text      string            `json:"text,omitempty"`
	Options   []store.AskOption `json:"options,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// agentAskOf is a as its agent reads it: its options those it offers now.
func (m *Manager) agentAskOf(a store.Ask) agentAsk {
	opts := a.Current()
	v := agentAsk{ID: a.ID, Kind: a.Kind, Title: a.Title, Body: a.Body, Options: opts, State: a.State,
		Verdict: askproxy.Verdict(a), Text: a.Text, CreatedAt: a.CreatedAt, ClosedAt: a.ClosedAt, Transfer: m.transferView(a.Transfer)}
	if a.State == store.AskOpen {
		v.Turn = map[string]string{store.TurnOperator: "operator", store.TurnAgent: "yours"}[a.Turn]
	}
	if a.Pick > 0 && a.Pick <= len(opts) {
		v.Picked = opts[a.Pick-1].Label
	}
	for _, n := range a.Notes {
		by := "operator"
		if n.Author == store.TurnAgent {
			by = "you"
		}
		v.Notes = append(v.Notes, agentNote{By: by, Text: n.Text, Options: n.Options, CreatedAt: n.CreatedAt})
	}
	return v
}

// postedNote is what an agent adds to an ask of its: what it found since,
// new options for a question, or resolve to close it itself.
type postedNote struct {
	Text    string            `json:"text"`
	Options []store.AskOption `json:"options"`
	Resolve bool              `json:"resolve"`
}

// postedAsk is an ask as an agent writes it.
type postedAsk struct {
	Kind    string            `json:"kind"`
	Title   string            `json:"title"`
	Body    string            `json:"body"`
	Options []store.AskOption `json:"options"`
	// Transfer is the file a transfer moves.
	Transfer *postedTransfer `json:"transfer"`
}

// askHistory is how many of its asks an agent, or the operator for one
// agent, sees.
const askHistory = 100

// askRoutes adds the agents' asks to isolation iso's mail port. Nothing it
// logs carries a path: the path carries the agent's token.
func (m *Manager) askRoutes(mux *http.ServeMux, iso string) {
	mux.HandleFunc("GET /a/{token}/asks", func(w http.ResponseWriter, r *http.Request) {
		a, err := m.askAgent(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		as, err := m.store.AsksOf(a.ID, askHistory)
		out := make([]agentAsk, 0, len(as))
		for _, x := range as {
			out = append(out, m.agentAskOf(x))
		}
		reply(w, out, err)
	})
	mux.HandleFunc("POST /a/{token}/asks", func(w http.ResponseWriter, r *http.Request) {
		a, err := m.askAgent(iso, r.PathValue("token"))
		if err != nil {
			reply(w, nil, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, askproxy.MaxBody+16<<10))
		var req postedAsk
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`ask: the body is JSON, as {"kind": "question", "title": "...", "body": "...", "options": [{"label": "..."}]}: %v`, err)))
			return
		}
		v, made, err := m.postAsk(r.Context(), a, req)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			if made {
				w.WriteHeader(http.StatusCreated)
			}
		}
		reply(w, v, err)
	})
	mux.HandleFunc("GET /a/{token}/asks/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, id, err := m.askOfAgent(iso, r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		x, err := m.store.GetAsk(id)
		if err != nil || x.Agent != a.ID {
			reply(w, nil, httpError{http.StatusNotFound, fmt.Errorf("ask: you have no ask #%d", id)})
			return
		}
		reply(w, m.agentAskOf(x), nil)
	})
	mux.HandleFunc("POST /a/{token}/asks/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, id, err := m.askOfAgent(iso, r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, askproxy.MaxBody+16<<10))
		var req postedNote
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`ask: what you add is JSON, as {"text": "...", "options": [{"label": "..."}], "resolve": false}: %v`, err)))
			return
		}
		v, err := m.addAgentNote(a, id, req)
		reply(w, v, err)
	})
	mux.HandleFunc("DELETE /a/{token}/asks/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, id, err := m.askOfAgent(iso, r)
		if err != nil {
			reply(w, nil, err)
			return
		}
		x, err := m.store.WithdrawAsk(a.ID, id)
		switch {
		case errors.Is(err, store.ErrAskClosed):
			err = httpError{http.StatusConflict, fmt.Errorf("ask: #%d is %s already", id, askproxy.Verdict(x))}
		case errors.Is(err, store.ErrNotFound):
			err = httpError{http.StatusNotFound, fmt.Errorf("ask: you have no ask #%d", id)}
		case err == nil:
			m.log.Printf("agent %s: ask #%d withdrawn", a.ID, id)
			m.pokeChanges()
		}
		reply(w, nil, err)
	})
}

// askAgent is the live agent of isolation iso whose address carries tok,
// while the isolation lets agents leave asks.
func (m *Manager) askAgent(iso, tok string) (store.Agent, error) {
	on, err := m.askOn(iso)
	if err != nil {
		return store.Agent{}, err
	}
	name := m.isoName(iso)
	if !on {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("ask: isolation %s's agents leave no asks; the operator turns that on with `psps iso expose %s ask`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return store.Agent{}, httpError{http.StatusForbidden, fmt.Errorf("ask: this is not the address of an agent in isolation %s; use $PSPS_ASK_URL as your process was started with it", name)}
	}
	return a, nil
}

func (m *Manager) askOfAgent(iso string, r *http.Request) (store.Agent, int64, error) {
	a, err := m.askAgent(iso, r.PathValue("token"))
	if err != nil {
		return a, 0, err
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(r.PathValue("id"), "#"), 10, 64)
	if err != nil {
		return a, 0, badRequest(fmt.Errorf("ask: %q is not an ask's id", r.PathValue("id")))
	}
	return a, id, nil
}

// postAsk keeps what agent a asks, once it reads as an ask.
func (m *Manager) postAsk(ctx context.Context, a store.Agent, req postedAsk) (agentAsk, bool, error) {
	x, err := checkAsk(req)
	if err != nil {
		return agentAsk{}, false, badRequest(err)
	}
	if x.Kind == askproxy.Transfer {
		if x.Transfer, err = m.transferOf(ctx, req.Transfer); err != nil {
			return agentAsk{}, false, err
		}
	}
	x.Agent = a.ID
	x, made, err := m.store.AddAsk(x, askproxy.MaxOpen)
	if errors.Is(err, store.ErrTooManyAsks) {
		return agentAsk{}, false, httpError{http.StatusTooManyRequests, fmt.Errorf("ask: you have %d asks open, as many as you may; withdraw one you no longer need (DELETE \"$PSPS_ASK_URL/ID\")", askproxy.MaxOpen)}
	}
	if err != nil {
		return agentAsk{}, false, err
	}
	v := m.agentAskOf(x)
	if !made {
		v.Note = "an ask of yours with this title is open already: this is it. Its answer comes when your operator gives it."
		return v, false, nil
	}
	m.log.Printf("agent %s: ask #%d (%s)", a.ID, x.ID, x.Kind)
	m.pokeChanges()
	v.Note = "Posted; it waits for your operator, however long that takes. Go on with what does not need the answer, and end your turn when nothing is left: the answer comes as a message of its own when you are idle. Do not poll for it."
	return v, true, nil
}

// addAgentNote adds what agent a posts to its open ask id, closing it as
// resolved if it says so.
func (m *Manager) addAgentNote(a store.Agent, id int64, req postedNote) (agentAsk, error) {
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "" && len(req.Options) == 0 && !req.Resolve:
		return agentAsk{}, badRequest(errors.New(`ask: add "text" (what you found, or your answer to their reply), new "options", or "resolve": true`))
	case len(text) > askproxy.MaxBody:
		return agentAsk{}, badRequest(fmt.Errorf(`ask: "text" is at most %d bytes; put the rest where your operator can read it and say where`, askproxy.MaxBody))
	case !utf8.ValidString(text):
		return agentAsk{}, badRequest(errors.New("ask: what you add is text, in UTF-8"))
	}
	var opts []store.AskOption
	if len(req.Options) > 0 {
		x, err := m.store.GetAsk(id)
		if err == nil && x.Agent == a.ID && x.Kind != askproxy.Question {
			return agentAsk{}, badRequest(fmt.Errorf(`ask: only a question has "options"; #%d is a %s`, id, x.Kind))
		}
		if opts, err = checkOptions(req.Options); err != nil {
			return agentAsk{}, badRequest(err)
		}
	}
	x, err := m.store.AddNote(id, a.ID, store.TurnAgent, text, opts, askproxy.MaxNotes, req.Resolve)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return agentAsk{}, httpError{http.StatusNotFound, fmt.Errorf("ask: you have no ask #%d", id)}
	case errors.Is(err, store.ErrAskClosed):
		return agentAsk{}, httpError{http.StatusConflict, fmt.Errorf("ask: #%d is %s already; leave a new ask if you still need them", id, askproxy.Verdict(x))}
	case errors.Is(err, store.ErrTooManyNotes):
		return agentAsk{}, httpError{http.StatusTooManyRequests, fmt.Errorf("ask: #%d has %d notes, as many as one may; resolve it, or withdraw it and leave a new one", id, askproxy.MaxNotes)}
	case err != nil:
		return agentAsk{}, err
	}
	m.log.Printf("agent %s: ask #%d %s", a.ID, id, map[bool]string{true: "resolved", false: "added to"}[req.Resolve])
	m.pokeChanges()
	v := m.agentAskOf(x)
	if req.Resolve {
		v.Note = "Resolved; your operator sees it closed, and is not asked any more."
	} else {
		v.Note = "Added; the ask waits for your operator again. Go on with what does not need them, and end your turn when nothing is left. Do not poll."
	}
	return v, nil
}

// checkAsk reads an ask as an agent wrote it, refusing what is not one.
func checkAsk(req postedAsk) (store.Ask, error) {
	x := store.Ask{Kind: strings.TrimSpace(req.Kind), Title: strings.TrimSpace(req.Title), Body: strings.TrimSpace(req.Body)}
	if x.Kind == "" {
		x.Kind = askproxy.Question
	}
	switch x.Kind {
	case askproxy.Question, askproxy.Confirm, askproxy.Task, askproxy.Transfer:
	default:
		return x, fmt.Errorf("ask: kind is %q, %q, %q or %q, not %q", askproxy.Question, askproxy.Confirm, askproxy.Task, askproxy.Transfer, x.Kind)
	}
	if req.Transfer != nil && x.Kind != askproxy.Transfer {
		return x, fmt.Errorf(`ask: only a transfer has "transfer"; this is a %s`, x.Kind)
	}
	switch {
	case x.Title == "":
		return x, errors.New(`ask: an ask needs a "title": one line, what to decide or do`)
	case strings.ContainsAny(x.Title, "\r\n"):
		return x, errors.New(`ask: the "title" is one line; put the rest in "body"`)
	case utf8.RuneCountInString(x.Title) > askproxy.MaxTitle:
		return x, fmt.Errorf(`ask: the "title" is at most %d characters; put the rest in "body"`, askproxy.MaxTitle)
	case len(x.Body) > askproxy.MaxBody:
		return x, fmt.Errorf(`ask: the "body" is at most %d bytes; put the rest where your operator can read it and say where`, askproxy.MaxBody)
	case !utf8.ValidString(x.Title + x.Body):
		return x, errors.New("ask: an ask is text, in UTF-8")
	}
	if len(req.Options) > 0 && x.Kind != askproxy.Question {
		return x, fmt.Errorf(`ask: only a question has "options"; a %s is answered %s`, x.Kind, map[string]string{askproxy.Confirm: "yes or no", askproxy.Task: "done or not", askproxy.Transfer: "yes or no"}[x.Kind])
	}
	opts, err := checkOptions(req.Options)
	x.Options = opts
	return x, err
}

// checkOptions reads a question's options as an agent wrote them.
func checkOptions(in []store.AskOption) ([]store.AskOption, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) < askproxy.MinOptions || len(in) > askproxy.MaxOptions {
		return nil, fmt.Errorf(`ask: a question has no "options", or %d to %d`, askproxy.MinOptions, askproxy.MaxOptions)
	}
	var out []store.AskOption
	for i, o := range in {
		o.Label, o.Note = strings.TrimSpace(o.Label), strings.TrimSpace(o.Note)
		if o.Label == "" || strings.ContainsAny(o.Label, "\r\n") || utf8.RuneCountInString(o.Label) > askproxy.MaxTitle || utf8.RuneCountInString(o.Note) > 1000 {
			return nil, fmt.Errorf(`ask: option %d needs a "label" of one line, at most %d characters (and a "note" of at most 1000)`, i+1, askproxy.MaxTitle)
		}
		out = append(out, o)
	}
	return out, nil
}

// --- what the operator asks ------------------------------------------------

// askViews shows asks as the operator reads them.
func (m *Manager) askViews(as []store.Ask) []api.Ask {
	agents := map[string]store.Agent{}
	out := make([]api.Ask, 0, len(as))
	for _, x := range as {
		a, ok := agents[x.Agent]
		if !ok {
			if got, err := m.store.Agent(x.Agent); err == nil {
				a = got
			} else {
				a = store.Agent{ID: x.Agent}
			}
			agents[x.Agent] = a
		}
		out = append(out, api.Ask{Ask: x, AgentID: shownID(a), AgentName: a.Name, Isolation: m.isoName(a.IsolationID), Verdict: askproxy.Verdict(x), Transfer: m.transferView(x.Transfer)})
	}
	return out
}

// OpenAsks are every agent's open asks, those waiting on the operator
// first, oldest first.
func (m *Manager) OpenAsks() ([]api.Ask, error) {
	as, err := m.store.OpenAsks()
	if err != nil {
		return nil, err
	}
	return m.askViews(as), nil
}

// AgentAsks are an agent's asks, newest first.
func (m *Manager) AgentAsks(idOrPrefix string) ([]api.Ask, error) {
	a, err := m.storedAgent(idOrPrefix)
	if err != nil {
		return nil, err
	}
	as, err := m.store.AsksOf(a.ID, askHistory)
	if err != nil {
		return nil, err
	}
	return m.askViews(as), nil
}

// GetAsk is ask id.
func (m *Manager) GetAsk(id int64) (api.Ask, error) {
	x, err := m.store.GetAsk(id)
	if err != nil {
		return api.Ask{}, err
	}
	return m.askViews([]store.Ask{x})[0], nil
}

// AnswerAsk closes open ask id with the operator's answer, or without one
// (req.Outcome "dismiss"); its agent hears of it when it is next free.
func (m *Manager) AnswerAsk(id int64, req api.AnswerAsk) (api.Ask, error) {
	x, err := m.store.GetAsk(id)
	if err != nil {
		return api.Ask{}, err
	}
	if x.State != store.AskOpen {
		return api.Ask{}, httpError{http.StatusConflict, fmt.Errorf("ask #%d is %s already", id, askproxy.Verdict(x))}
	}
	state, outcome, text := store.AskAnswered, strings.TrimSpace(req.Outcome), strings.TrimSpace(req.Text)
	if len(text) > askproxy.MaxText {
		return api.Ask{}, badRequest(fmt.Errorf("an answer is at most %d bytes", askproxy.MaxText))
	}
	if opts := x.Current(); req.Pick != 0 && (x.Kind != askproxy.Question || req.Pick < 1 || req.Pick > len(opts)) {
		if len(opts) == 0 {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d offers no options to pick", id))
		}
		return api.Ask{}, badRequest(fmt.Errorf("ask #%d's options are 1 to %d", id, len(opts)))
	}
	switch {
	case outcome == "dismiss":
		state, outcome = store.AskDismissed, ""
		if req.Pick != 0 {
			return api.Ask{}, badRequest(errors.New("an ask closed without an answer picks nothing"))
		}
	case x.Kind == askproxy.Question:
		if outcome != "" {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d is a question: pick an option or answer in words", id))
		}
		if req.Pick == 0 && text == "" {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d is a question: pick an option, answer in words, or both", id))
		}
	case x.Kind == askproxy.Confirm:
		if outcome != askproxy.Yes && outcome != askproxy.No {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d asks for a go-ahead: answer yes or no", id))
		}
	case x.Kind == askproxy.Task:
		if outcome != askproxy.Done && outcome != askproxy.Cant {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d is a thing to do: answer done or cant", id))
		}
	case x.Kind == askproxy.Transfer:
		if outcome != askproxy.Yes && outcome != askproxy.No {
			return api.Ask{}, badRequest(fmt.Errorf("ask #%d asks for a file to be moved: answer yes (it is copied then) or no", id))
		}
		if outcome == askproxy.Yes {
			// Copied first: a copy that fails leaves the ask open, to try
			// again or decline.
			if err := m.runTransfer(x); err != nil {
				return api.Ask{}, err
			}
		}
	}
	x, err = m.store.CloseAsk(id, state, outcome, req.Pick, text)
	if errors.Is(err, store.ErrAskClosed) {
		return api.Ask{}, httpError{http.StatusConflict, err}
	}
	if err != nil {
		return api.Ask{}, err
	}
	m.log.Printf("agent %s: ask #%d %s", x.Agent, x.ID, askproxy.Verdict(x))
	m.answersChanged(x.Agent)
	m.pokeChanges()
	return m.askViews([]store.Ask{x})[0], nil
}

// ReplyAsk adds the operator's words to open ask id and keeps it open:
// the agent hears them when it is next free, and may add to the ask in
// turn.
func (m *Manager) ReplyAsk(id int64, req api.ReplyAsk) (api.Ask, error) {
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "":
		return api.Ask{}, badRequest(errors.New("a reply needs words; to close the ask, answer it"))
	case len(text) > askproxy.MaxText:
		return api.Ask{}, badRequest(fmt.Errorf("a reply is at most %d bytes", askproxy.MaxText))
	}
	x, err := m.store.AddNote(id, "", store.TurnOperator, text, nil, askproxy.MaxNotes, false)
	switch {
	case errors.Is(err, store.ErrAskClosed):
		return api.Ask{}, httpError{http.StatusConflict, fmt.Errorf("ask #%d is %s already", id, askproxy.Verdict(x))}
	case errors.Is(err, store.ErrTooManyNotes):
		return api.Ask{}, httpError{http.StatusConflict, fmt.Errorf("ask #%d has %d notes, as many as one may: answer it or close it", id, askproxy.MaxNotes)}
	case err != nil:
		return api.Ask{}, err
	}
	m.log.Printf("agent %s: ask #%d replied to", x.Agent, x.ID)
	m.answersChanged(x.Agent)
	m.pokeChanges()
	return m.askViews([]store.Ask{x})[0], nil
}

// withAsks gives each agent how many of its open asks wait on the
// operator, and how many on it.
func (m *Manager) withAsks(as []api.Agent) error {
	counts, err := m.store.OpenAskCounts()
	if err != nil || len(counts) == 0 {
		return err
	}
	for i := range as {
		c := counts[as[i].ID]
		as[i].Asks, as[i].AsksWaiting, as[i].AskNotes = c.Operator, c.Agent, c.Notes
	}
	return nil
}

// --- the answers reaching their agents --------------------------------------

// answersChanged tells a live agent that answers wait for it.
func (m *Manager) answersChanged(agent string) {
	m.mu.Lock()
	r := m.runtimes[agent]
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := m.store.AnswersWaiting(agent); err == nil {
		r.answers = n
	}
	r.kickLocked()
}

// deliverAnswersLocked moves all the answers waiting for the agent into its
// outbox, as one message.
func (r *runtime) deliverAnswersLocked() {
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.DeliverAnswers(r.agent.ID, after, askproxy.Answers)
	if err == nil {
		r.outbox = append(r.outbox, p)
		r.drv.Send(p.Text)
		r.m.log.Printf("agent %s: answers delivered", r.agent.ID)
	} else if !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: answers: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.AnswersWaiting(r.agent.ID); err == nil {
		r.answers = n
	}
}
