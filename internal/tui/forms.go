package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
)

type fieldKind int

const (
	fSelect fieldKind = iota
	fText
	fCheck // several of options, idx is the one under the cursor
)

type field struct {
	label   string
	kind    fieldKind
	options []string
	idx     int
	checked map[string]bool
	text    textinput.Model
	hint    string
	// A lookup checklist's options are what the service found with the
	// env's credential; notes say more about each. Until it has looked,
	// or when it could not, it is disabled and hint says why. looked is
	// the request its options answer.
	lookup   bool
	disabled bool
	notes    map[string]string
	looked   string
	// caps is what a lookup checklist's entries narrow themselves to (aws's
	// session and command); narrowed is an entry's own when it keeps fewer.
	caps     []proxy.Cap
	narrowed map[string][]string
	// filter narrows a lookup checklist to the options holding it, in the
	// name or the note; while filtering, typed keys go into it.
	filter    string
	filtering bool
	// A single lookup is a text field (a role, a profile) offering the
	// options found beneath it while focused: ↑/↓ fill the input with one,
	// pick is that one (-1: none), and typed, what was typed last, narrows
	// them. status is how the lookup went.
	single bool
	pick   int
	typed  string
	status string
}

func (f *field) value() string {
	switch f.kind {
	case fSelect:
		if len(f.options) == 0 {
			return ""
		}
		return f.options[f.idx]
	case fCheck:
		return strings.Join(f.values(), ",")
	}
	return strings.TrimSpace(f.text.Value())
}

// values is a checklist's ticked options, in option order, each with
// the capabilities it keeps when that is not all of them.
func (f *field) values() []string {
	out := []string{}
	for _, o := range f.options {
		if f.checked[o] {
			out = append(out, f.entry(o))
		}
	}
	return out
}

func (f *field) entry(o string) string {
	if n := f.narrowed[o]; len(n) > 0 && len(n) < len(f.caps) {
		return o + "=" + strings.Join(n, ",")
	}
	return o
}

// tick ticks an entry as stored or typed: NAME, or NAME=CAP,CAP narrowing
// it. A suffix naming a capability the field does not have stays part of
// the name, for the service to refuse, rather than read as all of them.
func (f *field) tick(v string) {
	name := v
	if n, suffix, ok := strings.Cut(v, "="); ok && len(f.caps) > 0 {
		want := map[string]bool{}
		known := true
		for _, c := range strings.Split(suffix, ",") {
			if c = strings.TrimSpace(c); c != "" {
				want[c] = true
				known = known && hasCap(f.caps, c)
			}
		}
		var keep []string
		for _, c := range f.caps {
			if want[c.Name] {
				keep = append(keep, c.Name)
			}
		}
		if known && len(keep) > 0 {
			name = n
			f.narrowed[name] = keep
		}
	}
	if !f.checked[name] && !slices.Contains(f.options, name) {
		f.options = append(f.options, name)
	}
	f.checked[name] = true
}

func hasCap(caps []proxy.Cap, name string) bool {
	for _, c := range caps {
		if c.Name == name {
			return true
		}
	}
	return false
}

// toggleCap narrows or widens a ticked entry by one capability; the last
// one stays, since an entry with none is an entry unticked.
func (f *field) toggleCap(o string, n int) {
	if !f.checked[o] || n >= len(f.caps) {
		return
	}
	have := f.narrowed[o]
	if len(have) == 0 {
		for _, c := range f.caps {
			have = append(have, c.Name)
		}
	}
	name := f.caps[n].Name
	var next []string
	for _, c := range f.caps {
		on := slices.Contains(have, c.Name)
		if c.Name == name {
			on = !on
		}
		if on {
			next = append(next, c.Name)
		}
	}
	if len(next) > 0 {
		f.narrowed[o] = next
	}
}

// visible is a checklist's options as its filter leaves them; idx points
// into it.
func (f *field) visible() []string {
	q := strings.ToLower(f.filter)
	if q == "" {
		return f.options
	}
	var out []string
	for _, o := range f.options {
		if strings.Contains(strings.ToLower(o+" "+f.notes[o]), q) {
			out = append(out, o)
		}
	}
	return out
}

