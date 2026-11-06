package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/driver"
	"github.com/hongzio/psps/internal/event"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/controlproxy"
	"github.com/hongzio/psps/internal/proxy/jobproxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/proxy/teamproxy"
	"github.com/hongzio/psps/internal/store"
)

// --- the fake's jobs: run on this Mac, under the container's directory ---------

func (f *fakeContainers) jobDir(name, id string) string {
	return filepath.Join(f.base, name, "jobs", id)
}

// JobPrepare: with noLimits the container's cgroup cannot give the
// limits.
func (f *fakeContainers) JobPrepare(_ context.Context, name, id string, p isolation.JobPrep) (bool, error) {
	d := f.jobDir(name, id)
	os.RemoveAll(d)
	for _, s := range []string{"work", "out", "run"} {
		if err := os.MkdirAll(filepath.Join(d, s), 0o755); err != nil {
			return false, err
		}
	}
	f.mu.Lock()
	if f.jobPreps == nil {
		f.jobPreps = map[string]isolation.JobPrep{}
	}
	f.jobPreps[id] = p
	limits := !f.noLimits
	f.mu.Unlock()
	return limits, os.WriteFile(filepath.Join(d, "options.json"), p.Options, 0o444)
}

// JobCopy copies dir on this Mac through isolation.FilterTar, as the real
// one does between two containers.
func (f *fakeContainers) JobCopy(ctx context.Context, from, dir, to, id string, lim isolation.CopyLimits) (isolation.CopyStats, error) {
	var tarball, out bytes.Buffer
	c := exec.CommandContext(ctx, "tar", "-C", dir, "-cf", "-", ".")
	// No AppleDouble files: macOS's tar, not the isolation's.
	c.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	c.Stdout = &tarball
	if err := c.Run(); err != nil {
		return isolation.CopyStats{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	st, err := isolation.FilterTar(&tarball, &out, lim)
	if err != nil {
		return st, err
	}
	x := exec.CommandContext(ctx, "tar", "-C", filepath.Join(f.jobDir(to, id), "work"), "-xf", "-")
	x.Stdin = &out
	if b, err := x.CombinedOutput(); err != nil {
		return st, fmt.Errorf("writing the copy: %w: %s", err, b)
	}
	return st, nil
}

// JobStart runs the job's command on this Mac with the job's environment,
// its paths moved under the container's directory: what is between env -i
// and setpriv, and the sh -c at the end.
func (f *fakeContainers) JobStart(_ context.Context, name, id string, meta []byte, argv []string) error {
	d := f.jobDir(name, id)
	if err := os.WriteFile(filepath.Join(d, "meta.json"), meta, 0o444); err != nil {
		return err
	}
	f.mu.Lock()
	if f.jobArgvs == nil {
		f.jobArgvs = map[string][]string{}
	}
	f.jobArgvs[id] = argv
	f.mu.Unlock()
	if argv[0] != "env" || argv[1] != "-i" || argv[len(argv)-3] != "sh" || argv[len(argv)-2] != "-c" {
		return fmt.Errorf("unexpected argv %q", argv)
	}
	env := []string{"PATH=" + os.Getenv("PATH")}
	for _, kv := range argv[2:] {
		if kv == "setpriv" {
			break
		}
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, strings.ReplaceAll(kv, isolation.JobDir(id), d))
		}
	}
	cmd := exec.Command("sh", "-c", argv[len(argv)-1])
	cmd.Dir, cmd.Env = filepath.Join(d, "work"), env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	fr := &fakeRun{cmd: cmd, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = &fr.out, &fr.out
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		cmd.Wait()
		fr.exit = cmd.ProcessState.ExitCode()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			fr.exit = 128 + int(ws.Signal())
		}
		close(fr.done)
	}()
	f.mu.Lock()
	if f.runs == nil {
		f.runs = map[string]*fakeRun{}
	}
	f.runs[name+"/"+id] = fr
	f.mu.Unlock()
	return nil
}

func (f *fakeContainers) JobPoll(_ context.Context, name, id string) (isolation.JobState, error) {
	fr := f.evalRun(name, id)
	if fr == nil || fr.lost {
		return isolation.JobState{Lost: true}, nil
	}
	select {
	case <-fr.done:
		return isolation.JobState{Exit: fr.exit}, nil
	default:
		return isolation.JobState{Running: true}, nil
	}
}

