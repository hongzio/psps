package isolation

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// A job runs an operator's command (a runner's) in a runner isolation, on
// a copy of a directory from another, for a caller psps names: an agent,
// psps itself (a team's eval) or the operator. Each job has a directory
// of its own, JobRoot/ID, root's, which the agent user may pass through
// but not list or change:
//
//	JobRoot/ID/meta.json     who asked, as psps knows it (root's, 0444), written as it starts
//	JobRoot/ID/options.json  what the caller asked for, unread by psps (root's, 0444)
//	JobRoot/ID/work/         the copy (the agent user's)
//	JobRoot/ID/out/          where the command leaves result.json (the agent user's)
//	JobRoot/ID/run/          its process, exit status and output (root's alone)
//
// The command runs as the agent user in a cgroup of the job's own,
// JobCgroup(ID), with its memory and process limits; whatever it leaves
// running ends with it.
const JobRoot = "/var/lib/psps-job"

// JobDir is job id's directory in a runner isolation.
func JobDir(id string) string { return JobRoot + "/" + id }

// JobCgroup is job id's cgroup.
func JobCgroup(id string) string { return "/sys/fs/cgroup/psps-job-" + id }

// JobPrep is what a job's directory and cgroup start with.
type JobPrep struct {
	Options []byte
	Memory  int64 // bytes
	Pids    int
}

// jobCgKill ends everything in cgroup $cg and removes it, waiting a little
// for the dead to go: a cgroup with a process left will not be removed.
const jobCgKill = `if [ -d "$cg" ]; then
echo 1 > "$cg/cgroup.kill" 2>/dev/null || true
i=0; while [ $i -lt 50 ] && ! rmdir "$cg" 2>/dev/null; do i=$((i+1)); sleep 0.1; done
fi`

// jobPrepare makes a job's directory anew and its cgroup, and gives it
// the memory and pids limits where the isolation's cgroup can have the
// controllers turned on for its children. One whose cgroup has processes
// of its own, a container's under a cgroup namespace, cannot: the cgroup
// still holds the job's processes together and ends them, without limits.
// One that cannot even take a process (Apple's container) has none: the
// job's session is all that holds it together then. It says "limits", or
// "nolimits" for either of those.
const jobPrepare = `set -e
d=$1 cg=$2 mem=$3 pids=$4 opts=$5
umask 022
` + jobCgKill + `
rm -rf "$d"
mkdir -p -m 711 "${d%/*}"
chmod 711 "${d%/*}"
mkdir -m 711 "$d"
mkdir -m 700 "$d/run"
install -d -o ` + AgentUID + ` -g ` + AgentUID + ` -m 700 "$d/work" "$d/out"
printf '%s' "$opts" > "$d/options.json"
chmod 444 "$d/options.json"
ctl=/sys/fs/cgroup/cgroup.subtree_control
for c in memory pids; do
  grep -qw "$c" "$ctl" || echo "+$c" > "$ctl" 2>/dev/null || true
done
mkdir "$cg"
if ! sh -c 'echo $$ > "$1/cgroup.procs"' psps-job-probe "$cg" 2>/dev/null; then
  rmdir "$cg"
  echo nolimits
elif [ -f "$cg/memory.max" ] && [ -f "$cg/pids.max" ]; then
  echo "$mem" > "$cg/memory.max"
  [ ! -f "$cg/memory.swap.max" ] || echo 0 > "$cg/memory.swap.max"
  echo "$pids" > "$cg/pids.max"
  echo limits
else
  echo nolimits
fi`

// JobPrepare readies job id's directory and cgroup in container, and
// says whether its memory and pids limits hold (jobPrepare).
func (c CLI) JobPrepare(ctx context.Context, container, id string, p JobPrep) (bool, error) {
	var stderr bytes.Buffer
	cmd := c.Exec(ctx, container, ExecOpts{User: "0:0"}, "sh", "-c", jobPrepare, "psps-job-prepare",
		JobDir(id), JobCgroup(id), strconv.FormatInt(p.Memory, 10), strconv.Itoa(p.Pids), string(p.Options))
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("readying the job: %w: %s", err, strings.TrimSpace(stderr.String()+string(out)))
	}
	return strings.TrimSpace(string(out)) == "limits", nil
}

