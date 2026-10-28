package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
)

// A conversation is the agent's events folded into what a person reads:
// messages, tool calls with their results, approvals and their outcome.
// Streaming deltas grow a live entry that the final message replaces.

type entryKind int

const (
	eUser entryKind = iota
	eAssistant
	eReasoning
	eTool
	eApproval
	eNotice
	eError
	eInterrupted
)

type toolState int

const (
	toolRunning toolState = iota
	toolOK
	toolFailed
)

type entry struct {
	kind entryKind
	item string
	text string
	// from says who wrote a user entry that is mail from another agent;
	// text is then what the mail says.
	from     string
	live     bool
	tool     *toolEntry
	approval *event.Approval
}

type toolEntry struct {
	name, summary, output string
	state                 toolState
}

type convo struct {
	lastID  int64
	entries []*entry
	byItem  map[string]*entry
}

func newConvo() *convo { return &convo{byItem: map[string]*entry{}} }

// maxToolOutput bounds what a tool entry keeps; the full output is in the
// store and a tool result is rarely read beyond its first lines.
const maxToolOutput = 16 << 10

func (c *convo) apply(e event.Event) {
	if e.ID != 0 {
		if e.ID <= c.lastID {
			return
		}
		c.lastID = e.ID
	}
	add := func(en *entry) *entry {
		c.entries = append(c.entries, en)
		if en.item != "" {
			c.byItem[en.item] = en
		}
		return en
	}
	switch e.Kind {
	case event.KindUser:
		en := &entry{kind: eUser, text: e.Text}
		if l, ok := mailproxy.ReadLetter(e.Text); ok {
			en.from, en.text = fmt.Sprintf("✉ Mail #%d from %s · %s", l.ID, l.From, l.Isolation), l.Body
		}
		add(en)
	case event.KindDelta:
		en := c.byItem[e.Item]
		if en == nil || en.kind != eAssistant || !en.live {
			en = add(&entry{kind: eAssistant, item: e.Item, live: true})
		}
		en.text += e.Text
	case event.KindAssistant:
		if en := c.byItem[e.Item]; en != nil && en.kind == eAssistant && en.live {
			en.text, en.live = e.Text, false
			return
		}
		add(&entry{kind: eAssistant, item: e.Item, text: e.Text})
	case event.KindReasoning:
		add(&entry{kind: eReasoning, text: e.Text})
	case event.KindToolStart:
		t := &toolEntry{state: toolRunning}
		if e.Tool != nil {
			t.name, t.summary = e.Tool.Name, e.Tool.Summary
		}
		add(&entry{kind: eTool, item: e.Item, tool: t})
	case event.KindToolOutput:
		if en := c.byItem[e.Item]; en != nil && en.tool != nil && len(en.tool.output) < maxToolOutput {
			en.tool.output += e.Text
		}
	case event.KindToolEnd:
		en := c.byItem[e.Item]
		if en == nil || en.tool == nil {
			en = add(&entry{kind: eTool, item: e.Item, tool: &toolEntry{}})
		}
		t := en.tool
		if e.Tool != nil {
			if e.Tool.Name != "" {
				t.name = e.Tool.Name
			}
			if e.Tool.Summary != "" {
				t.summary = e.Tool.Summary
			}
			t.output = e.Tool.Output
			if len(t.output) > maxToolOutput {
				t.output = t.output[:maxToolOutput]
			}
			t.state = toolOK
			if e.Tool.IsError {
				t.state = toolFailed
			}
		}
	case event.KindApproval:
		a := *e.Approval
		add(&entry{kind: eApproval, approval: &a})
	case event.KindApprovalDone:
		for _, en := range c.entries {
			if en.kind == eApproval && en.approval.ID == e.Approval.ID {
				en.approval.Decision = e.Approval.Decision
			}
		}
	case event.KindTurnEnd:
		if e.Text == "interrupted" {
			add(&entry{kind: eInterrupted})
		}
		// A turn that ended mid-stream leaves no live entry behind.
		for _, en := range c.entries {
			en.live = false
		}
	case event.KindStatus:
		// Drivers report blocked right after every approval and leave it only
		// when none is open, so any other status means nothing is pending —
		// including approvals whose process died before they were answered.
		if e.Status != event.StatusBlocked {
			c.voidApprovals()
		}
	case event.KindError:
		add(&entry{kind: eError, text: e.Text})
	case event.KindNotice:
		// Thread bookkeeping is for logs, not for the person reading.
		if strings.HasPrefix(e.Text, "thread/") {
			return
		}
		add(&entry{kind: eNotice, text: e.Text})
	}
}

