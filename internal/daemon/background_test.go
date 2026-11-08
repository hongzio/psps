package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
)

// backgrounds gives agents tasks in the background their fake CLI cannot
// run: while one's process lives, its driver says it runs as many as set.
type backgrounds struct {
	mu sync.Mutex
	n  map[string]int
}

func withBackgrounds(t *testing.T) *backgrounds {
	b := &backgrounds{n: map[string]int{}}
	wrapDriver = func(agent string, d driver.Driver) driver.Driver { return bgDriver{d, b, agent} }
	t.Cleanup(func() { wrapDriver = nil })
	return b
}

func (b *backgrounds) set(agent string, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n[agent] = n
}

type bgDriver struct {
	driver.Driver
	b     *backgrounds
	agent string
}

func (d bgDriver) State() driver.State {
	st := d.Driver.State()
	d.b.mu.Lock()
	n := d.b.n[d.agent]
	d.b.mu.Unlock()
	if st.Alive && n > 0 {
		st.Background = n
		for i := range n {
			st.Tasks = append(st.Tasks, driver.Task{Kind: driver.TaskShell, Description: fmt.Sprintf("job %d", i+1)})
		}
	}
	return st
}

// A watch on an agent that ends its turn with tasks in the background says
// so once and waits for a turn that ends with none, saying what it said
// between; its process ending with them stalls it, saying nothing.
func TestWatchWaitsForTasksInTheBackground(t *testing.T) {
	bg := withBackgrounds(t)
	e, m, boss, mate, ctl, stop := watchEnv(t)
	defer stop()

	bg.set(mate.ID, 2)
	if code, out := ctl(boss, "POST", "/agents/mate/send", "start the recording"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the first receipt", func() bool { return len(receipts(e, boss.ID)) == 1 })
	if r := receipts(e, boss.ID)[0]; !strings.Contains(r, "ended its turn") || !strings.Contains(r, "runs 2 tasks in the background (job 1; job 2)") {
		t.Fatalf("%s", r)
	}
	v := openWatches(t, m, boss.ID)
	if len(v.Sent) != 1 || v.Sent[0].State != "background" {
		t.Fatalf("%+v", v.Sent)
	}
	// Another turn with them still running says nothing; one with none
	// ends it, with what it said between.
	if err := m.Send(mate.ID, "still recording"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("that turn", func() bool { return e.hasAssistant(mate.ID, "echo: still recording") })
	time.Sleep(200 * time.Millisecond)
	if n := len(receipts(e, boss.ID)); n != 1 {
		t.Fatalf("%d receipts", n)
	}
	bg.set(mate.ID, 0)
	if err := m.Send(mate.ID, "recorded"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the last receipt", func() bool { return len(receipts(e, boss.ID)) == 2 })
	r := receipts(e, boss.ID)[1]
	for _, want := range []string{"has finished with it", "Its last answer:\n\necho: recorded", "What it said at the end of its turns before", "echo: still recording", "Others wrote to it too"} {
		if !strings.Contains(r, want) {
			t.Fatalf("lacks %q:\n%s", want, r)
		}
	}

	// Its process ends while they run: stalled, and nothing is said.
	bg.set(mate.ID, 1)
	ctl(boss, "POST", "/agents/mate/send", "again")
	e.waitFor("its receipt", func() bool { return len(receipts(e, boss.ID)) == 3 })
	if err := syscall.Kill(agentPID(t, m, mate.ID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the stall", func() bool {
		v := openWatches(t, m, boss.ID)
		return len(v.Sent) == 1 && v.Sent[0].State == "stalled" && v.Sent[0].Stalled == 1
	})
	if a, _ := m.Agent(boss.ID); a.WatchStalled != 1 {
		t.Fatalf("stalled %d", a.WatchStalled)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(receipts(e, boss.ID)); n != 3 {
		t.Fatalf("%d receipts", n)
	}
	// Resumed by the operator, the turn after is judged as any.
	bg.set(mate.ID, 0)
	if err := m.Send(mate.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the receipt after", func() bool { return len(receipts(e, boss.ID)) == 4 })
	if r := receipts(e, boss.ID)[3]; !strings.Contains(r, "has finished with it") {
		t.Fatalf("%s", r)
	}
}

// A lead's agent that ends its turn with tasks in the background is at
// work until a turn ends with none: its lead hears of the first such turn,
// and of the last with what it said between; it may release it, or have
// each turn told; past its turn's limit it counts as done; its process
// ending with them leaves it at work, stalled, saying nothing.
func TestALeadsAgentIsAtWorkWhileItsTasksRunInTheBackground(t *testing.T) {
	bg := withBackgrounds(t)
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	boss, _ := withTeams(t, e, m, api.PutLead{})
	w := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "recorder", Kind: driver.Codex, Prompt: "ready", TurnMinutes: 1})
	made, err := m.storedAgent(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := made.ID
	allNews := func() string { return strings.Join(news(e, boss.ID), "\n") }
	e.waitFor("its first news", func() bool { return strings.Contains(allNews(), "echo: [psps team:") })
	l, _ := m.store.LeadOfLine(boss.ID)
	view := func() api.TeamAgent {
		code, out := teamAs(t, m, boss.ID, "GET", "/agents/"+w.ID, "")
		var v api.TeamAgent
		if code != 200 || json.Unmarshal([]byte(out), &v) != nil {
			t.Fatalf("%d %s", code, out)
		}
		return v
	}

	bg.set(id, 2)
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send", "record"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the news of tasks in the background", func() bool { return strings.Contains(allNews(), "but runs 2 tasks in the background (job 1; job 2)") })
	if v := view(); !v.InBackground || v.Background != 2 || v.Watch != "settle" || m.childrenBusy(l.Line) != 1 || m.childrenInTurns(l.Line) != 0 {
		t.Fatalf("%+v, %d busy", v, m.childrenBusy(l.Line))
	}
	before := len(news(e, boss.ID))
	if err := m.Send(id, "still recording"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("that turn", func() bool { return e.hasAssistant(id, "echo: still recording") })
	time.Sleep(200 * time.Millisecond)
	if n := len(news(e, boss.ID)); n != before {
		t.Fatalf("news of a turn with tasks still running: %q", news(e, boss.ID)[before:])
	}
	bg.set(id, 0)
	if err := m.Send(id, "recorded"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the news of the end", func() bool {
		s := allNews()
		return strings.Contains(s, "echo: recorded") && strings.Contains(s, "while its tasks ran in the background, oldest first:\n\n---\n\necho: still recording")
	})
	e.waitFor("it done", func() bool { return m.childrenBusy(l.Line) == 0 })

	// Released, it is done though its tasks run on; it hears of each turn.
	bg.set(id, 1)
	teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send", "record more")
	e.waitFor("the news", func() bool { return strings.Count(allNews(), "but runs 1 task in the background") == 1 })
	code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/release", "")
	var rel api.TeamRelease
	json.Unmarshal([]byte(out), &rel)
	if code != 200 || rel.Background != 1 || !strings.Contains(rel.Note, "counts as done") {
		t.Fatalf("%d %s", code, out)
	}
	if v := view(); v.InBackground || !v.Released || m.childrenBusy(l.Line) != 0 {
		t.Fatalf("%+v", v)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/release", ""); code != http.StatusConflict {
		t.Fatalf("twice: %d %s", code, out)
	}
	before = len(news(e, boss.ID))
	m.Send(id, "a leftover ended")
	e.waitFor("its news", func() bool {
		return strings.Contains(allNews(), "echo: a leftover ended") && strings.Contains(allNews(), "It still runs 1 task in the background")
	})

	// watch=turn: each turn's end is news, whatever runs.
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send?watch=turn", "quick look"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("its news", func() bool { return strings.Contains(allNews(), "quick look") })
	if v := view(); v.InBackground || v.Watch != "turn" {
		t.Fatalf("%+v", v)
	}
	if code, out := teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send?watch=later", "x"); code != http.StatusBadRequest {
		t.Fatalf("%d %s", code, out)
	}

	// Past its turn's limit, counted as the daemon runs, it is done.
	teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send", "long recording")
	e.waitFor("the news", func() bool { return strings.Count(allNews(), "but runs 1 task in the background") == 2 })
	t0 := time.Now()
	m.clocks(t0)
	m.clocks(t0.Add(30 * time.Second))
	if !view().InBackground {
		t.Fatal("done before its limit")
	}
	m.clocks(t0.Add(time.Minute))
	e.waitFor("the news of its limit", func() bool {
		return strings.Contains(allNews(), "1 minutes after its turn ended, its turn's limit; psps counts it as done")
	})
	if v := view(); v.InBackground || !v.Released {
		t.Fatalf("%+v", v)
	}

	// Its process ends with them: at work, stalled, and nothing is said.
	teamAs(t, m, boss.ID, "POST", "/agents/"+w.ID+"/send", "record again")
	e.waitFor("the news", func() bool { return strings.Count(allNews(), "but runs 1 task in the background") == 3 })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	before = len(news(e, boss.ID))
	if err := syscall.Kill(agentPID(t, m, id), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the stall", func() bool { v := view(); return v.Stalled == 1 && v.InBackground })
	m.clocks(t0.Add(time.Hour))
	m.clocks(t0.Add(2 * time.Hour))
	time.Sleep(200 * time.Millisecond)
	if v := view(); !v.InBackground || len(news(e, boss.ID)) != before || status(m, id) == event.StatusWorking {
		t.Fatalf("%+v, %d news, %s", v, len(news(e, boss.ID))-before, status(m, id))
	}
}