func (f *fakeContainers) JobCollect(_ context.Context, name, id string, maxResult, maxLog int) ([]byte, string, error) {
	out := ""
	if fr := f.evalRun(name, id); fr != nil {
		out = fr.out.String()
		if len(out) > maxLog {
			out = out[len(out)-maxLog:]
		}
	}
	b, err := os.ReadFile(filepath.Join(f.jobDir(name, id), "out", "result.json"))
	switch {
	case os.IsNotExist(err):
		return nil, out, nil
	case err != nil:
		return nil, out, err
	case len(b) > maxResult:
		return nil, out, fmt.Errorf("the job's result.json is over %d bytes", maxResult)
	}
	return b, out, nil
}

func (f *fakeContainers) JobSignal(_ context.Context, name, id string, kill bool) error {
	if fr := f.evalRun(name, id); fr != nil {
		sig := syscall.SIGTERM
		if kill {
			sig = syscall.SIGKILL
		}
		syscall.Kill(-fr.cmd.Process.Pid, sig)
	}
	return nil
}

func (f *fakeContainers) JobForget(_ context.Context, name, id string) error {
	if fr := f.evalRun(name, id); fr != nil {
		syscall.Kill(-fr.cmd.Process.Pid, syscall.SIGKILL)
	}
	f.mu.Lock()
	delete(f.runs, name+"/"+id)
	f.mu.Unlock()
	return os.RemoveAll(f.jobDir(name, id))
}

// --- the tests --------------------------------------------------------------------

// withJobs gives the manager the job service and an isolation "run" with
// runner sim, whose command is cmd, opened to dev; and an idle agent in
// dev.
func withJobs(t *testing.T, e *testEnv, m *Manager, cmd string, set map[string]any) (store.Agent, store.Isolation) {
	t.Helper()
	ctx := context.Background()
	jobService(t, m)
	iso, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "run"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"isolation": []string{"run"}, "command": cmd}
	for k, v := range set {
		cfg[k] = v
	}
	env, err := m.PutEnv(ctx, jobproxy.Name, "sim", proxy.Patch{Set: cfg}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(env.Config), `"isolation":["run"]`) {
		t.Fatalf("the runner shows its isolation by name: %s", env.Config)
	}
	if se, _ := e.st.Env(jobproxy.Name, "sim"); !strings.Contains(string(se.Config), iso.ID) {
		t.Fatalf("the runner keeps its isolation by ID: %s", se.Config)
	}
	if _, err := m.Expose(ctx, "dev", jobproxy.Name, api.Expose{Envs: []string{"sim"}}); err != nil {
		t.Fatal(err)
	}
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Handle: "asker"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("the agent", func() bool { return status(m, a.ID) == event.StatusIdle })
	return a, iso
}

// jobService gives m the job service, as every daemon has it.
func jobService(t *testing.T, m *Manager) {
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, jobproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	m.tunnelPort = func(int) int { return freePort(t) }
}

func jobAs(t *testing.T, m *Manager, from, method, path, body string) (int, string) {
	t.Helper()
	return mailAs(t, m, testIsoID, from, method, "/job"+path, body)
}

func askJob(t *testing.T, m *Manager, from string, req api.PostJob) api.CallerJob {
	t.Helper()
	b, _ := json.Marshal(req)
	code, out := jobAs(t, m, from, "POST", "/jobs", string(b))
	if code != http.StatusAccepted {
		t.Fatalf("no job: %d %s", code, out)
	}
	var j api.CallerJob
	json.Unmarshal([]byte(out), &j)
	return j
}

func jobNow(t *testing.T, m *Manager, from, id string, wait int) api.CallerJob {
	t.Helper()
	code, out := jobAs(t, m, from, "GET", fmt.Sprintf("/jobs/%s?wait=%d", id, wait), "")
	if code != http.StatusOK {
		t.Fatalf("job %s: %d %s", id, code, out)
	}
	var j api.CallerJob
	json.Unmarshal([]byte(out), &j)
	return j
}

