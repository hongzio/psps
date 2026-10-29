// Package event is gero's own vocabulary for what an agent did. Drivers
// translate Claude Code and Codex output into it; the store, the TUI and the
// web UI only ever see these.
package event

import (
	"encoding/json"
	"time"
)

type Kind string

const (
	KindStatus       Kind = "status"        // Status changed
	KindUser         Kind = "user"          // a user message reached the agent
	KindAssistant    Kind = "assistant"     // a complete assistant message
	KindDelta        Kind = "delta"         // partial assistant text (ephemeral)
	KindReasoning    Kind = "reasoning"     // reasoning or plan text
	KindToolStart    Kind = "tool_start"    // the agent began a tool call
	KindToolOutput   Kind = "tool_output"   // partial tool output (ephemeral)
	KindToolEnd      Kind = "tool_end"      // a tool call finished
	KindApproval     Kind = "approval"      // the agent is waiting on a decision
	KindApprovalDone Kind = "approval_done" // that decision was delivered
	KindTurnStart    Kind = "turn_start"
	KindTurnEnd      Kind = "turn_end"
	KindError        Kind = "error"
	KindNotice       Kind = "notice" // lifecycle and diagnostics worth showing
	// KindRewind: the agent went back to before a user message and has
	// forgotten everything from it on; Rewind says which events that was.
	KindRewind Kind = "rewind"
)

type Status string

const (
	StatusStarting    Status = "starting"
	StatusIdle        Status = "idle"
	StatusWorking     Status = "working"
	StatusBlocked     Status = "blocked" // waiting on an approval
	StatusInterrupted Status = "interrupted"
	StatusError       Status = "error"
)

type Event struct {
	// ID is assigned by the store, per agent, in order. Ephemeral events
	// have none.
	ID    int64  `json:"id,omitempty"`
	Agent string `json:"agent"`
	// Src is the shim log Seq this event was derived from.
	Src  int64     `json:"src"`
	Time time.Time `json:"time"`
	Kind Kind      `json:"kind"`
	// Turn and Item group related events: deltas and the final message share
	// an Item; a tool's start, output and end share one too.
	Turn string `json:"turn,omitempty"`
	Item string `json:"item,omitempty"`

	Text     string    `json:"text,omitempty"`
	Status   Status    `json:"status,omitempty"`
	Tool     *Tool     `json:"tool,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
	Rewind   *Rewind   `json:"rewind,omitempty"`
	// Ephemeral events are delivered live but never stored: the complete
	// message or tool result that follows them carries the same content.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

type Tool struct {
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input,omitempty"`
	Summary string          `json:"summary,omitempty"` // one line, for lists
	Output  string          `json:"output,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type Approval struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`               // command, file_change, tool, ...
	Summary  string `json:"summary"`            // what will happen if accepted
	Reason   string `json:"reason,omitempty"`   // why the agent wants it
	Detail   string `json:"detail,omitempty"`   // full input, if any
	Decision string `json:"decision,omitempty"` // on approval_done
	// Questions are what a question (kind "question") asks the person:
	// accepting it is answering every one.
	Questions []Question `json:"questions,omitempty"`
	// Answers, on a question's approval_done, are each question's text to
	// what the person answered.
	Answers map[string]string `json:"answers,omitempty"`
}

// Question is one of Claude's AskUserQuestion: options to pick from, or
// to pick several of, and an answer of the person's own is taken too.
type Question struct {
	Question    string   `json:"question"`
	Header      string   `json:"header,omitempty"`
	Options     []Option `json:"options,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Rewind names the events a rewind took out of the conversation: those
// derived from shim records From <= Src < To, From being the user message
// gone back to. The store keeps them; the agent no longer knows them.
type Rewind struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}
