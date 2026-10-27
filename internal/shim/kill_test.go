package shim

import "syscall"

func syscallKill(pid int) {
	if pid > 0 {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}
