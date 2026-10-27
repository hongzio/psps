package shim

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hongzio/gero/internal/shimproto"
)

var (
	errRunning    = errors.New("agent is already running")
	errNotRunning = errors.New("agent is not running")
)

// subscriberBuffer bounds how far a live reader may fall behind. A reader that
// overflows it is dropped rather than allowed to stall the agent's output: it
// reconnects with the last Seq it saw and catches up from the log.
const subscriberBuffer = 4096

// drainGrace is how long output pumps may keep reading after the agent's main
// process exits. A background child that inherited stdout would otherwise hold
// the pipe open and delay the exit record indefinitely.
const drainGrace = 2 * time.Second

type subscriber struct {
	ch   chan shimproto.Record
	dead chan struct{}
}

type state struct {
	Running bool     `json:"running"`
	PID     int      `json:"pid,omitempty"`
	Argv    []string `json:"argv,omitempty"`
}

// agent is one agent's durable log plus, at most, one live process.
type agent struct {
	id  string
	dir string

	// stdinMu orders writers end to end: the "in" record and the bytes on the
	// pipe happen in the same order for everyone. It is separate from mu so a
	// write blocked on a full pipe never stops the output pumps from logging.
	stdinMu sync.Mutex

	mu      sync.Mutex
	log     *os.File
	seq     int64
	subs    map[*subscriber]struct{}
	running bool
	pid     int
	stdin   *os.File
	done    chan struct{}
	// abandoned: the shim is gone as far as the log is concerned.
	abandoned bool
}

func openAgent(dir, id string) (*agent, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "log.jsonl")
	seq, good, err := scanTail(path)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// A crash mid-append leaves a torn last line; cut it so the next append
	// starts on a line boundary and every reader sees only whole records.
	if err := f.Truncate(good); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, err
	}
	return &agent{id: id, dir: dir, log: f, seq: seq, subs: map[*subscriber]struct{}{}}, nil
}

// scanTail returns the last good Seq in a log and the byte offset where the
// good prefix ends.
func scanTail(path string) (seq int64, good int64, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var off int64
	for {
		line, rerr := r.ReadBytes('\n')
		if rerr == nil {
			var rec shimproto.Record
			if json.Unmarshal(line, &rec) != nil {
				return seq, off, nil
			}
			seq = rec.Seq
			off += int64(len(line))
			continue
		}
		if rerr == io.EOF {
			return seq, off, nil
		}
		return 0, 0, rerr
	}
}

func (a *agent) readState() state {
	var st state
	b, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err == nil {
		json.Unmarshal(b, &st)
	}
	return st
}

func (a *agent) writeStateLocked(argv []string) {
	if a.abandoned {
		return
	}
	b, _ := json.Marshal(state{Running: a.running, PID: a.pid, Argv: argv})
	tmp := filepath.Join(a.dir, "state.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(a.dir, "state.json"))
	}
}

// recoverLost runs once when the shim starts. An agent recorded as running has
// no process under this shim any more; say so in the log. If the shim alone
// restarted (the container did not), the old process group may still be alive
// as an orphan — kill it, or a resumed agent would run twice on one session.
func (a *agent) recoverLost() {
	st := a.readState()
	if !st.Running {
		return
	}
	if st.PID > 0 && len(st.Argv) > 0 && processMatches(st.PID, st.Argv[0]) {
		syscall.Kill(-st.PID, syscall.SIGKILL)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running, a.pid = false, 0
	a.writeStateLocked(nil)
	a.appendLocked(shimproto.Record{Dir: shimproto.DirSys, Sys: &shimproto.Sys{Kind: shimproto.SysLost, PID: st.PID}})
}

// processMatches guards the orphan kill against PID reuse: after a reboot the
// recorded PID may belong to something unrelated.
func processMatches(pid int, argv0 string) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	first, _, _ := bytes.Cut(b, []byte{0})
	return filepath.Base(string(first)) == filepath.Base(argv0)
}

func (a *agent) appendLocked(rec shimproto.Record) shimproto.Record {
	if a.abandoned {
		return rec
	}
	a.seq++
	rec.Seq = a.seq
	rec.Time = time.Now().UTC()
	b, _ := json.Marshal(rec)
	b = append(b, '\n')
	if _, err := a.log.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "gero-shim: %s: log write: %v\n", a.id, err)
	}
	if rec.Dir == shimproto.DirSys {
		a.log.Sync()
	}
	for s := range a.subs {
		select {
		case s.ch <- rec:
		default:
			close(s.dead)
			delete(a.subs, s)
		}
	}
	return rec
}

func (a *agent) append(rec shimproto.Record) {
	a.mu.Lock()
	a.appendLocked(rec)
	a.mu.Unlock()
}

