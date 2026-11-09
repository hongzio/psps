package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
)

func TestAutocompactSettings(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	if v, err := m.Autocompact(); err != nil || v.On || v.Minutes != autocompactDefault {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := m.SetAutocompact(api.Autocompact{On: true, Minutes: 0}); err == nil {
		t.Fatal("0 minutes taken")
	}
	if _, err := m.SetAutocompact(api.Autocompact{On: true, Minutes: 7}); err != nil {
		t.Fatal(err)
	}
	if v, err := m.Autocompact(); err != nil || !v.On || v.Minutes != 7 {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestAutocompactCompactsAQuietAgentOnce(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hi"})
	e.waitFor("the first turn", func() bool { return e.hasAssistant(a.ID, "echo: hi") })
	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })
	if _, err := m.UpdateAgent(a.ID, api.UpdateAgent{Parked: ptr(true)}); err != nil {
		t.Fatal(err)
	}

	// Off, nothing is sent however quiet it is.
	m.autocompactTick(time.Now().Add(time.Hour))
	if _, err := m.SetAutocompact(api.Autocompact{On: true, Minutes: 10}); err != nil {
		t.Fatal(err)
	}
	// On, not before its minutes.
	m.autocompactTick(time.Now().Add(5 * time.Minute))
	time.Sleep(100 * time.Millisecond)
	if got := users(e, a.ID); !slices.Equal(got, []string{"hi"}) {
		t.Fatalf("turns %q", got)
	}

	m.autocompactTick(time.Now().Add(11 * time.Minute))
	e.waitFor("context compacted", func() bool {
		for _, ev := range e.events(a.ID) {
			if ev.Kind == event.KindNotice && ev.Text == "context compacted" {
				return true
			}
		}
		return false
	})
	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })
	v, err := m.Agent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Parked || v.Compacting {
		t.Fatalf("parked %v, compacting %v after autocompact", v.Parked, v.Compacting)
	}

	// Its last request was /compact: not again, however long it stays quiet.
	m.autocompactTick(time.Now().Add(3 * time.Hour))
	time.Sleep(100 * time.Millisecond)
	if got := users(e, a.ID); !slices.Equal(got, []string{"hi", "/compact"}) {
		t.Fatalf("turns %q", got)
	}

	// A new message starts a new quiet spell.
	if _, err := m.Message(a.ID, "again"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the second turn", func() bool { return e.hasAssistant(a.ID, "echo: again") })
	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })
	m.autocompactTick(time.Now().Add(11 * time.Minute))
	e.waitFor("the second /compact", func() bool {
		return slices.Equal(users(e, a.ID), []string{"hi", "/compact", "again", "/compact"})
	})
}