// jobStart starts the job's argv in its work directory, apart from the
// daemon, so it runs on when that stops: a root shell in a session of its
// own waits for it, and the job runs in another session, its ID in
// run/sid, and in the job's cgroup if it has one. The waiting shell stays
// out of both, so what the job does to them (running out of memory or
// processes, being killed) cannot keep its exit status from being
// written. What the job leaves running in them ends as it does.
const jobStart = `d=$1 cg=$2 meta=$3
shift 3
umask 022
printf '%s' "$meta" > "$d/meta.json" && chmod 444 "$d/meta.json" || exit 1
cat /proc/sys/kernel/random/boot_id > "$d/run/boot" || exit 1
setsid sh -c '
d=$1 cg=$2
shift 2
echo $$ > "$d/run/pid"
(cd -- "$d/work" && exec setsid sh -c '"'"'echo $$ > "$0/run/sid" && { [ ! -d "$1" ] || echo $$ > "$1/cgroup.procs"; } && shift && exec "$@"'"'"' "$d" "$cg" "$@") > "$d/run/log" 2>&1 < /dev/null
code=$?
echo 1 > "$cg/cgroup.kill" 2>/dev/null
s=$(cat "$d/run/sid" 2>/dev/null) && [ -n "$s" ] && pkill -KILL -s "$s"
echo $code > "$d/run/exit.new" && mv "$d/run/exit.new" "$d/run/exit"' psps-job "$d" "$cg" "$@" < /dev/null > /dev/null 2>&1 &`

