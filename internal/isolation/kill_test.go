package isolation

import (
	"slices"
	"testing"
)

const psOut = `  1 /sbin/launchd
20152 /opt/homebrew/Cellar/container/1.6.1/libexec/container-plugins/container-runtime-linux/bin/container-runtime-linux start --root /Users/me/Library/Application Support/com.apple.container/containers/psps-9e2bc50f --uuid psps-9e2bc50f
20153 /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
30900 /opt/homebrew/Cellar/container/1.6.1/libexec/container-plugins/container-runtime-linux/bin/container-runtime-linux start --root /Users/me/Library/Application Support/com.apple.container/containers/psps-9e2bc50f0 --uuid psps-9e2bc50f0
30910 /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine
40000 /usr/bin/mdworker
`

func TestKillFindsTheContainersRuntimeAndItsVMAlone(t *testing.T) {
	pids, root := runtimeOf(psOut, "psps-9e2bc50f")
	if !slices.Equal(pids, []int{20152}) || root != "/Users/me/Library/Application Support/com.apple.container/containers/psps-9e2bc50f" {
		t.Fatalf("runtime %v at %q", pids, root)
	}
	// lsof has the disk open in the VM, and in a process that is none.
	if vms := vmsOf(psOut, "20153\n40000\n"); !slices.Equal(vms, []int{20153}) {
		t.Fatalf("vms %v", vms)
	}
	if pids, _ := runtimeOf(psOut, "psps-missing"); pids != nil {
		t.Fatalf("a container with no runtime: %v", pids)
	}
}
