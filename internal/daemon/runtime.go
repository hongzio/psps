package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/shim"
	"github.com/hongzio/psps/internal/shimproto"
	"github.com/hongzio/psps/internal/store"
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
	// restartWhenIdle replaces the process at the next idle moment with
	// nothing in the background (envRestartDue), so a changed isolation
	// environment, or Claude's changed options, reach it.
	restartWhenIdle bool

	outbox []store.Pending
	// decisions are made but not yet in the log, re-applied to a driver
	// that attaches again.
	decisions map[string]decision
	// inbox is how many mails wait for the agent: read one per turn,
	// oldest first, whenever it is idle. crons is how many of its crons
	// have a run waiting for it, sent the same way after the mail.
	inbox, crons int
	// compacted is how many of its crons sent /compact first and have
	// their message wait for it: sent the moment it is free again, ahead
	// of anything else, so nothing comes between the two.
	compacted int
	// todoDue is set when a turn ends in idle: then, and only then, the
	// oldest auto todo is sent, ahead of mail and crons. Another turn, an
	// approval or a failure clears it.
	todoDue bool

	// news is how much of its team's news waits for it, as its lead: all
	// of it goes in one message whenever it is free.
	news int
	// turnSince is when the turn it is in began, answer the last thing it
	// said in it, and waits the approval it waits for: what its lead hears
	// of it. overtime is how far psps is in stopping a turn at its limit.
	turnSince time.Time
	answer    string
	waits     string
	overtime  overtime
	// spent is what was last kept of what it spent (store.PutSpent).
	spent store.Spent
	// receipts is how many receipts of its watches wait for it: all of
	// them go in one message whenever it is free (watches.go).
	receipts int
	// seenAt is the time of the last record of its log it read.
	seenAt time.Time
}

// overtime is where an agent's turn stopped at its limit is: asked to stop
// (its end is not news yet), asked for its final report, and that stopped
// too.
type overtime int

const (
	overNone overtime = iota
	overStopping
	overReporting
	overStopped
)

