package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
)

// The isolations page: create isolations, set their environment, log the
// agent CLIs in (the accounts page), and upgrade or delete them.

type isoMode int

const (
	isoList isoMode = iota
	isoNew
	isoEnv
	isoVerify
)

type isoPage struct {
	sel  int
	mode isoMode
	form *form
	busy string
	// expose, while open, is the selected isolation's services; clis its
	// accounts page.
	expose *exposePage
	clis   *clisPage
	// report is the last verify, shown until a key is pressed.
	report *api.Verify
}

type verifyMsg struct {
	v   api.Verify
	err error
}

func (m *Model) openIsolations() tea.Cmd {
	m.isoPage = &isoPage{}
	m.view = viewIsolations
	m.sel = selIsolations
	m.setFocus(focusMain)
	return m.fetchIsolations()
}

func (p *isoPage) current(isos []api.Isolation) (api.Isolation, bool) {
	if p.sel < 0 || p.sel >= len(isos) {
		return api.Isolation{}, false
	}
	return isos[p.sel], true
}

func (m *Model) handleIsoKey(msg tea.KeyMsg) tea.Cmd {
	p, k := m.isoPage, m.keys
	if p.busy != "" {
		return nil
	}
	if p.expose != nil {
		return m.handleExposeKey(msg)
	}
	if p.clis != nil {
		return m.handleCLIsKey(msg)
	}
	if p.mode == isoVerify {
		p.mode, p.report = isoList, nil
		return nil
	}
	if p.mode != isoList {
		if k.Is(msg, "back") {
			p.mode, p.form = isoList, nil
			return nil
		}
		return m.handleIsoFormKey(msg)
	}
	iso, ok := p.current(m.isolations)
	switch {
	case k.Is(msg, "back"):
		m.closeMainPage()
	case k.Is(msg, "up"):
		p.sel = max(0, p.sel-1)
	case k.Is(msg, "down"):
		p.sel = min(len(m.isolations)-1, p.sel+1)
	case k.Is(msg, "iso_new"):
		name := textField("Name", "lowercase letters, digits, dashes", "")
		mem := textField("Memory", "4G", "")
		mem.text.SetValue("4G")
		p.mode, p.form = isoNew, &form{fields: []*field{name, mem}}
		name.focus(true)
	case ok && k.Is(msg, "iso_env"):
		key := textField("Variable", "e.g. CLAUDE_CODE_OAUTH_TOKEN", "")
		val := textField("Value", "empty removes the variable", "")
		val.text.EchoMode = textinput.EchoPassword
		p.mode, p.form = isoEnv, &form{fields: []*field{key, val}}
		key.focus(true)
	case ok && k.Is(msg, "iso_login"):
		return m.openCLIs(iso.Name)
	case ok && k.Is(msg, "iso_services"):
		p.expose = &exposePage{iso: iso.Name}
		return m.fetchExposures(iso.Name)
	case ok && k.Is(msg, "iso_verify"):
		be, name := m.be, iso.Name
		p.busy = "measuring " + name + "… (the fence, what each proxy refuses, each env from the Mac)"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			v, err := be.Verify(ctx, name)
			return verifyMsg{v: v, err: err}
		}
	case ok && k.Is(msg, "iso_upgrade"):
		be, name := m.be, iso.Name
		m.confirm = &confirmation{
			prompt: fmt.Sprintf("upgrade %s to the current image? running agents restart; sessions are kept (y/n)", name),
			onYes: func() tea.Cmd {
				p.busy = "upgrading " + name + "…"
				return m.do("upgraded "+name, true, func(ctx context.Context) error {
					_, err := be.UpgradeIsolation(ctx, name)
					return err
				})
			},
		}
	case ok && k.Is(msg, "iso_delete"):
		be, name := m.be, iso.Name
		m.confirm = &confirmation{
			prompt: fmt.Sprintf("delete isolation %s? its volumes are kept (y/n)", name),
			onYes: func() tea.Cmd {
				return m.do("deleted "+name, true, func(ctx context.Context) error {
					return be.DeleteIsolation(ctx, name, false)
				})
			},
		}
	}
	return nil
}

func (m *Model) handleIsoFormKey(msg tea.KeyMsg) tea.Cmd {
	p, k := m.isoPage, m.keys
	iso, _ := p.current(m.isolations)
	switch p.mode {
	}

	if !(k.Is(msg, "submit") || (msg.String() == "enter" && p.form.cur == len(p.form.fields)-1)) {
		if msg.String() == "enter" {
			p.form.move(1)
			return nil
		}
		cmd, _ := p.form.handle(k, msg)
		return cmd
	}
	be := m.be
	switch p.mode {
	case isoNew:
		name, mem := p.form.fields[0].value(), p.form.fields[1].value()
		if name == "" {
			m.setFlash("a name is required", true)
			return nil
		}
		p.mode, p.form, p.busy = isoList, nil, "creating "+name+"… (the first isolation builds the image; a few minutes)"
		return m.do("isolation "+name+" is up; log an agent in with "+m.keys.Label("iso_login"), true, func(ctx context.Context) error {
			_, err := be.CreateIsolation(ctx, api.CreateIsolation{Name: name, Memory: mem})
			return err
		})
	case isoEnv:
		key, val := p.form.fields[0].value(), p.form.fields[1].text.Value()
		if key == "" {
			m.setFlash("a variable name is required", true)
			return nil
		}
		set := api.SetEnv{key: nil}
		if val != "" {
			set[key] = &val
		}
		p.mode, p.form = isoList, nil
		name := iso.Name
		return m.do(key+" saved for "+name+"; idle agents restart with it", true, func(ctx context.Context) error {
			return be.SetEnv(ctx, name, set)
		})
	}
	return nil
}