// suggestions is what a lookup text field offers: everything found while
// what was typed is one of them, else the ones holding it.
func (f *field) suggestions() []string {
	q := strings.ToLower(strings.TrimSpace(f.typed))
	if q == "" || slices.Contains(f.options, strings.TrimSpace(f.typed)) {
		return f.options
	}
	var out []string
	for _, o := range f.options {
		if strings.Contains(strings.ToLower(o+" "+f.notes[o]), q) {
			out = append(out, o)
		}
	}
	return out
}

// lookupList is a List lookup field: what is stored stays ticked, and the
// checklist is shut until the service has looked.
func lookupList(f proxy.Field, have []string) *field {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = f.Metavar + ", …"
	fd := &field{label: f.Key, kind: fCheck, checked: map[string]bool{}, text: in, lookup: true, disabled: true,
		hint: "looking up…", notes: map[string]string{}, caps: f.Caps, narrowed: map[string][]string{}}
	for _, v := range have {
		fd.tick(v)
	}
	return fd
}

// toText turns a lookup checklist the service could not fill into the
// list typed by hand, holding what is ticked; fromText turns it back,
// ticking what was typed.
func (f *field) toText(focused bool) {
	f.text.SetValue(strings.Join(f.values(), ", "))
	f.kind, f.filter, f.filtering = fText, "", false
	if focused {
		f.text.Focus()
	}
}

func (f *field) fromText() {
	typed := f.text.Value()
	f.text.Blur()
	f.kind, f.options, f.checked, f.narrowed = fCheck, nil, map[string]bool{}, map[string][]string{}
	for _, v := range strings.Split(typed, ",") {
		if v = strings.TrimSpace(v); v != "" {
			f.tick(v)
		}
	}
}

func checkField(label string, options, checked []string, hint string) *field {
	f := &field{label: label, kind: fCheck, options: options, checked: map[string]bool{}, hint: hint}
	for _, c := range checked {
		f.checked[c] = true
	}
	return f
}

