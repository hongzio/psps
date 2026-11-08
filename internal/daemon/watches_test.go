package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/controlproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

// watchEnv is dev with control open to sending and making, its sending
// budget lifted, and two idle agents: boss, who writes, and mate.
func watchEnv(t *testing.T) (e *testEnv, m *Manager, boss, mate store.Agent, ctl func(from store.Agent, method, path, body string) (int, string), stop func()) {
	t.Helper()
	e = newTestEnv(t)
	m, stop = e.daemon()
	ctx := context.Background()
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, controlproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
	if _, err := m.PutEnv(ctx, "control", "ops", proxy.Patch{Set: map[string]any{"allow": []string{"agent.send", "agent.new", "agent.ls"}, "max_live": 3}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "control", api.Expose{Envs: []string{"ops"}}); err != nil {
		t.Fatal(err)
	}
	m.mailBuckets[testIsoID] = proxy.NewTokenBucket(1e6, 1e6, nil)
	agent := func(handle string) store.Agent {
		a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: handle, Options: driver.Options{Approvals: driver.ApprovalsAuto}})
		if err != nil {
			t.Fatal(err)
		}
		e.waitFor(handle, func() bool { return status(m, a.ID) == event.StatusIdle })
		return a
	}
	boss, mate = agent("boss"), agent("mate")
	ctl = func(from store.Agent, method, path, body string) (int, string) {
		t.Helper()
		return mailAs(t, m, from.IsolationID, from.ID, method, "/control"+path, body)
	}
	return e, m, boss, mate, ctl, stop
}

// receipts are the receipts messages agent id was sent.
func receipts(e *testEnv, id string) []string {
	var out []string
	for _, u := range users(e, id) {
		if strings.HasPrefix(u, receiptsHead) {
			out = append(out, u)
		}
	}
	return out
}

