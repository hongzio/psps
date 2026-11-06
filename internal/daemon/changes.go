package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// changes tells the web which of its lists changed, so that it reads one
// again only then, where it read them all every two seconds and drew its
// page anew each time (a selection being made on it went with the old one).
//
// While anyone listens, the daemon looks at each list once a second, the
// isolations, which ask the container runtime about each one, every ten;
// and at all of them at once after any request that changes something. A
// list that differs from the last look is named to every listener.
type changes struct {
	mu      sync.Mutex
	subs    map[*changeSub]struct{}
	running bool
	poke    chan struct{}
}

type changeSub struct {
	mu      sync.Mutex
	pending map[string]bool
	wake    chan struct{}
}

func newChanges() *changes {
	return &changes{subs: map[*changeSub]struct{}{}, poke: make(chan struct{}, 1)}
}

// changesEvery and isolationsEvery are how often the lists are looked at;
// tests shorten them.
var (
	changesEvery    = time.Second
	isolationsEvery = 10
)

func (m *Manager) subscribeChanges() *changeSub {
	c := m.changes
	s := &changeSub{pending: map[string]bool{}, wake: make(chan struct{}, 1)}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subs[s] = struct{}{}
	if !c.running {
		c.running = true
		m.loops.Add(1)
		go m.watchChanges()
	}
	return s
}

func (m *Manager) unsubscribeChanges(s *changeSub) {
	c := m.changes
	c.mu.Lock()
	delete(c.subs, s)
	c.mu.Unlock()
}

// pokeChanges has the lists looked at now.
func (m *Manager) pokeChanges() {
	select {
	case m.changes.poke <- struct{}{}:
	default:
	}
}

// watchChanges looks at the lists until no one listens.
func (m *Manager) watchChanges() {
	defer m.loops.Done()
	c := m.changes
	last := map[string][32]byte{}
	tick := time.NewTicker(changesEvery)
	defer tick.Stop()
	for n := 0; ; n++ {
		all := n%isolationsEvery == 0
		if n > 0 {
			select {
			case <-tick.C:
			case <-c.poke:
				all = true
			case <-m.ctx.Done():
				c.mu.Lock()
				c.running = false
				c.mu.Unlock()
				return
			}
		}
		c.mu.Lock()
		if len(c.subs) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()

		var changed []string
		for topic, look := range m.changeLooks(all) {
			v, err := look()
			if err != nil {
				continue
			}
			b, _ := json.Marshal(v)
			sum := sha256.Sum256(b)
			if prev, ok := last[topic]; ok && prev != sum {
				changed = append(changed, topic)
			}
			last[topic] = sum
		}
		if len(changed) == 0 {
			continue
		}
		c.mu.Lock()
		for s := range c.subs {
			s.mu.Lock()
			for _, t := range changed {
				s.pending[t] = true
			}
			s.mu.Unlock()
			select {
			case s.wake <- struct{}{}:
			default:
			}
		}
		c.mu.Unlock()
	}
}

// changeLooks reads each list as the web does; the isolations only when
// all is set.
func (m *Manager) changeLooks(all bool) map[string]func() (any, error) {
	looks := map[string]func() (any, error){
		"agents": func() (any, error) {
			agents, err := m.Agents()
			if err != nil {
				return nil, err
			}
			order, err := m.Order()
			return []any{agents, order}, err
		},
		"services": func() (any, error) {
			envs, err := m.Envs("")
			if err != nil {
				return nil, err
			}
			isos, err := m.store.Isolations()
			if err != nil {
				return nil, err
			}
			exposures := map[string]any{}
			for _, iso := range isos {
				if exposures[iso.Name], err = m.Exposures(iso.Name); err != nil {
					return nil, err
				}
			}
			return []any{m.Services(), envs, exposures}, nil
		},
	}
	if all {
		looks["isolations"] = func() (any, error) {
			ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
			defer cancel()
			return m.Isolations(ctx)
		}
	}
	return looks
}

func (s *changeSub) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for t := range s.pending {
		out = append(out, t)
	}
	sort.Strings(out)
	clear(s.pending)
	return out
}

// handleChanges streams the names of the lists that change: an event
// "hello" first, after which the web reads them all (it may have missed
// some while it was away), then one event named for each list, agents,
// isolations or services, each time it changes.
func (m *Manager) handleChanges(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		reply(w, nil, fmt.Errorf("streaming unsupported"))
		return
	}
	sub := m.subscribeChanges()
	defer m.unsubscribeChanges(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	if _, err := fmt.Fprint(w, "event: hello\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-sub.wake:
			for _, t := range sub.take() {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", t); err != nil {
					return
				}
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-m.ctx.Done():
			return
		}
	}
}

// noteChanges has the lists looked at once a request that may have changed
// one is answered.
func (m *Manager) noteChanges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			m.pokeChanges()
		}
	})
}
