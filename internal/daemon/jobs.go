package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hongzio/psps/internal/api"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/jobproxy"
	"github.com/hongzio/psps/internal/store"
)

// Jobs (internal/proxy/jobproxy): a runner's command, the operator's, run
// in the runner's isolation on a copy of a directory, for a caller psps
// names in the job's meta.json: an agent (through its job address), psps
// itself (a team's eval on a runner) or the operator (`psps job run`).
// A runner runs so many at once (its max_live) and queues the rest, psps's
// own first. A job runs apart from the daemon, as an eval does, so one
// that outlives a daemon is followed by the next; what the caller gets of
// its result is what the command left for it, and the rest is the
// operator's.

var (
	// jobPoll is how often a running job is looked at.
	jobPoll = 5 * time.Second
	// jobGrace is how long a job asked to stop has before it is ended, and
	// how long past its limit one may run before psps ends it itself.
	jobGrace = 2 * time.Minute
	// jobCopyTime is how long readying a job and copying its directory may
	// take.
	jobCopyTime = 20 * time.Minute
)

const (
	// jobKeep is how long an ended job is kept.
	jobKeep = 14 * 24 * time.Hour
	// jobOutput is how much of a job's output is kept, for the operator.
	jobOutput = 64 << 10
	// jobWaitMax is the longest a caller's GET may wait for a job to end.
	jobWaitMax = 300
)

// errJobCancelled is a job cancelled before its command started.
var errJobCancelled = errors.New("cancelled before it started")

// --- runners and callers -----------------------------------------------------

// jobOf is isolation iso's job exposure, while it has one.
func (m *Manager) jobOf(iso string) (store.Exposure, bool, error) {
	if _, err := m.registry().Get(jobproxy.Name); err != nil {
		return store.Exposure{}, false, nil
	}
	x, err := m.store.Exposure(iso, jobproxy.Name)
	if errors.Is(err, store.ErrNotFound) {
		return x, false, nil
	}
	return x, err == nil, err
}

// jobEnv is an agent process's job address, while its isolation has the
// job service.
func (m *Manager) jobEnv(a store.Agent) []string {
	if _, on, err := m.jobOf(a.IsolationID); err != nil || !on {
		return nil
	}
	tok, err := m.store.MailToken(a.ID)
	if err != nil {
		m.log.Printf("agent %s: job token: %v", a.ID, err)
		return nil
	}
	return proxy.AgentEnv("JOB_URL", jobproxy.URL(tok), m.fromGero(a.IsolationID))
}

// runner is the runner (the job env) name.
func (m *Manager) runner(name string) (jobproxy.Env, error) {
	var e jobproxy.Env
	se, err := m.store.Env(jobproxy.Name, name)
	if err != nil {
		return e, err
	}
	return e, json.Unmarshal(se.Config, &e)
}

// A jobCaller is who asks for a job: its kind, the agent if one, the
// isolation the directory is in, its team, and where a relative
// directory is from.
type jobCaller struct {
	kind    string
	agent   store.Agent
	iso     string
	team    string
	workdir string
	why     string
}

func (m *Manager) agentCaller(a store.Agent) jobCaller {
	team, _ := m.store.LineTeam(cmp.Or(a.Line, a.ID))
	return jobCaller{kind: store.CallerAgent, agent: a, iso: a.IsolationID, team: team, workdir: a.Workdir}
}