// A job runs the runner's command on a copy of the caller's directory,
// told who asked in a file of psps's; the caller gets what the command
// left it, and the rest is the operator's.
func TestAJobRunsOnACopyAndItsCallerGetsWhatItIsLeft(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	cmd := `touch made; cp "$PSPS_JOB_META" "$PSPS_JOB_OUT/../seen-meta"; ls -a > "$PSPS_JOB_OUT/../seen-ls"
printf '{"caller":{"options":%s,"n":%s},"score":4.5,"secret":"for-the-operator"}' "$(cat "$PSPS_JOB_OPTIONS")" "$(cat n)" > "$PSPS_JOB_OUT/result.json"
echo printed-for-the-operator`
	a, iso := withJobs(t, e, m, cmd, nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "n"), []byte("3"), 0o644)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[core]"), 0o644)
	os.Symlink("n", filepath.Join(dir, "link"))
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	// What the runner keeps its meta and the copy in, the command reads
	// before it is removed: it copies them where the test can see them.
	keep := t.TempDir()
	cmdKeep := strings.ReplaceAll(cmd, `"$PSPS_JOB_OUT/../`, `"`+keep+`/`)
	if _, err := m.PutEnv(context.Background(), jobproxy.Name, "sim", proxy.Patch{Set: map[string]any{"command": cmdKeep}}, false); err != nil {
		t.Fatal(err)
	}

	j := askJob(t, m, a.ID, api.PostJob{Dir: dir, Options: json.RawMessage(`{"mode": "quick"}`)})
	if j.Runner != "sim" || store.JobEnded(j.Status) {
		t.Fatalf("%+v", j)
	}
	j = jobNow(t, m, a.ID, j.ID, 20)
	if j.Status != store.JobDone || string(j.Result) != `{"options":{"mode":"quick"},"n":3}` {
		t.Fatalf("the caller's view: %+v (%s)", j, j.Result)
	}
	code, out := jobAs(t, m, a.ID, "GET", "/jobs/"+j.ID, "")
	if code != http.StatusOK || strings.Contains(out, "for-the-operator") || strings.Contains(out, "4.5") {
		t.Fatalf("the caller sees more than its part: %s", out)
	}
	if j.Note != "copied 1 file, 1 bytes; left out 1 entry, not a file, directory or link (fifo)" {
		t.Fatalf("note %q", j.Note)
	}
	if _, err := os.Stat(filepath.Join(dir, "made")); err == nil {
		t.Fatal("the job ran on the directory itself")
	}
	if _, err := os.Stat(e.fc.jobDir(iso.Container, j.ID)); !os.IsNotExist(err) {
		t.Fatal("the job's directory outlived it:", err)
	}
	ls, _ := os.ReadFile(filepath.Join(keep, "seen-ls"))
	if strings.Contains(string(ls), ".git") || strings.Contains(string(ls), "fifo") || !strings.Contains(string(ls), "link") {
		t.Fatalf("the copy held: %s", ls)
	}
	var meta jobMeta
	b, _ := os.ReadFile(filepath.Join(keep, "seen-meta"))
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err, string(b))
	}
	if meta.Limits == nil || meta.Limits.Memory != 4<<30 || meta.Limits.Pids != jobproxy.DefaultPids || meta.Limits.Minutes != jobproxy.DefaultMinutes {
		t.Fatalf("limits %+v", meta.Limits)
	}
	if meta.V != 1 || meta.Job != j.ID || meta.Caller != store.CallerAgent || meta.Agent == nil || meta.Agent.ID != a.ID || meta.Agent.Handle != "asker" ||
		meta.Isolation != "dev" || meta.Dir != dir || meta.Runner != "sim" || meta.Deadline.Sub(meta.StartedAt) != jobproxy.DefaultMinutes*60e9 {
		t.Fatalf("meta %+v", meta)
	}

	// The operator sees all of it.
	op, err := m.Job(context.Background(), j.ID, 0)
	if err != nil || !strings.Contains(string(op.Result), "for-the-operator") || !strings.Contains(op.Output, "printed-for-the-operator") ||
		op.RunnerIsolation != "run" || op.FromIsolation != "dev" || op.AgentID != "asker" {
		t.Fatalf("the operator's view: %+v %v", op, err)
	}
	// Run as the agent user, with the runner's limits, the job's variables
	// and nothing of root's.
	e.fc.mu.Lock()
	prep, argv := e.fc.jobPreps[j.ID], strings.Join(e.fc.jobArgvs[j.ID], " ")
	e.fc.mu.Unlock()
	if prep.Memory != 4<<30 || prep.Pids != jobproxy.DefaultPids || string(prep.Options) != `{"mode":"quick"}` {
		t.Fatalf("prep %+v", prep)
	}
	for _, want := range []string{"env -i HOME=/home/agent", "PSPS_JOB_ID=" + j.ID, "PSPS_JOB_META=" + isolation.JobDir(j.ID) + "/meta.json",
		"setpriv --reuid=1000 --regid=1000 --clear-groups timeout --kill-after=120 3600s sh -c"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %q: %s", want, argv)
		}
	}
	if code, out := jobAs(t, m, a.ID, "GET", "", ""); code != http.StatusOK || !strings.Contains(out, `"name":"sim","default":true`) || !strings.Contains(out, j.ID) ||
		strings.Contains(out, `"about"`) || strings.Contains(out, "seen-meta") {
		t.Fatalf("the home: %d %s", code, out)
	}

	// The operator's word on it is in its listing; its command never is.
	if _, err := m.PutEnv(context.Background(), jobproxy.Name, "sim", proxy.Patch{Set: map[string]any{"about": "  replays a tape; options: {\"mode\"}  "}}, false); err != nil {
		t.Fatal(err)
	}
	if _, out := jobAs(t, m, a.ID, "GET", "", ""); !strings.Contains(out, `"about":"replays a tape; options: {\"mode\"}"`) || strings.Contains(out, "seen-meta") {
		t.Fatalf("the home with an about: %s", out)
	}
	if _, err := m.PutEnv(context.Background(), jobproxy.Name, "sim", proxy.Patch{Set: map[string]any{"about": ""}}, false); err != nil {
		t.Fatal(err)
	}
	if _, out := jobAs(t, m, a.ID, "GET", "", ""); strings.Contains(out, `"about"`) {
		t.Fatalf("a cleared about is still there: %s", out)
	}
}

