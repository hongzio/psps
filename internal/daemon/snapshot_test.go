package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
)

// A daemon that starts again takes up each agent where its snapshot left
// it and reads only the log after it.
func TestRestartGoesOnFromTheSnapshot(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") && status(m1, a.ID) == event.StatusIdle })
	stop1()

	seq, data, err := e.st.Snapshot(a.ID)
	if err != nil || data == nil {
		t.Fatalf("no snapshot after a turn: %v", err)
	}
	// A mark only the snapshot has: what the runtime takes up from it
	// rather than from reading the log over.
	var snap rtSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	snap.Answer = "from the snapshot"
	data, _ = json.Marshal(snap)
	if err := e.st.PutSnapshot(a.ID, seq, data); err != nil {
		t.Fatal(err)
	}
	before := len(e.events(a.ID))

	m2, stop2 := e.daemon()
	defer stop2()
	e.waitFor("reattach", func() bool {
		v, _ := m2.Agent(a.ID)
		return v.State.Alive && v.State.Status == event.StatusIdle
	})
	r := m2.runtimes[a.ID]
	r.mu.Lock()
	answer, lastSeen := r.answer, r.lastSeen
	r.mu.Unlock()
	if answer != "from the snapshot" || lastSeen < seq {
		t.Fatalf("not taken up from the snapshot at %d: answer %q, last seen %d", seq, answer, lastSeen)
	}
	if after := len(e.events(a.ID)); after != before {
		t.Fatalf("stored %d events twice", after-before)
	}
	if err := m2.Send(a.ID, "again"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("answer after restart", func() bool { return e.hasAssistant(a.ID, "echo: again") && status(m2, a.ID) == event.StatusIdle })
}

// A snapshot past the end of the log is not of that log: it is dropped,
// and the log read from the start.
func TestASnapshotPastTheLogIsDropped(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1, stop1 := e.daemon()
	a, err := m1.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: hello") && status(m1, a.ID) == event.StatusIdle })
	stop1()

	seq, data, _ := e.st.Snapshot(a.ID)
	var snap rtSnapshot
	json.Unmarshal(data, &snap)
	snap.Seq, snap.Answer = seq+1000, "from the snapshot"
	data, _ = json.Marshal(snap)
	if err := e.st.PutSnapshot(a.ID, snap.Seq, data); err != nil {
		t.Fatal(err)
	}

	m2, stop2 := e.daemon()
	defer stop2()
	e.waitFor("reattach from the start", func() bool {
		v, _ := m2.Agent(a.ID)
		return v.State.Alive && v.State.Status == event.StatusIdle
	})
	r := m2.runtimes[a.ID]
	r.mu.Lock()
	answer := r.answer
	r.mu.Unlock()
	if answer != "echo: hello" {
		t.Fatalf("answer %q: the log was not read from the start", answer)
	}
	if !strings.Contains(e.logs.String(), "before its snapshot") {
		t.Fatalf("logs: %s", e.logs.String())
	}
	if err := m2.Send(a.ID, "again"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("answer", func() bool { return e.hasAssistant(a.ID, "echo: again") })
}
