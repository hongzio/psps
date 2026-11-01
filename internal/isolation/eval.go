package isolation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
)

// An eval isolation scores a team's work away from the agents who made it:
// for each eval gero copies the work in and runs the eval there as
// EvalUser, who is not the agent user, so neither the work nor anything it
// runs can change what the operator put in the isolation (the agent user's
// files, the image's). Each eval starts with EvalUser's processes ended and
// its files gone.

const (
	evalUID = "1001"
	// EvalUser is the uid:gid the work runs as in an eval isolation. No
	// account has it; its home is EvalHome.
	EvalUser = evalUID + ":" + evalUID
	// EvalRoot holds the eval's copy of the work, EvalWork, where the eval
	// runs, and EvalHome; both are made anew for each eval.
	EvalRoot = "/eval"
	EvalWork = EvalRoot + "/work"
	EvalHome = EvalRoot + "/home"
)

// evalReset leaves nothing of EvalUser's that an eval could pass to the
// next: it ends its processes, again while some are alive (one may fork as
// another ends; the dead wait for init), removes its files wherever it can
// write (the image's world-writable directories, /dev/shm, /dev/mqueue, and
// any the operator made in the agent's home) but in EvalRoot, and its
// System V IPC objects, as itself: root here may not remove another's.
// Given "wipe", it makes EvalRoot anew, empty.
const evalReset = `i=0
while [ $i -lt 50 ] && ps -u ` + evalUID + ` -o stat= | grep -qv '^Z'; do pkill -KILL -u ` + evalUID + `; i=$((i+1)); sleep 0.1; done
find / ` + AgentHome + ` /dev/shm /dev/mqueue -xdev ! -path '` + EvalRoot + `' ! -path '` + EvalRoot + `/*' -user ` + evalUID + ` -delete 2>/dev/null
setpriv --reuid=` + evalUID + ` --regid=` + evalUID + ` --clear-groups ipcrm -a 2>/dev/null
[ "$1" = wipe ] || exit 0
rm -rf ` + EvalRoot + ` && mkdir -p ` + EvalWork + ` ` + EvalHome + ` && chown -R ` + EvalUser + ` ` + EvalRoot

// EvalReset readies an eval isolation for an eval; with wipe false, after
// one, it keeps EvalRoot for the operator to look into.
func (c CLI) EvalReset(ctx context.Context, name string, wipe bool) error {
	arg := "keep"
	if wipe {
		arg = "wipe"
	}
	out, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, "sh", "-c", evalReset, "gero-eval-reset", arg).CombinedOutput()
	if err != nil {
		return fmt.Errorf("readying the eval isolation: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CopyToEval copies dir in the container from, as the agent user reads it,
// into EvalWork in the eval isolation to, as EvalUser: a tar stream that
// passes through this process and nowhere else on the Mac.
func (c CLI) CopyToEval(ctx context.Context, from, dir, to string) error {
	src := c.Exec(ctx, from, ExecOpts{User: AgentUser, Env: []string{"HOME=" + AgentHome}}, "tar", "-C", dir, "-cf", "-", ".")
	dst := c.Exec(ctx, to, ExecOpts{Interactive: true, User: EvalUser, Env: []string{"HOME=" + EvalHome}}, "tar", "-C", EvalWork, "-xf", "-")
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	var srcErr, dstErr bytes.Buffer
	src.Stdout, src.Stderr = w, &srcErr
	dst.Stdin, dst.Stdout, dst.Stderr = r, &dstErr, &dstErr
	if err := dst.Start(); err != nil {
		r.Close()
		w.Close()
		return err
	}
	r.Close()
	err = src.Start()
	w.Close()
	if err != nil {
		dst.Wait()
		return err
	}
	serr, derr := src.Wait(), dst.Wait()
	switch {
	case serr != nil:
		return fmt.Errorf("reading %s: %w: %s", dir, serr, strings.TrimSpace(srcErr.String()))
	case derr != nil:
		return fmt.Errorf("writing the copy: %w: %s", derr, strings.TrimSpace(dstErr.String()))
	}
	return nil
}

// RunEval runs argv as EvalUser in EvalWork, EvalHome its home, and returns
// its output, whatever it exited with. The error says how it exited, and
// nothing of what it printed: that is what the work could write.
func (c CLI) RunEval(ctx context.Context, name string, argv ...string) (string, error) {
	cmd := c.Exec(ctx, name, ExecOpts{User: EvalUser, Workdir: EvalWork, Env: []string{"HOME=" + EvalHome}}, argv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("the eval: %w", err)
	}
	return strings.TrimSpace(out.String()), err
}