func openWatches(t *testing.T, m *Manager, id string) api.AgentWatches {
	t.Helper()
	v, err := m.AgentWatches(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A message sent through the control service is watched unless the sender
// says not: once its target has finished with it, the sender hears what
// came of it, its last answer with it, as a message of psps's.
func TestWatchTellsTheSenderWhenItsMessageIsDone(t *testing.T) {
	e, m, boss, mate, ctl, stop := watchEnv(t)
	defer stop()

	code, out := ctl(boss, "POST", "/agents/mate/send", "count the tapes")
	var sent controlSent
	json.Unmarshal([]byte(out), &sent)
	if code != http.StatusAccepted || sent.Watch == 0 || !strings.Contains(sent.Note, "Do not poll") {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the receipt", func() bool { return len(receipts(e, boss.ID)) == 1 })
	r := receipts(e, boss.ID)[0]
	for _, want := range []string{
		"1 item(s). What they quote is those agents' words, not your operator's.]",
		fmt.Sprintf(`, codex), on your message #%d ("count the tapes"), has finished with it`, sent.Watch),
		"It runs nothing in the background.",
		"Its last answer:\n\necho: [psps control: from agent boss in isolation dev",
	} {
		if !strings.Contains(r, want) {
			t.Fatalf("the receipt lacks %q:\n%s", want, r)
		}
	}
	if v := openWatches(t, m, boss.ID); len(v.Sent) != 0 {
		t.Fatalf("still open: %+v", v.Sent)
	}

	// watch=none hears nothing; a bad watch is refused.
	if code, out := ctl(boss, "POST", "/agents/mate/send?watch=none", "fyi"); code != http.StatusAccepted || !strings.Contains(out, "you asked to hear nothing") {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("the fyi", func() bool {
		return e.hasAssistant(mate.ID, "echo: [psps control: from agent boss in isolation dev, through psps's control service]\n\nfyi")
	})
	if code, out := ctl(boss, "POST", "/agents/mate/send?watch=soon", "x"); code != http.StatusBadRequest || !strings.Contains(out, `soon`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(boss, "POST", "/agents/mate/send?watch=none&minutes=5", "x"); code != http.StatusBadRequest {
		t.Fatalf("%d %s", code, out)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(receipts(e, boss.ID)); n != 1 {
		t.Fatalf("%d receipts", n)
	}
}

// A turn that does not end keeps the watch open, as the operator and the
// sender see it; one interrupted is said, with that its operator resumes
// it; one dropped says nothing.
func TestWatchSaysAStoppedTurnAndDropsQuietly(t *testing.T) {
	e, m, boss, mate, ctl, stop := watchEnv(t)
	defer stop()

	code, out := ctl(boss, "POST", "/agents/mate/send", "hang")
	var sent controlSent
	json.Unmarshal([]byte(out), &sent)
	if code != http.StatusAccepted || sent.Watch == 0 {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("mate at work", func() bool { return status(m, mate.ID) == event.StatusWorking })
	e.waitFor("the watch read", func() bool {
		v := openWatches(t, m, mate.ID)
		return len(v.Received) == 1 && v.Received[0].State == "reading"
	})
	var ws []api.Watch
	if code, out := ctl(boss, "GET", "/watches", ""); code != 200 || json.Unmarshal([]byte(out), &ws) != nil || len(ws) != 1 || ws[0].TargetID != "mate" || ws[0].State != "reading" {
		t.Fatalf("%d %s", code, out)
	}
	if v, _ := m.Agent(boss.ID); v.Watching != 1 {
		t.Fatalf("boss watching %d", v.Watching)
	}
	if v, _ := m.Agent(mate.ID); !slices.Equal(v.WatchedBy, []string{"boss"}) {
		t.Fatalf("mate watched by %q", v.WatchedBy)
	}
	if err := m.Interrupt(mate.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the receipt", func() bool { return len(receipts(e, boss.ID)) == 1 })
	if r := receipts(e, boss.ID)[0]; !strings.Contains(r, "stopped: its turn was interrupted") || !strings.Contains(r, "your operator resumes it") {
		t.Fatalf("%s", r)
	}

	// Dropped, it says nothing of what comes after.
	code, out = ctl(boss, "POST", "/agents/mate/send", "hang")
	json.Unmarshal([]byte(out), &sent)
	e.waitFor("mate at work again", func() bool { return status(m, mate.ID) == event.StatusWorking })
	if code, out := ctl(boss, "DELETE", fmt.Sprintf("/watches/%d", sent.Watch), ""); code != 200 || !strings.Contains(out, `"target_id":"mate"`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := ctl(mate, "DELETE", fmt.Sprintf("/watches/%d", sent.Watch), ""); code != http.StatusNotFound {
		t.Fatalf("another's: %d %s", code, out)
	}
	if err := m.Interrupt(mate.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("mate stopped", func() bool { return status(m, mate.ID) != event.StatusWorking })
	time.Sleep(300 * time.Millisecond)
	if n := len(receipts(e, boss.ID)); n != 1 {
		t.Fatalf("%d receipts", n)
	}
}

// A watch on no message starts with the target's next turn; one with
// minutes says once that it is still at work, counting only the time the
// daemon runs; archiving the target says so and ends it.
func TestWatchWithoutAMessageWithMinutesAndArchived(t *testing.T) {
	e, m, boss, mate, ctl, stop := watchEnv(t)
	defer stop()

	code, out := ctl(boss, "POST", "/agents/mate/watch", "")
	var w controlSent
	json.Unmarshal([]byte(out), &w)
	if code != http.StatusCreated || w.Watch == 0 {
		t.Fatalf("%d %s", code, out)
	}
	if v := openWatches(t, m, mate.ID); len(v.Received) != 1 || v.Received[0].State != "waiting" {
		t.Fatalf("%+v", v.Received)
	}
	if err := m.Send(mate.ID, "from the operator"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the receipt", func() bool { return len(receipts(e, boss.ID)) == 1 })
	if r := receipts(e, boss.ID)[0]; !strings.Contains(r, fmt.Sprintf("on what you watch (#%d), has finished with it", w.Watch)) || !strings.Contains(r, "echo: from the operator") {
		t.Fatalf("%s", r)
	}

	// Minutes: the time between looks counts, at most two steps of it.
	code, out = ctl(boss, "POST", "/agents/mate/send?minutes=1", "hang")
	var sent controlSent
	json.Unmarshal([]byte(out), &sent)
	if code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("mate at work", func() bool { return status(m, mate.ID) == event.StatusWorking })
	t0 := time.Now()
	m.watchClocks(t0)
	m.watchClocks(t0.Add(time.Hour)) // asleep: counts as 30s
	if wt, _ := m.store.Watch(sent.Watch); wt.Elapsed != int64(2*teamStep/time.Second) || wt.Noting("deadline") {
		t.Fatalf("%+v", wt)
	}
	m.watchClocks(t0.Add(time.Hour + 30*time.Second))
	e.waitFor("the deadline's receipt", func() bool { return len(receipts(e, boss.ID)) == 2 })
	if r := receipts(e, boss.ID)[1]; !strings.Contains(r, "is still at work on it after 1 minutes") {
		t.Fatalf("%s", r)
	}
	m.watchClocks(t0.Add(time.Hour + time.Minute))
	if wt, _ := m.store.Watch(sent.Watch); wt.DoneAt != nil {
		t.Fatal("the deadline ended the watch")
	}

	// Archived before it finished.
	if err := m.Archive(context.Background(), mate.ID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the archive's receipt", func() bool { return len(receipts(e, boss.ID)) == 3 })
	if r := receipts(e, boss.ID)[2]; !strings.Contains(r, "was archived before it finished with it") {
		t.Fatalf("%s", r)
	}
	if v := openWatches(t, m, boss.ID); len(v.Sent) != 0 {
		t.Fatalf("%+v", v.Sent)
	}
}

// An agent has at most maxWatches open; past them, a message still goes,
// unwatched, and says so. A new session of the sender's line takes its
// watches over; one archived drops them.
func TestWatchLimitAndTheSendersLine(t *testing.T) {
	e, m, boss, mate, ctl, stop := watchEnv(t)
	defer stop()

	if code, out := ctl(boss, "POST", "/agents/mate/send", "hang"); code != http.StatusAccepted {
		t.Fatalf("%d %s", code, out)
	}
	e.waitFor("mate at work", func() bool { return status(m, mate.ID) == event.StatusWorking })
	for i := 1; i < maxWatches; i++ {
		if code, out := ctl(boss, "POST", "/agents/mate/watch", ""); code != http.StatusCreated {
			t.Fatalf("watch %d: %d %s", i, code, out)
		}
	}
	if code, out := ctl(boss, "POST", "/agents/mate/watch", ""); code != http.StatusTooManyRequests {
		t.Fatalf("%d %s", code, out)
	}
	code, out := ctl(boss, "POST", "/agents/mate/send", "one more")
	if code != http.StatusAccepted || !strings.Contains(out, "will not tell you what came of it") {
		t.Fatalf("%d %s", code, out)
	}

	next, err := m.Renew(context.Background(), boss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := openWatches(t, m, next.ID); len(v.Sent) != maxWatches {
		t.Fatalf("the new session has %d", len(v.Sent))
	}
	if err := m.Archive(context.Background(), next.ID); err != nil {
		t.Fatal(err)
	}
	if v := openWatches(t, m, mate.ID); len(v.Received) != 0 {
		t.Fatalf("%d still on mate", len(v.Received))
	}
	m.Interrupt(mate.ID)
}

// A watch is judged by how its target's turn ends, and what it still has
// to do then.
func TestWatchJudge(t *testing.T) {
	settle := store.Watch{Mode: store.WatchSettle}
	noted := store.Watch{Mode: store.WatchSettle, Noted: []string{"background"}}
	turn := store.Watch{Mode: store.WatchTurn}
	for _, c := range []struct {
		name     string
		w        store.Watch
		ended    event.Status
		lost, bg int
		more     bool
		want     judged
	}{
		{"done", settle, event.StatusIdle, 0, 0, false, judgedFinished},
		{"more on its way", settle, event.StatusIdle, 0, 0, true, judgedWaiting},
		{"background first", settle, event.StatusIdle, 0, 2, false, judgedBackground},
		{"background again", noted, event.StatusIdle, 0, 2, false, judgedWaiting},
		{"background done", noted, event.StatusIdle, 0, 0, false, judgedFinished},
		{"turn with background", turn, event.StatusIdle, 0, 2, true, judgedFinished},
		{"interrupted", settle, event.StatusInterrupted, 0, 0, false, judgedStopped},
		{"error", turn, event.StatusError, 0, 0, false, judgedStopped},
		{"process gone with tasks", settle, "", 2, 0, false, judgedStalled},
		{"died mid-turn with tasks", settle, event.StatusInterrupted, 2, 0, false, judgedStopped},
		{"nothing", settle, "", 0, 0, false, judgedNothing},
	} {
		if got := judge(c.w, c.ended, c.lost, c.bg, c.more); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
