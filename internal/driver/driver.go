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
	"fmt"

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
	Queued int    `json:"queued,omitempty"`
	Error  string `json:"error,omitempty"`
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
	Approve(id string, d Decision) error
	Interrupt()
	SetOptions(o Options)
	// Hold keeps queued messages from starting turns while on. Everything
	// else — handshakes, approvals, interrupts, the turn in flight — goes on.
	Hold(on bool)
	State() State
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

func (b *base) Hold(on bool) { b.held = on }

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
