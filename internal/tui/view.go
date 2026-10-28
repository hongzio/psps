package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	h := m.height - 1
	sw := m.sidebarWidth()
	mw := m.width - sw - 1
	box := func(w int) lipgloss.Style { return lipgloss.NewStyle().Width(w).MaxWidth(w).Height(h).MaxHeight(h) }
	divStyle := m.th.rule
	if m.focus == focusSidebar {
		divStyle = m.th.focusRule
	}
	div := divStyle.Render(strings.TrimSuffix(strings.Repeat("│\n", h), "\n"))
	body := lipgloss.JoinHorizontal(lipgloss.Top,
		box(sw).Render(m.renderSidebar(sw)), div, box(mw).Render(m.renderMain(mw, h)))
	return body + "\n" + m.renderStatus()
}

// truncate cuts to w display cells, keeping styles intact.
func truncate(s string, w int) string { return ansi.Truncate(s, max(w, 1), "…") }

func (m *Model) renderSidebar(w int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(" " + th.title.Render("gero") + "\n\n")
	// row writes one sidebar entry; the selected one is a solid bar, so its
	// own colors are dropped.
	row := func(sel string, lines ...string) {
		for _, l := range lines {
			l = " " + truncate(l, w-2)
			if sel == m.sel {
				st := th.selectedDim
				if m.focus == focusSidebar {
					st = th.selected
				}
				b.WriteString(st.Width(w).Render(ansi.Strip(l)) + "\n")
			} else {
				b.WriteString(lipgloss.NewStyle().Width(w).Render(l) + "\n")
			}
		}
	}
	row(selNew, "+ New agent")
	b.WriteString("\n")
	archived := 0
	for _, a := range m.agents {
		if a.ArchivedAt != nil {
			archived++
			continue
		}
		meta := fmt.Sprintf("  %s · %s · %s", a.Kind, a.Isolation, a.State.Status)
		if a.Problem != "" {
			meta = "  " + th.err.Render("! ") + truncate(a.Problem, w-6)
		}
		row(a.ID, th.statusIcon(a.State.Status, m.frame)+" "+a.Name, th.dim.Render(meta))
	}
	b.WriteString("\n")
	arrow := "▸"
	if m.showArchived {
		arrow = "▾"
	}
	row(selArchived, th.dim.Render(fmt.Sprintf("%s Archived (%d)", arrow, archived)))
	if m.showArchived {
		for _, a := range m.agents {
			if a.ArchivedAt != nil {
				row(a.ID, th.dim.Render("  "+a.Name))
			}
		}
	}
	b.WriteString("\n")
	row(selIsolations, "⚙ Isolations")
	row(selServices, "⚿ Services")
	return b.String()
}

func (m *Model) renderMain(w, h int) string {
	th := m.th
	if m.help {
		return m.renderHelp(w)
	}
	switch m.view {
	case viewNewAgent:
		if m.newForm == nil {
			break
		}
		f := m.newForm
		var b strings.Builder
		b.WriteString(th.title.Render("New agent") + "\n\n")
		b.WriteString(f.render(th, w))
		b.WriteString("\n" + th.dim.Render(fmt.Sprintf("  %s create · %s next field · ←/→ change · %s cancel",
			m.keys.Label("submit"), m.keys.Label("focus_next"), m.keys.Label("back"))) + "\n")
		switch {
		case f.busy:
			b.WriteString("\n" + th.working.Render(spinner[m.frame%len(spinner)]+" starting the agent…") + "\n")
		case f.err != "":
			b.WriteString("\n" + th.err.Render("  "+f.err) + "\n")
		}
		return b.String()
	case viewIsolations:
		if m.isoPage != nil && m.isoPage.expose != nil {
			return m.renderExpose(w)
		}
		if m.isoPage != nil && m.isoPage.clis != nil {
			return m.renderCLIs(w)
		}
		if m.isoPage != nil {
			return m.renderIsolations(w)
		}
	case viewServices:
		if m.svcPage != nil {
			return m.renderServices(w)
		}
	}
	if _, ok := m.agent(m.openID); ok {
		return m.renderAgent(w)
	}
	return m.renderLanding()
}

