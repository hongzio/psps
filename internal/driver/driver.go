// Package driver translates between an agent CLI's own protocol and gero's
// event vocabulary.
//
// A driver never touches a process or a socket. It is a deterministic fold
// over the shim's log — both what the agent wrote and what was written to it —
// so a fresh driver fed the same log ends in the same state. That is what lets
// the daemon crash at any point: on restart it replays the log and knows
// exactly which handshake step, turn and approval it was in.
package driver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/shimproto"
)

type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
)

type Approvals string

const (
	// ApprovalsNever runs the agent unattended. The isolation, not the
	// agent's own permission prompts, is the boundary.
	ApprovalsNever Approvals = "never"
	// ApprovalsAuto runs the agent unattended with a reviewer model in the
	// person's place: Claude's auto mode classifier, Codex's auto-review
	// agent behind its own sandbox. Risky actions (credentials sent out,
	// destruction beyond the request, instructions planted in content) are
	// refused; what the reviewer keeps refusing comes to the person.
	ApprovalsAuto Approvals = "auto"
	// ApprovalsAsk surfaces tool use as approval requests.
	ApprovalsAsk Approvals = "ask"
	// ApprovalsDefault leaves it to the CLI's own configuration in the
	// isolation: Claude's ~/.claude/settings.json (permissions.defaultMode,
	// manual when it has none), Codex's ~/.codex/config.toml (approval and
	// sandbox settings, Codex's own defaults when it has none). Whatever that
	// would ask a person comes to gero as an approval.
	ApprovalsDefault Approvals = "default"
)

// ApprovalModes are the modes in the order forms offer them. An agent
// created with none chosen (the API, `gero agent new` without --approvals)
// runs as never; the new-agent forms start on default.
var ApprovalModes = []Approvals{ApprovalsNever, ApprovalsAuto, ApprovalsAsk, ApprovalsDefault}

// Valid reports whether a is a mode; "" is the default, never.
func (a Approvals) Valid() bool {
	if a == "" {
		return true
	}
	for _, m := range ApprovalModes {
		if a == m {
			return true
		}
	}
	return false
}

// Efforts are the effort levels kind's CLI takes.
func Efforts(kind Kind) []string {
	if kind == Codex {
		return []string{"low", "medium", "high", "xhigh"}
	}
	return []string{"low", "medium", "high", "xhigh", "max"}
}

type Options struct {
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Approvals Approvals `json:"approvals,omitempty"`
}

// Check refuses an approval mode that is not one: anything unknown would
// otherwise run as never, unattended.
func (o Options) Check() error {
	if !o.Approvals.Valid() {
		return fmt.Errorf("unknown approvals mode %q (never, auto, ask or default)", o.Approvals)
	}
	return nil
}

func (o Options) approvals() Approvals {
	if o.Approvals == "" {
		return ApprovalsNever
	}
	return o.Approvals
}

type Decision string

const (
	Accept  Decision = "accept"
	Decline Decision = "decline" // the agent carries on without it
	Cancel  Decision = "cancel"  // decline and end the turn
)

func ParseDecision(s string) (Decision, error) {
	switch d := Decision(s); d {
	case Accept, Decline, Cancel:
		return d, nil
	}
	return "", fmt.Errorf("unknown decision %q (accept, decline, cancel)", s)
}

type State struct {
	Status event.Status `json:"status"`
	// Alive: a process is running for this agent.
	Alive bool `json:"alive"`
	// Busy: a turn is in flight.
	Busy bool `json:"busy"`
	// SessionID is the agent's own session or thread id, kept for resume.
	SessionID string `json:"session_id,omitempty"`
	// Restart: the running process must be replaced for new options to take
	// effect. The caller does it when the agent is not busy.
	Restart bool `json:"restart,omitempty"`
	// Queued user messages are waiting for the agent to be ready.
	Queued int `json:"queued,omitempty"`
	// Background is how many tasks the process runs in the background,
	// after its turn: replacing it would end them.
	Background int    `json:"background,omitempty"`
	Error      string `json:"error,omitempty"`
	// Usage is the model and spending the CLI last stated.
	Usage *Usage `json:"usage,omitempty"`
}

type Driver interface {
	// Command is the process to start for the next generation.
	Command() []string
	// Observe folds one log record into the driver and returns the events
	// it implies.
	Observe(rec shimproto.Record) []event.Event
	// Next returns lines to write to the agent now. Call it only when caught
	// up with the log; it never returns the same line twice.
	Next() [][]byte
	Send(text string)
	// Rewind takes the conversation back to just before the user message
	// the log carries at src and sends text in its place: the agent forgets
	// that message and everything after it. Files stay as they are. Only an
	// agent with nothing in flight goes back, and the log says when it did
	// (a rewind event).
	Rewind(src int64, text string) error
	// Approve decides a pending approval. answers are a question's (each
	// question's text to its answer), required to accept one and refused
	// for anything else.
	Approve(id string, d Decision, answers map[string]string) error
	Interrupt()
	SetOptions(o Options)
	// Hold keeps queued messages from starting turns while on. Everything
	// else — handshakes, approvals, interrupts, the turn in flight — goes on.
	Hold(on bool)
	State() State
	// Commands are what the agent's CLI offers to be typed in a message to
	// it, and Models the models it runs, as its process last said; nil
	// until one has.
	Commands() []Command
	Models() []Choice
	// ForkFrom starts the agent's session, before it has one, as a copy of
	// the session from: what it was told and did there, it knows. A
	// session it already has goes on as it is.
	ForkFrom(from string)
}

