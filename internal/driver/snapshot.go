package driver

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hongzio/psps/internal/event"
)

// A snapshot is a driver's fold of its log up to some record, kept so a
// daemon that starts again goes on from there instead of reading the whole
// log over (a long-lived agent's runs to hundreds of megabytes). Only a
// driver that has seen nothing but its log may be snapshotted: what a
// caller asked of it (Send, Approve, Interrupt, SetOptions, Hold) is not
// in the log, and the daemon applies it again after catching up, as it
// does on a fresh fold.
//
// What the caller sets once (the agent, its options, OnHost, Hold,
// SetWritableRoots) is not kept: Restore goes onto a driver made as a
// fresh one would be, which already has them.

// snapVersion changes whenever what a snapshot holds does; a snapshot of
// another version is not one Restore takes, and the log is read over.
const snapVersion = 1

// ErrSnapshot is a snapshot Restore does not take: of another version or
// kind of driver. The caller reads the log from its start instead.
var ErrSnapshot = errors.New("snapshot is not one this driver takes")

type snapshot struct {
	Version int         `json:"version"`
	Kind    Kind        `json:"kind"`
	Base    baseSnap    `json:"base"`
	Claude  *claudeSnap `json:"claude,omitempty"`
	Codex   *codexSnap  `json:"codex,omitempty"`
}

// Snapshot is d's state, for Restore. d must have seen only its log.
func Snapshot(d Driver) ([]byte, error) {
	s := snapshot{Version: snapVersion}
	switch d := d.(type) {
	case *claude:
		s.Kind, s.Base, s.Claude = Claude, d.base.snap(), d.snap()
	case *codex:
		s.Kind, s.Base, s.Codex = Codex, d.base.snap(), d.snap()
	default:
		return nil, fmt.Errorf("driver %T has no snapshot", d)
	}
	return json.Marshal(s)
}

// Restore puts a snapshot onto d, a driver just made by New (and given
// ForkFrom and OnHost as the agent needs) that has seen nothing yet. It is
// then as a fresh driver fed the log up to the snapshot's record would be.
func Restore(d Driver, data []byte) error {
	var s snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshot, err)
	}
	if s.Version != snapVersion {
		return fmt.Errorf("%w: version %d, not %d", ErrSnapshot, s.Version, snapVersion)
	}
	switch d := d.(type) {
	case *claude:
		if s.Kind != Claude || s.Claude == nil {
			return fmt.Errorf("%w: a %s driver's, not claude's", ErrSnapshot, s.Kind)
		}
		d.base.restore(s.Base)
		d.restore(*s.Claude)
	case *codex:
		if s.Kind != Codex || s.Codex == nil {
			return fmt.Errorf("%w: a %s driver's, not codex's", ErrSnapshot, s.Kind)
		}
		d.base.restore(s.Base)
		d.restore(*s.Codex)
	default:
		return fmt.Errorf("driver %T has no snapshot", d)
	}
	return nil
}

type baseSnap struct {
	ForkOf      string       `json:"fork_of,omitempty"`
	Alive       bool         `json:"alive,omitempty"`
	Busy        bool         `json:"busy,omitempty"`
	Starting    bool         `json:"starting,omitempty"`
	Blocked     bool         `json:"blocked,omitempty"`
	Interrupted bool         `json:"interrupted,omitempty"`
	ErrText     string       `json:"err_text,omitempty"`
	LastStderr  string       `json:"last_stderr,omitempty"`
	Restart     bool         `json:"restart,omitempty"`
	Status      event.Status `json:"status"`
}

func (b *base) snap() baseSnap {
	return baseSnap{ForkOf: b.forkOf, Alive: b.alive, Busy: b.busy, Starting: b.starting, Blocked: b.blocked,
		Interrupted: b.interrupted, ErrText: b.errText, LastStderr: b.lastStderr, Restart: b.restart, Status: b.status}
}

func (b *base) restore(s baseSnap) {
	b.forkOf, b.alive, b.busy, b.starting, b.blocked = s.ForkOf, s.Alive, s.Busy, s.Starting, s.Blocked
	b.interrupted, b.errText, b.lastStderr, b.restart, b.status = s.Interrupted, s.ErrText, s.LastStderr, s.Restart, s.Status
}

// usageSnap keeps Limits nil or empty as it was: Usage leaves an empty
// one out, which would come back nil.
type usageSnap struct {
	Usage
	Limits []Limit `json:"limits"`
}

func snapUsage(u Usage) usageSnap { return usageSnap{Usage: u, Limits: u.Limits} }

func (s usageSnap) restore() Usage {
	u := s.Usage
	u.Limits = s.Limits
	return u
}

type promptSnap struct {
	Src int64  `json:"src"`
	At  string `json:"at,omitempty"`
	OK  bool   `json:"ok,omitempty"`
}

