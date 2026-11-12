package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/store"
)

func TestBtwQuestion(t *testing.T) {
	for in, want := range map[string]string{
		"/btw why?":          "why?",
		"  /btw  a\nb ":      "a\nb",
		"/btw":               "",
		"/btw\nwhat is this": "what is this",
	} {
		if q, ok := btwQuestion(in); !ok || q != want {
			t.Errorf("btwQuestion(%q) = %q, %v; want %q", in, q, ok, want)
		}
	}
	for _, in := range []string{"/btwx", "btw hi", "say /btw hi", "/bt"} {
		if _, ok := btwQuestion(in); ok {
			t.Errorf("btwQuestion(%q) took it", in)
		}
	}
	if n := sideName("  first   line\nsecond"); n != "first line" {
		t.Errorf("sideName: %q", n)
	}
	if n := sideName(strings.Repeat("가", 200)); len([]rune(n)) != sideNameMax || !strings.HasSuffix(n, "…") {
		t.Errorf("a long sideName: %q", n)
	}
}

func TestBtwForksASideAgentThatIsDeletedNotArchived(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()

	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one", Handle: "id1"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("an answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })

	if _, err := m.Message("id1", "/btw"); err == nil {
		t.Fatal("/btw with no question was taken")
	}
	sent, err := m.Message("id1", "/btw is it\nsafe?")
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Side || sent.Agent == "" {
		t.Fatalf("sent: %+v", sent)
	}
	s, err := e.st.Agent(sent.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if s.SideOf != a.ID || s.ForkedFrom != a.ID || s.ForkOf != "th-fake" || s.Workdir != a.Workdir || s.Name != "is it" {
		t.Fatalf("the side agent: %+v", s)
	}
	// It answers its question, not the agent it is of; its conversation is
	// its own, after a note of where it starts.
	e.waitFor("its answer", func() bool { return e.hasAssistant(s.ID, "echo: is it\nsafe?") })
	if e.hasAssistant(a.ID, "echo: is it\nsafe?") {
		t.Fatal("the agent itself was sent the question")
	}
	evs, _ := e.st.Events(s.ID, 0, 0)
	if len(evs) == 0 || evs[0].Kind != event.KindNotice || !strings.HasPrefix(evs[0].Text, "A side agent of id1") {
		t.Fatalf("its conversation: %+v", evs)
	}
	for _, ev := range evs {
		if ev.Kind == event.KindUser && ev.Text == "one" {
			t.Fatal("the conversation it is forked from was copied into its own")
		}
	}
	// No side agent of a side agent, and none starts over.
	if _, err := m.Message(s.ID, "/btw more"); err == nil {
		t.Fatal("a side agent made one of its own")
	}
	if _, err := m.Message(s.ID, "/new"); err == nil {
		t.Fatal("a side agent started over")
	}

	// Deleted live: it goes, never kept archived.
	e.waitFor("idle", func() bool { return status(m, s.ID) == event.StatusIdle })
	if err := m.DeleteAgent(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	e.waitFor("it to be gone", func() bool { _, err := e.st.Agent(s.ID); return errors.Is(err, store.ErrNotFound) })

	// Moved out, it is an agent of its own, right under the one it was of,
	// which no longer takes it along.
	out, err := m.Message("id1", "/btw out")
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("its answer", func() bool { return e.hasAssistant(out.Agent, "echo: out") })
	if _, err := m.Detach(ctx, a.ID); err == nil {
		t.Fatal("an agent that is no subagent was moved out")
	}
	v, err := m.Detach(ctx, out.Agent)
	if err != nil {
		t.Fatal(err)
	}
	if v.SideOf != "" {
		t.Fatalf("moved out: %+v", v)
	}
	if s, _ := e.st.Agent(out.Agent); s.SideOf != "" {
		t.Fatalf("stored: %+v", s)
	}
	order, _ := e.st.Order()
	if p, q := order[store.LiveList][a.ID], order[store.LiveList][out.Agent]; q != p+1 {
		t.Fatalf("its place: %d, the agent's %d", q, p)
	}
	if _, err := m.Detach(ctx, out.Agent); err == nil {
		t.Fatal("moved out twice")
	}

	// The agent's archive takes its side agents with it.
	sent, err = m.Message("id1", "/btw another")
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("its answer", func() bool { return e.hasAssistant(sent.Agent, "echo: another") })
	if err := m.Archive(ctx, "id1"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("it to be gone", func() bool { _, err := e.st.Agent(sent.Agent); return errors.Is(err, store.ErrNotFound) })
	if _, err := e.st.Agent(a.ID); err != nil {
		t.Fatalf("the agent itself: %v", err)
	}
	if s, err := e.st.Agent(out.Agent); err != nil || s.ArchivedAt != nil {
		t.Fatalf("the one moved out: %+v, %v", s, err)
	}
}
