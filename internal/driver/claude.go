package driver

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/shimproto"
)

// Claude speaks `claude -p` with stream-json on both sides: one JSON message
// per line, user messages in, transcript out. Options are command-line flags,
// so changing them means restarting the process with --resume.
//
// Permission prompts use the control protocol the Agent SDK uses
// (`--permission-prompt-tool stdio`): a control_request arrives on stdout and
// a control_response answers it on stdin. Both allow and deny were verified
// against Claude Code 2.1.311 inside an isolation.

type clPending struct {
	requestID string
	publicID  string
	subtype   string
	input     json.RawMessage
	approval  bool // false: a control request gero answers with an error
	decision  Decision
	sent      bool
}

type claude struct {
	base
	sessionID string
	// sessionUsed: some generation initialised this session, so the next one
	// must --resume it rather than claim the id afresh.
	sessionUsed  bool
	gen          int
	pendingTurns int
	queue        []string
	pending      []*clPending
	stop         bool
	ctlSeq       int
	streamMsg    string
}

func newClaude(agentID, sessionID string, opts Options) *claude {
	if sessionID == "" {
		sessionID = newUUID()
	}
	return &claude{base: base{agent: agentID, opts: opts, status: event.StatusIdle}, sessionID: sessionID}
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (c *claude) Command() []string {
	argv := []string{"claude", "-p",
		"--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages"}
	if c.sessionUsed {
		argv = append(argv, "--resume", c.sessionID)
	} else {
		argv = append(argv, "--session-id", c.sessionID)
	}
	if c.opts.approvals() == ApprovalsAsk {
		argv = append(argv, "--permission-mode", "manual", "--permission-prompt-tool", "stdio")
	} else {
		argv = append(argv, "--permission-mode", "bypassPermissions")
	}
	if c.opts.Model != "" {
		argv = append(argv, "--model", c.opts.Model)
	}
	if c.opts.Effort != "" {
		argv = append(argv, "--effort", c.opts.Effort)
	}
	return argv
}

func (c *claude) State() State {
	return State{Status: c.status, Alive: c.alive, Busy: c.busy, SessionID: c.sessionID, Error: c.errText, Restart: c.restart, Queued: len(c.queue)}
}

func (c *claude) Send(text string) { c.queue = append(c.queue, text) }

func (c *claude) Interrupt() { c.stop = true }

func (c *claude) SetOptions(o Options) {
	c.opts = o
	if c.alive {
		c.restart = true
	}
}

func (c *claude) Approve(id string, d Decision) error {
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

func (c *claude) Next() [][]byte {
	if !c.alive {
		return nil
	}
	var out [][]byte
	for _, p := range c.pending {
		if p.sent {
			continue
		}
		resp := map[string]any{"subtype": "success", "request_id": p.requestID}
		switch {
		case !p.approval:
			resp = map[string]any{"subtype": "error", "request_id": p.requestID,
				"error": "gero does not support control request " + p.subtype}
		case p.decision == Accept:
			resp["response"] = map[string]any{"behavior": "allow", "updatedInput": p.input}
		case p.decision == Decline:
			resp["response"] = map[string]any{"behavior": "deny", "message": "The user declined this in gero."}
		case p.decision == Cancel:
			resp["response"] = map[string]any{"behavior": "deny", "message": "The user declined this in gero and stopped the turn.", "interrupt": true}
		default:
			continue
		}
		out = append(out, line(map[string]any{"type": "control_response", "response": resp}))
		p.sent = true
	}
	if c.stop {
		c.stop = false
		if c.busy {
			c.ctlSeq++
			out = append(out, line(map[string]any{"type": "control_request",
				"request_id": fmt.Sprintf("gero-%d-%d", c.gen, c.ctlSeq),
				"request":    map[string]any{"subtype": "interrupt"}}))
		}
	}
	if c.held {
		return out
	}
	// Claude queues user messages itself, so there is no need to hold one
	// back until the previous turn ends.
	for _, text := range c.queue {
		out = append(out, line(map[string]any{"type": "user",
			"message": map[string]any{"role": "user", "content": text}}))
	}
	c.queue = nil
	return out
}

type clMessage struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
}

type clBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type clContent struct {
	ID      string          `json:"id"`
	Content json.RawMessage `json:"content"`
}

// blocks decodes message content, which is either a string or a block list.
func blocks(raw json.RawMessage) (string, []clBlock) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var bs []clBlock
	json.Unmarshal(raw, &bs)
	return "", bs
}

