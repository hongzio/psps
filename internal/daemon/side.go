package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

// Side agents: /btw <question> forks the agent it is sent to into one that
// answers the question on the side, with its tools, knowing the
// conversation so far. It is that agent's alone: shown only in its drawer,
// it is on no line of the list and in no team; autocompact compacts it like
// any other; and it is deleted, never kept archived, as is every one of an
// agent archived, started over (/new) or deleted.

// sideNameMax is how long a side agent's name, its question's start, is.
const sideNameMax = 80

// sideName is a side agent's name: its question's first line, cut.
func sideName(question string) string {
	q, _, _ := strings.Cut(strings.TrimSpace(question), "\n")
	q = strings.Join(strings.Fields(q), " ")
	if utf8.RuneCountInString(q) <= sideNameMax {
		return q
	}
	r := []rune(q)
	return strings.TrimSpace(string(r[:sideNameMax-1])) + "…"
}

// btwQuestion reads a message that is /btw: the question after it, which
// may run over several lines.
func btwQuestion(text string) (string, bool) {
	t := strings.TrimSpace(text)
	rest, ok := strings.CutPrefix(t, "/btw")
	if !ok || (rest != "" && !strings.ContainsAny(rest[:1], " \t\n")) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// Btw forks the live agent id into a side agent and sends it question.
func (m *Manager) Btw(ctx context.Context, id, question string) (api.Agent, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return api.Agent{}, badRequest(errors.New("/btw takes a question: /btw <question>"))
	}
	r, err := m.runtime(id)
	if err != nil {
		return api.Agent{}, err
	}
	from, _, _ := r.view()
	if from.SideOf != "" {
		return api.Agent{}, badRequest(errors.New("a side agent has no side agents of its own: ask it here"))
	}
	a, err := m.forkAgent(ctx, from, "", question)
	if err != nil {
		return api.Agent{}, err
	}
	if err := m.Send(a.ID, question); err != nil {
		return api.Agent{}, err
	}
	return m.viewOf(a)
}

// Detach makes the live side agent id one of its own: out of its agent's
// drawer, it is in the list after that one, in its team, as a fork is, and
// is archived, not deleted, and no longer goes with it.
func (m *Manager) Detach(ctx context.Context, id string) (api.Agent, error) {
	a, err := m.storedAgent(id)
	if err != nil {
		return api.Agent{}, err
	}
	if a.SideOf == "" {
		return api.Agent{}, badRequest(fmt.Errorf("agent %s is not a subagent", a.ID))
	}
	if a.ArchivedAt != nil {
		return api.Agent{}, httpError{http.StatusConflict, fmt.Errorf("subagent %s is being deleted", a.ID)}
	}
	r, err := m.runtime(a.ID)
	if err != nil {
		return api.Agent{}, err
	}
	from, err := m.store.Agent(a.SideOf)
	if err != nil {
		return api.Agent{}, err
	}
	if err := r.detach(); err != nil {
		return api.Agent{}, err
	}
	a.SideOf = ""
	m.joinFork(a, from)
	m.addEvents(a.ID, event.Event{Kind: event.KindNotice, Text: fmt.Sprintf("Moved out of %s's subagents: an agent of its own, in the list.", cmp.Or(from.Handle, from.ID))})
	m.log.Printf("agent %s: a subagent of %s no longer", a.ID, from.ID)
	return m.viewOf(a)
}

// deleteSide deletes the side agent a: stopped and out of every list at
// once (archived, which a side agent is only on its way out), its records
// once its process has exited.
func (m *Manager) deleteSide(a store.Agent) error {
	if a.ArchivedAt == nil {
		now := time.Now().UTC()
		if err := m.store.SetArchived(a.ID, &now); err != nil {
			return err
		}
	}
	m.mu.Lock()
	r := m.runtimes[a.ID]
	delete(m.runtimes, a.ID)
	m.mu.Unlock()
	if r != nil {
		r.stop(m.ctx)
		m.watchesGone(a, "was deleted")
	}
	m.log.Printf("agent %s: side agent of %s: deleting", a.ID, a.SideOf)
	go m.finishSide(a.ID)
	return nil
}

// finishSide deletes the archived side agent id's records, trying again
// while its process is still exiting.
func (m *Manager) finishSide(id string) {
	for try := 0; ; try++ {
		err := m.DeleteAgent(m.ctx, id, false)
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return
		}
		var he httpError
		if !errors.As(err, &he) || he.code != http.StatusConflict || try == 60 {
			m.log.Printf("agent %s: deleting it: %v", id, err)
			return
		}
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// deleteSides deletes the side agents of the agent id.
func (m *Manager) deleteSides(id string) {
	all, err := m.store.Agents()
	if err != nil {
		m.log.Printf("agent %s: its side agents: %v", id, err)
		return
	}
	for _, a := range all {
		if a.SideOf == id {
			if err := m.deleteSide(a); err != nil {
				m.log.Printf("agent %s: deleting it: %v", a.ID, err)
			}
		}
	}
}

// sweepSides finishes what a daemon that stopped left: a side agent
// archived on its way out, or one whose agent is no longer live.
func (m *Manager) sweepSides() {
	all, err := m.store.Agents()
	if err != nil {
		m.log.Printf("side agents: %v", err)
		return
	}
	live := map[string]bool{}
	for _, a := range all {
		if a.ArchivedAt == nil {
			live[a.ID] = true
		}
	}
	for _, a := range all {
		if a.SideOf != "" && (a.ArchivedAt != nil || !live[a.SideOf]) {
			if err := m.deleteSide(a); err != nil {
				m.log.Printf("agent %s: deleting it: %v", a.ID, err)
			}
		}
	}
}