func (m *Model) renderLanding() string {
	th, k := m.th, m.keys
	lines := []string{
		th.title.Render("No agent selected"),
		"",
		fmt.Sprintf("  %s  new agent", th.key.Render(k.Label("new_agent"))),
		fmt.Sprintf("  %s  isolations", th.key.Render(k.Label("isolations"))),
		fmt.Sprintf("  %s  services (credential proxies)", th.key.Render(k.Label("services"))),
		fmt.Sprintf("  %s  all keys", th.key.Render(k.Label("help"))),
	}
	if len(m.isolations) == 0 {
		lines = append(lines, "", th.dim.Render("  Start by creating an isolation: agents run inside one."))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderAgent(w int) string {
	th, k := m.th, m.keys
	a, _ := m.agent(m.openID)
	c := m.convo(a.ID)

	status := th.statusIcon(a.State.Status, m.frame) + " " + string(a.State.Status)
	if a.ArchivedAt != nil {
		status = th.dim.Render("archived")
	}
	left := th.title.Render(truncate(a.Name, w/2)) + "  " + th.dim.Render(fmt.Sprintf("%s · %s · %s", a.Kind, a.Isolation, a.ID))
	gap := max(w-lipgloss.Width(left)-lipgloss.Width(status)-1, 1)
	header := left + strings.Repeat(" ", gap) + status
	ruleStyle := th.rule
	if m.focus == focusConvo {
		ruleStyle = th.focusRule
	}
	second := ruleStyle.Render(strings.Repeat("─", w))
	if a.Problem != "" {
		second = th.err.Render(truncate("⚠ "+a.Problem, w))
	}

	var parts []string
	parts = append(parts, header, second, m.vp.View())

	if pend := c.pending(); len(pend) > 0 {
		ap := pend[0]
		more := ""
		if len(pend) > 1 {
			more = fmt.Sprintf(" (+%d more)", len(pend)-1)
		}
		line1 := th.warn.Render("⚠ "+ap.Kind+more+": ") + truncate(ap.Summary, w-lipgloss.Width(ap.Kind)-12)
		line2 := th.dim.Render(fmt.Sprintf("  %s to answer", k.Label("approvals")))
		if m.approving {
			line2 = th.selected.Render(fmt.Sprintf(" %s accept · %s decline · %s decline and stop · %s later ",
				k.Label("accept"), k.Label("decline"), k.Label("cancel_turn"), k.Label("back")))
		}
		parts = append(parts, line1, line2)
	}
	if m.settings != nil {
		parts = append(parts, th.title.Render("Agent settings"), strings.TrimRight(m.settings.render(th, w), "\n"))
		parts = append(parts, th.dim.Render(fmt.Sprintf("  %s save · %s close · ←/→ change", k.Label("submit"), k.Label("back"))))
	}

	inRule := th.rule
	if m.focus == focusInput {
		inRule = th.focusRule
	}
	parts = append(parts, inRule.Render(strings.Repeat("─", w)))
	button := th.dim.Render("⚙ " + k.Label("agent_settings"))
	if m.settings != nil {
		button = th.user.Render("⚙ settings")
	}
	input := lipgloss.NewStyle().Width(w - 13).Render(m.input.View())
	parts = append(parts, lipgloss.JoinHorizontal(lipgloss.Top, input, " ", lipgloss.NewStyle().Width(12).Align(lipgloss.Right).Render(button)))
	return strings.Join(parts, "\n")
}

func (m *Model) renderStatus() string {
	th, k := m.th, m.keys
	var left string
	switch {
	case m.confirm != nil:
		left = th.warn.Render(" " + m.confirm.prompt)
	case m.flash != "" && time.Now().Before(m.flashUntil):
		if m.flashErr {
			left = th.flashErr.Render(" " + m.flash)
		} else {
			left = th.ok.Render(" " + m.flash)
		}
	default:
		var hints []string
		switch m.focus {
		case focusSidebar:
			hints = []string{"↑↓ move", k.Label("open") + " open", k.Label("new_agent") + " new", k.Label("archive") + " archive",
				k.Label("toggle_archived") + " archived", k.Label("focus_next") + " next pane"}
		case focusConvo:
			hints = []string{"↑↓ scroll", k.Label("page_up") + "/" + k.Label("page_down") + " page", k.Label("focus_next") + " next pane"}
		case focusInput:
			hints = []string{k.Label("send") + " send", k.Label("newline") + " newline", k.Label("agent_settings") + " settings",
				k.Label("interrupt") + " interrupt", k.Label("back") + " back"}
		case focusMain:
			hints = []string{k.Label("back") + " back"}
		}
		hints = append(hints, k.Label("help")+" keys")
		left = th.dim.Render(" " + strings.Join(hints, " · "))
	}
	right := th.dim.Render(fmt.Sprintf("%d agents ", len(m.agents)))
	if m.connErr != "" {
		right = th.err.Render("daemon unreachable ")
	}
	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(right), 1)
	return truncate(left, m.width-lipgloss.Width(right)) + strings.Repeat(" ", gap) + right
}

func (m *Model) renderHelp(w int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.title.Render("Keys") + th.dim.Render("   (any key closes; rebind them in ~/.config/gero/keys.toml)") + "\n")
	ctx := ""
	for _, bd := range defaultBindings {
		if bd.context != ctx {
			ctx = bd.context
			b.WriteString("\n" + th.user.Render(ctx) + "\n")
		}
		keys := strings.Join(m.keys.keys[bd.action], ", ")
		b.WriteString(fmt.Sprintf("  %-22s %s\n", th.key.Render(truncate(keys, 22)), bd.help))
	}
	return b.String()
}
