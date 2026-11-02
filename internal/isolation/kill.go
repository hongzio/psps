package isolation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// vmProcess is the Virtualization framework's process a container's VM runs
// in; container starts it as an XPC service, so it is no child of the
// container's runtime helper.
const vmProcess = "com.apple.Virtualization.VirtualMachine"

// Kill ends a container the hard way: SIGKILL to its runtime helper and to
// the process its VM runs in, as pulling its plug would. It is for a
// container that container itself no longer answers for: a VM so starved
// that nothing starts in it holds up every exec, and stop and kill too. The
// container is stopped after it; what its disks had not written is lost.
// It returns the processes it killed, none when the container was not
// running.
func (c CLI) Kill(ctx context.Context, name string) ([]int, error) {
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	runtime, root := runtimeOf(string(out), name)
	if root == "" {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "Library", "Application Support", "com.apple.container", "containers", name)
	}
	// The VM is the process that has the container's root disk open.
	var vms []int
	if lsof, err := exec.CommandContext(ctx, "lsof", "-t", filepath.Join(root, "rootfs.ext4")).Output(); err == nil {
		vms = vmsOf(string(out), string(lsof))
	}
	var killed []int
	for _, pid := range append(runtime, vms...) {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return killed, fmt.Errorf("killing %d: %w", pid, err)
		}
		killed = append(killed, pid)
	}
	return killed, nil
}

// runtimeOf finds, in ps's "pid command" lines, the runtime helper of the
// container name and the container's directory it was started with.
func runtimeOf(ps, name string) (pids []int, root string) {
	for line := range strings.SplitSeq(ps, "\n") {
		pid, cmd, ok := psLine(line)
		if !ok || !strings.Contains(cmd, "container-runtime-linux") {
			continue
		}
		f := strings.Fields(cmd)
		if !slicesHasPair(f, "--uuid", name) {
			continue
		}
		pids = append(pids, pid)
		// --root has spaces in it (Application Support): it runs to the
		// next flag.
		if _, r, ok := strings.Cut(cmd, " --root "); ok {
			if i := strings.Index(r, " --"); i >= 0 {
				r = r[:i]
			}
			root = r
		}
	}
	return pids, root
}

// vmsOf keeps, of lsof -t's pids, those that are a VM's process.
func vmsOf(ps, lsof string) []int {
	vm := map[int]bool{}
	for line := range strings.SplitSeq(ps, "\n") {
		if pid, cmd, ok := psLine(line); ok && strings.Contains(cmd, vmProcess) {
			vm[pid] = true
		}
	}
	var out []int
	for f := range strings.FieldsSeq(lsof) {
		if pid, err := strconv.Atoi(f); err == nil && vm[pid] {
			out = append(out, pid)
		}
	}
	return out
}

func psLine(line string) (int, string, bool) {
	p, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
	if !ok {
		return 0, "", false
	}
	pid, err := strconv.Atoi(p)
	return pid, strings.TrimSpace(cmd), err == nil
}

func slicesHasPair(f []string, k, v string) bool {
	for i := 0; i+1 < len(f); i++ {
		if f[i] == k && f[i+1] == v {
			return true
		}
	}
	return false
}
