package driver

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	// prompts are the conversation's user messages; lastEntry is the uuid
	// of the newest transcript entry claude printed, which is where the
	// next message attaches.
	prompts   []prompt
	lastEntry string
	// rewind is asked for and not started yet; going is the generation
	// started with it, until claude's init shows the resume took.
	rewind *clRewind
	going  *clRewind
	// asked: this generation was sent the initialize request, which
	// claude answers with its commands and models, whenever it is sent.
	// terminal are the commands it says need its terminal.
	asked    bool
	commands []Command
	models   []Choice
	terminal []string
}

// clHidden are claude's commands gero keeps out of its list: /model and
// /effort are gero's to set, on the command line of each process, and
// /clear starts a session gero does not follow.
var clHidden = []string{"clear", "model", "effort"}

// clRewind is a generation that takes the conversation back: resumed up to
// a transcript entry, or, when the point is the session's start, a session
// of its own. Its command line is all the log keeps of it.
type clRewind struct {
	at      string // --resume-session-at; "" for a new session
	session string // that new session
	from    int64  // the first message it drops
	start   int64  // the generation's started record
	before  string // lastEntry before it, back if it fails
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
	switch {
	case c.rewind != nil && c.rewind.at == "":
		argv = append(argv, "--session-id", c.rewind.session)
	case c.rewind != nil:
		// The session itself, not a fork: its transcript entries keep
		// their uuids, so an earlier message stays reachable.
		argv = append(argv, "--resume", c.sessionID, "--resume-session-at", c.rewind.at)
	case c.sessionUsed:
		argv = append(argv, "--resume", c.sessionID)
	default:
		argv = append(argv, "--session-id", c.sessionID)
	}
	switch c.opts.approvals() {
	case ApprovalsAsk:
		argv = append(argv, "--permission-mode", "manual", "--permission-prompt-tool", "stdio")
	case ApprovalsAuto:
		// The classifier decides; an action it keeps blocking falls back to
		// the prompt tool, which is the person in gero.
		argv = append(argv, "--permission-mode", "auto", "--permission-prompt-tool", "stdio")
	case ApprovalsDefault:
		// No mode: settings.json's defaultMode decides (manual if it has
		// none), and whatever that mode would ask comes to gero.
		argv = append(argv, "--permission-prompt-tool", "stdio")
	default:
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

func (c *claude) Commands() []Command {
	var out []Command
	for _, cm := range c.commands {
		if !slices.Contains(clHidden, cm.Name) && !slices.Contains(c.terminal, cm.Name) && !strings.HasPrefix(cm.Name, "_") {
			out = append(out, cm)
		}
	}
	return out
}

func (c *claude) Models() []Choice { return c.models }

func (c *claude) Interrupt() { c.stop = true }

// Rewind starts the next generation from the transcript entry the message
// followed. That needs a new process, so a live one is replaced first, and
// the message waits for the new one.
func (c *claude) Rewind(src int64, text string) error {
	if c.rewind != nil || c.going != nil {
		return errors.New("the agent is already going back to a message")
	}
	i, err := c.canRewind(c.prompts, src)
	if err != nil {
		return err
	}
	p := c.prompts[i]
	if !p.ok {
		return errors.New("that message reached claude while it was working, so there is no point before it to go back to")
	}
	rw := &clRewind{at: p.at}
	if p.at == "" {
		rw.session = newUUID()
	}
	c.rewind = rw
	c.queue = append(c.queue, text)
	if c.alive {
		c.restart = true
	}
	return nil
}

// rewindIn reads a generation's command line for a rewind: a resume up to
// an entry, or a session other than this one started afresh.
func (c *claude) rewindIn(argv []string) *clRewind {
	flag := func(name string) (string, bool) {
		for i, a := range argv {
			if a == name && i+1 < len(argv) {
				return argv[i+1], true
			}
		}
		return "", false
	}
	var rw *clRewind
	if at, ok := flag("--resume-session-at"); ok {
		rw = &clRewind{at: at}
	} else if sid, ok := flag("--session-id"); ok && sid != c.sessionID && len(c.prompts) > 0 {
		if _, resumed := flag("--resume"); !resumed {
			rw = &clRewind{session: sid}
		}
	}
	if rw == nil {
		return nil
	}
	for _, p := range c.prompts {
		if p.ok && p.at == rw.at {
			rw.from = p.src
			break
		}
	}
	return rw
}

// wentBack drops the messages a rewind took out, now that claude resumed
// without them.
func (c *claude) wentBack(rec shimproto.Record, rw *clRewind) []event.Event {
	if rw.from == 0 {
		return nil
	}
	kept := c.prompts[:0]
	for _, p := range c.prompts {
		if p.src < rw.from || p.src > rw.start {
			kept = append(kept, p)
		}
	}
	c.prompts = kept
	e := c.ev(rec, event.KindRewind)
	e.Rewind = &event.Rewind{From: rw.from, To: rw.start}
	return []event.Event{e}
}

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
	if !c.asked {
		// Asked once a process: it costs no turn, and claude answers it
		// even mid-conversation, so one started before gero asked is asked
		// when gero next attaches.
		c.asked = true
		out = append(out, line(map[string]any{"type": "control_request",
			"request_id": fmt.Sprintf("gero-init-%d", c.gen),
			"request":    map[string]any{"subtype": "initialize"}}))
	}
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
	// A message for a rewind is for the process that starts with it.
	if c.held || c.rewind != nil {
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
	UUID      string          `json:"uuid"`
	Message   json.RawMessage `json:"message"`
	Event     json.RawMessage `json:"event"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`
	Result    string          `json:"result"`
	IsError   bool            `json:"is_error"`
	// Terminal are, in init, the commands that need claude's terminal.
	Terminal []string `json:"terminal_slash_commands"`
	// ParentToolUseID is set on a subagent's messages, which are in a
	// transcript of their own.
	ParentToolUseID *string `json:"parent_tool_use_id"`
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
			c.alive, c.restart, c.errText, c.asked = true, false, "", false
			c.busy, c.blocked, c.pendingTurns, c.pending = false, false, 0, nil
			evs = append(evs, c.text(rec, event.KindNotice, "", fmt.Sprintf("claude started (pid %d)", rec.Sys.PID)))
			if rw := c.rewindIn(rec.Sys.Argv); rw != nil {
				rw.start, rw.before = rec.Seq, c.lastEntry
				c.rewind, c.going, c.lastEntry = nil, rw, rw.at
			}
		case shimproto.SysExited, shimproto.SysLost:
			c.pending, c.pendingTurns = nil, 0
			evs = c.processEnded(rec, "claude")
			if rw := c.going; rw != nil {
				// It never resumed: the conversation is the one before, and
				// what was sent to it went nowhere to go back to.
				c.going, c.lastEntry = nil, rw.before
				for i := range c.prompts {
					if c.prompts[i].src > rw.start {
						c.prompts[i].ok = false
					}
				}
				msg := "claude could not go back to the edited message, so the conversation is as it was"
				if c.lastStderr != "" {
					msg += ": " + c.lastStderr
				}
				c.errText = msg
				evs = append(evs, c.text(rec, event.KindError, "", msg))
			}
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
		// One written while a turn runs joins claude's queue, and where it
		// lands in the transcript is claude's to decide.
		c.prompts = append(c.prompts, prompt{src: rec.Seq, at: c.lastEntry, ok: c.pendingTurns == 0})
		c.pendingTurns++
		c.userTurn()
		return []event.Event{c.text(rec, event.KindUser, "", blockText(msg.Content))}
	case "control_request":
		var r struct {
			Subtype string `json:"subtype"`
		}
		json.Unmarshal(m.Request, &r)
		if r.Subtype == "initialize" {
			c.asked = true
		}
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
	if (m.Type == "assistant" || m.Type == "user") && m.UUID != "" && m.ParentToolUseID == nil {
		c.lastEntry = m.UUID
	}
	switch m.Type {
	case "system":
		if m.Subtype == "init" {
			c.sessionUsed = true
			c.terminal = m.Terminal
			if m.SessionID != "" {
				c.sessionID = m.SessionID
			}
			if rw := c.going; rw != nil {
				c.going = nil
				return c.wentBack(rec, rw)
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

	case "control_response":
		c.answered(m.Response)

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

// answered takes claude's commands and models from its answer to gero's
// initialize request.
func (c *claude) answered(raw json.RawMessage) {
	var r struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Response  struct {
			Commands []struct {
				Name         string `json:"name"`
				Description  string `json:"description"`
				ArgumentHint string `json:"argumentHint"`
			} `json:"commands"`
			Models []struct {
				Value       string `json:"value"`
				DisplayName string `json:"displayName"`
				Description string `json:"description"`
			} `json:"models"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Subtype != "success" || !strings.HasPrefix(r.RequestID, "gero-init-") {
		return
	}
	c.commands, c.models = nil, nil
	for _, cm := range r.Response.Commands {
		if cm.Name != "" {
			c.commands = append(c.commands, Command{Prefix: "/", Name: cm.Name, Description: cm.Description, Hint: cm.ArgumentHint})
		}
	}
	for _, md := range r.Response.Models {
		if md.Value != "" {
			c.models = append(c.models, Choice{Value: md.Value, Description: joinDesc(md.DisplayName, md.Description)})
		}
	}
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
