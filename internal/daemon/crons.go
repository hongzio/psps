package daemon

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/cron"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/store"
)

// Crons: messages the operator set to be sent to an agent on a schedule.
// RunCrons finds the ones that came due and hands each to its agent's
// runtime, which sends it if the agent is free (idle, as mail needs it)
// and otherwise skips that run or keeps it waiting, as the cron says. A
// waiting run goes the next time the agent is free, from kickLocked, as
// mail does; runs that come due meanwhile are folded into it. Sending is
// moving the message to the outbox, in the transaction that records it
// sent, so it reaches the agent exactly once whenever the daemon dies.

// cronMaxPrompt caps a cron's message, as a mail's.
const cronMaxPrompt = 32 << 10

// cronLetter is a cron's message as its agent gets it.
func cronLetter(c store.Cron) string {
	if c.Self {
		return fmt.Sprintf("[gero cron #%d, on schedule %q. You set this message yourself, with the gero-cron skill, to be sent to you on a schedule; your operator may not be watching.]\n\n%s",
			c.ID, c.Schedule, c.Prompt)
	}
	return fmt.Sprintf("[gero cron #%d, on schedule %q. Your operator set this message to be sent on a schedule and may not be watching.]\n\n%s",
		c.ID, c.Schedule, c.Prompt)
}

// zoneName is what the Mac's time zone is called: its tz name if there is
// one to be found, with its offset from UTC.
func zoneName() string {
	name := os.Getenv("TZ")
	if name == "" {
		if l, err := os.Readlink("/etc/localtime"); err == nil {
			if _, after, ok := strings.Cut(l, "zoneinfo/"); ok {
				name = after
			} else {
				name = filepath.Base(l)
			}
		}
	}
	abbr, off := time.Now().Zone()
	utc := fmt.Sprintf("UTC%+03d:%02d", off/3600, abs(off%3600)/60)
	if name == "" {
		return abbr + " (" + utc + ")"
	}
	return name + " (" + utc + ")"
}

func abs(n int) int { return max(n, -n) }

func (m *Manager) Crons(agent string) (api.Crons, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return api.Crons{}, err
	}
	cs, err := m.store.Crons(a.ID)
	if cs == nil {
		cs = []store.Cron{}
	}
	return api.Crons{Zone: zoneName(), Crons: cs}, err
}

// AddCron gives an agent a new cron, on from now.
func (m *Manager) AddCron(agent string, req api.PutCron) (store.Cron, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Cron{}, err
	}
	return m.addCron(a, req, false)
}

// addCron gives a a new cron; self: a sets it itself.
func (m *Manager) addCron(a store.Agent, req api.PutCron, self bool) (store.Cron, error) {
	if a.ArchivedAt != nil {
		return store.Cron{}, badRequest(errors.New("an archived agent is sent nothing"))
	}
	c := store.Cron{Agent: a.ID, IfBusy: store.CronSkip, Enabled: true, Self: self}
	if req.Schedule == nil || req.Prompt == nil {
		return store.Cron{}, badRequest(errors.New("a cron needs a schedule and a message"))
	}
	if err := setCron(&c, req, time.Now()); err != nil {
		return store.Cron{}, err
	}
	if self {
		if err := m.selfCron(c, true); err != nil {
			return store.Cron{}, err
		}
	}
	var err error
	if c, err = m.store.AddCron(c); err != nil {
		return store.Cron{}, err
	}
	m.cronsChanged(a.ID)
	return c, nil
}

// UpdateCron changes what is set of a cron. A new schedule, or a cron
// turned on, counts from now; one turned off, or set to skip, keeps no
// run waiting.
func (m *Manager) UpdateCron(agent string, id int64, req api.PutCron) (store.Cron, error) {
	a, err := m.storedAgent(agent)
	if err != nil {
		return store.Cron{}, err
	}
	return m.updateCron(a, id, req, false)
}

// updateCron changes a's cron id; self: a changes it itself, so it must be
// one a set.
func (m *Manager) updateCron(a store.Agent, id int64, req api.PutCron, self bool) (store.Cron, error) {
	if a.ArchivedAt != nil {
		return store.Cron{}, badRequest(errors.New("an archived agent is sent nothing"))
	}
	m.cronMu.Lock()
	defer m.cronMu.Unlock()
	c, err := m.store.Cron(a.ID, id)
	if err != nil {
		return store.Cron{}, err
	}
	if self && !c.Self {
		return store.Cron{}, operatorsCron(id)
	}
	if err := setCron(&c, req, time.Now()); err != nil {
		return store.Cron{}, err
	}
	if self {
		if err := m.selfCron(c, false); err != nil {
			return store.Cron{}, err
		}
	}
	if err := m.store.PutCron(c); err != nil {
		return store.Cron{}, err
	}
	m.cronsChanged(a.ID)
	return m.store.Cron(a.ID, id)
}

