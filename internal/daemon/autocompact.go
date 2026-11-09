package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/store"
)

// Autocompact: while it is on, psps looks at every live agent once a
// minute, and sends /compact to one that is free and whose last message
// is the operator's number of minutes old, as if the operator had typed
// it, unless its last request was /compact already: one compaction per
// quiet spell. Agents a team's lead made are left to their lead, whose
// turns' ends they report to.

const (
	autocompactKey     = "autocompact"
	autocompactDefault = 30
	autocompactEvery   = time.Minute
)

// Autocompact is autocompact as it is set; off, with the default minutes,
// until it is.
func (m *Manager) Autocompact() (api.Autocompact, error) {
	v := api.Autocompact{Minutes: autocompactDefault}
	s, ok, err := m.store.Meta(autocompactKey)
	if err != nil || !ok {
		return v, err
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return api.Autocompact{Minutes: autocompactDefault}, nil
	}
	if v.Minutes < 1 {
		v.Minutes = autocompactDefault
	}
	return v, nil
}

// SetAutocompact turns autocompact on or off, after how many minutes.
func (m *Manager) SetAutocompact(v api.Autocompact) (api.Autocompact, error) {
	if v.Minutes < 1 {
		return api.Autocompact{}, httpError{http.StatusBadRequest, errors.New("minutes must be at least 1")}
	}
	b, _ := json.Marshal(v)
	if err := m.store.SetMeta(autocompactKey, string(b)); err != nil {
		return api.Autocompact{}, err
	}
	m.log.Printf("autocompact: on %v, after %d minute(s)", v.On, v.Minutes)
	go m.autocompactTick(time.Now())
	return v, nil
}

// RunAutocompact looks at the agents once a minute, until the daemon stops.
func (m *Manager) RunAutocompact() {
	t := time.NewTicker(autocompactEvery)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-t.C:
			m.autocompactTick(now)
		}
	}
}

// autocompactTick sends /compact to each live agent due it at now.
func (m *Manager) autocompactTick(now time.Time) {
	v, err := m.Autocompact()
	if err != nil {
		m.log.Printf("autocompact: %v", err)
		return
	}
	if !v.On {
		return
	}
	m.mu.Lock()
	rs := make([]*runtime, 0, len(m.runtimes))
	for _, r := range m.runtimes {
		rs = append(rs, r)
	}
	m.mu.Unlock()
	quiet := time.Duration(v.Minutes) * time.Minute
	for _, r := range rs {
		r.autocompact(now, quiet)
	}
}

// autocompact sends the agent /compact if, at now, it is free and its last
// message is quiet old, and its last request was not /compact.
func (r *runtime) autocompact(now time.Time, quiet time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agent
	if a.ArchivedAt != nil || a.MadeBy != "" || a.LastMessageAt == nil || now.Sub(*a.LastMessageAt) < quiet {
		return
	}
	if !r.caughtUp || r.problem != "" {
		return
	}
	st := r.drv.State()
	if !r.freeLocked(st) || st.Background > 0 {
		return
	}
	if ok, _ := r.m.fenceOK(a.IsolationID); !ok {
		return
	}
	last, ok, err := r.m.store.LastUserText(a.ID)
	if err != nil {
		r.m.log.Printf("agent %s: autocompact: %v", a.ID, err)
		return
	}
	if !ok || strings.TrimSpace(last) == store.CompactCommand {
		return
	}
	if err := r.sendLocked(store.CompactCommand); err != nil {
		r.m.log.Printf("agent %s: autocompact: %v", a.ID, err)
		return
	}
	r.autoCompact = true
	r.m.log.Printf("agent %s: autocompact: /compact sent, its last message %s ago", a.ID, now.Sub(*a.LastMessageAt).Round(time.Minute))
}

// compacting is whether the turn the agent is in is autocompact's.
func (r *runtime) compacting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoCompact
}