func (m *Model) renderIsolations(width int) string {
	p, th, k := m.isoPage, m.th, m.keys
	var b strings.Builder
	b.WriteString(th.title.Render("Isolations") + "\n\n")
	if len(m.isolations) == 0 {
		b.WriteString(th.dim.Render("  none yet") + "\n")
	}
	for i, iso := range m.isolations {
		env := strings.Join(iso.EnvKeys, ", ")
		if env == "" {
			env = "—"
		}
		// Pad before styling: escape codes would count toward the width.
		state := fmt.Sprintf("%-10s", iso.State)
		if iso.State == "running" {
			state = th.ok.Render(state)
		} else {
			state = th.warn.Render(state)
		}
		fence := th.ok.Render("fenced")
		switch iso.FenceState {
		case "verified":
		case "off":
			fence = th.warn.Render("unfenced")
		default:
			fence = th.err.Render("fence ✗")
		}
		line := fmt.Sprintf("%-14s %s %s  %-24s env: %s", iso.Name, state, fence, iso.Image, env)
		if i == p.sel {
			b.WriteString(th.user.Render("› ") + th.title.Render(line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if iso, ok := p.current(m.isolations); ok && iso.FenceState != "verified" && iso.FenceState != "off" {
		b.WriteString(th.err.Width(width-2).PaddingLeft(2).Render("network fence: "+iso.FenceState) + "\n")
	}
	b.WriteString("\n")
	switch p.mode {
	case isoNew:
		b.WriteString(th.title.Render("New isolation") + "\n" + p.form.render(th, width))
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s create · %s cancel", k.Label("submit"), k.Label("back"))) + "\n")
	case isoEnv:
		iso, _ := p.current(m.isolations)
		b.WriteString(th.title.Render("Environment for "+iso.Name) + "\n" + p.form.render(th, width))
		b.WriteString(th.dim.Render("  agents read it when their process starts; idle ones restart to pick it up") + "\n")
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s save · %s cancel", k.Label("submit"), k.Label("back"))) + "\n")
	case isoVerify:
		b.WriteString(m.renderVerify(*p.report, width))
		b.WriteString(th.dim.Render("  any key closes · `gero iso verify "+p.report.Isolation+" --json` for the full record") + "\n")
	default:
		hints := []string{
			k.Label("iso_new") + " new", k.Label("iso_env") + " env", k.Label("iso_login") + " accounts", k.Label("iso_services") + " services",
			k.Label("iso_verify") + " verify", k.Label("iso_upgrade") + " upgrade", k.Label("iso_delete") + " delete", k.Label("back") + " back",
		}
		b.WriteString(th.dim.Render("  "+strings.Join(hints, " · ")) + "\n")
	}
	if p.busy != "" {
		b.WriteString("\n" + th.working.Render(spinner[m.frame%len(spinner)]+" "+p.busy) + "\n")
	}
	return b.String()
}

// renderVerify is a verify report: a line per check, the reason under each
// failure. FAIL is a broken guarantee, WARN something set up wrong.
func (m *Model) renderVerify(v api.Verify, width int) string {
	th := m.th
	var b strings.Builder
	b.WriteString(th.title.Render("Verify "+v.Isolation) + "\n")
	w := 0
	for _, c := range v.Checks {
		w = max(w, len([]rune(c.Check)))
	}
	w = min(w, max(width/2, 20))
	failed, broken := 0, 0
	for _, c := range v.Checks {
		mark := th.ok.Render("PASS")
		switch {
		case c.OK:
		case c.Severity == api.SevIsolation:
			mark, broken = th.err.Render("FAIL"), broken+1
		default:
			mark = th.warn.Render("WARN")
		}
		if !c.OK {
			failed++
		}
		line := fmt.Sprintf("%-*s  %s", w, truncate(c.Check, w), c.Got)
		b.WriteString("  " + mark + " " + truncate(line, max(width-9, 10)) + "\n")
		if !c.OK && c.Detail != "" {
			b.WriteString(th.dim.Width(max(width-9, 10)).PaddingLeft(7).Render(c.Detail) + "\n")
		}
	}
	switch {
	case failed == 0:
		b.WriteString(th.ok.Render(fmt.Sprintf("  %d checks passed", len(v.Checks))) + "\n")
	case broken > 0:
		b.WriteString(th.err.Render(fmt.Sprintf("  %d/%d failed (%d isolation)", failed, len(v.Checks), broken)) + "\n")
	default:
		b.WriteString(th.warn.Render(fmt.Sprintf("  %d/%d failed (setup only)", failed, len(v.Checks))) + "\n")
	}
	return b.String()
}