func (m *Manager) DeleteCron(agent string, id int64) error {
	a, err := m.storedAgent(agent)
	if err != nil {
		return err
	}
	return m.deleteCron(a, id, false)
}

// deleteCron removes a's cron id; self: a removes it itself, so it must be
// one a set.
func (m *Manager) deleteCron(a store.Agent, id int64, self bool) error {
	m.cronMu.Lock()
	defer m.cronMu.Unlock()
	if self {
		c, err := m.store.Cron(a.ID, id)
		if err != nil {
			return err
		}
		if !c.Self {
			return operatorsCron(id)
		}
	}
	if err := m.store.DeleteCron(a.ID, id); err != nil {
		return err
	}
	m.cronsChanged(a.ID)
	return nil
}

func operatorsCron(id int64) error {
	return httpError{http.StatusForbidden, fmt.Errorf("cron: cron %d is your operator's; only they change or remove it", id)}
}

// setCron applies req to c at now, checking it.
func setCron(c *store.Cron, req api.PutCron, now time.Time) error {
	reschedule := c.NextAt == nil
	if req.Schedule != nil {
		s, err := cron.Parse(*req.Schedule)
		if err != nil {
			return badRequest(err)
		}
		reschedule = reschedule || s.String() != c.Schedule
		c.Schedule = s.String()
	}
	if req.Prompt != nil {
		p := strings.TrimSpace(*req.Prompt)
		switch {
		case p == "":
			return badRequest(errors.New("the message is empty"))
		case len(p) > cronMaxPrompt:
			return badRequest(fmt.Errorf("a cron's message is at most %d bytes", cronMaxPrompt))
		}
		c.Prompt = p
	}
	if req.IfBusy != nil {
		switch *req.IfBusy {
		case store.CronSkip, store.CronWait:
			c.IfBusy = *req.IfBusy
		default:
			return badRequest(fmt.Errorf("if_busy is %q or %q, not %q", store.CronSkip, store.CronWait, *req.IfBusy))
		}
	}
	if req.Enabled != nil {
		c.Enabled = *req.Enabled
	}
	if !c.Enabled {
		c.NextAt, c.WaitingSince = nil, nil
		return nil
	}
	if c.IfBusy == store.CronSkip {
		c.WaitingSince = nil
	}
	if reschedule {
		s, _ := cron.Parse(c.Schedule)
		// In whole seconds, as the store keeps it.
		next := s.After(now, now).Truncate(time.Second)
		if next.IsZero() {
			return badRequest(fmt.Errorf("%q does not come in the next five years", c.Schedule))
		}
		c.NextAt = &next
	}
	return nil
}

// cronsChanged tells a live agent's runtime its crons changed, and the
// loop that a cron may be due sooner.
func (m *Manager) cronsChanged(agent string) {
	m.mu.Lock()
	r := m.runtimes[agent]
	m.mu.Unlock()
	if r != nil {
		r.cronsChanged()
	}
	m.wakeCrons()
}

func (m *Manager) wakeCrons() {
	select {
	case m.cronWake <- struct{}{}:
	default:
	}
}

// cronStep is the longest RunCrons sleeps: it waits by the wall clock, in
// steps, since a timer does not count the time the Mac sleeps. A Mac that
// slept through a cron's time finds it due when it wakes, once.
const cronStep = 30 * time.Second

// RunCrons sends crons as they come due, until the daemon stops.
func (m *Manager) RunCrons() {
	for {
		t := time.NewTimer(m.cronTick(time.Now()))
		select {
		case <-t.C:
		case <-m.cronWake:
			t.Stop()
		case <-m.ctx.Done():
			t.Stop()
			return
		}
	}
}

