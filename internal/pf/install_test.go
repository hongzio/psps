package pf

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeLaunchd keeps the label loaded for a few prints after bootout, and
// refuses a bootstrap while it is, as launchd does.
type fakeLaunchd struct {
	calls     []string
	lingering int
	loaded    bool
}

func (f *fakeLaunchd) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args[0])
	switch args[0] {
	case "bootout":
		return nil, nil
	case "print":
		if f.lingering > 0 {
			f.lingering--
			return nil, nil
		}
		return nil, errors.New("exit status 113")
	case "bootstrap":
		if f.lingering > 0 {
			return []byte("Bootstrap failed: 5: Input/output error"), errors.New("exit status 5")
		}
		f.loaded = true
		return nil, nil
	}
	return nil, errors.New("unexpected")
}

func withLaunchd(t *testing.T, f *fakeLaunchd) {
	prev := launchctl
	launchctl = f.run
	t.Cleanup(func() { launchctl = prev })
}

func TestAReinstallWaitsForTheOldHelperToGo(t *testing.T) {
	f := &fakeLaunchd{lingering: 3}
	withLaunchd(t, f)
	if err := reload("p.plist", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if !f.loaded || f.calls[len(f.calls)-1] != "bootstrap" || strings.Count(strings.Join(f.calls, " "), "bootstrap") != 1 {
		t.Fatalf("%v", f.calls)
	}
}

func TestABootstrapThatKeepsFailingSaysWhy(t *testing.T) {
	f := &fakeLaunchd{lingering: 1 << 30}
	withLaunchd(t, f)
	err := reload("p.plist", 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "Input/output error") || f.loaded {
		t.Fatalf("%v %v", err, f.calls)
	}
}