// A Command is one a person types in a message: Prefix and Name, then what
// Hint says it takes. Claude's are its slash commands and skills, which it
// runs itself at the start of a message; Codex's are its skills, which it
// picks out of a message by name anywhere in it, and /compact, which the
// driver runs.
type Command struct {
	Prefix      string `json:"prefix"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Hint        string `json:"hint,omitempty"`
	// Args are the values it takes, when they are few and known.
	Args []Choice `json:"args,omitempty"`
}

// A Choice is one value a command takes.
type Choice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// joinDesc is the parts of a description there are, in a line.
func joinDesc(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

// Rooted is a driver whose own sandbox is told which folders besides the
// workspace it may write to (Codex's auto). Claude is told on its command
// line instead.
type Rooted interface {
	SetWritableRoots(roots []string)
}

// New returns a driver for one agent. sessionID is empty for an agent that
// has never run.
func New(kind Kind, agentID, sessionID string, opts Options) (Driver, error) {
	switch kind {
	case Claude:
		return newClaude(agentID, sessionID, opts), nil
	case Codex:
		return newCodex(agentID, sessionID, opts), nil
	}
	return nil, fmt.Errorf("unknown agent kind %q", kind)
}

// base carries what both drivers share: the status rules and event helpers.
type base struct {
	agent string
	opts  Options
	// forkOf is the session to start the agent's own from, until it has one.
	forkOf string

	alive       bool
	busy        bool
	starting    bool // a process is up but not yet ready for input
	blocked     bool // an approval is pending
	interrupted bool // a process died with a turn in flight
	errText     string
	lastStderr  string
	restart     bool
	held        bool

	status event.Status
}

func (b *base) ForkFrom(from string) { b.forkOf = from }

func (b *base) Hold(on bool) { b.held = on }

// prompt is a user message in the agent's current conversation, with what
// a rewind to it needs: where the conversation stood before it.
type prompt struct {
	src int64 // the log record that carried it
	// at is the agent's own name for the point: Claude's last transcript
	// entry before the message ("" when the session starts with it),
	// Codex's turn for it. ok: at is known.
	at string
	ok bool
}

func findPrompt(ps []prompt, src int64) int {
	for i, p := range ps {
		if p.src == src {
			return i
		}
	}
	return -1
}

// canRewind refuses a rewind while anything is in flight.
func (b *base) canRewind(ps []prompt, src int64) (int, error) {
	if b.busy || b.blocked || b.starting {
		return 0, errors.New("the agent is working; stop it before editing a message")
	}
	i := findPrompt(ps, src)
	if i < 0 {
		return 0, errors.New("that message is not in the agent's current conversation")
	}
	return i, nil
}

func (b *base) ev(rec shimproto.Record, kind event.Kind) event.Event {
	return event.Event{Agent: b.agent, Src: rec.Seq, Time: rec.Time, Kind: kind}
}

func (b *base) text(rec shimproto.Record, kind event.Kind, item, text string) event.Event {
	e := b.ev(rec, kind)
	e.Item, e.Text = item, text
	return e
}

func (b *base) computeStatus() event.Status {
	switch {
	case b.blocked:
		return event.StatusBlocked
	case b.busy:
		return event.StatusWorking
	case b.errText != "":
		return event.StatusError
	case b.interrupted:
		return event.StatusInterrupted
	case b.starting:
		return event.StatusStarting
	}
	return event.StatusIdle
}

// settle appends a status event if the status changed.
func (b *base) settle(rec shimproto.Record, evs []event.Event) []event.Event {
	s := b.computeStatus()
	if s == b.status {
		return evs
	}
	b.status = s
	e := b.ev(rec, event.KindStatus)
	e.Status = s
	if s == event.StatusError {
		e.Text = b.errText
	}
	return append(evs, e)
}

// userTurn resets what a fresh user message clears.
func (b *base) userTurn() {
	b.busy = true
	b.interrupted = false
	b.errText = ""
}

// processEnded handles a generation's end. A turn in flight becomes an
// interruption the user resolves; a non-clean exit while idle is an error.
func (b *base) processEnded(rec shimproto.Record, name string) []event.Event {
	var evs []event.Event
	if b.busy {
		b.interrupted = true
		e := b.ev(rec, event.KindTurnEnd)
		e.Text = "interrupted"
		evs = append(evs, e)
	}
	b.alive, b.busy, b.starting, b.blocked = false, false, false, false
	sys := rec.Sys
	switch {
	case sys.Kind == shimproto.SysLost:
		evs = append(evs, b.text(rec, event.KindNotice, "", name+" was stopped with the isolation"))
	case sys.ExitCode == -1:
		b.errText = name + " failed to start: " + sys.Error
	case cleanExit(sys.ExitCode):
		evs = append(evs, b.text(rec, event.KindNotice, "", fmt.Sprintf("%s exited (%d)", name, sys.ExitCode)))
	case !b.interrupted:
		msg := fmt.Sprintf("%s exited with code %d", name, sys.ExitCode)
		if b.lastStderr != "" {
			msg += ": " + b.lastStderr
		}
		b.errText = msg
	}
	return evs
}

// cleanExit covers success and the signals gero itself sends to stop or
// restart an agent.
func cleanExit(code int) bool { return code == 0 || code == 128+2 || code == 128+15 }

func line(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