// submitJob queues a job for c on runner name, which the caller is
// already let use.
func (m *Manager) submitJob(c jobCaller, name, dir string, options json.RawMessage) (store.Job, error) {
	env, err := m.runner(name)
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, httpError{http.StatusNotFound, fmt.Errorf("job: there is no runner %q", name)}
	}
	if err != nil {
		return store.Job{}, err
	}
	dir = strings.TrimSpace(dir)
	switch {
	case dir == "":
		return store.Job{}, badRequest(errors.New(`job: say which directory to copy: "dir"`))
	case !path.IsAbs(dir) && c.workdir == "":
		return store.Job{}, badRequest(fmt.Errorf("job: the directory is absolute, not %q", dir))
	case !path.IsAbs(dir):
		dir = path.Join(c.workdir, dir)
	}
	dir = path.Clean(dir)
	opts, err := jobOptions(options)
	if err != nil {
		return store.Job{}, err
	}
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	live, err := m.store.Jobs(true)
	if err != nil {
		return store.Job{}, err
	}
	waiting := 0
	for _, j := range live {
		if j.Runner == name && j.Status == store.JobQueued {
			waiting++
		}
	}
	// psps's own (an eval) are never refused for the queue.
	if c.kind != store.CallerSystem && waiting >= env.MaxQueued {
		return store.Job{}, httpError{http.StatusTooManyRequests, fmt.Errorf("job: runner %s has %d jobs waiting, its most; ask again when one has started", name, waiting)}
	}
	j := store.Job{ID: "j" + newID(), Runner: name, Isolation: env.Iso(), Caller: c.kind, Agent: c.agent.ID, From: c.iso, Team: c.team,
		Dir: dir, Why: c.why, Options: opts, Status: store.JobQueued, RequestedAt: time.Now().UTC()}
	if err := m.store.PutJob(j); err != nil {
		return j, err
	}
	m.store.PruneJobs(time.Now().Add(-jobKeep))
	m.log.Printf("job %s: queued on runner %s for %s", j.ID, name, m.jobWho(j))
	m.wakeJobs()
	return j, nil
}

// jobOptions is a job's options as kept: a JSON object, compact; none is
// {}.
func jobOptions(raw json.RawMessage) (string, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return "{}", nil
	}
	if len(t) > jobproxy.OptionsMax {
		return "", httpError{http.StatusRequestEntityTooLarge, fmt.Errorf("job: the options are at most %d bytes; put the rest in the directory", jobproxy.OptionsMax)}
	}
	var obj map[string]json.RawMessage
	if t[0] != '{' || json.Unmarshal(t, &obj) != nil {
		return "", badRequest(errors.New(`job: "options" is a JSON object`))
	}
	var b bytes.Buffer
	json.Compact(&b, t)
	return b.String(), nil
}

// jobWho says who asked for j, for the log.
func (m *Manager) jobWho(j store.Job) string {
	switch j.Caller {
	case store.CallerAgent:
		if a, err := m.storedAgent(j.Agent); err == nil {
			return "agent " + shownID(a)
		}
		return "agent " + j.Agent
	case store.CallerSystem:
		return "psps (" + cmp.Or(j.Why, "its own") + ")"
	}
	return "the operator"
}

// cancelJob asks job id to stop: one waiting ends at once, one under way
// is sent SIGTERM and ended jobGrace later.
func (m *Manager) cancelJob(id string) (store.Job, error) {
	m.jobsMu.Lock()
	j, err := m.store.Job(id)
	if err != nil {
		m.jobsMu.Unlock()
		return j, err
	}
	if store.JobEnded(j.Status) {
		m.jobsMu.Unlock()
		return j, httpError{http.StatusConflict, fmt.Errorf("job %s has ended: %s", id, j.Status)}
	}
	now := time.Now().UTC()
	if j.CancelAt == nil {
		j.CancelAt = &now
	}
	if j.Status == store.JobQueued {
		j.Status, j.Error, j.EndedAt = store.JobCancelled, errJobCancelled.Error(), &now
	}
	if err := m.store.PutJob(j); err != nil {
		m.jobsMu.Unlock()
		return j, err
	}
	m.jobsMu.Unlock()
	if j.Status == store.JobCancelled {
		m.jobEnded(j)
		return j, nil
	}
	if j.Status == store.JobRunning {
		if iso, err := m.store.Isolation(j.Isolation); err == nil {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			if err := m.cli.JobSignal(ctx, iso.Container, j.ID, false); err != nil {
				m.log.Printf("job %s: %v", j.ID, err)
			}
			cancel()
		}
	}
	m.log.Printf("job %s: asked to stop", j.ID)
	return j, nil
}

// --- running them ------------------------------------------------------------

func (m *Manager) wakeJobs() {
	select {
	case m.jobWake <- struct{}{}:
	default:
	}
}

// RunJobs starts the runners' jobs as they have room, until the daemon
// stops: first it takes up what a daemon before it left under way.
func (m *Manager) RunJobs() {
	m.resumeJobs()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		m.scheduleJobs()
		select {
		case <-m.ctx.Done():
			return
		case <-m.jobWake:
		case <-t.C:
		}
	}
}

