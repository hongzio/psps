package daemon

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/store"
)

func cronOf(t *testing.T, m *Manager, agent string, id int64) store.Cron {
	t.Helper()
	c, err := m.store.Cron(agent, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestACronIsSentWhenItComesDue(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex})
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })

	c, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr(" @every   1m "), Prompt: ptr(" tick \n")})
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule != "@every 1m" || c.Prompt != "tick" || c.IfBusy != store.CronSkip || !c.Enabled || c.NextAt == nil {
		t.Fatalf("%+v", c)
	}
	if v, _ := m.Agent(a.ID); v.Crons != 1 {
		t.Fatalf("crons %d", v.Crons)
	}
	due := *c.NextAt
	if wait := m.cronTick(due.Add(-time.Second)); wait > time.Second || wait <= 0 {
		t.Fatalf("waits %s for the cron a second away", wait)
	}
	if n := len(users(e, a.ID)); n != 0 {
		t.Fatalf("sent early: %d", n)
	}
	m.cronTick(due)
	e.waitFor("the cron's turn", func() bool { return e.hasAssistant(a.ID, "echo: "+cronLetter(c)) })
	if got := users(e, a.ID); len(got) != 1 || !strings.HasPrefix(got[0], `[gero cron #1, on schedule "@every 1m". `) || !strings.HasSuffix(got[0], "]\n\ntick") {
		t.Fatalf("turns %q", got)
	}
	// An agent made without a prompt is named after the cron's message.
	if v, _ := m.Agent(a.ID); v.Name != "tick" {
		t.Fatalf("named %q", v.Name)
	}
	c = cronOf(t, m, a.ID, c.ID)
	if c.LastResult != "sent" || c.SentAt == nil || !c.NextAt.Equal(due.Add(time.Minute)) {
		t.Fatalf("%+v", c)
	}
	// Due again a while later: once, on the beat.
	e.waitFor("idle", func() bool { return status(m, a.ID) == event.StatusIdle })
	m.cronTick(due.Add(5*time.Minute + 10*time.Second))
	e.waitFor("the second turn", func() bool { return len(users(e, a.ID)) == 2 })
	if c = cronOf(t, m, a.ID, c.ID); !c.NextAt.Equal(due.Add(6 * time.Minute)) {
		t.Fatalf("next %s, due %s", c.NextAt, due)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(users(e, a.ID)); n != 2 {
		t.Fatalf("%d turns", n)
	}
}