// cronTick hands the crons due at now to their agents, and says how long
// until the next one is.
func (m *Manager) cronTick(now time.Time) time.Duration {
	m.cronMu.Lock()
	defer m.cronMu.Unlock()
	due, err := m.store.DueCrons(now)
	if err != nil {
		m.log.Printf("crons: %v", err)
	}
	for _, c := range due {
		m.cronDue(c, now)
	}
	next, err := m.store.NextCronAt(now)
	if err != nil {
		m.log.Printf("crons: %v", err)
	}
	if next == nil {
		return cronStep
	}
	return min(max(next.Sub(now), 0), cronStep)
}

// cronDue hands one due cron to its agent. One whose agent is not there to
// take it (not connected yet, or behind a fence not measured) stays due:
// the agent takes it when it can, as a run of its own.
func (m *Manager) cronDue(c store.Cron, now time.Time) {
	s, err := cron.Parse(c.Schedule)
	if err != nil {
		// Not one this daemon reads: it is turned off, saying why.
		m.log.Printf("cron %d of agent %s: %v; turned off", c.ID, c.Agent, err)
		c.Enabled, c.NextAt, c.WaitingSince = false, nil, nil
		if err := m.store.PutCron(c); err != nil {
			m.log.Printf("cron %d: %v", c.ID, err)
		}
		return
	}
	next := s.After(*c.NextAt, now)
	m.mu.Lock()
	r := m.runtimes[c.Agent]
	m.mu.Unlock()
	if r != nil {
		r.cronDue(c, now, next)
	}
}

// cronsChanged follows the agent's crons being set: one may wait no
// longer, or a waiting one be gone.
func (r *runtime) cronsChanged() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n, err := r.m.store.WaitingCrons(r.agent.ID); err == nil {
		r.crons = n
	}
	r.kickLocked()
}

// cronDue is cron c coming due at now, next coming at next.
func (r *runtime) cronDue(c store.Cron, now, next time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.att == nil || !r.caughtUp {
		return
	}
	if fenced, _ := r.m.fenceOK(r.agent.IsolationID); !fenced {
		return
	}
	if st := r.drv.State(); !r.freeLocked(st) {
		why := busyWhy(st, r.starting || len(r.outbox) > 0)
		wait := c.IfBusy == store.CronWait
		result := "skipped: " + why
		if wait {
			result = "waiting: " + why
		}
		if err := r.m.store.CronCame(c.ID, now, next, result, wait); err != nil {
			r.m.log.Printf("agent %s: cron %d: %v", r.agent.ID, c.ID, err)
			return
		}
		if n, err := r.m.store.WaitingCrons(r.agent.ID); err == nil {
			r.crons = n
		}
		return
	}
	r.sendCronLocked(c, now, &next)
	r.kickLocked()
}

// sendWaitingCronLocked sends the cron that has waited longest for the
// agent.
func (r *runtime) sendWaitingCronLocked() {
	c, err := r.m.store.WaitingCron(r.agent.ID)
	if err == nil {
		r.sendCronLocked(c, time.Now(), nil)
	} else if !errors.Is(err, store.ErrNotFound) {
		r.m.log.Printf("agent %s: cron: %v", r.agent.ID, err)
		return
	}
	if n, err := r.m.store.WaitingCrons(r.agent.ID); err == nil {
		r.crons = n
	}
}

// sendCronLocked hands c to the driver through the outbox: from then on it
// is a message like the operator's. next: it came due now.
func (r *runtime) sendCronLocked(c store.Cron, now time.Time, next *time.Time) {
	after := max(r.lastSeen, r.agent.LastSrc)
	p, err := r.m.store.SendCron(c.ID, cronLetter(c), after, now, next)
	if err != nil {
		r.m.log.Printf("agent %s: cron %d: %v", r.agent.ID, c.ID, err)
		return
	}
	r.outbox = append(r.outbox, p)
	r.nameLocked(c.Prompt)
	r.drv.Send(p.Text)
	r.m.log.Printf("agent %s: cron %d sent", r.agent.ID, c.ID)
}

// busyWhy is what kept an agent from taking a cron when it came.
func busyWhy(st driver.State, sending bool) string {
	switch st.Status {
	case event.StatusWorking:
		return "it was working"
	case event.StatusBlocked:
		return "it was waiting for an approval"
	case event.StatusInterrupted:
		return "it was interrupted, and waits for your next message"
	case event.StatusError:
		return "it was in error, and waits for your next message"
	case event.StatusStarting:
		return "it was starting"
	}
	if sending || st.Queued > 0 {
		return "a message was on its way to it"
	}
	return "its process was being replaced"
}