// JobStart writes job id's meta.json and starts argv as the job
// (jobStart). argv is whole: it says the user it runs as and its
// environment.
func (c CLI) JobStart(ctx context.Context, container, id string, meta []byte, argv []string) error {
	args := append([]string{"sh", "-c", jobStart, "psps-job-start", JobDir(id), JobCgroup(id), string(meta)}, argv...)
	out, err := c.Exec(ctx, container, ExecOpts{User: "0:0"}, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("starting the job: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// JobState is what JobPoll found of a job.
type JobState struct {
	Running, Lost bool
	Exit          int
}

// jobPoll says "done CODE", "running", or "lost": its directory gone with
// the container, or its process gone (with the VM, or killed) without an
// exit status.
const jobPoll = `d=$1
[ -d "$d/run" ] || { echo lost; exit 0; }
if [ -f "$d/run/exit" ]; then echo "done $(cat "$d/run/exit")"; exit 0; fi
p=$(cat "$d/run/pid" 2>/dev/null)
if [ "$(cat "$d/run/boot" 2>/dev/null)" = "$(cat /proc/sys/kernel/random/boot_id)" ] && [ -n "$p" ] && ps -o stat= -p "$p" | grep -qv Z; then echo running; else echo lost; fi`

// JobPoll says how job id is.
func (c CLI) JobPoll(ctx context.Context, container, id string) (JobState, error) {
	out, err := c.Exec(ctx, container, ExecOpts{User: "0:0"}, "sh", "-c", jobPoll, "psps-job-poll", JobDir(id)).Output()
	if err != nil {
		return JobState{}, fmt.Errorf("looking at the job: %w", err)
	}
	return parseJobState(strings.TrimSpace(string(out)))
}

func parseJobState(s string) (JobState, error) {
	switch {
	case s == "running":
		return JobState{Running: true}, nil
	case s == "lost":
		return JobState{Lost: true}, nil
	case strings.HasPrefix(s, "done "):
		code, err := strconv.Atoi(strings.TrimPrefix(s, "done "))
		if err != nil {
			return JobState{}, fmt.Errorf("looking at the job: exit status %q", s)
		}
		return JobState{Exit: code}, nil
	}
	return JobState{}, fmt.Errorf("looking at the job: unexpected %q", s)
}

// JobCollect reads what job id left: out/result.json, read as the agent
// user so a link there reaches nothing that user could not, up to
// maxResult bytes (more is an error), and the last maxLog bytes of its
// output. A job that wrote no result.json has nil.
func (c CLI) JobCollect(ctx context.Context, container, id string, maxResult, maxLog int) ([]byte, string, error) {
	res, err := c.Exec(ctx, container, ExecOpts{User: AgentUser, Env: []string{"HOME=" + AgentHome}}, "sh", "-c",
		`f=$1/out/result.json; [ -e "$f" ] || exit 3; head -c $2 "$f"`, "psps-job-result", JobDir(id), strconv.Itoa(maxResult+1)).Output()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee) && ee.ExitCode() == 3:
		res, err = nil, nil
	case err != nil:
		return nil, "", fmt.Errorf("reading the job's result.json: %w", err)
	case len(res) > maxResult:
		return nil, "", fmt.Errorf("the job's result.json is over %d bytes", maxResult)
	}
	log, lerr := c.Exec(ctx, container, ExecOpts{User: "0:0"}, "tail", "-c", strconv.Itoa(maxLog), JobDir(id)+"/run/log").Output()
	if lerr != nil {
		log = nil
	}
	return res, string(log), err
}

// JobSignal sends SIGTERM to everything in job id's cgroup, or with kill
// ends it all at once (cgroup.kill).
func (c CLI) JobSignal(ctx context.Context, container, id string, kill bool) error {
	script := `cg=$1 d=$2; s=$(cat "$d/run/sid" 2>/dev/null) && [ -n "$s" ] && pkill -TERM -s "$s"
[ ! -d "$cg" ] || for p in $(cat "$cg/cgroup.procs"); do kill -TERM "$p" 2>/dev/null; done; true`
	if kill {
		script = `cg=$1 d=$2; s=$(cat "$d/run/sid" 2>/dev/null) && [ -n "$s" ] && pkill -KILL -s "$s"
[ ! -d "$cg" ] || echo 1 > "$cg/cgroup.kill" 2>/dev/null; true`
	}
	out, err := c.Exec(ctx, container, ExecOpts{User: "0:0"}, "sh", "-c", script, "psps-job-signal", JobCgroup(id), JobDir(id)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("signalling the job: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// JobForget ends what is left of job id and removes its cgroup and
// directory.
func (c CLI) JobForget(ctx context.Context, container, id string) error {
	out, err := c.Exec(ctx, container, ExecOpts{User: "0:0"}, "sh", "-c", `cg=$1
s=$(cat "$2/run/sid" 2>/dev/null) && [ -n "$s" ] && pkill -KILL -s "$s"
`+jobCgKill+`
rm -rf -- "$2"`, "psps-job-forget", JobCgroup(id), JobDir(id)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("removing the job: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- the copy -----------------------------------------------------------------

// CopyLimits bound a job's copy: the bytes of its files, its entries, and
// the names (any part of a path) left out.
type CopyLimits struct {
	MaxBytes int64
	MaxFiles int
	Exclude  []string
}

// CopyStats is what a copy carried, and what it left out.
type CopyStats struct {
	Files   int
	Bytes   int64
	Skipped []string // entries that are not files, directories or links, by path
}

// ErrCopyLimit is a copy over its CopyLimits.
var ErrCopyLimit = errors.New("over the runner's limit")

// FilterTar copies the tar stream r to w as a job's copy may have it:
// regular files, directories and symbolic links, as links, never
// followed; a hard link only to a file before it. Devices, FIFOs and
// sockets are left out, as is anything under an excluded name or reached
// through a link in the copy. A name that leaves the tree is an error,
// and so is a copy over lim.
func FilterTar(r io.Reader, w io.Writer, lim CopyLimits) (CopyStats, error) {
	var st CopyStats
	tr, tw := tar.NewReader(r), tar.NewWriter(w)
	excluded := map[string]bool{}
	for _, e := range lim.Exclude {
		excluded[e] = true
	}
	links := map[string]bool{} // symbolic links in the copy
	files := map[string]bool{} // regular files, for hard links
	entries := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("reading the directory: %w", err)
		}
		name, ok := cleanName(h.Name)
		if !ok {
			return st, fmt.Errorf("%q leaves the directory", h.Name)
		}
		if name == "." {
			continue
		}
		parts := strings.Split(name, "/")
		skip := false
		for i, p := range parts {
			if excluded[p] || (i < len(parts)-1 && links[strings.Join(parts[:i+1], "/")]) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		case tar.TypeLink:
			target, ok := cleanName(h.Linkname)
			if !ok || !files[target] {
				st.Skipped = append(st.Skipped, name)
				continue
			}
			h.Linkname = target
		case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		default:
			st.Skipped = append(st.Skipped, name)
			continue
		}
		entries++
		if lim.MaxFiles > 0 && entries > lim.MaxFiles {
			return st, fmt.Errorf("the directory has over %d entries, %w", lim.MaxFiles, ErrCopyLimit)
		}
		if h.Typeflag == tar.TypeReg {
			st.Bytes += h.Size
			if lim.MaxBytes > 0 && st.Bytes > lim.MaxBytes {
				return st, fmt.Errorf("the directory's files are over %d bytes, %w", lim.MaxBytes, ErrCopyLimit)
			}
			st.Files++
			files[name] = true
		}
		if h.Typeflag == tar.TypeSymlink {
			links[name] = true
		}
		out := &tar.Header{Typeflag: h.Typeflag, Name: name, Linkname: h.Linkname, Size: h.Size, Mode: h.Mode & 0o777, ModTime: h.ModTime, Format: tar.FormatPAX}
		if h.Typeflag == tar.TypeDir {
			out.Name += "/"
		}
		if h.Typeflag != tar.TypeReg {
			out.Size = 0
		}
		if err := tw.WriteHeader(out); err != nil {
			return st, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return st, fmt.Errorf("reading %s: %w", name, err)
			}
		}
	}
	return st, tw.Close()
}

// cleanName is a tar member's name relative to the tree's top, or false
// if it leaves it.
func cleanName(n string) (string, bool) {
	if strings.HasPrefix(n, "/") {
		return "", false
	}
	c := path.Clean(n)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// JobCopy copies dir in container from, as the agent user reads it, into
// job id's work directory in container to, as the agent user there,
// through FilterTar in this process and nowhere else on the Mac.
func (c CLI) JobCopy(ctx context.Context, from, dir, to, id string, lim CopyLimits) (CopyStats, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	src := c.Exec(ctx, from, ExecOpts{User: AgentUser, Env: []string{"HOME=" + AgentHome}}, "tar", "-C", dir, "-cf", "-", ".")
	dst := c.Exec(ctx, to, ExecOpts{Interactive: true, User: AgentUser, Env: []string{"HOME=" + AgentHome}},
		"tar", "-C", JobDir(id)+"/work", "-xf", "-", "--no-same-owner")
	var srcErr, dstErr bytes.Buffer
	srcOut, err := src.StdoutPipe()
	if err != nil {
		return CopyStats{}, err
	}
	src.Stderr = &srcErr
	r, w, err := os.Pipe()
	if err != nil {
		return CopyStats{}, err
	}
	dst.Stdin, dst.Stdout, dst.Stderr = r, &dstErr, &dstErr
	if err := dst.Start(); err != nil {
		r.Close()
		w.Close()
		return CopyStats{}, err
	}
	r.Close()
	if err := src.Start(); err != nil {
		w.Close()
		dst.Wait()
		return CopyStats{}, err
	}
	st, ferr := FilterTar(srcOut, w, lim)
	w.Close()
	if ferr != nil {
		cancel()
	}
	io.Copy(io.Discard, srcOut)
	serr, derr := src.Wait(), dst.Wait()
	var ee *exec.ExitError
	switch {
	case ferr != nil:
		return st, ferr
	// tar's 1 is a file that changed as it was read: a live directory's.
	case serr != nil && !(errors.As(serr, &ee) && ee.ExitCode() == 1):
		return st, fmt.Errorf("reading %s: %w: %s", dir, serr, strings.TrimSpace(srcErr.String()))
	case derr != nil:
		return st, fmt.Errorf("writing the copy: %w: %s", derr, strings.TrimSpace(dstErr.String()))
	}
	return st, nil
}