// resumeJobs follows the jobs whose command runs, as it ran on without
// the daemon, and queues again those stopped while they were readied.
func (m *Manager) resumeJobs() {
	live, err := m.store.Jobs(true)
	if err != nil {
		m.log.Printf("jobs under way: %v", err)
		return
	}
	for _, j := range live {
		switch j.Status {
		case store.JobStarting:
			m.log.Printf("job %s: stopped while it was readied; queued again", j.ID)
			j.Status = store.JobQueued
			m.store.PutJob(j)
		case store.JobRunning:
			m.log.Printf("job %s: following it, running since %s", j.ID, j.StartedAt.Local().Format(time.TimeOnly))
			m.loops.Add(1)
			go func() {
				defer m.loops.Done()
				m.finishJob(m.followJob(j))
			}()
		}
	}
}

// jobOrder is the order the waiting jobs start in: psps's own first, then
// as they were asked for.
func jobOrder(js []store.Job) {
	sort.SliceStable(js, func(a, b int) bool {
		sa, sb := js[a].Caller == store.CallerSystem, js[b].Caller == store.CallerSystem
		if sa != sb {
			return sa
		}
		return js[a].RequestedAt.Before(js[b].RequestedAt)
	})
}

// scheduleJobs starts the waiting jobs the runners have room for.
func (m *Manager) scheduleJobs() {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	live, err := m.store.Jobs(true)
	if err != nil {
		m.log.Printf("jobs: %v", err)
		return
	}
	busy := map[string]int{}
	var waiting []store.Job
	for _, j := range live {
		if j.Status == store.JobQueued {
			waiting = append(waiting, j)
		} else {
			busy[j.Runner]++
		}
	}
	jobOrder(waiting)
	envs := map[string]*jobproxy.Env{}
	for _, j := range waiting {
		if m.ctx.Err() != nil {
			return
		}
		env, seen := envs[j.Runner]
		if !seen {
			if e, err := m.runner(j.Runner); err == nil {
				env = &e
			}
			envs[j.Runner] = env
		}
		if env == nil {
			now := time.Now().UTC()
			j.Status, j.Error, j.EndedAt = store.JobFailed, fmt.Sprintf("runner %s is gone", j.Runner), &now
			m.store.PutJob(j)
			go m.jobEnded(j)
			continue
		}
		if busy[j.Runner] >= env.MaxLive {
			continue
		}
		busy[j.Runner]++
		j.Status, j.Isolation = store.JobStarting, env.Iso()
		if err := m.store.PutJob(j); err != nil {
			m.log.Printf("job %s: %v", j.ID, err)
			continue
		}
		m.loops.Add(1)
		go func(j store.Job, env jobproxy.Env) {
			defer m.loops.Done()
			j, err := m.startJob(j, env)
			if err != nil {
				m.finishJob(m.failJob(j, err), nil)
				return
			}
			m.finishJob(m.followJob(j))
		}(j, *env)
	}
}