// What a caller may not ask for is refused before anything runs.
func TestAJobIsRefusedWhatItsCallerMayNotHave(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	a, _ := withJobs(t, e, m, "true", nil)
	ctx := context.Background()
	if _, err := m.PutEnv(ctx, jobproxy.Name, "other", proxy.Patch{Set: map[string]any{"isolation": []string{"run"}, "command": "true"}}, true); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		body string
		code int
		want string
	}{
		{`{"runner": "other", "dir": "/tmp"}`, http.StatusForbidden, `other\" is not open to isolation dev (it has sim)`},
		{`{"runner": "nope", "dir": "/tmp"}`, http.StatusForbidden, `nope\" is not open`},
		{`{"dir": "/tmp", "options": [1]}`, http.StatusBadRequest, `options\" is a JSON object`},
		{`{"dir": "/tmp", "options": {"x": "` + strings.Repeat("a", jobproxy.OptionsMax) + `"}}`, http.StatusRequestEntityTooLarge, "at most"},
		{`{"options": {}}`, http.StatusBadRequest, "say which directory"},
		{`not json`, http.StatusBadRequest, "the body is JSON"},
	} {
		if code, out := jobAs(t, m, a.ID, "POST", "/jobs", c.body); code != c.code || !strings.Contains(out, c.want) {
			t.Errorf("%.60s: %d %s", c.body, code, out)
		}
	}
	if code, out := mailAt(m, testIsoID, "GET", "/a/"+proxy.WrongToken+"/job", ""); code != http.StatusForbidden || !strings.Contains(out, "not the address of an agent") {
		t.Errorf("a wrong token: %d %s", code, out)
	}
	if err := m.Unexpose(ctx, "dev", jobproxy.Name); err != nil {
		t.Fatal(err)
	}
	if code, out := jobAs(t, m, a.ID, "GET", "", ""); code != http.StatusForbidden || !strings.Contains(out, "has no runners") {
		t.Errorf("no exposure: %d %s", code, out)
	}
	// A runner has one isolation and a command.
	for set, want := range map[string]map[string]any{
		"in one isolation": {"command": "true"},
		"has a command":    {"isolation": []string{"run"}},
		"no isolation":     {"isolation": []string{"gone"}, "command": "true"},
		"is 1 to":          {"isolation": []string{"run"}, "command": "true", "max_live": 0},
		"at most 4096":     {"isolation": []string{"run"}, "command": "true", "about": strings.Repeat("x", 4097)},
	} {
		if _, err := m.PutEnv(ctx, jobproxy.Name, "bad", proxy.Patch{Set: want}, true); err == nil || !strings.Contains(err.Error(), set) {
			t.Errorf("%v: %v", want, err)
		}
	}
}

