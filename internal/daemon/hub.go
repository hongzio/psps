package daemon

import (
	"sync"

	"github.com/hongzio/gero/internal/event"
)

// hub fans events out to live subscribers. A subscriber that cannot keep up
// is dropped; it reconnects and reads what it missed from the store.
type hub struct {
	mu   sync.Mutex
	subs map[*subscription]struct{}
}

type subscription struct {
	agent string // "" for every agent
	ch    chan event.Event
	gone  chan struct{}
}

func newHub() *hub { return &hub{subs: map[*subscription]struct{}{}} }

func (h *hub) subscribe(agent string) *subscription {
	s := &subscription{agent: agent, ch: make(chan event.Event, 1024), gone: make(chan struct{})}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.gone)
	}
}

func (h *hub) publish(e event.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.agent != "" && s.agent != e.Agent {
			continue
		}
		select {
		case s.ch <- e:
		default:
			delete(h.subs, s)
			close(s.gone)
		}
	}
}
