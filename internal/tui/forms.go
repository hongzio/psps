package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
)

type fieldKind int

const (
	fSelect fieldKind = iota
	fText
	fArea
	fCheck // several of options, idx is the one under the cursor
)

type field struct {
	label   string
	kind    fieldKind
	options []string
	idx     int
	checked map[string]bool
	text    textinput.Model
	area    textarea.Model
	hint    string
	// A lookup checklist's options are what the service found with the
	// env's credential; notes say more about each. Until it has looked,
	// or when it could not, it is disabled and hint says why. looked is
	// the request its options answer.
	lookup   bool
	disabled bool
	notes    map[string]string
	looked   string
}

func (f *field) value() string {
	switch f.kind {
	case fSelect:
		if len(f.options) == 0 {
			return ""
		}
		return f.options[f.idx]
	case fText:
		return strings.TrimSpace(f.text.Value())
	case fCheck:
		return strings.Join(f.values(), ",")
	}
	return strings.TrimSpace(f.area.Value())
}

// values is a checklist's ticked options, in option order.
func (f *field) values() []string {
	out := []string{}
	for _, o := range f.options {
		if f.checked[o] {
			out = append(out, o)
		}
	}
	return out
}

func checkField(label string, options, checked []string, hint string) *field {
	f := &field{label: label, kind: fCheck, options: options, checked: map[string]bool{}, hint: hint}
	for _, c := range checked {
		f.checked[c] = true
	}
	return f
}

func (f *field) focus(on bool) {
	switch f.kind {
	case fText:
		if on {
			f.text.Focus()
		} else {
			f.text.Blur()
		}
	case fArea:
		if on {
			f.area.Focus()
		} else {
			f.area.Blur()
		}
	}
}

func selectField(label string, options []string, hint string) *field {
	return &field{label: label, kind: fSelect, options: options, hint: hint}
}

func textField(label, placeholder, hint string) *field {
	in := textinput.New()
	in.Placeholder = placeholder
	in.Prompt = ""
	in.Width = 40
	return &field{label: label, kind: fText, text: in, hint: hint}
}

func efforts(kind string) []string {
	if kind == string(driver.Codex) {
		return []string{"default", "low", "medium", "high", "xhigh"}
	}
	return []string{"default", "low", "medium", "high", "xhigh", "max"}
}

func indexOf(options []string, v string) int {
	for i, o := range options {
		if o == v {
			return i
		}
	}
	return 0
}

// form is a column of fields with one under the cursor.
type form struct {
	fields []*field
	cur    int
}

func (f *form) move(d int) {
	f.fields[f.cur].focus(false)
	f.cur = (f.cur + d + len(f.fields)) % len(f.fields)
	f.fields[f.cur].focus(true)
}

func (f *form) handle(k Keymap, msg tea.KeyMsg) (tea.Cmd, bool) {
	cur := f.fields[f.cur]
	switch {
	case k.Is(msg, "focus_next"):
		f.move(1)
		return nil, true
	case k.Is(msg, "focus_prev"):
		f.move(-1)
		return nil, true
	case cur.disabled:
		return nil, true
	case cur.kind == fCheck && msg.String() == "up":
		cur.idx = max(cur.idx-1, 0)
		return nil, true
	case cur.kind == fCheck && msg.String() == "down":
		cur.idx = min(cur.idx+1, max(len(cur.options)-1, 0))
		return nil, true
	case cur.kind == fSelect && k.Is(msg, "prev_option"):
		cur.idx = (cur.idx - 1 + len(cur.options)) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fSelect && (k.Is(msg, "next_option") || msg.String() == " "):
		cur.idx = (cur.idx + 1) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fCheck && k.Is(msg, "prev_option"):
		cur.idx = (cur.idx - 1 + len(cur.options)) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fCheck && k.Is(msg, "next_option"):
		cur.idx = (cur.idx + 1) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fCheck && msg.String() == " ":
		if len(cur.options) > 0 {
			o := cur.options[cur.idx]
			cur.checked[o] = !cur.checked[o]
		}
		return nil, true
	case cur.kind == fCheck:
		return nil, true
	}
	var cmd tea.Cmd
	switch cur.kind {
	case fText:
		cur.text, cmd = cur.text.Update(msg)
	case fArea:
		cur.area, cmd = cur.area.Update(msg)
	}
	return cmd, false
}

