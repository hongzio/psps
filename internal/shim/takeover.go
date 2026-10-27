package shim

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// TakeOver makes this process the isolation's one tunnel. A previous tunnel
// can outlive its daemon — an exec client killed on the host may leave its
// process running here with stdin never closing — and it would hold the
// proxy ports the new one needs. So the old one is asked to go, then made
// to, before this one writes its own pid.
//
// The pid file can name a reused pid after a restart, so the process is
// only signalled if its command line says it is a tunnel.
func TakeOver(pidfile, marker string) error {
	if b, err := os.ReadFile(pidfile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid != os.Getpid() && isTunnel(pid, marker) {
			syscall.Kill(pid, syscall.SIGTERM)
			if !waitGone(pid, 3*time.Second) {
				syscall.Kill(pid, syscall.SIGKILL)
				waitGone(pid, 2*time.Second)
			}
		}
	}
	tmp := pidfile + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, pidfile)
}

func isTunnel(pid int, marker string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}
	for _, arg := range bytes.Split(b, []byte{0}) {
		if string(arg) == marker {
			return true
		}
	}
	return false
}

func waitGone(pid int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		// A zombie still answers signal 0, so its state is read too.
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) || !exists(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func exists(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// state is the field after the parenthesised command name; Z is a zombie.
	if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) {
		return b[i+2] != 'Z'
	}
	return true
}
