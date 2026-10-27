package driver

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/shimproto"
)

// Codex speaks `codex app-server`: JSON-RPC 2.0 over stdio, one thread per
// agent. The thread id survives the process, so every generation after the
// first opens with thread/resume.

type cxPhase int

const (
	cxDown         cxPhase = iota
	cxStarted              // process up; initialize not sent
	cxInitializing         // initialize sent
	cxInitialized          // need `initialized` and an auth check
	cxCheckingAuth         // account/read sent
	cxNeedThread           // need thread/start or thread/resume
	cxOpening              // thread request sent
	cxReady
	cxNoAuth // not logged in; nothing works until the user logs in
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (m rpcMsg) hasID() bool { return len(m.ID) > 0 && string(m.ID) != "null" }

type cxPending struct {
	rpcID    string // the server's request id, raw JSON, echoed back verbatim
	method   string
	publicID string
	approval bool // false: an unsupported request we answer with an error
	decision Decision
	sent     bool
}

type codex struct {
	base
	threadID string
	gen      int
	phase    cxPhase
	nextID   int64
	calls    map[int64]string
	turnID   string
	pending  []*cxPending
	queue    []string
	// sending: a turn/start is written but not yet seen in the log. It holds
	// back the next queued message without claiming the agent is working.
	sending bool
	stop    bool // interrupt requested
	itemSum map[string]string
}

func newCodex(agentID, threadID string, opts Options) *codex {
	return &codex{base: base{agent: agentID, opts: opts, status: event.StatusIdle}, threadID: threadID}
}

func (c *codex) Command() []string { return []string{"codex", "app-server"} }

func (c *codex) State() State {
	return State{Status: c.status, Alive: c.alive, Busy: c.busy, SessionID: c.threadID, Error: c.errText, Restart: c.restart, Queued: len(c.queue)}
}

func (c *codex) Send(text string) {
	c.queue = append(c.queue, text)
	if c.phase == cxNoAuth {
		// The user may have logged in since. Auth is read at startup, so a
		// fresh process is the reliable way to pick it up.
		c.restart = true
	}
}

func (c *codex) Interrupt() { c.stop = true }

// SetOptions applies from the next turn: Codex takes model and effort per
// turn/start, so the process never needs replacing.
func (c *codex) SetOptions(o Options) { c.opts = o }

func (c *codex) Approve(id string, d Decision) error {
	for _, p := range c.pending {
		if p.publicID == id && p.approval {
			if p.decision != "" {
				return fmt.Errorf("approval %s already decided", id)
			}
			p.decision = d
			return nil
		}
	}
	return fmt.Errorf("no pending approval %s", id)
}

func (c *codex) call(method string, params any) []byte {
	id := c.nextID
	c.nextID++
	c.calls[id] = method
	return line(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// sandbox is always off: the isolation is the boundary, and Codex's own
// sandbox needs bubblewrap, which the container does not provide. Approvals
// are a separate question, answered by approvalPolicy.
func (c *codex) sandbox() string { return "danger-full-access" }

func (c *codex) approvalPolicy() string {
	if c.opts.approvals() == ApprovalsAsk {
		return "untrusted"
	}
	return "never"
}

func (c *codex) Next() [][]byte {
	if !c.alive {
		return nil
	}
	var out [][]byte
	switch c.phase {
	case cxStarted:
		out = append(out, c.call("initialize", map[string]any{
			"clientInfo": map[string]any{"name": "gero", "title": "gero", "version": "0.1.0"}}))
		c.phase = cxInitializing
	case cxInitialized:
		out = append(out, line(map[string]any{"jsonrpc": "2.0", "method": "initialized"}))
		out = append(out, c.call("account/read", map[string]any{}))
		c.phase = cxCheckingAuth
	case cxNeedThread:
		params := map[string]any{"approvalPolicy": c.approvalPolicy(), "sandbox": c.sandbox()}
		if c.opts.Model != "" {
			params["model"] = c.opts.Model
		}
		if c.threadID != "" {
			params["threadId"] = c.threadID
			out = append(out, c.call("thread/resume", params))
		} else {
			out = append(out, c.call("thread/start", params))
		}
		c.phase = cxOpening
	}

	for _, p := range c.pending {
		if p.sent {
			continue
		}
		switch {
		case !p.approval:
			out = append(out, line(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(p.rpcID),
				"error": map[string]any{"code": -32601, "message": "gero does not support " + p.method}}))
			p.sent = true
		case p.decision != "":
			out = append(out, line(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(p.rpcID),
				"result": map[string]any{"decision": string(p.decision)}}))
			p.sent = true
		}
	}

	if c.stop {
		c.stop = false
		if c.turnID != "" {
			out = append(out, c.call("turn/interrupt", map[string]any{"threadId": c.threadID, "turnId": c.turnID}))
		}
	}

	if c.phase == cxReady && !c.busy && !c.sending && !c.held && len(c.queue) > 0 {
		text := c.queue[0]
		c.queue = c.queue[1:]
		params := map[string]any{"threadId": c.threadID,
			"input": []any{map[string]any{"type": "text", "text": text}}}
		if c.opts.Effort != "" {
			params["effort"] = c.opts.Effort
		}
		if c.opts.Model != "" {
			params["model"] = c.opts.Model
		}
		out = append(out, c.call("turn/start", params))
		c.sending = true
	}
	return out
}

func (c *codex) Observe(rec shimproto.Record) []event.Event {
	var evs []event.Event
	switch rec.Dir {
	case shimproto.DirSys:
		evs = c.observeSys(rec)
	case shimproto.DirErr:
		c.lastStderr = rec.Line
	case shimproto.DirIn, shimproto.DirOut:
		var m rpcMsg
		if json.Unmarshal([]byte(rec.Line), &m) != nil {
			return nil
		}
		if rec.Dir == shimproto.DirIn {
			evs = c.observeIn(rec, m)
		} else {
			evs = c.observeOut(rec, m)
		}
	}
	return c.settle(rec, evs)
}

func (c *codex) observeSys(rec shimproto.Record) []event.Event {
	switch rec.Sys.Kind {
	case shimproto.SysStarted:
		c.gen++
		c.alive, c.starting, c.restart, c.errText, c.sending = true, true, false, "", false
		c.phase, c.nextID, c.calls = cxStarted, 1, map[int64]string{}
		c.turnID, c.pending, c.itemSum = "", nil, map[string]string{}
		c.busy, c.blocked = false, false
		return []event.Event{c.text(rec, event.KindNotice, "", fmt.Sprintf("codex started (pid %d)", rec.Sys.PID))}
	case shimproto.SysExited, shimproto.SysLost:
		c.phase, c.pending, c.turnID, c.sending = cxDown, nil, "", false
		return c.processEnded(rec, "codex")
	}
	return nil
}

// observeIn tracks what was written to the agent, so a replay knows which
// requests are already in flight.
func (c *codex) observeIn(rec shimproto.Record, m rpcMsg) []event.Event {
	switch {
	case m.Method != "" && m.hasID():
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return nil
		}
		c.calls[id] = m.Method
		if id >= c.nextID {
			c.nextID = id + 1
		}
		switch m.Method {
		case "initialize":
			c.phase = cxInitializing
		case "account/read":
			c.phase = cxCheckingAuth
		case "thread/start", "thread/resume":
			c.phase = cxOpening
		case "turn/start":
			var p struct {
				Input []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"input"`
			}
			json.Unmarshal(m.Params, &p)
			var parts []string
			for _, in := range p.Input {
				if in.Type == "text" {
					parts = append(parts, in.Text)
				}
			}
			c.sending = false
			c.userTurn()
			return []event.Event{c.text(rec, event.KindUser, "", strings.Join(parts, "\n"))}
		}
	case m.Method == "" && m.hasID():
		// Our answer to one of the server's requests.
		for i, p := range c.pending {
			if p.rpcID != string(m.ID) {
				continue
			}
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.blocked = c.hasOpenApprovals()
			if !p.approval {
				return nil
			}
			var r struct {
				Decision string `json:"decision"`
			}
			json.Unmarshal(m.Result, &r)
			e := c.ev(rec, event.KindApprovalDone)
			e.Approval = &event.Approval{ID: p.publicID, Decision: r.Decision}
			return []event.Event{e}
		}
	}
	return nil
}

func (c *codex) hasOpenApprovals() bool {
	for _, p := range c.pending {
		if p.approval {
			return true
		}
	}
	return false
}

func (c *codex) observeOut(rec shimproto.Record, m rpcMsg) []event.Event {
	switch {
	case m.Method != "" && m.hasID():
		return c.serverRequest(rec, m)
	case m.Method == "" && m.hasID():
		id, err := strconv.ParseInt(string(m.ID), 10, 64)
		if err != nil {
			return nil
		}
		method, ok := c.calls[id]
		if !ok {
			return nil
		}
		delete(c.calls, id)
		return c.response(rec, method, m)
	case m.Method != "":
		return c.notification(rec, m)
	}
	return nil
}

func (c *codex) response(rec shimproto.Record, method string, m rpcMsg) []event.Event {
	if m.Error != nil {
		switch method {
		case "thread/resume":
			// The thread is gone (deleted, or a different CODEX_HOME). Carry
			// on in a new one, and say so: the agent has lost its context.
			old := c.threadID
			c.threadID, c.phase = "", cxNeedThread
			return []event.Event{c.text(rec, event.KindError, "",
				fmt.Sprintf("could not resume codex thread %s (%s); starting a new thread without the earlier context", old, m.Error.Message))}
		case "turn/start":
			c.busy = false
		case "initialize", "account/read", "thread/start":
			c.starting = false
			c.errText = fmt.Sprintf("codex %s failed: %s", method, m.Error.Message)
			return nil
		}
		return []event.Event{c.text(rec, event.KindError, "", fmt.Sprintf("codex %s: %s", method, m.Error.Message))}
	}
	switch method {
	case "initialize":
		c.phase = cxInitialized
	case "account/read":
		var r struct {
			Account            json.RawMessage `json:"account"`
			RequiresOpenaiAuth bool            `json:"requiresOpenaiAuth"`
		}
		json.Unmarshal(m.Result, &r)
		if r.RequiresOpenaiAuth && (len(r.Account) == 0 || string(r.Account) == "null") {
			c.phase, c.starting = cxNoAuth, false
			c.errText = "codex is not logged in in this isolation; run `gero iso login <isolation> codex`"
			return nil
		}
		c.phase = cxNeedThread
	case "thread/start", "thread/resume":
		var r struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		json.Unmarshal(m.Result, &r)
		if r.Thread.ID != "" {
			c.threadID = r.Thread.ID
		}
		c.phase, c.starting = cxReady, false
		return []event.Event{c.text(rec, event.KindNotice, "", method+" "+c.threadID)}
	case "turn/start":
		var r struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		json.Unmarshal(m.Result, &r)
		c.turnID = r.Turn.ID
	}
	return nil
}

func (c *codex) serverRequest(rec shimproto.Record, m rpcMsg) []event.Event {
	p := &cxPending{rpcID: string(m.ID), method: m.Method, publicID: fmt.Sprintf("%d.%s", c.gen, m.ID)}
	c.pending = append(c.pending, p)
	var params struct {
		ItemID  string `json:"itemId"`
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
		Reason  string `json:"reason"`
	}
	json.Unmarshal(m.Params, &params)
	var a *event.Approval
	switch m.Method {
	case "item/commandExecution/requestApproval":
		a = &event.Approval{Kind: "command", Summary: params.Command, Reason: params.Reason, Detail: params.Cwd}
	case "item/fileChange/requestApproval":
		sum := c.itemSum[params.ItemID]
		if sum == "" {
			sum = "apply file changes"
		}
		a = &event.Approval{Kind: "file_change", Summary: sum, Reason: params.Reason}
	default:
		return []event.Event{c.text(rec, event.KindNotice, "", "declined unsupported codex request "+m.Method)}
	}
	p.approval = true
	a.ID = p.publicID
	c.blocked = true
	e := c.ev(rec, event.KindApproval)
	e.Turn, e.Item, e.Approval = c.turnID, params.ItemID, a
	return []event.Event{e}
}

type cxItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	Summary          []string        `json:"summary"`
	Command          string          `json:"command"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Status           string          `json:"status"`
	Changes          []cxChange      `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
}

type cxChange struct {
	Path string `json:"path"`
	Kind struct {
		Type string `json:"type"`
	} `json:"kind"`
	Diff string `json:"diff"`
}

func (it cxItem) tool() (*event.Tool, bool) {
	switch it.Type {
	case "commandExecution":
		t := &event.Tool{Name: "shell", Summary: it.Command}
		if it.AggregatedOutput != nil {
			t.Output = *it.AggregatedOutput
		}
		t.IsError = it.Status == "failed" || it.Status == "declined" || (it.ExitCode != nil && *it.ExitCode != 0)
		return t, true
	case "fileChange":
		var paths, diffs []string
		for _, ch := range it.Changes {
			paths = append(paths, ch.Kind.Type+" "+ch.Path)
			diffs = append(diffs, ch.Diff)
		}
		return &event.Tool{Name: "edit", Summary: strings.Join(paths, ", "), Output: strings.Join(diffs, "\n"),
			IsError: it.Status == "failed" || it.Status == "declined"}, true
	case "mcpToolCall":
		t := &event.Tool{Name: it.Server + "/" + it.Tool, Input: it.Arguments}
		if it.Error != nil {
			t.Output, t.IsError = it.Error.Message, true
		} else if len(it.Result) > 0 && string(it.Result) != "null" {
			t.Output = string(it.Result)
		}
		return t, true
	case "dynamicToolCall":
		return &event.Tool{Name: it.Tool, Input: it.Arguments, IsError: it.Status == "failed"}, true
	case "webSearch":
		return &event.Tool{Name: "web_search", Summary: it.Query}, true
	}
	return nil, false
}

func (c *codex) notification(rec shimproto.Record, m rpcMsg) []event.Event {
	switch m.Method {
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		json.Unmarshal(m.Params, &p)
		c.turnID, c.busy = p.Turn.ID, true
		e := c.ev(rec, event.KindTurnStart)
		e.Turn = c.turnID
		return []event.Event{e}

	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		json.Unmarshal(m.Params, &p)
		c.busy, c.turnID = false, ""
		e := c.ev(rec, event.KindTurnEnd)
		e.Turn, e.Text = p.Turn.ID, p.Turn.Status
		evs := []event.Event{e}
		if p.Turn.Status == "failed" && p.Turn.Error != nil {
			c.errText = p.Turn.Error.Message
			evs = append(evs, c.text(rec, event.KindError, "", p.Turn.Error.Message))
		}
		return evs

	case "item/started", "item/completed":
		var p struct {
			Item   cxItem `json:"item"`
			TurnID string `json:"turnId"`
		}
		if json.Unmarshal(m.Params, &p) != nil {
			return nil
		}
		it := p.Item
		if t, ok := it.tool(); ok {
			c.itemSum[it.ID] = t.Summary
			kind := event.KindToolStart
			if m.Method == "item/completed" {
				kind = event.KindToolEnd
			} else {
				t.Output, t.IsError = "", false
			}
			e := c.ev(rec, kind)
			e.Turn, e.Item, e.Tool = p.TurnID, it.ID, t
			return []event.Event{e}
		}
		if m.Method != "item/completed" {
			return nil
		}
		var e event.Event
		switch it.Type {
		case "agentMessage":
			e = c.text(rec, event.KindAssistant, it.ID, it.Text)
		case "reasoning":
			if len(it.Summary) == 0 {
				return nil
			}
			e = c.text(rec, event.KindReasoning, it.ID, strings.Join(it.Summary, "\n"))
		case "plan":
			e = c.text(rec, event.KindReasoning, it.ID, it.Text)
		case "contextCompaction":
			e = c.text(rec, event.KindNotice, it.ID, "context compacted")
		default:
			return nil
		}
		e.Turn = p.TurnID
		return []event.Event{e}

	case "item/agentMessage/delta", "item/commandExecution/outputDelta":
		var p struct {
			Delta  string `json:"delta"`
			ItemID string `json:"itemId"`
			TurnID string `json:"turnId"`
		}
		json.Unmarshal(m.Params, &p)
		kind := event.KindDelta
		if m.Method == "item/commandExecution/outputDelta" {
			kind = event.KindToolOutput
		}
		e := c.text(rec, kind, p.ItemID, p.Delta)
		e.Turn, e.Ephemeral = p.TurnID, true
		return []event.Event{e}

	case "error":
		var p struct {
			Error struct {
				Message           string `json:"message"`
				AdditionalDetails string `json:"additionalDetails"`
			} `json:"error"`
			WillRetry bool `json:"willRetry"`
		}
		json.Unmarshal(m.Params, &p)
		msg := p.Error.Message
		if p.Error.AdditionalDetails != "" {
			msg += ": " + p.Error.AdditionalDetails
		}
		if p.WillRetry {
			return []event.Event{c.text(rec, event.KindNotice, "", msg)}
		}
		return []event.Event{c.text(rec, event.KindError, "", msg)}

	case "thread/status/changed":
		var p struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		}
		json.Unmarshal(m.Params, &p)
		if p.Status.Type == "systemError" {
			c.errText = "codex reported a system error"
		}
	}
	return nil
}