func (f *form) render(th theme, width int) string {
	var b strings.Builder
	lw := 11
	for _, fd := range f.fields {
		lw = max(lw, len(fd.label))
	}
	for i, fd := range f.fields {
		marker, label := "  ", th.dim.Render(fmt.Sprintf("%-*s", lw, fd.label))
		if i == f.cur {
			marker, label = th.user.Render("› "), th.title.Render(fmt.Sprintf("%-*s", lw, fd.label))
		}
		var val string
		switch fd.kind {
		case fSelect:
			v := fd.value()
			if v == "" {
				v = "(none)"
			}
			if i == f.cur {
				val = th.user.Render("‹ " + v + " ›")
			} else {
				val = "  " + v
			}
		case fText:
			fd.text.Width = max(width-20, 10)
			val = fd.text.View()
		case fCheck:
			if fd.disabled {
				val = th.dim.Render("  " + orDash(strings.Join(fd.values(), ", ")))
				line := marker + label + " " + val + "  " + th.warn.Render(fd.hint)
				b.WriteString(line + "\n")
				continue
			}
			if fd.lookup {
				b.WriteString(marker + label + " " + th.dim.Render(fmt.Sprintf("%d of %d ticked · ↑/↓ move · space ticks", len(fd.values()), len(fd.options))) + "\n")
				b.WriteString(fd.renderTall(th, i == f.cur, width))
				continue
			}
			if len(fd.options) == 0 {
				val = th.dim.Render("  (none to choose from)")
				break
			}
			var items []string
			for j, o := range fd.options {
				box := "[ ] "
				if fd.checked[o] {
					box = "[x] "
				}
				item := box + o
				if i == f.cur && j == fd.idx {
					item = th.user.Render("‹" + item + "›")
				} else {
					item = " " + item + " "
				}
				items = append(items, item)
			}
			val = " " + strings.Join(items, " ")
		case fArea:
			fd.area.SetWidth(max(width-6, 10))
			b.WriteString(marker + label + "\n")
			b.WriteString(indent(fd.area.View(), "    ") + "\n")
			continue
		}
		line := marker + label + " " + val
		if fd.hint != "" && i == f.cur {
			line += "  " + th.dim.Render(fd.hint)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// renderTall draws a long checklist one option a line, a window of them
// around the cursor.
func (f *field) renderTall(th theme, focused bool, width int) string {
	const rows = 8
	if len(f.options) == 0 {
		return th.dim.Render("      (nothing to choose from)") + "\n"
	}
	start := min(max(f.idx-rows/2, 0), max(len(f.options)-rows, 0))
	end := min(start+rows, len(f.options))
	var b strings.Builder
	if start > 0 {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↑ %d more", start)) + "\n")
	}
	for j := start; j < end; j++ {
		o := f.options[j]
		box := "[ ] "
		if f.checked[o] {
			box = "[x] "
		}
		line := box + o
		if n := f.notes[o]; n != "" {
			line += "  " + th.dim.Render(n)
		}
		line = truncate(line, max(width-8, 10))
		if focused && j == f.idx {
			b.WriteString("    " + th.user.Render("› ") + line + "\n")
		} else {
			b.WriteString("      " + line + "\n")
		}
	}
	if end < len(f.options) {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↓ %d more", len(f.options)-end)) + "\n")
	}
	return b.String()
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// --- New agent

type agentForm struct {
	form
	err  string
	busy bool
}

const (
	nfIso = iota
	nfKind
	nfRepo
	nfEffort
	nfModel
	nfApprovals
	nfPrompt
)

func newAgentForm(isos []api.Isolation) *agentForm {
	prompt := textarea.New()
	prompt.Placeholder = "What should the agent do?"
	prompt.ShowLineNumbers = false
	prompt.SetHeight(6)
	prompt.CharLimit = 0
	f := &agentForm{form: form{fields: []*field{
		selectField("Isolation", nil, "required"),
		selectField("Agent", []string{string(driver.Claude), string(driver.Codex)}, ""),
		textField("Repository", "optional git URL to clone into the workspace", ""),
		selectField("Effort", efforts(string(driver.Claude)), ""),
		textField("Model", "optional; the CLI's default otherwise", ""),
		selectField("Approvals", approvalModes(),
			"never: runs unattended inside the isolation · auto: unattended, a reviewer model refuses risky actions · ask: the agent asks first for what its own rules flag · default: the CLI's own settings decide"),
		{label: "Prompt", kind: fArea, area: prompt},
	}}}
	// A new agent starts on the CLI's own settings.
	f.fields[nfApprovals].idx = indexOf(f.fields[nfApprovals].options, string(driver.ApprovalsDefault))
	f.setIsolations(isos)
	f.cur = nfPrompt
	f.fields[nfPrompt].focus(true)
	return f
}

func (f *agentForm) setIsolations(isos []api.Isolation) {
	fd := f.fields[nfIso]
	cur := fd.value()
	fd.options = nil
	for _, iso := range isos {
		fd.options = append(fd.options, iso.Name)
	}
	fd.idx = indexOf(fd.options, cur)
}

func (m *Model) openNewAgent() tea.Cmd {
	m.newForm = newAgentForm(m.isolations)
	m.view = viewNewAgent
	m.sel = selNew
	m.setFocus(focusMain)
	return m.fetchIsolations()
}

func (m *Model) closeMainPage() {
	m.view = viewAgent
	if m.svcPage != nil && m.svcPage.login != nil && m.svcPage.login.stop != nil {
		m.svcPage.login.stop()
	}
	m.newForm, m.isoPage, m.svcPage = nil, nil, nil
	if _, ok := m.agent(m.openID); ok {
		m.sel = m.openID
	}
	m.setFocus(focusSidebar)
}

func (m *Model) handleNewAgentKey(msg tea.KeyMsg) tea.Cmd {
	f := m.newForm
	if f.busy {
		return nil
	}
	k := m.keys
	switch {
	case k.Is(msg, "back"):
		m.closeMainPage()
		return nil
	case k.Is(msg, "submit"):
		return m.submitNewAgent()
	}
	kind := f.fields[nfKind].value()
	cmd, _ := f.handle(k, msg)
	if nk := f.fields[nfKind].value(); nk != kind {
		eff := f.fields[nfEffort]
		v := eff.value()
		eff.options = efforts(nk)
		eff.idx = indexOf(eff.options, v)
	}
	return cmd
}

func (m *Model) submitNewAgent() tea.Cmd {
	f := m.newForm
	iso, prompt := f.fields[nfIso].value(), f.fields[nfPrompt].value()
	switch {
	case iso == "":
		f.err = errNoIsolation.Error()
		return nil
	case prompt == "":
		f.err = "an initial prompt is required"
		return nil
	}
	opts := driver.Options{Model: f.fields[nfModel].value(), Approvals: driver.Approvals(f.fields[nfApprovals].value())}
	if e := f.fields[nfEffort].value(); e != "default" {
		opts.Effort = e
	}
	req := api.CreateAgent{Isolation: iso, Kind: driver.Kind(f.fields[nfKind].value()), Prompt: prompt,
		Repo: f.fields[nfRepo].value(), Options: opts}
	f.busy, f.err = true, ""
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		a, err := be.CreateAgent(ctx, req)
		return createdMsg{a, err}
	}
}

// --- Agent settings: the panel behind the button beside the input

type settingsForm struct {
	form
	agent string
}

func (m *Model) openSettings() tea.Cmd {
	a, ok := m.agent(m.openID)
	if !ok || a.ArchivedAt != nil {
		m.setFlash("open a live agent first", false)
		return nil
	}
	model := textField("Model", "the CLI's default", "")
	model.text.SetValue(a.Options.Model)
	eff := selectField("Effort", efforts(string(a.Kind)), "")
	eff.idx = indexOf(eff.options, a.Options.Effort)
	appr := selectField("Approvals", approvalModes(), "")
	appr.idx = indexOf(appr.options, string(a.Options.Approvals))
	if a.Options.Approvals == "" {
		appr.idx = indexOf(appr.options, string(driver.ApprovalsNever)) // what "" has always meant
	}
	m.settings = &settingsForm{form: form{fields: []*field{eff, model, appr}}, agent: a.ID}
	m.setFocus(focusMain)
	return nil
}

func (m *Model) handleSettingsKey(msg tea.KeyMsg) tea.Cmd {
	k := m.keys
	switch {
	case k.Is(msg, "back"), k.Is(msg, "agent_settings"):
		m.settings = nil
		m.setFocus(focusInput)
		return nil
	case k.Is(msg, "submit"), msg.String() == "enter":
		s := m.settings
		o := driver.Options{Model: s.fields[1].value(), Approvals: driver.Approvals(s.fields[2].value())}
		if e := s.fields[0].value(); e != "default" {
			o.Effort = e
		}
		m.settings = nil
		m.setFocus(focusInput)
		be, id := m.be, s.agent
		note := "settings saved; they apply from the next turn"
		if a, ok := m.agent(id); ok && a.Kind == driver.Claude {
			note = "settings saved; claude restarts with them when idle"
		}
		return m.do(note, true, func(ctx context.Context) error { return be.SetOptions(ctx, id, o) })
	}
	cmd, _ := m.settings.handle(k, msg)
	return cmd
}

func approvalModes() []string {
	var out []string
	for _, m := range driver.ApprovalModes {
		out = append(out, string(m))
	}
	return out
}