func (a *agent) info() shimproto.AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return shimproto.AgentInfo{ID: a.id, Running: a.running, PID: a.pid, LastSeq: a.seq}
}

func (a *agent) start(spec shimproto.Spec, baseEnv []string, cred *syscall.Credential) error {
	if len(spec.Argv) == 0 {
		return errors.New("empty argv")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return errRunning
	}

	// Our own pipes rather than cmd.StdoutPipe: with *os.File the child gets
	// the descriptors directly, so cmd.Wait reports the exit as soon as the
	// process ends instead of waiting for every inheritor of the pipe.
	inR, inW, err := os.Pipe()
	if err != nil {
		return err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return err
	}

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = append(append([]string{}, baseEnv...), spec.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: cred}
	startErr := cmd.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		a.appendLocked(shimproto.Record{Dir: shimproto.DirSys, Sys: &shimproto.Sys{
			Kind: shimproto.SysExited, Argv: spec.Argv, ExitCode: -1, Error: startErr.Error()}})
		return startErr
	}

	a.running, a.pid, a.stdin, a.done = true, cmd.Process.Pid, inW, make(chan struct{})
	a.writeStateLocked(spec.Argv)
	a.appendLocked(shimproto.Record{Dir: shimproto.DirSys, Sys: &shimproto.Sys{
		Kind: shimproto.SysStarted, PID: a.pid, Argv: spec.Argv}})

	var pumps sync.WaitGroup
	pumps.Add(2)
	go a.pump(outR, shimproto.DirOut, &pumps)
	go a.pump(errR, shimproto.DirErr, &pumps)
	go func(done chan struct{}) {
		code := exitCode(cmd.Wait())
		drained := make(chan struct{})
		go func() { pumps.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(drainGrace):
			outR.Close()
			errR.Close()
			<-drained
		}
		a.stdinMu.Lock()
		a.mu.Lock()
		a.stdin.Close()
		a.running, a.pid, a.stdin = false, 0, nil
		a.writeStateLocked(nil)
		a.appendLocked(shimproto.Record{Dir: shimproto.DirSys, Sys: &shimproto.Sys{
			Kind: shimproto.SysExited, ExitCode: code}})
		a.mu.Unlock()
		a.stdinMu.Unlock()
		close(done)
	}(a.done)
	return nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

func (a *agent) pump(r *os.File, dir shimproto.Dir, wg *sync.WaitGroup) {
	defer wg.Done()
	defer r.Close()
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadString('\n')
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			a.append(shimproto.Record{Dir: dir, Line: line})
		}
		if err != nil {
			return
		}
	}
}

// write logs line as an "in" record and then delivers it to the agent. The
// record goes first so a reader of the log never sees an agent's reply before
// the request that caused it.
func (a *agent) write(line []byte) error {
	a.stdinMu.Lock()
	defer a.stdinMu.Unlock()
	a.mu.Lock()
	if !a.running || a.stdin == nil {
		a.mu.Unlock()
		return errNotRunning
	}
	w := a.stdin
	a.appendLocked(shimproto.Record{Dir: shimproto.DirIn, Line: string(line)})
	a.mu.Unlock()
	_, err := w.Write(append(line, '\n'))
	return err
}

func (a *agent) signal(sig syscall.Signal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running {
		return errNotRunning
	}
	return syscall.Kill(-a.pid, sig)
}

// subscribe registers a live reader and returns the newest Seq at that
// instant. Everything up to it must be read from the log; everything after
// arrives on the channel. Taking both under one lock is what makes the
// hand-over gapless.
func (a *agent) subscribe() (*subscriber, int64) {
	s := &subscriber{ch: make(chan shimproto.Record, subscriberBuffer), dead: make(chan struct{})}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.subs[s] = struct{}{}
	return s, a.seq
}

func (a *agent) unsubscribe(s *subscriber) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.subs[s]; ok {
		delete(a.subs, s)
		close(s.dead)
	}
}

// replay calls fn for every logged record with after < Seq <= upto.
func (a *agent) replay(after, upto int64, fn func(shimproto.Record) error) error {
	if upto <= after {
		return nil
	}
	f, err := os.Open(filepath.Join(a.dir, "log.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		var rec shimproto.Record
		if json.Unmarshal(line, &rec) != nil {
			return nil
		}
		if rec.Seq <= after {
			continue
		}
		if rec.Seq > upto {
			return nil
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
}

// stop sends SIGTERM and waits up to grace for the process to exit.
func (a *agent) stop(grace time.Duration) {
	a.mu.Lock()
	if !a.running {
		a.mu.Unlock()
		return
	}
	done := a.done
	syscall.Kill(-a.pid, syscall.SIGTERM)
	a.mu.Unlock()
	select {
	case <-done:
	case <-time.After(grace):
	}
}
