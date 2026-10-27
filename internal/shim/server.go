// Package shim owns agent processes inside an isolation and keeps a durable,
// sequenced log of everything they read and write. It runs as the isolation
// container's main process, so agents outlive any daemon connection: the
// daemon attaches, detaches, crashes or upgrades, and the agent keeps working.
package shim

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hongzio/gero/internal/shimproto"
)

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type Server struct {
	// Root holds agents/<id>/{log.jsonl,state.json}. It should be on durable
	// storage that survives the container being recreated.
	Root string
	// BaseEnv is every agent's environment before its Spec.Env.
	BaseEnv []string
	// Cred, when set, is the user agents run as.
	Cred *syscall.Credential

	mu     sync.Mutex
	agents map[string]*agent
}

// Open loads every agent on disk and records the ones whose process did not
// survive the shim's last exit.
func (s *Server) Open() error {
	s.agents = map[string]*agent{}
	dir := filepath.Join(s.Root, "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !validID.MatchString(e.Name()) {
			continue
		}
		a, err := openAgent(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			return fmt.Errorf("agent %s: %w", e.Name(), err)
		}
		a.recoverLost()
		s.agents[a.id] = a
	}
	return nil
}

func (s *Server) get(id string, create bool) (*agent, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid agent id %q", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[id]; ok {
		return a, nil
	}
	if !create {
		return nil, fmt.Errorf("unknown agent %q", id)
	}
	a, err := openAgent(filepath.Join(s.Root, "agents", id), id)
	if err != nil {
		return nil, err
	}
	s.agents[id] = a
	return a, nil
}

func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go s.handle(c)
	}
}

// Shutdown asks every running agent to exit and waits up to grace, so a clean
// container stop logs real exits instead of leaving "lost" for the next boot.
func (s *Server) Shutdown(grace time.Duration) {
	s.mu.Lock()
	agents := make([]*agent, 0, len(s.agents))
	for _, a := range s.agents {
		agents = append(agents, a)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func(a *agent) { defer wg.Done(); a.stop(grace) }(a)
	}
	wg.Wait()
}

// Abandon stops all logging and leaves agent processes running, the way a
// shim that crashed would. It exists so tests can stage a crash or a reboot.
func (s *Server) Abandon() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		a.mu.Lock()
		a.abandoned = true
		for sub := range a.subs {
			close(sub.dead)
			delete(a.subs, sub)
		}
		a.mu.Unlock()
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 64<<10)
	w := bufio.NewWriter(c)
	reply := func(resp shimproto.Response) {
		b, _ := json.Marshal(resp)
		w.Write(append(b, '\n'))
		w.Flush()
	}
	fail := func(err error) { reply(shimproto.Response{Error: err.Error()}) }

	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req shimproto.Request
	if err := json.Unmarshal(line, &req); err != nil {
		fail(fmt.Errorf("bad request: %w", err))
		return
	}

	switch req.Op {
	case shimproto.OpPing:
		reply(shimproto.Response{OK: true, Version: shimproto.Version})

	case shimproto.OpList:
		s.mu.Lock()
		infos := make([]shimproto.AgentInfo, 0, len(s.agents))
		for _, a := range s.agents {
			infos = append(infos, a.info())
		}
		s.mu.Unlock()
		sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
		reply(shimproto.Response{OK: true, Agents: infos})

	case shimproto.OpStart:
		a, err := s.get(req.Agent, true)
		if err != nil {
			fail(err)
			return
		}
		if req.Spec == nil {
			fail(errors.New("start needs a spec"))
			return
		}
		if err := a.start(*req.Spec, s.BaseEnv, s.Cred); err != nil {
			fail(err)
			return
		}
		info := a.info()
		reply(shimproto.Response{OK: true, Agent: &info})

	case shimproto.OpSignal:
		a, err := s.get(req.Agent, false)
		if err != nil {
			fail(err)
			return
		}
		sig, ok := map[string]syscall.Signal{"TERM": syscall.SIGTERM, "INT": syscall.SIGINT, "KILL": syscall.SIGKILL}[req.Signal]
		if !ok {
			fail(fmt.Errorf("unsupported signal %q", req.Signal))
			return
		}
		if err := a.signal(sig); err != nil {
			fail(err)
			return
		}
		reply(shimproto.Response{OK: true})

	case shimproto.OpAttach:
		// Attaching to an agent that has never started creates it, so a
		// caller can attach first and start second without missing the
		// "started" record.
		a, err := s.get(req.Agent, true)
		if err != nil {
			fail(err)
			return
		}
		s.attach(a, req.After, r, w, reply)

	default:
		fail(fmt.Errorf("unknown op %q", req.Op))
	}
}

func (s *Server) attach(a *agent, after int64, r *bufio.Reader, w *bufio.Writer, reply func(shimproto.Response)) {
	sub, last := a.subscribe()
	defer a.unsubscribe(sub)
	info := a.info()
	reply(shimproto.Response{OK: true, LastSeq: last, Agent: &info})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Everything the client sends after the request is agent stdin. Its EOF
	// is the client detaching.
	go func() {
		defer cancel()
		for {
			line, err := r.ReadBytes('\n')
			if line = trimNewline(line); len(line) > 0 {
				if werr := a.write(line); werr != nil {
					fmt.Fprintf(os.Stderr, "gero-shim: %s: dropped stdin line: %v\n", a.id, werr)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	send := func(rec shimproto.Record) error {
		b, _ := json.Marshal(rec)
		_, err := w.Write(append(b, '\n'))
		return err
	}
	if err := a.replay(after, last, send); err != nil {
		return
	}
	if w.Flush() != nil {
		return
	}
	for {
		select {
		case rec := <-sub.ch:
			if rec.Seq <= last || rec.Seq <= after {
				continue
			}
			if send(rec) != nil || w.Flush() != nil {
				return
			}
		case <-sub.dead:
			return
		case <-ctx.Done():
			return
		}
	}
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
