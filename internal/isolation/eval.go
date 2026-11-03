package isolation

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// An eval isolation scores a team's work away from the agents who made it:
// for each eval psps copies the work in and runs the eval there as
// EvalUser, who is not the agent user, so neither the work nor anything it
// runs can change what the operator put in the isolation (the agent user's
// files, the image's). Each eval starts with EvalUser's processes ended and
// its files gone.

const (
	EvalUID = "1001"
	// EvalUser is the uid:gid the work runs as in an eval isolation. No
	// account has it; its home is EvalHome.
	EvalUser = EvalUID + ":" + EvalUID
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
while [ $i -lt 50 ] && ps -u ` + EvalUID + ` -o stat= | grep -qv '^Z'; do pkill -KILL -u ` + EvalUID + `; i=$((i+1)); sleep 0.1; done
find / ` + AgentHome + ` /dev/shm /dev/mqueue -xdev ! -path '` + EvalRoot + `' ! -path '` + EvalRoot + `/*' -user ` + EvalUID + ` -delete 2>/dev/null
setpriv --reuid=` + EvalUID + ` --regid=` + EvalUID + ` --clear-groups ipcrm -a 2>/dev/null
[ "$1" = wipe ] || exit 0
rm -rf ` + EvalRoot + ` && mkdir -p ` + EvalWork + ` ` + EvalHome + ` && chown -R ` + EvalUser + ` ` + EvalRoot

// EvalReset readies an eval isolation for an eval; with wipe false, after
// one, it keeps EvalRoot for the operator to look into.
func (c CLI) EvalReset(ctx context.Context, name string, wipe bool) error {
	arg := "keep"
	if wipe {
		arg = "wipe"
	}
	out, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, "sh", "-c", evalReset, "psps-eval-reset", arg).CombinedOutput()
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

// An EvalRun is how an eval is started in an isolation: argv as the user
// with that UID (and GID), in Dir with Home its home.
type EvalRun struct {
	UID  string
	Dir  string
	Home string
	Argv []string
}

// evalRun is where root keeps run's output, exit status, process and the
// boot it ran in, out of the eval user's reach.
func evalRun(container, run string) string {
	if Gero(container) {
		return "/var/lib/gero-eval/" + run
	}
	return "/var/lib/psps-eval/" + run
}

// evalStart starts the eval apart from `container exec`, in a session of
// its own: it runs on when the daemon that started it stops, and its
// output and exit status wait in its run's directory for one to read them.
const evalStart = `d=$1 uid=$2 dir=$3 home=$4
shift 4
mkdir -p -m 700 "${d%/*}" && rm -rf "$d" && mkdir -m 700 "$d" || exit 1
cat /proc/sys/kernel/random/boot_id > "$d/boot"
setsid sh -c '
d=$1 uid=$2 dir=$3 home=$4
shift 4
echo $$ > "$d/pid"
(cd -- "$dir" && exec env HOME="$home" setpriv --reuid="$uid" --regid="$uid" --clear-groups "$@") > "$d/out" 2>&1 < /dev/null
echo $? > "$d/exit.new" && mv "$d/exit.new" "$d/exit"' psps-eval "$d" "$uid" "$dir" "$home" "$@" < /dev/null > /dev/null 2>&1 &`

// evalPoll says how a run is: "done CODE" and then the end of its output,
// "running", or "lost": its directory gone with the container, or its
// process gone (with the VM it ran in, or killed) without an exit status.
const evalPoll = `d=$1
[ -d "$d" ] || { echo lost; exit 0; }
if [ -f "$d/exit" ]; then echo "done $(cat "$d/exit")"; tail -c $2 "$d/out"; exit 0; fi
p=$(cat "$d/pid" 2>/dev/null)
if [ "$(cat "$d/boot")" = "$(cat /proc/sys/kernel/random/boot_id)" ] && [ -n "$p" ] && ps -o stat= -p "$p" | grep -qv Z; then echo running; else echo lost; fi`

// EvalState is what EvalPoll found of a run.
type EvalState struct {
	Running, Lost bool
	Exit          int
	Output        string // the end of it
}

// EvalStart starts r as run, apart from the daemon (evalStart).
func (c CLI) EvalStart(ctx context.Context, name, run string, r EvalRun) error {
	argv := append([]string{"sh", "-c", evalStart, "psps-eval-start", evalRun(name, run), r.UID, r.Dir, r.Home}, r.Argv...)
	out, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, argv...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting the eval: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// EvalPoll says how run is, with the last max bytes of its output once it
// is done.
func (c CLI) EvalPoll(ctx context.Context, name, run string, max int) (EvalState, error) {
	out, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, "sh", "-c", evalPoll, "psps-eval-poll", evalRun(name, run), strconv.Itoa(max)).Output()
	if err != nil {
		return EvalState{}, fmt.Errorf("looking at the eval: %w", err)
	}
	head, rest, _ := strings.Cut(string(out), "\n")
	switch {
	case head == "running":
		return EvalState{Running: true}, nil
	case head == "lost":
		return EvalState{Lost: true}, nil
	case strings.HasPrefix(head, "done "):
		code, err := strconv.Atoi(strings.TrimPrefix(head, "done "))
		if err != nil {
			return EvalState{}, fmt.Errorf("looking at the eval: exit status %q", head)
		}
		return EvalState{Exit: code, Output: strings.TrimSpace(rest)}, nil
	}
	return EvalState{}, fmt.Errorf("looking at the eval: unexpected %q", head)
}

// EvalStop ends run's processes, all of its session, as its time is up
// and its timeout(1) has not ended it.
func (c CLI) EvalStop(ctx context.Context, name, run string) error {
	_, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, "sh", "-c", `p=$(cat "$1/pid" 2>/dev/null) && pkill -KILL -s "$p"; true`, "psps-eval-stop", evalRun(name, run)).Output()
	return err
}

// EvalForget removes what run left (evalRun).
func (c CLI) EvalForget(ctx context.Context, name, run string) error {
	_, err := c.Exec(ctx, name, ExecOpts{User: "0:0"}, "rm", "-rf", "--", evalRun(name, run)).Output()
	return err
}