// pending returns approvals still waiting for a decision, oldest first.
func (c *convo) pending() []*event.Approval {
	var out []*event.Approval
	for _, en := range c.entries {
		if en.kind == eApproval && en.approval.Decision == "" {
			out = append(out, en.approval)
		}
	}
	return out
}

// voidApprovals clears approvals a dead process can no longer receive.
func (c *convo) voidApprovals() {
	for _, a := range c.pending() {
		a.Decision = "void"
	}
}

func (c *convo) render(width int, agentLabel string, th theme) string {
	if width < 10 {
		width = 10
	}
	body := lipgloss.NewStyle().Width(width - 2).PaddingLeft(2)
	var b strings.Builder
	prev := entryKind(-1)
	for _, en := range c.entries {
		// A blank line whenever the speaker changes; tool calls and notices
		// stay tight against what they belong to.
		if prev != -1 && (en.kind == eUser || en.kind == eAssistant || prev == eUser) {
			b.WriteString("\n")
		}
		prev = en.kind
		switch en.kind {
		case eUser:
			who := "You"
			if en.from != "" {
				who = en.from
			}
			b.WriteString(th.user.Render(who) + "\n")
			b.WriteString(body.Render(en.text) + "\n")
		case eAssistant:
			b.WriteString(th.agent.Render(agentLabel) + "\n")
			text := en.text
			if en.live {
				text += "▍"
			}
			b.WriteString(body.Render(text) + "\n")
		case eReasoning:
			b.WriteString(th.dim.Italic(true).Width(width-2).PaddingLeft(2).Render(clip(en.text, 4)) + "\n")
		case eTool:
			t := en.tool
			mark := th.working.Render("…")
			switch t.state {
			case toolOK:
				mark = th.ok.Render("✓")
			case toolFailed:
				mark = th.err.Render("✗")
			}
			head := fmt.Sprintf("  %s %s %s", mark, th.tool.Render(t.name), t.summary)
			b.WriteString(lipgloss.NewStyle().Width(width).MaxHeight(2).Render(head) + "\n")
			if out := strings.TrimRight(t.output, "\n"); out != "" {
				b.WriteString(th.dim.Width(width-4).PaddingLeft(4).Render(clip(out, 6)) + "\n")
			}
		case eApproval:
			a := en.approval
			line := th.warn.Render("⚠ approval") + "  " + a.Summary
			switch a.Decision {
			case "":
			case "accept":
				line += "  " + th.ok.Render("✓ accepted")
			case "void":
				line += "  " + th.dim.Render("(the process ended)")
			case "cancel":
				line += "  " + th.err.Render("✗ declined, turn stopped")
			default:
				line += "  " + th.err.Render("✗ declined")
			}
			b.WriteString(lipgloss.NewStyle().Width(width).PaddingLeft(2).Render(line) + "\n")
			if a.Reason != "" && a.Decision == "" {
				b.WriteString(th.dim.Width(width-4).PaddingLeft(4).Render(a.Reason) + "\n")
			}
		case eNotice:
			b.WriteString(th.dim.Width(width-2).PaddingLeft(2).Render("· "+en.text) + "\n")
		case eError:
			b.WriteString(th.err.Width(width-2).PaddingLeft(2).Render("✗ "+en.text) + "\n")
		case eInterrupted:
			b.WriteString(th.warn.Width(width-2).PaddingLeft(2).Render("⏸ this turn was interrupted; send a message to continue") + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// clip keeps the first n lines and says how many it dropped.
func clip(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-n)
}