// A runner runs so many at once and queues so many more; a waiting job
// cancelled ends at once, one running is stopped, and the next starts.
func TestARunnerQueuesItsJobsAndAJobIsCancelled(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	gate, ready := filepath.Join(t.TempDir(), "go"), t.TempDir()
	cmd := fmt.Sprintf(`trap 'echo stopped > "$PSPS_JOB_OUT/result.json"; exit 7' TERM; touch %s/$PSPS_JOB_ID; while [ ! -f %s ]; do sleep 0.02; done; echo '{"caller":"ok"}' > "$PSPS_JOB_OUT/result.json"`, ready, gate)
	a, _ := withJobs(t, e, m, cmd, map[string]any{"max_live": 1, "max_queued": 1})
	dir := t.TempDir()
	first := askJob(t, m, a.ID, api.PostJob{Dir: dir})
	e.waitFor("the first to run", func() bool { return jobNow(t, m, a.ID, first.ID, 0).Status == store.JobRunning })
	second := askJob(t, m, a.ID, api.PostJob{Dir: dir})
	if second.Status != store.JobQueued || second.Queued != 1 {
		t.Fatalf("%+v", second)
	}
	if code, out := jobAs(t, m, a.ID, "POST", "/jobs", `{"dir": "`+dir+`"}`); code != http.StatusTooManyRequests || !strings.Contains(out, "1 jobs waiting") {
		t.Fatalf("past the queue: %d %s", code, out)
	}
	// psps's own wait past the queue's limit, and go first.
	sys, err := m.submitJob(jobCaller{kind: store.CallerSystem, iso: testIsoID, why: "a test"}, "sim", dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if places := m.queuePlaces(); places[sys.ID] != 1 || places[second.ID] != 2 {
		t.Fatalf("places %v", places)
	}
	if code, out := jobAs(t, m, a.ID, "GET", "/jobs/"+sys.ID, ""); code != http.StatusNotFound {
		t.Fatalf("an agent sees psps's own job: %d %s", code, out)
	}

	if code, out := jobAs(t, m, a.ID, "DELETE", "/jobs/"+second.ID, ""); code != http.StatusOK || !strings.Contains(out, `"status":"cancelled"`) {
		t.Fatalf("cancelling a waiting one: %d %s", code, out)
	}
	e.waitFor("the first to trap SIGTERM", func() bool { _, err := os.Stat(filepath.Join(ready, first.ID)); return err == nil })
	if code, out := jobAs(t, m, a.ID, "DELETE", "/jobs/"+first.ID, ""); code != http.StatusOK {
		t.Fatalf("cancelling the running one: %d %s", code, out)
	}
	j := jobNow(t, m, a.ID, first.ID, 20)
	if j.Status != store.JobCancelled || j.Exit == nil || *j.Exit != 7 {
		t.Fatalf("the cancelled one: %+v", j)
	}
	if code, _ := jobAs(t, m, a.ID, "DELETE", "/jobs/"+first.ID, ""); code != http.StatusConflict {
		t.Fatalf("cancelling an ended one: %d", code)
	}
	os.WriteFile(gate, nil, 0o644)
	got, err := m.Job(context.Background(), sys.ID, 20)
	if err != nil || got.Status != store.JobDone || string(got.Result) != `{"caller":"ok"}` {
		t.Fatalf("psps's own: %+v %v", got, err)
	}
}

// A job runs apart from the daemon: the next one follows it to its end,
// one whose isolation started again is lost, and a command's exit status
// says how it failed.
func TestAJobRunsOnWhileTheDaemonStopsAndTheNextFollowsIt(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	gate := filepath.Join(t.TempDir(), "go")
	a, iso := withJobs(t, e, m, fmt.Sprintf(`while [ ! -f %s ]; do sleep 0.02; done; echo '{"caller":1}' > "$PSPS_JOB_OUT/result.json"; exit "$(cat code)"`, gate), nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "code"), []byte("0"), 0o644)
	j := askJob(t, m, a.ID, api.PostJob{Dir: dir})
	e.waitFor("it to run", func() bool { s, _ := e.st.Job(j.ID); return s.Status == store.JobRunning })
	stop()
	os.WriteFile(gate, nil, 0o644)
	<-e.fc.evalRun(iso.Container, j.ID).done
	if s, _ := e.st.Job(j.ID); s.Status != store.JobRunning {
		t.Fatalf("a stopping daemon settled it: %+v", s)
	}
	m, stop = e.daemon()
	jobService(t, m)
	e.waitFor("its end", func() bool { s, _ := e.st.Job(j.ID); return s.Status == store.JobDone })

	os.Remove(gate)
	j = askJob(t, m, a.ID, api.PostJob{Dir: dir})
	e.waitFor("it to run", func() bool { s, _ := e.st.Job(j.ID); return s.Status == store.JobRunning })
	stop()
	e.fc.reboot(iso.Container, true)
	m, stop = e.daemon()
	defer stop()
	jobService(t, m)
	e.waitFor("it lost", func() bool { s, _ := e.st.Job(j.ID); return s.Status == store.JobLost })

	os.WriteFile(gate, nil, 0o644)
	os.WriteFile(filepath.Join(dir, "code"), []byte("124"), 0o644)
	j = askJob(t, m, a.ID, api.PostJob{Dir: dir})
	if got := jobNow(t, m, a.ID, j.ID, 20); got.Status != store.JobFailed || got.Error != "out of time: it may run 1h0m0s" || string(got.Result) != "1" {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(filepath.Join(dir, "code"), []byte("3"), 0o644)
	j = askJob(t, m, a.ID, api.PostJob{Dir: dir})
	if got := jobNow(t, m, a.ID, j.ID, 20); got.Status != store.JobFailed || got.Error != "the command: exit status 3" {
		t.Fatalf("%+v", got)
	}
	// An isolation whose cgroup cannot give the limits runs it without, and
	// says so.
	e.fc.mu.Lock()
	e.fc.noLimits = true
	e.fc.mu.Unlock()
	os.WriteFile(filepath.Join(dir, "code"), []byte("0"), 0o644)
	j = askJob(t, m, a.ID, api.PostJob{Dir: dir})
	if got := jobNow(t, m, a.ID, j.ID, 20); got.Status != store.JobDone || !strings.Contains(got.Note, "no memory or pids limit: isolation run's cgroup cannot give them") {
		t.Fatalf("%+v", got)
	}
	// A copy over the runner's limits never runs.
	if _, err := m.PutEnv(context.Background(), jobproxy.Name, "sim", proxy.Patch{Set: map[string]any{"max_files": 1}}, false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "more"), nil, 0o644)
	j = askJob(t, m, a.ID, api.PostJob{Dir: dir})
	if got := jobNow(t, m, a.ID, j.ID, 20); got.Status != store.JobFailed || !strings.Contains(got.Error, "psps could not run it: copying "+dir+" from isolation dev: the directory has over 1 entries, over the runner's limit") {
		t.Fatalf("%+v", got)
	}
}

