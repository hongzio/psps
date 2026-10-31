package daemon

import (
	"testing"

	"github.com/hongzio/gero/internal/driver"
)

// A new environment waits for the tasks the process runs in the
// background, which would die with it; options and rewinds do not.
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
