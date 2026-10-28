package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/shim"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

// runtime is the daemon's side of one agent: a driver folding the agent's
// shim log, and the attachment that feeds it.
//
// Each attachment starts from a fresh driver and replays the whole log. That
// costs a re-read after a reconnect, and buys one rule with no exceptions:
// the driver's state is always exactly what the log says. Messages the log
// does not show yet sit in the outbox — in the store, written before any
// delivery is attempted — and are re-applied after catch-up, so a message
// survives the daemon dying before it reached the agent.
type runtime struct {
	m      *Manager
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	agent    store.Agent
	drv      driver.Driver
	client   *shim.Client
	att      *shim.Attachment
	lastSeen int64
	caughtUp bool
	starting bool
	stopping bool
	problem  string
	// restartWhenIdle replaces the process at the next idle moment, so a
	// changed isolation environment reaches it.
	restartWhenIdle bool

	outbox    []store.Pending
	decisions map[string]driver.Decision
}

func (m *Manager) newRuntime(a store.Agent) (*runtime, error) {
	drv, err := driver.New(a.Kind, a.ID, a.SessionID, a.Options)
	if err != nil {
		return nil, err
	}
	outbox, err := m.store.Pending(a.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	return &runtime{m: m, ctx: ctx, cancel: cancel, agent: a, drv: drv, outbox: outbox,
		decisions: map[string]driver.Decision{}}, nil
}

func (r *runtime) loop() {
	backoff := time.Second
	for {
		began := time.Now()
		err := r.session()
		if r.ctx.Err() != nil {
			return
		}
		r.setProblem(err)
		r.m.log.Printf("agent %s: %v", r.agent.ID, err)
		if time.Since(began) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-r.ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (r *runtime) session() error {
	client, err := r.m.ensureIsolation(r.ctx, r.agent.IsolationID)
	if err != nil {
		return err
	}
	att, err := client.Attach(r.ctx, r.agent.ID, 0)
	if err != nil {
		return err
	}
	defer att.Close()

	drv, err := driver.New(r.agent.Kind, r.agent.ID, r.agent.SessionID, r.agent.Options)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.client, r.att, r.drv = client, att, drv
	r.lastSeen, r.starting, r.stopping = 0, false, false
	r.caughtUp = false
	if att.LastSeq == 0 {
		r.catchUpLocked()
	}
	r.mu.Unlock()
	r.setProblem(nil)

	for rec := range att.Records {
		r.observe(rec, att.LastSeq)
	}

	r.mu.Lock()
	r.att, r.caughtUp = nil, false
	r.mu.Unlock()
	if err := att.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return errors.New("shim connection closed")
}

func (r *runtime) observe(rec shimproto.Record, liveAfter int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec.Seq <= r.lastSeen {
		return
	}
	r.lastSeen = rec.Seq
	evs := r.drv.Observe(rec)

	for _, e := range evs {
		switch e.Kind {
		case event.KindUser:
			if len(r.outbox) > 0 && rec.Seq > r.outbox[0].After && r.outbox[0].Text == e.Text {
				if err := r.m.store.DeletePending(r.outbox[0].ID); err != nil {
					r.m.log.Printf("agent %s: outbox: %v", r.agent.ID, err)
				}
				r.outbox = r.outbox[1:]
			}
		case event.KindApprovalDone:
			delete(r.decisions, e.Approval.ID)
		}
	}

	persist := false
	for _, e := range evs {
		persist = persist || !e.Ephemeral
	}
	// Records at or below LastSrc were committed by an earlier pass over
	// this log; their events are already in the store.
	if persist && rec.Seq > r.agent.LastSrc {
		committed, err := r.m.store.Commit(r.agent.ID, rec.Seq, evs)
		if err != nil {
			r.m.log.Printf("agent %s: commit: %v", r.agent.ID, err)
		} else {
			r.agent.LastSrc = rec.Seq
			for _, e := range committed {
				r.m.hub.publish(e)
			}
		}
	}
	if rec.Seq > liveAfter {
		for _, e := range evs {
			if e.Ephemeral {
				r.m.hub.publish(e)
			}
		}
	}

	if st := r.drv.State(); st.SessionID != "" && st.SessionID != r.agent.SessionID {
		r.agent.SessionID = st.SessionID
		r.m.store.SetSessionID(r.agent.ID, st.SessionID)
	}
	if rec.Dir == shimproto.DirSys && rec.Sys.Kind != shimproto.SysStarted {
		r.stopping = false
	}

	if !r.caughtUp && rec.Seq >= liveAfter {
		r.catchUpLocked()
		return
	}
	r.kickLocked()
}

// catchUpLocked runs once per attachment, when the driver has seen the whole
// log: it re-applies what the user asked for that the log does not show yet.
func (r *runtime) catchUpLocked() {
	r.caughtUp = true
	for _, m := range r.outbox {
		r.drv.Send(m.Text)
	}
	for id, d := range r.decisions {
		if r.drv.Approve(id, d) != nil {
			delete(r.decisions, id) // void: its process is gone
		}
	}
	r.kickLocked()
}

// kickLocked moves the agent toward what the driver wants: lines written, a
// process started for queued work, or a process replaced for new options.
func (r *runtime) kickLocked() {
	if r.att == nil || !r.caughtUp {
		return
	}
	// Fail closed on turns, not just on processes: an agent that was
	// already running gets no new work behind a fence nobody measured.
	fenced, why := r.m.fenceOK(r.agent.IsolationID)
	r.drv.Hold(!fenced)
	if !fenced {
		if msg := "waiting for the network fence: " + why; msg != r.problem {
			r.problem = msg
			r.m.hub.publish(event.Event{Agent: r.agent.ID, Time: time.Now().UTC(), Kind: event.KindNotice, Text: msg, Ephemeral: true})
		}
		r.m.waitForFence(r.agent.IsolationID)
	} else if strings.HasPrefix(r.problem, "waiting for the network fence") {
		r.problem = ""
	}
	st := r.drv.State()
	if st.Alive {
		for _, l := range r.drv.Next() {
			if err := r.att.Write(l); err != nil {
				// The attachment is dying; the next one replays and
				// re-applies the outbox.
				r.m.log.Printf("agent %s: write: %v", r.agent.ID, err)
				return
			}
		}
		if st = r.drv.State(); (st.Restart || r.restartWhenIdle) && !st.Busy && !r.stopping {
			r.stopping, r.restartWhenIdle = true, false
			client, id := r.client, r.agent.ID
			go client.Signal(r.ctx, id, "TERM")
		}
		return
	}
	// No process: the next one reads the environment afresh anyway.
	r.restartWhenIdle = false
	if st.Queued > 0 && !r.starting && !r.stopping && fenced {
		r.starting = true
		env, mcp := r.m.launchEnv(r.agent.IsolationID)
		spec := shimproto.Spec{Argv: launchArgv(r.agent.Kind, r.drv.Command(), mcp), Dir: r.agent.Workdir, Env: env}
		client, id := r.client, r.agent.ID
		go func() {
			_, err := client.Start(r.ctx, id, spec)
			r.mu.Lock()
			r.starting = false
			r.mu.Unlock()
			if err != nil {
				r.setProblem(fmt.Errorf("start %s: %w", r.agent.Kind, err))
			}
		}()
	}
}

func (r *runtime) setProblem(err error) {
	r.mu.Lock()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	changed := msg != r.problem
	r.problem = msg
	r.mu.Unlock()
	if changed && msg != "" {
		r.m.hub.publish(event.Event{Agent: r.agent.ID, Time: time.Now().UTC(), Kind: event.KindNotice,
			Text: msg, Ephemeral: true})
	}
}

func (r *runtime) send(text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Before catch-up lastSeen trails the log; LastSrc is the furthest this
	// daemon's store has read, which keeps an old identical message in the
	// replay from being mistaken for this one.
	after := max(r.lastSeen, r.agent.LastSrc)
	id, err := r.m.store.AddPending(r.agent.ID, text, after)
	if err != nil {
		return err
	}
	r.outbox = append(r.outbox, store.Pending{ID: id, Text: text, After: after})
	if r.agent.Name == "" {
		// Made without a prompt and not named since: its first message
		// names it.
		if name := nameFrom(text); name != "" {
			if err := r.m.store.SetAgentName(r.agent.ID, name); err != nil {
				r.m.log.Printf("agent %s: name: %v", r.agent.ID, err)
			} else {
				r.agent.Name = name
			}
		}
	}
	if r.caughtUp {
		r.drv.Send(text)
		r.kickLocked()
	}
	return nil
}

func (r *runtime) envChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restartWhenIdle = true
	r.kickLocked()
}

func (r *runtime) approve(id string, d driver.Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp {
		return errors.New("agent is not connected; try again shortly")
	}
	if err := r.drv.Approve(id, d); err != nil {
		return err
	}
	r.decisions[id] = d
	r.kickLocked()
	return nil
}

func (r *runtime) interrupt() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp {
		return errors.New("agent is not connected")
	}
	r.drv.Interrupt()
	r.kickLocked()
	return nil
}

func (r *runtime) setOptions(o driver.Options) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agent.Options = o
	r.drv.SetOptions(o)
	r.kickLocked()
}

// relabel stores the agent's new name or own ID and shows them, under the
// lock a first message takes to name an unnamed agent, so neither undoes
// the other.
func (r *runtime) relabel(name, handle *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.m.store.RenameAgent(r.agent.ID, name, handle); err != nil {
		return err
	}
	if name != nil {
		r.agent.Name = *name
	}
	if handle != nil {
		r.agent.Handle = *handle
	}
	return nil
}

func (r *runtime) view() (store.Agent, driver.State, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agent, r.drv.State(), r.problem
}

func (r *runtime) stop(ctx context.Context) {
	r.mu.Lock()
	client, alive := r.client, r.drv.State().Alive
	r.mu.Unlock()
	if client != nil && alive {
		client.Signal(ctx, r.agent.ID, "TERM")
	}
	r.cancel()
}

func sortAgents(as []agentView) {
	sort.Slice(as, func(i, j int) bool { return as[i].CreatedAt.Before(as[j].CreatedAt) })
}