func (m *Manager) newRuntime(a store.Agent) (*runtime, error) {
	drv, err := driver.New(a.Kind, a.ID, a.SessionID, a.Options)
	if err != nil {
		return nil, err
	}
	drv.ForkFrom(a.ForkOf)
	if a.IsolationID == store.HostIsolation {
		drv.OnHost()
	}
	outbox, err := m.store.Pending(a.ID)
	if err != nil {
		return nil, err
	}
	inbox, err := m.store.InboxCount(a.ID)
	if err != nil {
		return nil, err
	}
	crons, err := m.store.WaitingCrons(a.ID)
	if err != nil {
		return nil, err
	}
	compacted, err := m.store.CompactedCrons(a.ID)
	if err != nil {
		return nil, err
	}
	news, err := m.leadNewsWaiting(a)
	if err != nil {
		return nil, err
	}
	spent, err := m.store.SpentOf()
	if err != nil {
		return nil, err
	}
	receipts, err := m.store.ReceiptsWaiting(a.ID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(m.ctx)
	return &runtime{m: m, ctx: ctx, cancel: cancel, agent: a, drv: drv, outbox: outbox,
		decisions: map[string]decision{}, inbox: inbox, crons: crons, compacted: compacted, news: news, spent: spent[a.ID], receipts: receipts}, nil
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
	drv.ForkFrom(r.agent.ForkOf)
	if r.agent.IsolationID == store.HostIsolation {
		drv.OnHost()
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
	r.seenAt = rec.Time
	prev := r.drv.State()
	was := prev.Status
	evs := r.drv.Observe(rec)
	// Not on a replay: what happened then was told then.
	live := rec.Seq > r.agent.LastSrc
	var ended event.Status
	waited := ""
	started := false
	// cut: a turn that ended was stopped, which its status (idle) does
	// not say.
	cut := false

	cur := was
	for _, e := range evs {
		switch e.Kind {
		case event.KindAssistant:
			r.answer = e.Text
		case event.KindApproval:
			if e.Approval != nil {
				r.waits = e.Approval.Summary
				if live {
					waited = cmp.Or(e.Approval.Summary, e.Approval.Kind)
				}
			}
		case event.KindApprovalDone:
			r.waits = ""
		case event.KindTurnEnd:
			cut = cut || e.Text == "interrupted"
		case event.KindStatus:
			from := cur
			cur = e.Status
			inTurn := from == event.StatusWorking || from == event.StatusBlocked
			switch {
			case e.Status == event.StatusWorking && !inTurn:
				r.turnSince, r.answer = rec.Time, ""
				started = true
			case inTurn && (e.Status == event.StatusIdle || e.Status == event.StatusError || e.Status == event.StatusInterrupted):
				r.waits = ""
				if live {
					ended = e.Status
				}
			}
		}
	}

	var read []int64
	for _, e := range evs {
		switch e.Kind {
		case event.KindUser:
			if len(r.outbox) > 0 && rec.Seq > r.outbox[0].After && r.outbox[0].Text == e.Text {
				if err := r.m.store.DeletePending(r.outbox[0].ID); err != nil {
					r.m.log.Printf("agent %s: outbox: %v", r.agent.ID, err)
				}
				read = append(read, r.outbox[0].ID)
				r.outbox = r.outbox[1:]
			} else if live {
				read = append(read, 0)
			}
		case event.KindApprovalDone:
			delete(r.decisions, e.Approval.ID)
		case event.KindStatus:
			switch e.Status {
			case event.StatusIdle:
				// Not on a replay: what was due then is not now.
				if was == event.StatusWorking && rec.Seq > r.agent.LastSrc {
					r.todoDue = true
				}
			case event.StatusStarting:
			default:
				r.todoDue = false
			}
			// Something new (a turn, an approval, a failure) wants a look
			// again. A record replayed from before does not.
			if r.agent.Parked && rec.Seq > r.agent.LastSrc && e.Status != event.StatusIdle && e.Status != event.StatusStarting {
				if err := r.m.store.SetParked(r.agent.ID, false); err != nil {
					r.m.log.Printf("agent %s: unpark: %v", r.agent.ID, err)
				} else {
					r.agent.Parked = false
				}
			}
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

	// What its watchers hear of (watches.go): what it read, a turn that
	// began, and, live, a turn that ended, an approval it waits for, or
	// its process that ended taking tasks in the background with it.
	if len(read) > 0 {
		r.watchReadLocked(rec.Seq, read)
	}
	if live && started {
		r.watchTurnStartLocked(rec.Seq)
	}
	lost := 0
	if rec.Dir == shimproto.DirSys && rec.Sys != nil && (rec.Sys.Kind == shimproto.SysExited || rec.Sys.Kind == shimproto.SysLost) {
		lost = prev.Background
	}
	if live && (ended != "" || waited != "" || lost > 0) {
		r.watchJudgeLocked(ended, cut, waited, lost, rec.Time)
	}
	if ended != "" || waited != "" {
		r.teamTurnLocked(ended, waited, rec.Time)
	}
	r.noteBusyLocked()
	if r.caughtUp {
		r.keepSpentLocked()
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
	r.keepSpentLocked()
	for _, m := range r.outbox {
		// A rewind the log already shows (or one no longer possible)
		// leaves the message to go as it is.
		if m.Rewind == 0 || r.drv.Rewind(m.Rewind, m.Text) != nil {
			r.drv.Send(m.Text)
		}
	}
	for id, d := range r.decisions {
		if r.drv.Approve(id, d.d, d.answers) != nil {
			delete(r.decisions, id) // void: its process is gone
		}
	}
	r.kickLocked()
	// A cron that came due while the agent was not connected is its now.
	r.m.wakeCrons()
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
	// A process about to be replaced starts no turn either: it would die
	// in it. What is queued waits for the next process, which starts with
	// what the replacement is for.
	st := r.drv.State()
	replacing := st.Alive && (r.stopping || st.Restart || r.envRestartDue(st))
	r.drv.Hold(!fenced || replacing)
	if !fenced {
		if msg := "waiting for the network fence: " + why; msg != r.problem {
			r.problem = msg
			r.m.hub.publish(event.Event{Agent: r.agent.ID, Time: time.Now().UTC(), Kind: event.KindNotice, Text: msg, Ephemeral: true})
		}
		r.m.waitForFence(r.agent.IsolationID)
	} else if strings.HasPrefix(r.problem, "waiting for the network fence") {
		r.problem = ""
	}
	st = r.drv.State()
	if fenced && r.compacted > 0 && r.freeLocked(st) {
		r.sendCompactedCronLocked()
		st = r.drv.State()
	}
	if fenced && r.todoDue && r.freeLocked(st) {
		r.sendTodoLocked()
		st = r.drv.State()
	}
	if fenced && r.news > 0 && r.freeLocked(st) {
		r.deliverNewsLocked()
		st = r.drv.State()
	}
	if fenced && r.receipts > 0 && r.freeLocked(st) {
		r.deliverReceiptsLocked()
		st = r.drv.State()
	}
	if fenced && r.mailDueLocked(st) {
		r.deliverMailLocked()
		st = r.drv.State()
	}
	if fenced && r.crons > 0 && r.freeLocked(st) {
		r.sendWaitingCronLocked()
		st = r.drv.State()
	}
	if st.Alive {
		for _, l := range r.drv.Next() {
			if err := r.att.Write(l); err != nil {
				// The attachment is dying; the next one replays and
				// re-applies the outbox.
				r.m.log.Printf("agent %s: write: %v", r.agent.ID, err)
				return
			}
		}
		if st = r.drv.State(); (st.Restart || r.envRestartDue(st)) && !st.Busy && !r.stopping {
			r.stopping, r.restartWhenIdle = true, false
			if st.Background > 0 {
				r.m.log.Printf("agent %s: replacing its process ends %d task(s) it ran in the background", r.agent.ID, st.Background)
			}
			client, id := r.client, r.agent.ID
			go client.Signal(r.ctx, id, "TERM")
		}
		return
	}
	// No process: the next one reads the environment afresh anyway.
	r.restartWhenIdle = false
	if st.Queued > 0 && !r.starting && !r.stopping && fenced {
		r.starting = true
		// On the Mac the agent has the user's own environment and psps
		// itself; what an isolation is given to reach psps it needs not,
		// only an account.
		var env []string
		var mcp []proxy.MCPServer
		var shares []isolation.Mount
		if r.agent.IsolationID == store.HostIsolation {
			// A shared account, if admin mode was given one; else the
			// CLI's own login on the Mac.
			env = r.m.accountEnv(store.HostIsolation)
		} else {
			env, mcp = r.m.launchEnv(r.agent.IsolationID)
			env = append(env, r.m.mailEnv(r.agent)...)
			env = append(env, r.m.cronEnv(r.agent)...)
			env = append(env, r.m.teamEnv(r.agent)...)
			env = append(env, r.m.controlEnv(r.agent)...)
			env = append(env, r.m.jobEnv(r.agent)...)
			env = append(env, r.m.awsEnv(r.agent)...)
			shares, _ = r.m.mountsFor(r.agent.IsolationID)
		}
		if d, ok := r.drv.(driver.Rooted); ok {
			var roots []string
			for _, s := range shares {
				if !s.ReadOnly {
					roots = append(roots, s.Target)
				}
			}
			d.SetWritableRoots(roots)
		}
		spec := shimproto.Spec{Argv: launchArgv(r.agent.Kind, r.drv.Command(), mcp, shares), Dir: r.agent.Workdir, Env: env}
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
	return r.sendLocked(text)
}

func (r *runtime) sendLocked(text string) error {
	_, err := r.sendWatchedLocked(text, nil)
	return err
}

// rewind takes the agent back to before the user message at log Seq src and
// sends text in its place, through the outbox like any message, so a daemon
// that dies on the way still delivers it after the rewind.
func (r *runtime) rewind(src int64, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp {
		return errors.New("agent is not connected; try again shortly")
	}
	if len(r.outbox) > 0 || r.drv.State().Queued > 0 || r.starting {
		return httpError{http.StatusConflict, errors.New("a message is still on its way to the agent; edit once it has arrived")}
	}
	after := max(r.lastSeen, r.agent.LastSrc)
	id, err := r.m.store.AddRewind(r.agent.ID, text, after, src)
	if err != nil {
		return err
	}
	if err := r.drv.Rewind(src, text); err != nil {
		if derr := r.m.store.DeletePending(id); derr != nil {
			r.m.log.Printf("agent %s: outbox: %v", r.agent.ID, derr)
		}
		return httpError{http.StatusConflict, err}
	}
	r.outbox = append(r.outbox, store.Pending{ID: id, Text: text, After: after, Rewind: src})
	r.kickLocked()
	return nil
}

// nameLocked names an agent made without a prompt and not named since
// after its first message, text.
func (r *runtime) nameLocked(text string) {
	if r.agent.Name != "" {
		return
	}
	if name := nameFrom(text); name != "" {
		if err := r.m.store.SetAgentName(r.agent.ID, name); err != nil {
			r.m.log.Printf("agent %s: name: %v", r.agent.ID, err)
		} else {
			r.agent.Name = name
		}
	}
}

// mailDueLocked is whether the agent is free for its next mail.
func (r *runtime) mailDueLocked(st driver.State) bool {
	return r.inbox > 0 && r.freeLocked(st)
}

// freeLocked is whether the agent is free for a message that is not the
// operator's (mail, a cron): idle, with nothing of the operator's still on
// its way to it, and no process about to be replaced. An agent in error or
// interrupted is not free until the operator's next message: one that
// cannot work would otherwise spend its inbox failing.
func (r *runtime) freeLocked(st driver.State) bool {
	return len(r.outbox) == 0 && st.Queued == 0 && st.Status == event.StatusIdle &&
		!r.starting && !r.stopping && !(st.Alive && (st.Restart || r.envRestartDue(st)))
}

// envRestartDue is whether the process is to be replaced now for a new
// environment or options it runs without (State's Stale): not while it
// runs tasks in the background, which would die with it. Until they end it
// goes on with what it has, taking turns as before; one that never ends
// keeps it.
func (r *runtime) envRestartDue(st driver.State) bool {
	return (r.restartWhenIdle || st.Stale) && st.Background == 0
}

// deliverMailLocked moves the oldest mail from the inbox to the outbox,
// in one transaction, and hands it to the driver: from then on it is a
// message like the operator's, delivered after a daemon restart as theirs
// are. Mail waits while the agent's isolation takes none.
func (r *runtime) deliverMailLocked() {
	if on, _, err := r.m.mailReach(r.agent.IsolationID); err != nil || !on {
		return
	}
	ml, err := r.m.store.NextMail(r.agent.ID)
	if err == nil {
		after := max(r.lastSeen, r.agent.LastSrc)
		var p store.Pending
		if p, err = r.m.store.DeliverMail(ml.ID, r.m.letter(ml), after); err == nil {
			r.outbox = append(r.outbox, p)
			r.nameLocked(ml.Body)
			r.drv.Send(p.Text)
			r.m.log.Printf("agent %s: mail #%d from %s delivered", r.agent.ID, ml.ID, ml.From)
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: mail: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.InboxCount(r.agent.ID); err == nil {
		r.inbox = n
	}
}

// inboxChanged follows mail arriving or taken out of the inbox.
func (r *runtime) inboxChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := r.m.store.InboxCount(r.agent.ID); err == nil {
		r.inbox = n
	}
	r.kickLocked()
}

func (r *runtime) envChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restartWhenIdle = true
	r.kickLocked()
}

type decision struct {
	d       driver.Decision
	answers map[string]string
}

func (r *runtime) approve(id string, d driver.Decision, answers map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.caughtUp {
		return errors.New("agent is not connected; try again shortly")
	}
	if err := r.drv.Approve(id, d, answers); err != nil {
		return badRequest(err)
	}
	r.decisions[id] = decision{d, answers}
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
	// Options saved as they were replace no process.
	if o == r.agent.Options {
		return
	}
	r.agent.Options = o
	// Claude reads its options as flags as it starts: what its process
	// cannot switch to while it runs, the driver says is stale.
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

// park parks the agent while it is idle, or takes it out of being parked.
func (r *runtime) park(on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.drv.State().Status; on && st != event.StatusIdle {
		return httpError{http.StatusConflict, fmt.Errorf("agent %s is %s; only an idle agent is parked", r.agent.ID, st)}
	}
	if err := r.m.store.SetParked(r.agent.ID, on); err != nil {
		return err
	}
	r.agent.Parked = on
	return nil
}

func (r *runtime) view() (store.Agent, driver.State, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agent, r.drv.State(), r.problem
}

// shown is view as the agent is shown: until its runtime is connected to
// the shim and has read its log, what the driver last knew is not what the
// agent is now, so it is starting, unless a problem says why it is not.
func (r *runtime) shown() (store.Agent, driver.State, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.drv.State()
	if (r.att == nil || !r.caughtUp) && r.problem == "" {
		st.Status = event.StatusStarting
	}
	return r.agent, st, r.problem
}

func (r *runtime) stop(ctx context.Context) {
	r.mu.Lock()
	client, alive := r.client, r.drv.State().Alive
	r.mu.Unlock()
	if client != nil && alive {
		client.Signal(ctx, r.agent.ID, "TERM")
	}
	r.cancel()
	if r.agent.MadeBy != "" {
		r.m.setChildBusy(r.agent.MadeBy, r.agent.ID, false)
	}
}

// sortAgents puts agents in the order they were made, a line where its
// first agent was: a new session stays where the one it took over from was.
func sortAgents(as []agentView) {
	line := func(a agentView) string { return cmp.Or(a.Line, a.ID) }
	began := map[string]time.Time{}
	for _, a := range as {
		if t, ok := began[line(a)]; !ok || a.CreatedAt.Before(t) {
			began[line(a)] = a.CreatedAt
		}
	}
	sort.SliceStable(as, func(i, j int) bool {
		bi, bj := began[line(as[i])], began[line(as[j])]
		if !bi.Equal(bj) {
			return bi.Before(bj)
		}
		return as[i].CreatedAt.Before(as[j].CreatedAt)
	})
}