func snapPrompts(ps []prompt) []promptSnap {
	if ps == nil {
		return nil
	}
	out := make([]promptSnap, len(ps))
	for i, p := range ps {
		out[i] = promptSnap{Src: p.src, At: p.at, OK: p.ok}
	}
	return out
}

func restorePrompts(ps []promptSnap) []prompt {
	if ps == nil {
		return nil
	}
	out := make([]prompt, len(ps))
	for i, p := range ps {
		out[i] = prompt{src: p.Src, at: p.At, ok: p.OK}
	}
	return out
}

type claudeSnap struct {
	SessionID    string          `json:"session_id"`
	SessionUsed  bool            `json:"session_used,omitempty"`
	Gen          int             `json:"gen,omitempty"`
	PendingTurns int             `json:"pending_turns,omitempty"`
	Tasks        []Task          `json:"tasks"`
	Queue        []string        `json:"queue"`
	Pending      []clPendingSnap `json:"pending"`
	Stop         bool            `json:"stop,omitempty"`
	Stopping     bool            `json:"stopping,omitempty"`
	CtlSeq       int             `json:"ctl_seq,omitempty"`
	StreamMsg    string          `json:"stream_msg,omitempty"`
	Prompts      []promptSnap    `json:"prompts"`
	LastEntry    string          `json:"last_entry,omitempty"`
	Compacted    int64           `json:"compacted,omitempty"`
	Rewind       *clRewindSnap   `json:"rewind,omitempty"`
	Going        *clRewindSnap   `json:"going,omitempty"`
	Forking      bool            `json:"forking,omitempty"`
	Noted        bool            `json:"noted,omitempty"`
	Asked        bool            `json:"asked,omitempty"`
	Commands     []Command       `json:"commands"`
	Models       []Choice        `json:"models"`
	Terminal     []string        `json:"terminal"`
	Usage        usageSnap       `json:"usage"`
	Running      Options         `json:"running"`
	Known        bool            `json:"known,omitempty"`
	Switching    *clSwitchSnap   `json:"switching,omitempty"`
	Refused      Approvals       `json:"refused,omitempty"`
	SentMode     Approvals       `json:"sent_mode,omitempty"`
	Changed      bool            `json:"changed,omitempty"`
}

type clPendingSnap struct {
	RequestID string            `json:"request_id"`
	PublicID  string            `json:"public_id"`
	Subtype   string            `json:"subtype,omitempty"`
	Input     json.RawMessage   `json:"input,omitempty"`
	Approval  bool              `json:"approval,omitempty"`
	Decision  Decision          `json:"decision,omitempty"`
	Sent      bool              `json:"sent,omitempty"`
	Questions []event.Question  `json:"questions"`
	Answers   map[string]string `json:"answers"`
}

type clRewindSnap struct {
	At      string `json:"at,omitempty"`
	Session string `json:"session,omitempty"`
	From    int64  `json:"from"`
	Start   int64  `json:"start,omitempty"`
	Before  string `json:"before,omitempty"`
}

type clSwitchSnap struct {
	ID   string    `json:"id"`
	Mode Approvals `json:"mode"`
}

func (r *clRewind) snap() *clRewindSnap {
	if r == nil {
		return nil
	}
	return &clRewindSnap{At: r.at, Session: r.session, From: r.from, Start: r.start, Before: r.before}
}

func (s *clRewindSnap) restore() *clRewind {
	if s == nil {
		return nil
	}
	return &clRewind{at: s.At, session: s.Session, from: s.From, start: s.Start, before: s.Before}
}

func (c *claude) snap() *claudeSnap {
	s := &claudeSnap{SessionID: c.sessionID, SessionUsed: c.sessionUsed, Gen: c.gen, PendingTurns: c.pendingTurns,
		Tasks: c.tasks, Queue: c.queue, Stop: c.stop, Stopping: c.stopping, CtlSeq: c.ctlSeq, StreamMsg: c.streamMsg,
		Prompts: snapPrompts(c.prompts), LastEntry: c.lastEntry, Compacted: c.compacted, Rewind: c.rewind.snap(), Going: c.going.snap(),
		Forking: c.forking, Noted: c.noted, Asked: c.asked, Commands: c.commands, Models: c.models, Terminal: c.terminal,
		Usage: snapUsage(c.usage), Running: c.running, Known: c.known, Refused: c.refused, SentMode: c.sentMode, Changed: c.changed}
	for _, p := range c.pending {
		s.Pending = append(s.Pending, clPendingSnap{RequestID: p.requestID, PublicID: p.publicID, Subtype: p.subtype, Input: p.input,
			Approval: p.approval, Decision: p.decision, Sent: p.sent, Questions: p.questions, Answers: p.answers})
	}
	if c.switching != nil {
		s.Switching = &clSwitchSnap{ID: c.switching.id, Mode: c.switching.mode}
	}
	return s
}

