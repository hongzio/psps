package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
)

// userEvent is the stored event of the message text the agent got.
func (e *testEnv) userEvent(id, text string) event.Event {
	e.t.Helper()
	for _, ev := range e.events(id) {
		if ev.Kind == event.KindUser && ev.Text == text {
			return ev
		}
	}
	e.t.Fatalf("no message %q", text)
	return event.Event{}
}

func TestRewindReplacesAMessageAndWhatFollowed(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") })
	m.Send(a.ID, "two")
	e.waitFor("second answer", func() bool { return e.hasAssistant(a.ID, "echo: two") })
	m.Send(a.ID, "three")
	e.waitFor("third answer", func() bool { return e.hasAssistant(a.ID, "echo: three") })
	two := e.userEvent(a.ID, "two")

	var he httpError
	if err := m.Rewind(a.ID, two.ID+1, "deux"); !errors.As(err, &he) || he.code != http.StatusBadRequest {
		t.Fatalf("rewind to an event that is no message: %v", err)
	}
	if err := m.Rewind(a.ID, two.ID, "deux"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the edited message answered", func() bool { return e.hasAssistant(a.ID, "echo: deux") })
	var rw *event.Rewind
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindRewind {
			rw = ev.Rewind
		}
	}
	if rw == nil || rw.From != two.Src || rw.To <= e.userEvent(a.ID, "three").Src || rw.To >= e.userEvent(a.ID, "deux").Src {
		t.Fatalf("rewind = %+v, want from message two to before deux", rw)
	}
	m.Send(a.ID, "recall")
	e.waitFor("what the agent remembers", func() bool { return e.hasAssistant(a.ID, "echo: recall=one,deux") })

	// The message gone back from is no longer in the conversation.
	if err := m.Rewind(a.ID, two.ID, "zwei"); !errors.As(err, &he) || he.code != http.StatusConflict {
		t.Fatalf("rewind to a message rewound away: %v", err)
	}
}

// A rewind accepted by a daemon that died before the agent saw it is done
// by the next one, once.
func TestARewindSurvivesDaemonDeath(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") })
	m1.Send(a.ID, "two")
	e.waitFor("second answer", func() bool { return e.hasAssistant(a.ID, "echo: two") })
	stop1()

	// As the dead daemon left it: accepted, nothing written to the agent.
	stored, err := e.st.Agent(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddRewind(a.ID, "deux", stored.LastSrc, e.userEvent(a.ID, "two").Src); err != nil {
		t.Fatal(err)
	}
	m2, stop2 := e.daemon()
	defer stop2()
	e.waitFor("the edited message answered", func() bool { return e.hasAssistant(a.ID, "echo: deux") })
	m2.Send(a.ID, "recall")
	e.waitFor("what the agent remembers", func() bool { return e.hasAssistant(a.ID, "echo: recall=one,deux") })
	n := 0
	for _, ev := range e.events(a.ID) {
		if ev.Kind == event.KindRewind {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d rewinds", n)
	}
}