// A run that finds the agent busy is skipped, or waits for it and is sent
// once it is free again, however many runs came due meanwhile, across a
// daemon restart. An interrupted agent is not free until the operator's
// next message, which goes first.
func TestABusyAgentSkipsOrKeepsACronWaiting(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hang"})
	e.waitFor("the hanging turn", func() bool { return status(m, a.ID) == event.StatusWorking })

	skip, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr("*/5 * * * *"), Prompt: ptr("skipped")})
	if err != nil {
		t.Fatal(err)
	}
	wait, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr("*/5 * * * *"), Prompt: ptr("waited"), IfBusy: ptr(store.CronWait)})
	if err != nil {
		t.Fatal(err)
	}
	due := *wait.NextAt
	for i := range 3 {
		m.cronTick(due.Add(time.Duration(i) * 5 * time.Minute))
	}
	skip, wait = cronOf(t, m, a.ID, skip.ID), cronOf(t, m, a.ID, wait.ID)
	if skip.LastResult != "skipped: it was working" || skip.WaitingSince != nil || !skip.NextAt.Equal(due.Add(15*time.Minute)) {
		t.Fatalf("skip %+v", skip)
	}
	if wait.LastResult != "waiting: it was working" || wait.WaitingSince == nil || !wait.WaitingSince.Equal(due) {
		t.Fatalf("wait %+v", wait)
	}
	stop()

	m, stop = e.daemon()
	defer stop()
	e.waitFor("the turn, still on", func() bool { return status(m, a.ID) == event.StatusWorking })
	if err := syscall.Kill(agentPID(t, m, a.ID), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the interruption", func() bool { return status(m, a.ID) == event.StatusInterrupted })
	m.cronTick(due.Add(15 * time.Minute))
	time.Sleep(300 * time.Millisecond)
	if got := users(e, a.ID); len(got) != 1 {
		t.Fatalf("an interrupted agent was sent %q", got)
	}
	if c := cronOf(t, m, a.ID, wait.ID); !strings.HasPrefix(c.LastResult, "waiting: it was interrupted") {
		t.Fatalf("%+v", c)
	}
	if err := m.Send(a.ID, "go on"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the waiting cron", func() bool { return e.hasAssistant(a.ID, "echo: "+cronLetter(wait)) })
	time.Sleep(300 * time.Millisecond)
	if got := users(e, a.ID); len(got) != 3 || got[1] != "go on" || got[2] != cronLetter(wait) {
		t.Fatalf("turns %q", got)
	}
	if c := cronOf(t, m, a.ID, wait.ID); c.WaitingSince != nil || c.LastResult != "sent" {
		t.Fatalf("%+v", c)
	}
}

func TestSettingACron(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := m.CreateAgent(context.Background(), api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "hang"})
	e.waitFor("the hanging turn", func() bool { return status(m, a.ID) == event.StatusWorking })

	for _, c := range []struct {
		req  api.PutCron
		want string
	}{
		{api.PutCron{Prompt: ptr("x")}, "needs a schedule"},
		{api.PutCron{Schedule: ptr("* * * *"), Prompt: ptr("x")}, "has 4 fields"},
		{api.PutCron{Schedule: ptr("@every 10s"), Prompt: ptr("x")}, "too often"},
		{api.PutCron{Schedule: ptr("@daily"), Prompt: ptr("  ")}, "message is empty"},
		{api.PutCron{Schedule: ptr("@daily"), Prompt: ptr(strings.Repeat("x", cronMaxPrompt+1))}, "at most"},
		{api.PutCron{Schedule: ptr("@daily"), Prompt: ptr("x"), IfBusy: ptr("queue")}, `not "queue"`},
	} {
		var he httpError
		if _, err := m.AddCron(a.ID, c.req); !errors.As(err, &he) || he.code != 400 || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.req, err)
		}
	}

	c, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr("@every 1h"), Prompt: ptr("x"), IfBusy: ptr(store.CronWait)})
	if err != nil {
		t.Fatal(err)
	}
	m.cronTick(*c.NextAt)
	if c = cronOf(t, m, a.ID, c.ID); c.WaitingSince == nil {
		t.Fatalf("%+v", c)
	}
	// A new message leaves when it next comes and the waiting run alone.
	next := *c.NextAt
	if c, err = m.UpdateCron(a.ID, c.ID, api.PutCron{Prompt: ptr("y")}); err != nil || c.Prompt != "y" || !c.NextAt.Equal(next) || c.WaitingSince == nil {
		t.Fatalf("%+v %v", c, err)
	}
	// A new schedule counts from now.
	before := time.Now()
	if c, err = m.UpdateCron(a.ID, c.ID, api.PutCron{Schedule: ptr("@every 2h")}); err != nil || c.NextAt.Before(before.Add(2*time.Hour-time.Second)) {
		t.Fatalf("%+v %v", c, err)
	}
	// Skipping keeps no run waiting; off, it does not come at all.
	if c, err = m.UpdateCron(a.ID, c.ID, api.PutCron{IfBusy: ptr(store.CronSkip)}); err != nil || c.WaitingSince != nil {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = m.UpdateCron(a.ID, c.ID, api.PutCron{Enabled: ptr(false)}); err != nil || c.NextAt != nil {
		t.Fatalf("%+v %v", c, err)
	}
	if v, _ := m.Agent(a.ID); v.Crons != 0 {
		t.Fatalf("crons %d", v.Crons)
	}
	if next, _ := m.store.NextCronAt(time.Now()); next != nil {
		t.Fatalf("an off cron comes at %s", next)
	}
	if c, err = m.UpdateCron(a.ID, c.ID, api.PutCron{Enabled: ptr(true)}); err != nil || c.NextAt == nil {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := m.UpdateCron(a.ID, 99, api.PutCron{Enabled: ptr(true)}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}

	// An archived agent's crons do not come; it is given none, and deleting
	// it deletes them.
	if err := m.Archive(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if due, _ := m.store.DueCrons(c.NextAt.Add(time.Hour)); len(due) != 0 {
		t.Fatalf("due %+v", due)
	}
	if _, err := m.AddCron(a.ID, api.PutCron{Schedule: ptr("@daily"), Prompt: ptr("x")}); err == nil {
		t.Fatal("an archived agent was given a cron")
	}
	e.waitFor("its process to end", func() bool { return m.DeleteAgent(context.Background(), a.ID, false) == nil })
	if cs, _ := m.store.Crons(a.ID); len(cs) != 0 {
		t.Fatalf("%+v", cs)
	}
}
