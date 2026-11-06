package daemon

import (
	"testing"

	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/shimproto"
	"github.com/hongzio/psps/internal/store"
)

// A new environment waits for the tasks the process runs in the
// background, which would die with it; a rewind does not.
func TestANewEnvironmentWaitsForWhatRunsInTheBackground(t *testing.T) {
	r := &runtime{restartWhenIdle: true}
	if r.envRestartDue(driver.State{Alive: true, Background: 1}) {
		t.Fatal("replaced with a task in the background")
	}
	if !r.envRestartDue(driver.State{Alive: true}) {
		t.Fatal("not replaced with nothing in the background")
	}
	if r.freeLocked(driver.State{Alive: true, Background: 1, Status: "idle"}) == false {
		t.Fatal("an agent waiting on its background for a new environment takes no turns")
	}
}

// Claude's new options wait for what its process runs in the background,
// as a new environment does, but for an approvals mode it switches to
// while it runs; Codex's reach its next turn, and options saved as they were
// replace nothing.
func TestNewOptionsWaitForWhatRunsInTheBackground(t *testing.T) {
	for _, c := range []struct {
		kind    driver.Kind
		was, o  driver.Options
		replace bool
	}{
		{driver.Claude, driver.Options{Approvals: driver.ApprovalsAuto}, driver.Options{Approvals: driver.ApprovalsAuto, Effort: "high"}, true},
		{driver.Claude, driver.Options{Approvals: driver.ApprovalsAuto}, driver.Options{Approvals: driver.ApprovalsAuto}, false},
		{driver.Claude, driver.Options{Approvals: driver.ApprovalsAsk}, driver.Options{Approvals: driver.ApprovalsAuto}, false},
		{driver.Claude, driver.Options{Approvals: driver.ApprovalsAsk}, driver.Options{Approvals: driver.ApprovalsNever}, true},
		{driver.Codex, driver.Options{Approvals: driver.ApprovalsAuto}, driver.Options{Approvals: driver.ApprovalsAuto, Effort: "high"}, false},
	} {
		d, err := driver.New(c.kind, "a1", "", c.was)
		if err != nil {
			t.Fatal(err)
		}
		d.Observe(shimproto.Record{Dir: shimproto.DirSys, Sys: &shimproto.Sys{Kind: shimproto.SysStarted, PID: 1, Argv: d.Command()}})
		r := &runtime{agent: store.Agent{ID: "a1", Kind: c.kind, Options: c.was}, drv: d}
		r.setOptions(c.o)
		st := r.drv.State()
		if st.Restart {
			t.Fatalf("%s %+v: replaced at once, whatever runs in the background", c.kind, c.o)
		}
		busy := st
		busy.Background = 1
		if r.envRestartDue(busy) {
			t.Fatalf("%s %+v: replaced with a task in the background", c.kind, c.o)
		}
		if got := r.envRestartDue(st); got != c.replace {
			t.Fatalf("%s %+v: replaced with nothing in the background = %v, want %v", c.kind, c.o, got, c.replace)
		}
	}
}