func (f *field) focus(on bool) {
	if f.kind != fText {
		return
	}
	if on {
		f.text.Focus()
	} else {
		f.text.Blur()
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
	return append([]string{"default"}, driver.Efforts(driver.Kind(kind))...)
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
	f.fields[f.cur].filtering = false
	f.cur = (f.cur + d + len(f.fields)) % len(f.fields)
	f.fields[f.cur].focus(true)
}

// capturing is whether the field under the cursor takes every key, Enter
// and Esc too: a checklist's filter being typed.
func (f *form) capturing() bool {
	cur := f.fields[f.cur]
	return cur.kind == fCheck && cur.filtering
}

func (f *form) handle(k Keymap, msg tea.KeyMsg) (tea.Cmd, bool) {
	cur := f.fields[f.cur]
	if cur.filtering {
		switch {
		case k.Is(msg, "back"):
			cur.filter, cur.filtering, cur.idx = "", false, 0
			return nil, true
		case msg.String() == "enter":
			cur.filtering = false
			return nil, true
		case msg.Type == tea.KeyBackspace:
			if r := []rune(cur.filter); len(r) > 0 {
				cur.filter, cur.idx = string(r[:len(r)-1]), 0
			}
			return nil, true
		case msg.Type == tea.KeyRunes:
			cur.filter, cur.idx = cur.filter+string(msg.Runes), 0
			return nil, true
		}
	}
	switch {
	case k.Is(msg, "focus_next"):
		f.move(1)
		return nil, true
	case k.Is(msg, "focus_prev"):
		f.move(-1)
		return nil, true
	case cur.disabled:
		return nil, true
	case cur.kind == fCheck && cur.lookup && msg.String() == "/":
		cur.filtering = true
		return nil, true
	case cur.kind == fCheck && len(cur.caps) > 0 && len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] < '1'+rune(len(cur.caps)):
		if vis := cur.visible(); cur.idx < len(vis) {
			cur.toggleCap(vis[cur.idx], int(msg.Runes[0]-'1'))
		}
		return nil, true
	case cur.kind == fCheck && msg.String() == "up":
		cur.idx = max(cur.idx-1, 0)
		return nil, true
	case cur.kind == fCheck && msg.String() == "down":
		cur.idx = min(cur.idx+1, max(len(cur.visible())-1, 0))
		return nil, true
	case cur.single && (msg.String() == "up" || msg.String() == "down") && len(cur.suggestions()) > 0:
		list := cur.suggestions()
		if msg.String() == "up" {
			cur.pick = max(cur.pick-1, 0)
		} else {
			cur.pick = min(cur.pick+1, len(list)-1)
		}
		cur.pick = min(cur.pick, len(list)-1)
		cur.text.SetValue(list[cur.pick])
		cur.text.CursorEnd()
		return nil, true
	case cur.kind == fSelect && k.Is(msg, "prev_option"):
		cur.idx = (cur.idx - 1 + len(cur.options)) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fSelect && (k.Is(msg, "next_option") || msg.String() == " "):
		cur.idx = (cur.idx + 1) % max(len(cur.options), 1)
		return nil, true
	case cur.kind == fCheck && k.Is(msg, "prev_option"):
		n := max(len(cur.visible()), 1)
		cur.idx = (cur.idx - 1 + n) % n
		return nil, true
	case cur.kind == fCheck && k.Is(msg, "next_option"):
		cur.idx = (cur.idx + 1) % max(len(cur.visible()), 1)
		return nil, true
	case cur.kind == fCheck && msg.String() == " ":
		if vis := cur.visible(); cur.idx < len(vis) {
			o := vis[cur.idx]
			cur.checked[o] = !cur.checked[o]
		}
		return nil, true
	case cur.kind == fCheck:
		return nil, true
	}
	var cmd tea.Cmd
	if cur.kind == fText {
		was := cur.text.Value()
		cur.text, cmd = cur.text.Update(msg)
		if cur.text.Value() != was {
			cur.typed, cur.pick = cur.text.Value(), -1
		}
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
			if fd.single && i == f.cur {
				line := marker + label + " " + val
				if fd.hint != "" {
					line += "  " + th.dim.Render(fd.hint)
				}
				b.WriteString(line + "\n" + fd.renderSuggestions(th, width))
				continue
			}
			if fd.lookup && !fd.single && fd.hint != "" {
				// Why a checklist is typed instead stays in sight.
				b.WriteString(marker + label + " " + val + "  " + th.warn.Render(fd.hint) + "\n")
				continue
			}
		case fCheck:
			if fd.disabled {
				val = th.dim.Render("  " + orDash(strings.Join(fd.values(), ", ")))
				line := marker + label + " " + val + "  " + th.warn.Render(fd.hint)
				b.WriteString(line + "\n")
				continue
			}
			if fd.lookup {
				keys := fmt.Sprintf("%d of %d ticked · space ticks", len(fd.values()), len(fd.options))
				if len(fd.caps) > 0 {
					var cs []string
					for j, c := range fd.caps {
						cs = append(cs, fmt.Sprintf("%d %s", j+1, c.Name))
					}
					keys += " · " + strings.Join(cs, " ") + " narrow a ticked one"
				}
				keys += " · / filters · ↑/↓ move"
				b.WriteString(marker + label + " " + th.dim.Render(truncate(keys, max(width-lw-4, 10))) + "\n")
				if fd.filtering || fd.filter != "" {
					cursor := ""
					if fd.filtering {
						cursor = "▏"
					}
					b.WriteString("      " + th.user.Render("/ "+fd.filter+cursor) + th.dim.Render(fmt.Sprintf("  %d match", len(fd.visible()))) + "\n")
				}
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
	opts := f.visible()
	if len(opts) == 0 {
		return th.dim.Render("      (nothing to choose from)") + "\n"
	}
	start := min(max(f.idx-rows/2, 0), max(len(opts)-rows, 0))
	end := min(start+rows, len(opts))
	var b strings.Builder
	if start > 0 {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↑ %d more", start)) + "\n")
	}
	for j := start; j < end; j++ {
		o := opts[j]
		box := "[ ] "
		if f.checked[o] {
			box = "[x] "
		}
		line := box + o
		if f.checked[o] && len(f.caps) > 0 {
			var on []string
			for _, c := range f.caps {
				if n := f.narrowed[o]; len(n) == 0 || slices.Contains(n, c.Name) {
					on = append(on, c.Name)
				}
			}
			line += "  " + th.user.Render(strings.Join(on, "+"))
		}
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
	if end < len(opts) {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↓ %d more", len(opts)-end)) + "\n")
	}
	return b.String()
}

// renderSuggestions draws what a focused lookup text field offers, a
// window of them around the one picked.
func (f *field) renderSuggestions(th theme, width int) string {
	const rows = 6
	list := f.suggestions()
	var b strings.Builder
	status := f.status
	if len(list) > 0 {
		status = fmt.Sprintf("%d listed · ↑/↓ picks one, or type", len(list))
	}
	if status != "" {
		b.WriteString("      " + th.dim.Render(status) + "\n")
	}
	start := min(max(f.pick-rows/2, 0), max(len(list)-rows, 0))
	end := min(start+rows, len(list))
	if start > 0 {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↑ %d more", start)) + "\n")
	}
	for j := start; j < end; j++ {
		line := list[j]
		if n := f.notes[line]; n != "" {
			line += "  " + th.dim.Render(n)
		}
		line = truncate(line, max(width-8, 10))
		if j == f.pick {
			b.WriteString("    " + th.user.Render("› ") + line + "\n")
		} else {
			b.WriteString("      " + line + "\n")
		}
	}
	if end < len(list) {
		b.WriteString(th.dim.Render(fmt.Sprintf("      ↓ %d more", len(list)-end)) + "\n")
	}
	return b.String()
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
	nfID
	nfEffort
	nfModel
	nfApprovals
	nfPrompt
)

// newAgentForm makes an agent and sends it the first message, if one is
// typed; without one it waits for it in the input it opens on.
func newAgentForm(isos []api.Isolation) *agentForm {
	f := &agentForm{form: form{fields: []*field{
		selectField("Isolation", nil, "required"),
		selectField("Agent", []string{string(driver.Claude), string(driver.Codex)}, ""),
		textField("ID", "optional: lowercase letters, digits, dashes", "unique among all agents, archived ones too; an adjective-noun like brave-otter otherwise"),
		selectField("Effort", efforts(string(driver.Claude)), ""),
		textField("Model", "optional; the CLI's default otherwise", ""),
		selectField("Approvals", approvalModes(),
			"never: runs unattended inside the isolation · auto: unattended, a reviewer model refuses risky actions · ask: the agent asks first for what its own rules flag · default: the CLI's own settings decide"),
		textField("Message", "optional; it waits for your first message otherwise", "sent as soon as it is made, and names it"),
	}}}
	// A new agent starts on the CLI's own settings.
	f.fields[nfApprovals].idx = indexOf(f.fields[nfApprovals].options, string(driver.ApprovalsDefault))
	f.setIsolations(isos)
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
	case k.Is(msg, "submit"), msg.String() == "enter" && f.cur == len(f.fields)-1:
		return m.submitNewAgent()
	case msg.String() == "enter":
		f.move(1)
		return nil
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
	iso := f.fields[nfIso].value()
	if iso == "" {
		f.err = errNoIsolation.Error()
		return nil
	}
	opts := driver.Options{Model: f.fields[nfModel].value(), Approvals: driver.Approvals(f.fields[nfApprovals].value())}
	if e := f.fields[nfEffort].value(); e != "default" {
		opts.Effort = e
	}
	req := api.CreateAgent{Isolation: iso, Kind: driver.Kind(f.fields[nfKind].value()), Handle: f.fields[nfID].value(),
		Prompt: f.fields[nfPrompt].value(), Options: opts}
	f.busy, f.err = true, ""
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		a, err := be.CreateAgent(ctx, req)
		return createdMsg{a, req.Prompt != "", err}
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