// jobMeta is meta.json: who asked, as psps knows it.
type jobMeta struct {
	V           int        `json:"v"`
	Job         string     `json:"job"`
	Runner      string     `json:"runner"`
	Caller      string     `json:"caller"`
	Agent       *metaAgent `json:"agent"`
	Isolation   string     `json:"isolation"`
	Team        *metaTeam  `json:"team"`
	Dir         string     `json:"dir"`
	Why         string     `json:"why,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	StartedAt   time.Time  `json:"started_at"`
	Deadline    time.Time  `json:"deadline"`
}

type metaAgent struct {
	ID     string `json:"id"`
	Handle string `json:"handle,omitempty"`
	Name   string `json:"name"`
}

type metaTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (m *Manager) jobMetaOf(j store.Job, started, deadline time.Time) []byte {
	meta := jobMeta{V: 1, Job: j.ID, Runner: j.Runner, Caller: j.Caller, Isolation: m.isoName(j.From), Dir: j.Dir, Why: j.Why,
		RequestedAt: j.RequestedAt, StartedAt: started, Deadline: deadline}
	if j.Agent != "" {
		meta.Agent = &metaAgent{ID: j.Agent}
		if a, err := m.storedAgent(j.Agent); err == nil {
			meta.Agent.Handle, meta.Agent.Name = a.Handle, a.Name
		}
	}
	if j.Team != "" {
		meta.Team = &metaTeam{ID: j.Team}
		if t, err := m.store.Team(j.Team); err == nil {
			meta.Team.Name = t.Name
		}
	}
	b, _ := json.Marshal(meta)
	return b
}

// jobArgv is how job j's command starts in its isolation iso: as the agent
// user, with nothing of root's environment, the isolation's own (its
// proxies' addresses and the operator's variables) and the job's, ended
// by timeout(1) at its limit.
func (m *Manager) jobArgv(j store.Job, env jobproxy.Env, iso store.Isolation, deadline time.Time) []string {
	d := isolation.JobDir(j.ID)
	vars := []string{"HOME=" + isolation.AgentHome, "USER=agent", "LOGNAME=agent", "LANG=C.UTF-8",
		"PATH=" + isolation.AgentHome + "/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	vars = append(vars, m.serviceWorkspace(iso.ID).env...)
	keys := make([]string, 0, len(iso.Env))
	for k := range iso.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vars = append(vars, k+"="+iso.Env[k])
	}
	gero := isolation.Gero(iso.Container)
	for _, kv := range [][2]string{{"JOB_ID", j.ID}, {"JOB_RUNNER", j.Runner}, {"JOB_META", d + "/meta.json"}, {"JOB_OPTIONS", d + "/options.json"},
		{"JOB_WORK", d + "/work"}, {"JOB_OUT", d + "/out"}, {"JOB_DEADLINE", deadline.UTC().Format(time.RFC3339)}} {
		vars = append(vars, proxy.AgentEnv(kv[0], kv[1], gero)...)
	}
	argv := append([]string{"env", "-i"}, vars...)
	argv = append(argv, "setpriv", "--reuid="+isolation.AgentUID, "--regid="+isolation.AgentUID, "--clear-groups",
		"timeout", fmt.Sprintf("--kill-after=%d", int(jobGrace.Seconds())), fmt.Sprintf("%ds", env.Minutes*60), "sh", "-c", env.Command)
	return argv
}

// startJob readies j in its runner's isolation, copies its directory
// there and starts the command, apart from the daemon.
func (m *Manager) startJob(j store.Job, env jobproxy.Env) (store.Job, error) {
	ctx, cancel := context.WithTimeout(m.ctx, jobCopyTime)
	defer cancel()
	iso, err := m.store.Isolation(j.Isolation)
	if err != nil {
		return j, fmt.Errorf("the runner's isolation: %w", err)
	}
	from, err := m.store.Isolation(j.From)
	if err != nil {
		return j, fmt.Errorf("the isolation the directory is in: %w", err)
	}
	if err := m.readyForEval(ctx, iso); err != nil {
		return j, err
	}
	if _, err := m.ensureIsolation(ctx, from.ID); err != nil {
		return j, err
	}
	if err := m.cli.JobPrepare(ctx, iso.Container, j.ID, isolation.JobPrep{Options: []byte(j.Options), Memory: env.MemoryBytes(), Pids: env.Pids}); err != nil {
		return j, err
	}
	forget := func() {
		fctx, fcancel := context.WithTimeout(m.ctx, time.Minute)
		defer fcancel()
		m.cli.JobForget(fctx, iso.Container, j.ID)
	}
	st, err := m.cli.JobCopy(ctx, from.Container, j.Dir, iso.Container, j.ID, env.Limits())
	if err != nil {
		forget()
		return j, fmt.Errorf("copying %s from isolation %s: %w", j.Dir, from.Name, err)
	}
	j.Note = copyNote(st)
	if cur, err := m.store.Job(j.ID); err == nil && cur.CancelAt != nil {
		forget()
		j.CancelAt = cur.CancelAt
		return j, errJobCancelled
	}
	started := time.Now().UTC()
	j.Limit = time.Duration(env.Minutes) * time.Minute
	deadline := started.Add(j.Limit)
	if err := m.cli.JobStart(ctx, iso.Container, j.ID, m.jobMetaOf(j, started, deadline), m.jobArgv(j, env, iso, deadline)); err != nil {
		forget()
		return j, err
	}
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	// A cancel that came meanwhile found it starting and sent nothing:
	// it is sent here. One after this finds it running and sends it itself.
	if cur, err := m.store.Job(j.ID); err == nil && cur.CancelAt != nil {
		j.CancelAt = cur.CancelAt
		if err := m.cli.JobSignal(ctx, iso.Container, j.ID, false); err != nil {
			m.log.Printf("job %s: %v", j.ID, err)
		}
	}
	j.Status, j.StartedAt = store.JobRunning, &started
	if err := m.store.PutJob(j); err != nil {
		m.log.Printf("job %s: %v", j.ID, err)
	}
	m.log.Printf("job %s: running on runner %s in isolation %s (%s)", j.ID, j.Runner, iso.Name, j.Note)
	return j, nil
}

// copyNote says what a copy carried and left out.
func copyNote(st isolation.CopyStats) string {
	s := fmt.Sprintf("copied %s, %s", plural(st.Files, "file"), sizeText(st.Bytes))
	if n := len(st.Skipped); n > 0 {
		shown := st.Skipped
		if n > 5 {
			shown = shown[:5]
		}
		s += fmt.Sprintf("; left out %s, not a file, directory or link (%s", plural(n, "entry"), strings.Join(shown, ", "))
		if n > 5 {
			s += ", ..."
		}
		s += ")"
	}
	return s
}

func plural(n int, one string) string {
	switch {
	case n == 1:
		return "1 " + one
	case strings.HasSuffix(one, "y"):
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(one, "y"))
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func sizeText(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", b)
}

// failJob is j ended by err before its command ran, or cancelled then.
func (m *Manager) failJob(j store.Job, err error) store.Job {
	now := time.Now().UTC()
	j.EndedAt = &now
	if errors.Is(err, errJobCancelled) {
		j.Status, j.Error = store.JobCancelled, err.Error()
	} else {
		j.Status, j.Error = store.JobFailed, "psps could not run it: "+err.Error()
	}
	return j
}

// followJob follows j until its command ends and says how it ended; nil
// when the daemon stops first, leaving it as it is for the next.
func (m *Manager) followJob(j store.Job) (store.Job, *jobEnd) {
	iso, err := m.store.Isolation(j.Isolation)
	if err != nil {
		return j, &jobEnd{err: fmt.Errorf("the runner's isolation: %w", err)}
	}
	killed := false
	var st isolation.JobState
	for {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		state, serr := m.cli.State(ctx, iso.Container)
		if serr == nil && state != "running" {
			st, err = isolation.JobState{Lost: true}, nil
		} else {
			st, err = m.cli.JobPoll(ctx, iso.Container, j.ID)
		}
		cancel()
		if m.ctx.Err() != nil {
			return j, nil
		}
		if err == nil && !st.Running {
			break
		}
		cur, cerr := m.store.Job(j.ID)
		if cerr == nil {
			j.CancelAt = cur.CancelAt
		}
		now := time.Now()
		over := j.StartedAt != nil && now.After(j.StartedAt.Add(j.Limit+jobGrace+30*time.Second))
		stop := j.CancelAt != nil && now.After(j.CancelAt.Add(jobGrace))
		if over || stop {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			if kerr := m.cli.JobSignal(ctx, iso.Container, j.ID, true); kerr != nil {
				m.log.Printf("job %s: %v", j.ID, kerr)
			}
			cancel()
			killed = killed || over
		}
		select {
		case <-m.ctx.Done():
			return j, nil
		case <-time.After(jobPoll):
		}
	}
	// A cancel that came as it ended still counts.
	if cur, cerr := m.store.Job(j.ID); cerr == nil {
		j.CancelAt = cur.CancelAt
	}
	end := &jobEnd{state: st, overdue: killed, iso: iso}
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	defer cancel()
	if !st.Lost {
		end.result, end.output, end.collectErr = m.cli.JobCollect(ctx, iso.Container, j.ID, jobproxy.ResultMax, jobOutput)
	}
	if ferr := m.cli.JobForget(ctx, iso.Container, j.ID); ferr != nil && !st.Lost {
		m.log.Printf("job %s: %v", j.ID, ferr)
	}
	return j, end
}

// jobEnd is how a job's command ended, and what it left.
type jobEnd struct {
	err        error
	state      isolation.JobState
	overdue    bool
	iso        store.Isolation
	result     []byte
	output     string
	collectErr error
}

// finishJob keeps what came of j and tells whoever waits; with no end, the
// daemon stopped first and j is left for the next.
func (m *Manager) finishJob(j store.Job, end *jobEnd) {
	if end == nil && j.EndedAt == nil {
		return
	}
	if end != nil {
		j = settleJob(j, *end)
	}
	if err := m.store.PutJob(j); err != nil {
		m.log.Printf("job %s: %v", j.ID, err)
	}
	m.log.Printf("job %s: %s%s", j.ID, j.Status, prefixed(": ", j.Error))
	m.jobEnded(j)
	m.wakeJobs()
}

func prefixed(p, s string) string {
	if s == "" {
		return ""
	}
	return p + s
}

// settleJob is j with what end says of it.
func settleJob(j store.Job, end jobEnd) store.Job {
	now := time.Now().UTC()
	j.EndedAt = &now
	j.Output = tail(end.output, jobOutput)
	switch {
	case end.err != nil:
		j.Status, j.Error = store.JobFailed, "psps could not follow it: "+end.err.Error()
		return j
	case end.state.Lost:
		j.Status, j.Error = store.JobLost, fmt.Sprintf("isolation %s started again while it ran, and nothing of it is left", end.iso.Name)
		return j
	}
	code := end.state.Exit
	j.Exit = &code
	switch {
	case j.CancelAt != nil:
		j.Status, j.Error = store.JobCancelled, "cancelled; "+exitText(code)
	case end.overdue:
		j.Status, j.Error = store.JobFailed, fmt.Sprintf("still running past its %s; ended", j.Limit)
	case code == 0:
		j.Status = store.JobDone
	case code == 124:
		j.Status, j.Error = store.JobFailed, fmt.Sprintf("out of time: it may run %s", j.Limit)
	default:
		j.Status, j.Error = store.JobFailed, "the command: "+exitText(code)
	}
	switch {
	case end.collectErr != nil:
		j.Note = joinNote(j.Note, end.collectErr.Error())
	case end.result == nil:
		j.Note = joinNote(j.Note, "it left no result.json")
	default:
		var obj map[string]json.RawMessage
		if json.Unmarshal(end.result, &obj) != nil {
			j.Note = joinNote(j.Note, "its result.json is not a JSON object")
			break
		}
		var b bytes.Buffer
		json.Compact(&b, end.result)
		j.Result = b.String()
	}
	return j
}

func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// jobEnded tells those waiting on j that it has ended, and what asked for
// it for its own (a team's eval).
func (m *Manager) jobEnded(j store.Job) {
	m.jobsMu.Lock()
	ch := m.jobWaiters[j.ID]
	delete(m.jobWaiters, j.ID)
	hook := m.jobHooks[j.ID]
	delete(m.jobHooks, j.ID)
	m.jobsMu.Unlock()
	if ch != nil {
		close(ch)
	}
	if hook != nil {
		hook(j)
	}
}

// jobWait is closed when job id ends.
func (m *Manager) jobWait(id string) <-chan struct{} {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	ch := m.jobWaiters[id]
	if ch == nil {
		ch = make(chan struct{})
		m.jobWaiters[id] = ch
	}
	return ch
}

// waitJob is job id once it has ended, or as it is after secs, or when
// ctx ends.
func (m *Manager) waitJob(ctx context.Context, id string, secs int) (store.Job, error) {
	j, err := m.store.Job(id)
	if err != nil || secs <= 0 || store.JobEnded(j.Status) {
		return j, err
	}
	ch := m.jobWait(id)
	if j, err = m.store.Job(id); err != nil || store.JobEnded(j.Status) {
		return j, err
	}
	t := time.NewTimer(time.Duration(min(secs, jobWaitMax)) * time.Second)
	defer t.Stop()
	select {
	case <-ch:
	case <-t.C:
	case <-ctx.Done():
	case <-m.ctx.Done():
	}
	return m.store.Job(id)
}

// --- views ---------------------------------------------------------------------

// queuePlaces are the waiting jobs' places, from 1, by runner.
func (m *Manager) queuePlaces() map[string]int {
	live, err := m.store.Jobs(true)
	if err != nil {
		return nil
	}
	var waiting []store.Job
	for _, j := range live {
		if j.Status == store.JobQueued {
			waiting = append(waiting, j)
		}
	}
	jobOrder(waiting)
	out, n := map[string]int{}, map[string]int{}
	for _, j := range waiting {
		n[j.Runner]++
		out[j.ID] = n[j.Runner]
	}
	return out
}

// callerResult is what result.json left for the caller: its "caller".
func callerResult(result string) json.RawMessage {
	if result == "" {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &obj) != nil {
		return nil
	}
	return obj["caller"]
}

func (m *Manager) callerJob(j store.Job, places map[string]int) api.CallerJob {
	v := api.CallerJob{ID: j.ID, Runner: j.Runner, Dir: j.Dir, Status: j.Status, Error: j.Error, Note: j.Note, Queued: places[j.ID],
		RequestedAt: j.RequestedAt, StartedAt: j.StartedAt, EndedAt: j.EndedAt, Exit: j.Exit, Result: callerResult(j.Result)}
	if j.Agent != "" {
		if a, err := m.storedAgent(j.Agent); err == nil {
			v.Agent = shownID(a)
		}
	}
	if j.StartedAt != nil {
		d := j.StartedAt.Add(j.Limit)
		v.Deadline = &d
	}
	return v
}

func (m *Manager) jobView(j store.Job, places map[string]int) api.Job {
	v := api.Job{Job: j, RunnerIsolation: m.isoName(j.Isolation), FromIsolation: m.isoName(j.From), Queued: places[j.ID],
		Options: json.RawMessage(cmp.Or(j.Options, "{}"))}
	if j.Result != "" {
		v.Result = json.RawMessage(j.Result)
	}
	if j.Agent != "" {
		if a, err := m.storedAgent(j.Agent); err == nil {
			v.AgentID = shownID(a)
		}
	}
	if j.Team != "" {
		if t, err := m.store.Team(j.Team); err == nil {
			v.TeamName = t.Name
		}
	}
	return v
}

// --- the operator's ---------------------------------------------------------------

// Jobs are the jobs psps keeps, newest first; with live, those that have
// not ended.
func (m *Manager) Jobs(live bool) ([]api.Job, error) {
	js, err := m.store.Jobs(live)
	if err != nil {
		return nil, err
	}
	places := m.queuePlaces()
	out := []api.Job{}
	for _, j := range js {
		out = append(out, m.jobView(j, places))
	}
	return out, nil
}

// Job is job id, all of it, once it ends or after wait seconds.
func (m *Manager) Job(ctx context.Context, id string, wait int) (api.Job, error) {
	j, err := m.waitJob(ctx, id, wait)
	if err != nil {
		return api.Job{}, err
	}
	return m.jobView(j, m.queuePlaces()), nil
}

// RunJob queues the operator's own job, on a copy of a directory in
// req.Isolation.
func (m *Manager) RunJob(req api.PostJob) (api.Job, error) {
	if req.Runner == "" {
		return api.Job{}, badRequest(errors.New("job: say which runner"))
	}
	iso, err := m.store.IsolationNamed(req.Isolation)
	if err != nil {
		return api.Job{}, badRequest(fmt.Errorf("job: isolation %q: %w", req.Isolation, err))
	}
	j, err := m.submitJob(jobCaller{kind: store.CallerOperator, iso: iso.ID}, req.Runner, req.Dir, req.Options)
	if err != nil {
		return api.Job{}, err
	}
	return m.jobView(j, m.queuePlaces()), nil
}

// CancelJob asks job id to stop.
func (m *Manager) CancelJob(id string) (api.Job, error) {
	j, err := m.cancelJob(id)
	if err != nil {
		return api.Job{}, err
	}
	return m.jobView(j, m.queuePlaces()), nil
}

// --- an agent's address ----------------------------------------------------------

// jobCallerOf is the live agent of isolation iso whose address carries
// tok, and its isolation's job exposure.
func (m *Manager) jobCallerOf(iso, tok string) (store.Agent, store.Exposure, error) {
	name := m.isoName(iso)
	x, on, err := m.jobOf(iso)
	if err != nil {
		return store.Agent{}, x, err
	}
	if !on {
		return store.Agent{}, x, httpError{http.StatusForbidden, fmt.Errorf("job: isolation %s has no runners; the operator opens one with `psps iso expose %s job RUNNER`", name, name)}
	}
	a, err := m.store.AgentByMailToken(tok)
	if err != nil || a.IsolationID != iso || a.ArchivedAt != nil {
		return a, x, httpError{http.StatusForbidden, fmt.Errorf("job: this is not the address of an agent in isolation %s; use $PSPS_JOB_URL as your process was started with it", name)}
	}
	return a, x, nil
}

// jobRoutes adds the job service to isolation iso's mail port. Nothing it
// logs carries a path: the path carries the agent's token.
func (m *Manager) jobRoutes(mux *http.ServeMux, iso string) {
	route := func(pattern string, f func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure)) {
		method, p, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /a/{token}/job"+p, func(w http.ResponseWriter, r *http.Request) {
			a, x, err := m.jobCallerOf(iso, r.PathValue("token"))
			if err != nil {
				reply(w, nil, err)
				return
			}
			f(w, r, a, x)
		})
	}
	// mine are the agents' jobs asked for from iso, newest first.
	mine := func() ([]store.Job, error) {
		js, err := m.store.Jobs(false)
		out := []store.Job{}
		for _, j := range js {
			if j.Caller == store.CallerAgent && j.From == iso {
				out = append(out, j)
			}
		}
		return out, err
	}
	one := func(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
		j, err := m.store.Job(r.PathValue("id"))
		if err != nil || j.Caller != store.CallerAgent || j.From != iso {
			reply(w, nil, fmt.Errorf("job: no job of this isolation's is %s (GET $PSPS_JOB_URL/jobs lists them): %w", r.PathValue("id"), store.ErrNotFound))
			return j, false
		}
		return j, true
	}

	route("GET ", func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure) {
		live, err := m.store.Jobs(true)
		if err != nil {
			reply(w, nil, err)
			return
		}
		v := api.JobHome{Runners: []api.Runner{}, Jobs: []api.CallerJob{}}
		for _, name := range x.Envs {
			e, err := m.runner(name)
			if err != nil {
				continue
			}
			rv := api.Runner{Name: name, Default: name == x.Default, Minutes: e.Minutes, Memory: e.Memory, Pids: e.Pids, MaxLive: e.MaxLive,
				MaxQueued: e.MaxQueued, MaxMB: e.MaxMB, MaxFiles: e.MaxFiles, Exclude: e.Exclude}
			for _, j := range live {
				if j.Runner != name {
					continue
				}
				if j.Status == store.JobQueued {
					rv.Waiting++
				} else {
					rv.Running++
				}
			}
			v.Runners = append(v.Runners, rv)
		}
		js, err := mine()
		places := m.queuePlaces()
		for i, j := range js {
			if i == 20 {
				break
			}
			v.Jobs = append(v.Jobs, m.callerJob(j, places))
		}
		reply(w, v, err)
	})
	route("GET /jobs", func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure) {
		js, err := mine()
		places := m.queuePlaces()
		out := []api.CallerJob{}
		for _, j := range js {
			out = append(out, m.callerJob(j, places))
		}
		reply(w, out, err)
	})
	route("POST /jobs", func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure) {
		var req api.PostJob
		body, err := io.ReadAll(io.LimitReader(r.Body, jobproxy.OptionsMax+64<<10))
		if err == nil {
			err = json.Unmarshal(body, &req)
		}
		if err != nil {
			reply(w, nil, badRequest(fmt.Errorf(`job: the body is JSON, as {"runner": "NAME", "dir": "/home/agent/work/x", "options": {...}}: %v`, err)))
			return
		}
		name := cmp.Or(req.Runner, x.Default)
		open := false
		for _, e := range x.Envs {
			open = open || e == name
		}
		if !open {
			reply(w, nil, httpError{http.StatusForbidden, fmt.Errorf("job: runner %q is not open to isolation %s (it has %s); only your operator opens more",
				name, m.isoName(iso), orNothing(strings.Join(x.Envs, ", ")))})
			return
		}
		j, err := m.submitJob(m.agentCaller(a), name, req.Dir, req.Options)
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
		}
		reply(w, m.callerJob(j, m.queuePlaces()), err)
	})
	route("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure) {
		j, ok := one(w, r)
		if !ok {
			return
		}
		wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
		j, err := m.waitJob(r.Context(), j.ID, wait)
		reply(w, m.callerJob(j, m.queuePlaces()), err)
	})
	route("DELETE /jobs/{id}", func(w http.ResponseWriter, r *http.Request, a store.Agent, x store.Exposure) {
		j, ok := one(w, r)
		if !ok {
			return
		}
		j, err := m.cancelJob(j.ID)
		reply(w, m.callerJob(j, m.queuePlaces()), err)
	})
}

func orNothing(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