func blockText(raw json.RawMessage) string {
	s, bs := blocks(raw)
	if s != "" {
		return s
	}
	var parts []string
	for _, b := range bs {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (c *claude) Observe(rec shimproto.Record) []event.Event {
	var evs []event.Event
	switch rec.Dir {
	case shimproto.DirSys:
		switch rec.Sys.Kind {
		case shimproto.SysStarted:
			c.gen++
			c.alive, c.restart, c.errText = true, false, ""
			c.busy, c.blocked, c.pendingTurns, c.pending = false, false, 0, nil
			evs = append(evs, c.text(rec, event.KindNotice, "", fmt.Sprintf("claude started (pid %d)", rec.Sys.PID)))
		case shimproto.SysExited, shimproto.SysLost:
			c.pending, c.pendingTurns = nil, 0
			evs = c.processEnded(rec, "claude")
		}
	case shimproto.DirErr:
		c.lastStderr = rec.Line
	case shimproto.DirIn, shimproto.DirOut:
		var m clMessage
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

func (c *claude) observeIn(rec shimproto.Record, m clMessage) []event.Event {
	switch m.Type {
	case "user":
		var msg clContent
		json.Unmarshal(m.Message, &msg)
		c.pendingTurns++
		c.userTurn()
		return []event.Event{c.text(rec, event.KindUser, "", blockText(msg.Content))}
	case "control_response":
		var r struct {
			RequestID string `json:"request_id"`
			Response  struct {
				Behavior string `json:"behavior"`
			} `json:"response"`
		}
		json.Unmarshal(m.Response, &r)
		for i, p := range c.pending {
			if p.requestID != r.RequestID {
				continue
			}
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.blocked = c.hasOpenApprovals()
			if !p.approval {
				return nil
			}
			decision := "accept"
			if r.Response.Behavior != "allow" {
				decision = "decline"
			}
			e := c.ev(rec, event.KindApprovalDone)
			e.Approval = &event.Approval{ID: p.publicID, Decision: decision}
			return []event.Event{e}
		}
	}
	return nil
}

func (c *claude) hasOpenApprovals() bool {
	for _, p := range c.pending {
		if p.approval {
			return true
		}
	}
	return false
}

func (c *claude) observeOut(rec shimproto.Record, m clMessage) []event.Event {
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			c.sessionUsed = true
			if m.SessionID != "" {
				c.sessionID = m.SessionID
			}
		}

	case "assistant":
		var msg clContent
		json.Unmarshal(m.Message, &msg)
		_, bs := blocks(msg.Content)
		var evs []event.Event
		for i, b := range bs {
			item := fmt.Sprintf("%s:%d", msg.ID, i)
			switch b.Type {
			case "text":
				evs = append(evs, c.text(rec, event.KindAssistant, msg.ID, b.Text))
			case "thinking":
				if b.Thinking != "" {
					evs = append(evs, c.text(rec, event.KindReasoning, item, b.Thinking))
				}
			case "tool_use":
				e := c.ev(rec, event.KindToolStart)
				e.Item = b.ID
				e.Tool = &event.Tool{Name: b.Name, Input: b.Input, Summary: toolSummary(b.Name, b.Input)}
				evs = append(evs, e)
			}
		}
		return evs

	case "user":
		var msg clContent
		json.Unmarshal(m.Message, &msg)
		_, bs := blocks(msg.Content)
		var evs []event.Event
		for _, b := range bs {
			if b.Type != "tool_result" {
				continue
			}
			e := c.ev(rec, event.KindToolEnd)
			e.Item = b.ToolUseID
			e.Tool = &event.Tool{Output: blockText(b.Content), IsError: b.IsError}
			evs = append(evs, e)
		}
		return evs

	case "stream_event":
		var se struct {
			Type    string `json:"type"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		json.Unmarshal(m.Event, &se)
		switch se.Type {
		case "message_start":
			c.streamMsg = se.Message.ID
		case "content_block_delta":
			if se.Delta.Type == "text_delta" && se.Delta.Text != "" {
				e := c.text(rec, event.KindDelta, c.streamMsg, se.Delta.Text)
				e.Ephemeral = true
				return []event.Event{e}
			}
		}

	case "result":
		if c.pendingTurns > 0 {
			c.pendingTurns--
		}
		if c.pendingTurns == 0 {
			c.busy = false
		}
		e := c.ev(rec, event.KindTurnEnd)
		e.Text = m.Subtype
		evs := []event.Event{e}
		if m.IsError {
			msg := m.Result
			if msg == "" {
				msg = "claude turn failed (" + m.Subtype + ")"
			}
			if strings.Contains(msg, "Not logged in") || strings.Contains(msg, "authenticate") {
				msg += "; run `gero iso login <isolation> claude`"
			}
			c.errText = msg
			evs = append(evs, c.text(rec, event.KindError, "", msg))
		}
		return evs

	case "control_request":
		var r struct {
			Subtype   string          `json:"subtype"`
			ToolName  string          `json:"tool_name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		}
		json.Unmarshal(m.Request, &r)
		p := &clPending{requestID: m.RequestID, publicID: fmt.Sprintf("%d.%s", c.gen, m.RequestID),
			subtype: r.Subtype, input: r.Input}
		c.pending = append(c.pending, p)
		if r.Subtype != "can_use_tool" {
			return []event.Event{c.text(rec, event.KindNotice, "", "declined unsupported claude control request "+r.Subtype)}
		}
		p.approval = true
		c.blocked = true
		e := c.ev(rec, event.KindApproval)
		e.Item = r.ToolUseID
		e.Approval = &event.Approval{ID: p.publicID, Kind: "tool", Summary: r.ToolName + ": " + toolSummary(r.ToolName, r.Input),
			Detail: string(r.Input)}
		return []event.Event{e}
	}
	return nil
}

// toolSummary picks the one input field a person scans a tool call for.
func toolSummary(name string, input json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