func (c *claude) restore(s claudeSnap) {
	c.sessionID, c.sessionUsed, c.gen, c.pendingTurns = s.SessionID, s.SessionUsed, s.Gen, s.PendingTurns
	c.tasks, c.queue, c.stop, c.stopping, c.ctlSeq, c.streamMsg = s.Tasks, s.Queue, s.Stop, s.Stopping, s.CtlSeq, s.StreamMsg
	c.prompts, c.lastEntry, c.compacted, c.rewind, c.going = restorePrompts(s.Prompts), s.LastEntry, s.Compacted, s.Rewind.restore(), s.Going.restore()
	c.forking, c.noted, c.asked, c.commands, c.models, c.terminal = s.Forking, s.Noted, s.Asked, s.Commands, s.Models, s.Terminal
	c.usage, c.running, c.known, c.refused, c.sentMode, c.changed = s.Usage.restore(), s.Running, s.Known, s.Refused, s.SentMode, s.Changed
	c.pending = nil
	for _, p := range s.Pending {
		c.pending = append(c.pending, &clPending{requestID: p.RequestID, publicID: p.PublicID, subtype: p.Subtype, input: p.Input,
			approval: p.Approval, decision: p.Decision, sent: p.Sent, questions: p.Questions, answers: p.Answers})
	}
	c.switching = nil
	if s.Switching != nil {
		c.switching = &clSwitch{id: s.Switching.ID, mode: s.Switching.Mode}
	}
}

type codexSnap struct {
	ThreadID  string            `json:"thread_id,omitempty"`
	Gen       int               `json:"gen,omitempty"`
	Phase     cxPhase           `json:"phase"`
	NextID    int64             `json:"next_id,omitempty"`
	Calls     map[int64]string  `json:"calls"`
	TurnID    string            `json:"turn_id,omitempty"`
	Pending   []cxPendingSnap   `json:"pending"`
	Queue     []string          `json:"queue"`
	Sending   bool              `json:"sending,omitempty"`
	Defaults  *cxApproval       `json:"defaults,omitempty"`
	Reading   bool              `json:"reading,omitempty"`
	Stop      bool              `json:"stop,omitempty"`
	ItemSum   map[string]string `json:"item_sum"`
	Prompts   []promptSnap      `json:"prompts"`
	Revert    string            `json:"revert,omitempty"`
	Reverting *cxRevertSnap     `json:"reverting,omitempty"`
	Listed    bool              `json:"listed,omitempty"`
	Skills    []Command         `json:"skills"`
	Models    []Choice          `json:"models"`
	Usage     usageSnap         `json:"usage"`
}

type cxPendingSnap struct {
	RPCID    string   `json:"rpc_id"`
	Method   string   `json:"method"`
	PublicID string   `json:"public_id"`
	Approval bool     `json:"approval,omitempty"`
	Decision Decision `json:"decision,omitempty"`
	Sent     bool     `json:"sent,omitempty"`
}

type cxRevertSnap struct {
	Turn string `json:"turn"`
	From int64  `json:"from"`
	Req  int64  `json:"req"`
}

func (c *codex) snap() *codexSnap {
	s := &codexSnap{ThreadID: c.threadID, Gen: c.gen, Phase: c.phase, NextID: c.nextID, Calls: c.calls, TurnID: c.turnID,
		Queue: c.queue, Sending: c.sending, Defaults: c.defaults, Reading: c.reading, Stop: c.stop, ItemSum: c.itemSum,
		Prompts: snapPrompts(c.prompts), Revert: c.revert, Listed: c.listed, Skills: c.skills, Models: c.models, Usage: snapUsage(c.usage)}
	for _, p := range c.pending {
		s.Pending = append(s.Pending, cxPendingSnap{RPCID: p.rpcID, Method: p.method, PublicID: p.publicID, Approval: p.approval, Decision: p.decision, Sent: p.sent})
	}
	if c.reverting != nil {
		s.Reverting = &cxRevertSnap{Turn: c.reverting.turn, From: c.reverting.from, Req: c.reverting.req}
	}
	return s
}

func (c *codex) restore(s codexSnap) {
	c.threadID, c.gen, c.phase, c.nextID, c.calls, c.turnID = s.ThreadID, s.Gen, s.Phase, s.NextID, s.Calls, s.TurnID
	c.queue, c.sending, c.defaults, c.reading, c.stop, c.itemSum = s.Queue, s.Sending, s.Defaults, s.Reading, s.Stop, s.ItemSum
	c.prompts, c.revert, c.listed, c.skills, c.models, c.usage = restorePrompts(s.Prompts), s.Revert, s.Listed, s.Skills, s.Models, s.Usage.restore()
	c.pending = nil
	for _, p := range s.Pending {
		c.pending = append(c.pending, &cxPending{rpcID: p.RPCID, method: p.Method, publicID: p.PublicID, approval: p.Approval, decision: p.Decision, sent: p.Sent})
	}
	c.reverting = nil
	if s.Reverting != nil {
		c.reverting = &cxRevert{turn: s.Reverting.Turn, from: s.Reverting.From, req: s.Reverting.Req}
	}
}