// A team's eval may be a job of a runner's: psps asks for it as itself,
// the score is result.json's "score", and the lead hears that alone.
func TestATeamsEvalIsAJobOfARunner(t *testing.T) {
	e := newTestEnv(t)
	m, stop := e.daemon()
	defer stop()
	ctx := context.Background()
	dir, keep := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(dir, "n"), []byte("5"), 0o644)
	boss, team := withTeams(t, e, m, api.PutLead{Budget: ptr(int64(1_000_000)), EvalDir: ptr(dir)})
	m.SetProxies(proxy.NewRegistry(mailproxy.Service{}, teamproxy.Service{}, controlproxy.Service{}, jobproxy.Service{}), creds.TestStore(filepath.Join(t.TempDir(), "credentials")))
	if _, err := m.CreateIsolation(ctx, api.CreateIsolation{Name: "run"}, nil); err != nil {
		t.Fatal(err)
	}
	cmd := fmt.Sprintf(`cp "$PSPS_JOB_META" %s/meta; [ "$(cat n)" = fail ] && exit 3; [ "$(cat n)" = none ] && { echo '{}' > "$PSPS_JOB_OUT/result.json"; exit 0; }
printf '{"score": %%s, "detail": "the-holdout-says"}' "$(cat n)" > "$PSPS_JOB_OUT/result.json"; echo printed-the-holdout`, keep)
	if _, err := m.PutEnv(ctx, jobproxy.Name, "sim", proxy.Patch{Set: map[string]any{"isolation": []string{"run"}, "command": cmd}}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Eval: ptr("true"), EvalJob: ptr("sim")}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both: %v", err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalJob: ptr("nope")}); err == nil || !strings.Contains(err.Error(), "the eval's runner") {
		t.Fatalf("no runner: %v", err)
	}
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalJob: ptr("sim")}); err != nil {
		t.Fatal(err)
	}
	if err := m.EvalTeam("lab"); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the eval's news", func() bool { return len(news(e, boss.ID)) == 1 })
	run, _ := m.TeamRun("lab")
	if len(run.Evals) != 1 || run.Evals[0].Score == nil || *run.Evals[0].Score != 5 || !run.Evals[0].Private || !strings.Contains(run.Evals[0].Output, "printed-the-holdout") {
		t.Fatalf("%+v", run.Evals)
	}
	if n := news(e, boss.ID)[0]; strings.Contains(n, "the-holdout-says") || strings.Contains(n, "printed-the-holdout") {
		t.Fatalf("the lead heard more than the score: %s", n)
	}
	var meta jobMeta
	b, _ := os.ReadFile(filepath.Join(keep, "meta"))
	if err := json.Unmarshal(b, &meta); err != nil || meta.Caller != store.CallerSystem || meta.Agent != nil || meta.Team == nil ||
		meta.Team.ID != team.ID || meta.Team.Name != "lab" || !strings.HasPrefix(meta.Why, "the team's eval: ") || meta.Dir != dir {
		t.Fatalf("meta %+v %v", meta, err)
	}
	// Control's agents cannot change an eval that is a runner's.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", EvalIsolation: ptr("run")}); err == nil || !strings.Contains(err.Error(), "leave --eval-iso out") {
		t.Fatalf("a runner and an eval isolation: %v", err)
	}

	os.WriteFile(filepath.Join(dir, "n"), []byte("fail"), 0o644)
	m.EvalTeam("lab")
	e.waitFor("the failed eval's news", func() bool { return len(news(e, boss.ID)) == 2 })
	if n := news(e, boss.ID)[1]; !strings.Contains(n, "psps says: the eval's job") || !strings.Contains(n, "the command: exit status 3") {
		t.Fatal(n)
	}
	os.WriteFile(filepath.Join(dir, "n"), []byte("none"), 0o644)
	m.EvalTeam("lab")
	e.waitFor("the unscored eval's news", func() bool { return len(news(e, boss.ID)) == 3 })
	if n := news(e, boss.ID)[2]; !strings.Contains(n, `left no score: its result.json has no "score" number`) {
		t.Fatal(n)
	}
	// A round ends with the runner's eval, as with a command.
	os.WriteFile(filepath.Join(dir, "n"), []byte("7"), 0o644)
	before := len(news(e, boss.ID))
	a := makeAgent(t, m, boss.ID, api.TeamAgentNew{Role: "one", Kind: driver.Codex, Prompt: "hello"})
	e.waitFor("the turn's news", func() bool { return strings.Contains(strings.Join(news(e, boss.ID), "\n"), a.ID+` ("one", codex) ended its turn`) })
	e.waitFor("the lead idle", func() bool { return free(m, boss.ID) })
	m.teamCheck(team.ID)
	e.waitFor("the round's eval", func() bool {
		ns := news(e, boss.ID)
		return len(ns) > before+1 && strings.Contains(ns[len(ns)-1], "(the round ended)")
	})
	if n := news(e, boss.ID); !strings.Contains(n[len(n)-1], "SCORE 7") {
		t.Fatal(n[len(n)-1])
	}

	// A command again is no runner's.
	if _, err := m.SetLead(ctx, "lab", api.PutLead{Agent: "boss", Eval: ptr("echo SCORE: 1")}); err != nil {
		t.Fatal(err)
	}
	if run, _ := m.TeamRun("lab"); run.Settings.EvalJob != "" || run.Settings.Eval != "echo SCORE: 1" {
		t.Fatalf("%+v", run.Settings)
	}
}
