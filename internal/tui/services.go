package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/client"
	"github.com/hongzio/gero/internal/proxy"
)

// The services page: the credential proxies' environments, defined once on
// the Mac and exposed per isolation (from the isolations page). Envs are
// added and edited through forms drawn from each service's field list; a
// secret is typed into a masked form, or, for a service that logs in through
// the browser, the login runs here.

type svcMode int

const (
	svcList svcMode = iota
	svcEnvForm
	svcSecret
	svcLogin
	svcOutput
)

type svcRow struct {
	svc api.Service
	env *api.Env
}

type svcPage struct {
	services []api.Service
	envs     []api.Env
	loaded   bool
	sel      int
	mode     svcMode
	form     *form
	formSvc  api.Service
	formEnv  string // the env being edited; "" while adding
	// formHead is how many fields come before the service's own (the name
	// while adding, then formSecrets secret fields).
	formHead, formSecrets int
	output                []string
	outErr                bool
	login                 *loginState
	busy                  string
}

type loginState struct {
	svc, env string
	l        api.Login
	paste    *form
	stop     func()
	caught   <-chan string
}

type (
	servicesMsg struct {
		services []api.Service
		envs     []api.Env
		err      error
	}
	checkMsg struct {
		title string
		out   string
		err   error
	}
	loginStartedMsg struct {
		st  *loginState
		err error
	}
	choicesMsg struct {
		form    *form
		field   *field
		looked  string
		choices []proxy.Choice
		err     error
	}
	loginCaughtMsg struct {
		st    *loginState
		query string
	}
)

func (m *Model) openServices() tea.Cmd {
	m.svcPage = &svcPage{}
	m.view = viewServices
	m.sel = selServices
	m.setFocus(focusMain)
	return m.fetchServices()
}

func (m *Model) fetchServices() tea.Cmd {
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		svcs, err := be.Services(ctx)
		if err != nil {
			return servicesMsg{err: err}
		}
		envs, err := be.Envs(ctx, "")
		return servicesMsg{svcs, envs, err}
	}
}

func (p *svcPage) rows() []svcRow {
	var out []svcRow
	for _, s := range p.services {
		out = append(out, svcRow{svc: s})
		for i := range p.envs {
			if p.envs[i].Service == s.Name {
				out = append(out, svcRow{svc: s, env: &p.envs[i]})
			}
		}
	}
	return out
}

func (p *svcPage) current() (svcRow, bool) {
	rows := p.rows()
	if p.sel < 0 || p.sel >= len(rows) {
		return svcRow{}, false
	}
	return rows[p.sel], true
}

func (m *Model) handleSvcKey(msg tea.KeyMsg) tea.Cmd {
	p, k := m.svcPage, m.keys
	if p.busy != "" {
		return nil
	}
	if p.mode != svcList {
		if p.form != nil && p.form.capturing() {
			cmd, _ := p.form.handle(k, msg)
			return cmd
		}
		if k.Is(msg, "back") {
			m.closeSvcMode()
			return nil
		}
		return m.handleSvcFormKey(msg)
	}
	row, ok := p.current()
	switch {
	case k.Is(msg, "back"):
		m.closeMainPage()
	case k.Is(msg, "up"):
		p.sel = max(0, p.sel-1)
	case k.Is(msg, "down"):
		p.sel = min(len(p.rows())-1, p.sel+1)
	case ok && k.Is(msg, "svc_new") && len(row.svc.Fields) == 0:
		m.setFlash(row.svc.Name+" has nothing to define: turn it on for an isolation from the isolations page", false)
	case ok && k.Is(msg, "svc_new"):
		return m.openEnvForm(row.svc, nil)
	case ok && row.env != nil && (k.Is(msg, "svc_edit") || k.Is(msg, "open")):
		return m.openEnvForm(row.svc, row.env)
	case ok && row.env != nil && k.Is(msg, "svc_secret"):
		return m.openSecret(row.svc, row.env.Name)
	case ok && row.env != nil && k.Is(msg, "svc_check"):
		be, svc, name := m.be, row.svc.Name, row.env.Name
		p.busy = "checking " + svc + "/" + name + "…"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			res, err := be.CheckEnv(ctx, svc, name)
			if err == nil && res.Error != "" {
				err = fmt.Errorf("%s", res.Error)
			}
			return checkMsg{title: svc + "/" + name, out: res.Output, err: err}
		}
	case ok && row.env != nil && k.Is(msg, "svc_delete"):
		be, svc, name, exposed := m.be, row.svc.Name, row.env.Name, row.env.ExposedTo
		prompt := fmt.Sprintf("remove %s env %s? (y/n)", svc, name)
		if len(exposed) > 0 {
			prompt = fmt.Sprintf("remove %s env %s? %s stop serving it (y/n)", svc, name, strings.Join(exposed, ", "))
		}
		m.confirm = &confirmation{prompt: prompt, onYes: func() tea.Cmd {
			return m.do("removed "+svc+"/"+name+"; its secret, if any, stays until `gero env secret "+svc+" "+name+" --clear`", true,
				func(ctx context.Context) error { return be.DeleteEnv(ctx, svc, name, len(exposed) > 0) })
		}}
	}
	return nil
}

func (m *Model) closeSvcMode() {
	p := m.svcPage
	if p.login != nil && p.login.stop != nil {
		p.login.stop()
	}
	p.mode, p.form, p.login, p.output = svcList, nil, nil, nil
}

// asksSecretFirst is whether the service looks some field's choices up
// with the env's secret, which its env form then asks for first.
func asksSecretFirst(svc api.Service) bool {
	for _, f := range svc.Fields {
		if f.Lookup {
			return len(svc.Secrets) > 0
		}
	}
	return false
}

func (m *Model) openEnvForm(svc api.Service, env *api.Env) tea.Cmd {
	p := m.svcPage
	cur := map[string]any{}
	var fields []*field
	p.formHead = 0
	if env == nil {
		name := textField("name", "letters, digits, - and _", "the env's name: what isolations expose")
		fields = append(fields, name)
		p.formHead++
	} else {
		cur = decodeMap(env.Config)
	}
	p.formSecrets = 0
	if asksSecretFirst(svc) {
		for _, s := range svc.Secrets {
			hint := "stored on the Mac only, never shown again"
			if env != nil && len(env.MissingSecrets) == 0 && !env.CredentialElsewhere {
				hint = "stored; type a new one to replace it"
			}
			fields = append(fields, secretField(s, hint))
			p.formSecrets++
		}
		p.formHead += p.formSecrets
	}
	fields = append(fields, genFields(svc.Fields, cur, env == nil)...)
	for i, f := range svc.Fields {
		switch {
		case f.Lookup && f.Kind == proxy.List:
			fields[p.formHead+i] = lookupList(f, proxy.Strings(cur, f.Key))
		case f.Lookup && f.Kind == proxy.String && len(f.Choices) == 0:
			fd := fields[p.formHead+i]
			fd.lookup, fd.single, fd.pick, fd.typed = true, true, -1, fd.text.Value()
		}
	}
	p.mode, p.form, p.formSvc = svcEnvForm, &form{fields: fields}, svc
	p.formEnv = ""
	if env != nil {
		p.formEnv = env.Name
	}
	p.form.fields[0].focus(true)
	return m.lookUp()
}

// lookUp asks the service for each lookup field's choices with what the
// form has now, when that differs from what the field last asked with.
func (m *Model) lookUp() tea.Cmd {
	p := m.svcPage
	if p == nil || p.form == nil || p.mode != svcEnvForm {
		return nil
	}
	svc, be, form := p.formSvc, m.be, p.form
	secret := map[string]string{}
	for i, s := range svc.Secrets[:p.formSecrets] {
		if v := form.fields[p.formHead-p.formSecrets+i].text.Value(); strings.TrimSpace(v) != "" {
			secret[s] = strings.TrimSpace(v)
		}
	}
	var cmds []tea.Cmd
	for i, f := range svc.Fields {
		fd := form.fields[p.formHead+i]
		if !fd.lookup {
			continue
		}
		set := map[string]any{}
		for _, key := range f.LookupWith {
			for j, g := range svc.Fields {
				if g.Key == key && g.Kind == proxy.String {
					if v := form.fields[p.formHead+j].value(); v != "" && v != optDefault {
						set[key] = v
					}
				}
			}
		}
		req := api.ChoicesRequest{Env: p.formEnv, Set: set, Secret: secret}
		b, _ := json.Marshal(req)
		if string(b) == fd.looked {
			continue
		}
		fd.looked = string(b)
		if fd.single {
			fd.status = "looking up…"
		} else {
			fd.disabled, fd.hint = true, "looking up…"
		}
		key, looked := f.Key, string(b)
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			cs, err := be.Choices(ctx, svc.Name, key, req)
			return choicesMsg{form: form, field: fd, looked: looked, choices: cs, err: err}
		})
	}
	return tea.Batch(cmds...)
}

// onChoices fills a lookup field with what the service found, keeping
// every value already ticked. A checklist the service could not fill is
// typed by hand instead, until it can.
func (m *Model) onChoices(msg choicesMsg) {
	p := m.svcPage
	if p == nil || p.form != msg.form || msg.field.looked != msg.looked {
		return
	}
	fd := msg.field
	focused := p.form.fields[p.form.cur] == fd
	if fd.single {
		fd.options, fd.notes, fd.pick, fd.status = nil, map[string]string{}, -1, ""
		if msg.err != nil {
			fd.status = msg.err.Error()
			return
		}
		for _, c := range msg.choices {
			fd.options = append(fd.options, c.Value)
			fd.notes[c.Value] = c.Note
		}
		if fd.options == nil {
			fd.status = "nothing to list; type one"
		}
		return
	}
	if msg.err != nil {
		if fd.kind == fCheck {
			fd.toText(focused)
		}
		fd.disabled, fd.hint = false, msg.err.Error()+" · typed instead, comma-separated"
		return
	}
	if fd.kind == fText {
		fd.fromText()
	}
	var ticked []string
	for _, o := range fd.options {
		if fd.checked[o] {
			ticked = append(ticked, o)
		}
	}
	fd.options, fd.notes = nil, map[string]string{}
	listed := map[string]bool{}
	for _, c := range msg.choices {
		listed[c.Value] = true
	}
	for _, v := range ticked {
		if !listed[v] {
			fd.options = append(fd.options, v)
			fd.notes[v] = "not in the listing"
		}
	}
	for _, c := range msg.choices {
		fd.options = append(fd.options, c.Value)
		fd.notes[c.Value] = c.Note
	}
	fd.disabled, fd.hint, fd.idx, fd.filter = false, "", 0, ""
}

func (m *Model) openSecret(svc api.Service, env string) tea.Cmd {
	p := m.svcPage
	if svc.Login {
		be := m.be
		p.busy = "starting the " + svc.Name + " login…"
		return func() tea.Msg {
			st := &loginState{svc: svc.Name, env: env}
			redirect := ""
			if r, caught, stop, err := client.CatchRedirect(); err == nil {
				redirect, st.caught, st.stop = r, caught, stop
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			l, err := be.StartLogin(ctx, svc.Name, env, redirect)
			if err != nil && st.stop != nil {
				st.stop()
			}
			st.l = l
			return loginStartedMsg{st, err}
		}
	}
	if len(svc.Secrets) == 0 {
		m.setFlash(svc.Name+" keeps no secret in the credentials file", false)
		return nil
	}
	var fields []*field
	for _, s := range svc.Secrets {
		fd := textField(s, "", "stored on the Mac only, never shown again")
		fd.text.EchoMode = textinput.EchoPassword
		fields = append(fields, fd)
	}
	p.mode, p.form, p.formSvc, p.formEnv = svcSecret, &form{fields: fields}, svc, env
	fields[0].focus(true)
	return nil
}

// waitCaught waits for a browser on this Mac to land on the login's
// loopback address.
func waitCaught(st *loginState) tea.Cmd {
	if st.caught == nil {
		return nil
	}
	return func() tea.Msg {
		q, ok := <-st.caught
		if !ok {
			return nil
		}
		return loginCaughtMsg{st, q}
	}
}

func (m *Model) finishLogin(st *loginState, callback string) tea.Cmd {
	p := m.svcPage
	be := m.be
	p.busy = "finishing the login…"
	return m.do("logged in; ["+st.svc+" "+st.env+"] is stored", true, func(ctx context.Context) error {
		if st.stop != nil {
			defer st.stop()
		}
		return be.FinishLogin(ctx, st.svc, st.env, st.l.ID, callback)
	})
}

func (m *Model) handleSvcFormKey(msg tea.KeyMsg) tea.Cmd {
	p, k := m.svcPage, m.keys
	switch p.mode {
	case svcOutput:
		m.closeSvcMode()
		return nil
	case svcLogin:
		if k.Is(msg, "copy") && !p.login.pasteFocused() {
			m.copyToClipboard(p.login.l.URL)
			m.setFlash("the login address is on your clipboard (if the terminal allows OSC 52)", false)
			return nil
		}
		if k.Is(msg, "submit") || msg.String() == "enter" {
			if v := p.login.paste.fields[0].value(); v != "" {
				st := p.login
				p.mode, p.login = svcList, nil
				return m.finishLogin(st, v)
			}
			return nil
		}
		cmd, _ := p.login.paste.handle(k, msg)
		return cmd
	}
	if !(k.Is(msg, "submit") || (msg.String() == "enter" && p.form.cur == len(p.form.fields)-1)) {
		was := p.form.cur
		if msg.String() == "enter" {
			p.form.move(1)
		} else if cmd, _ := p.form.handle(k, msg); cmd != nil {
			return cmd
		}
		// Leaving a field may have changed what a lookup should ask with.
		if p.form.cur != was {
			return m.lookUp()
		}
		return nil
	}
	be, svc := m.be, p.formSvc
	switch p.mode {
	case svcEnvForm:
		fields, name := p.form.fields, p.formEnv
		var cur map[string]any
		secret := map[string]string{}
		for i, s := range svc.Secrets[:p.formSecrets] {
			if v := strings.TrimSpace(fields[p.formHead-p.formSecrets+i].text.Value()); v != "" {
				secret[s] = v
			}
		}
		if name == "" {
			name = fields[0].value()
			if name == "" {
				m.setFlash("a name is required", true)
				return nil
			}
			cur = map[string]any{}
		}
		fields = fields[p.formHead:]
		var exposed []string
		if p.formEnv != "" {
			for _, e := range p.envs {
				if e.Service == svc.Name && e.Name == name {
					cur, exposed = decodeMap(e.Config), e.ExposedTo
				}
			}
		}
		patch, err := patchFrom(svc.Fields, fields, cur)
		if err != nil {
			m.setFlash(err.Error(), true)
			return nil
		}
		adding := p.formEnv == ""
		p.mode, p.form = svcList, nil
		save := func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			var e api.Env
			var err error
			if adding {
				e, err = be.AddEnv(ctx, svc.Name, name, patch)
			} else {
				e, err = be.EditEnv(ctx, svc.Name, name, patch)
			}
			if err == nil && len(secret) > 0 {
				if err = be.SetSecret(ctx, svc.Name, name, secret); err == nil {
					e.MissingSecrets = nil
				}
			}
			flash := fmt.Sprintf("%s env %s saved", svc.Name, name)
			if err == nil && adding && (len(e.MissingSecrets) > 0 || svc.Login) {
				flash += "; " + m.keys.Label("svc_secret") + " stores its secret"
			}
			if err == nil && len(e.Warnings) > 0 {
				flash += " — " + strings.Join(e.Warnings, "; ")
			}
			return doneMsg{flash: flash, err: err, refresh: true}
		}
		if svc.Replaces && len(exposed) > 0 {
			m.confirm = &confirmation{
				prompt: fmt.Sprintf("save it and restart %s? a changed folder is mounted only as the VM starts; running agents end (y/n)", strings.Join(exposed, ", ")),
				onYes:  func() tea.Cmd { return save },
			}
			return nil
		}
		return save
	case svcSecret:
		values := map[string]string{}
		for i, s := range svc.Secrets {
			values[s] = p.form.fields[i].text.Value()
		}
		name := p.formEnv
		p.mode, p.form = svcList, nil
		return m.do("stored ["+svc.Name+" "+name+"]; proxies read it on their next request", true, func(ctx context.Context) error {
			return be.SetSecret(ctx, svc.Name, name, values)
		})
	}
	return nil
}

func (l *loginState) pasteFocused() bool {
	return l != nil && l.paste != nil && l.paste.fields[0].text.Focused() && l.paste.fields[0].text.Value() != ""
}

// copyToClipboard asks the terminal to put s on the clipboard (OSC 52),
// which reaches the operator's own machine even over SSH.
func (m *Model) copyToClipboard(s string) {
	if m.clipboard != nil {
		m.clipboard(s)
		return
	}
	os.Stdout.WriteString(ansi.SetSystemClipboard(s))
}

func (m *Model) renderServices(width int) string {
	p, th, k := m.svcPage, m.th, m.keys
	var b strings.Builder
	b.WriteString(th.title.Render("Services") + th.dim.Render("   credentials stay on the Mac; isolations get proxies") + "\n\n")
	if !p.loaded {
		b.WriteString(th.dim.Render("  loading…") + "\n")
	}
	for i, r := range p.rows() {
		var line string
		if r.env == nil {
			n := 0
			for _, e := range p.envs {
				if e.Service == r.svc.Name {
					n++
				}
			}
			line = th.title.Render(fmt.Sprintf("%-11s", r.svc.Name)) + th.dim.Render(fmt.Sprintf(" %d env(s) · %s", n, r.svc.Summary))
		} else {
			e := r.env
			secret := "-"
			if len(r.svc.Secrets) > 0 {
				switch {
				case e.SecretError != "":
					secret = th.err.Render("unreadable")
				case len(e.MissingSecrets) > 0 && r.svc.Login:
					secret = th.warn.Render("not logged in")
				case len(e.MissingSecrets) > 0:
					secret = th.warn.Render("no secret")
				default:
					secret = th.ok.Render("secret set")
				}
			}
			exposed := strings.Join(e.ExposedTo, ",")
			if exposed == "" {
				exposed = "-"
			}
			line = fmt.Sprintf("  %-12s %s  → %s  %s", e.Name, secret, exposed, th.dim.Render(compact(e.Config)))
		}
		line = truncate(line, width-3)
		if i == p.sel && p.mode == svcList {
			b.WriteString(th.user.Render("› ") + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	switch p.mode {
	case svcEnvForm:
		title := "New " + p.formSvc.Name + " env"
		if p.formEnv != "" {
			title = "Edit " + p.formSvc.Name + "/" + p.formEnv
		}
		b.WriteString(th.title.Render(title) + "\n" + p.form.render(th, width))
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s save · %s next · ←/→ change · space tick · %s cancel",
			k.Label("submit"), k.Label("focus_next"), k.Label("back"))) + "\n")
	case svcSecret:
		b.WriteString(th.title.Render("Secret for "+p.formSvc.Name+"/"+p.formEnv) + "\n" + p.form.render(th, width))
		b.WriteString(th.dim.Render(fmt.Sprintf("  written to the Mac's credentials file (0600); %s save · %s cancel", k.Label("submit"), k.Label("back"))) + "\n")
	case svcLogin:
		l := p.login
		b.WriteString(th.title.Render("Log in to "+l.svc+" for "+l.env) + "\n\n")
		b.WriteString("  Open this address in a browser and approve the access:\n\n")
		b.WriteString(l.l.URL + "\n\n")
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s copies it to your clipboard. A browser on this Mac comes back here by itself;", k.Label("copy"))) + "\n")
		b.WriteString(th.dim.Render("  on any other device the page it lands on will not load: paste that page's address below.") + "\n")
		b.WriteString(l.paste.render(th, width))
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s finish · %s cancel", k.Label("submit"), k.Label("back"))) + "\n")
	case svcOutput:
		st := th.title
		if p.outErr {
			st = th.err
		}
		for i, l := range p.output {
			if i == 0 {
				b.WriteString(st.Render(l) + "\n")
				continue
			}
			b.WriteString("  " + l + "\n")
		}
		b.WriteString(th.dim.Render("  any key closes") + "\n")
	default:
		hints := []string{k.Label("svc_new") + " new env", k.Label("svc_edit") + " edit", k.Label("svc_secret") + " secret / login",
			k.Label("svc_check") + " check", k.Label("svc_delete") + " remove", k.Label("back") + " back"}
		b.WriteString(th.dim.Render("  "+strings.Join(hints, " · ")) + "\n")
		b.WriteString(th.dim.Render("  expose an env to an isolation from the isolations page ("+k.Label("isolations")+", then "+k.Label("iso_services")+")") + "\n")
	}
	if p.busy != "" {
		b.WriteString("\n" + th.working.Render(spinner[m.frame%len(spinner)]+" "+p.busy) + "\n")
	}
	return b.String()
}

// --- exposures, from the isolations page ---

type exposePage struct {
	iso       string
	exposures []api.Exposure
	services  []api.Service
	envs      []api.Env
	loaded    bool
	sel       int
	form      *form
	formSvc   api.Service
	// formSettings are the form's settings fields: the service's, an
	// isolations field offering the other isolations.
	formSettings []proxy.Field
	// formEnvs: the form starts with envs and a default. A service with
	// nothing to define (mail) has none.
	formEnvs bool
}

type exposuresMsg struct {
	iso       string
	exposures []api.Exposure
	services  []api.Service
	envs      []api.Env
	err       error
}

func (m *Model) fetchExposures(iso string) tea.Cmd {
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		x, err := be.Exposures(ctx, iso)
		if err != nil {
			return exposuresMsg{iso: iso, err: err}
		}
		svcs, err := be.Services(ctx)
		if err != nil {
			return exposuresMsg{iso: iso, err: err}
		}
		envs, err := be.Envs(ctx, "")
		return exposuresMsg{iso, x, svcs, envs, err}
	}
}

func (e *exposePage) exposure(svc string) api.Exposure {
	for _, x := range e.exposures {
		if x.Service == svc {
			return x
		}
	}
	return api.Exposure{Service: svc}
}

func (m *Model) handleExposeKey(msg tea.KeyMsg) tea.Cmd {
	e, k := m.isoPage.expose, m.keys
	if e.form != nil {
		if k.Is(msg, "back") {
			e.form = nil
			return nil
		}
		if !(k.Is(msg, "submit") || (msg.String() == "enter" && e.form.cur == len(e.form.fields)-1)) {
			if msg.String() == "enter" {
				e.form.move(1)
				return nil
			}
			cmd, _ := e.form.handle(k, msg)
			return cmd
		}
		svc, iso := e.formSvc, e.iso
		var envs []string
		def, rest := "", e.form.fields
		if e.formEnvs {
			envs, def, rest = e.form.fields[0].values(), e.form.fields[1].value(), e.form.fields[2:]
			if def == optDefault {
				def = ""
			}
		}
		settings, err := patchFrom(e.formSettings, rest, decodeMap(e.exposure(svc.Name).Settings))
		if err != nil {
			m.setFlash(err.Error(), true)
			return nil
		}
		was := e.exposure(svc.Name)
		e.form = nil
		be := m.be
		run := func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			x, err := be.Expose(ctx, iso, svc.Name, api.Expose{Envs: envs, Default: def, Settings: settings})
			flash := fmt.Sprintf("%s is on for %s", svc.Name, iso)
			switch {
			case err == nil && svc.Replaces:
				flash = fmt.Sprintf("%s has %s", iso, orDash(sharedPaths(x.Envs)))
			case err == nil && len(x.Envs) > 0:
				flash += ": " + strings.Join(x.Envs, ", ") + " (default " + x.Default + "); agents pick it up at their next idle moment"
			case err == nil:
				flash += "; agents pick it up at their next idle moment"
			}
			return doneMsg{flash: flash, err: err, refresh: true}
		}
		// A share is mounted only when the VM starts: a change replaces it.
		if svc.Replaces && (!was.Enabled || strings.Join(was.Envs, ",") != strings.Join(envs, ",")) {
			m.confirm = &confirmation{
				prompt: fmt.Sprintf("restart %s with %s? its container is replaced; running agents end and start again on their next message (y/n)", iso, orDash(sharedPaths(envs))),
				onYes:  func() tea.Cmd { m.setFlash("restarting "+iso+"…", false); return run },
			}
			return nil
		}
		return run
	}
	switch {
	case k.Is(msg, "back"):
		m.isoPage.expose = nil
	case k.Is(msg, "up"):
		e.sel = max(0, e.sel-1)
	case k.Is(msg, "down"):
		e.sel = min(len(e.services)-1, e.sel+1)
	case k.Is(msg, "open"), k.Is(msg, "svc_edit"):
		if e.sel < len(e.services) {
			m.openExposeForm(e.services[e.sel])
		}
	case k.Is(msg, "svc_off"):
		if e.sel >= len(e.services) {
			return nil
		}
		svc, iso, be := e.services[e.sel].Name, e.iso, m.be
		if !e.exposure(svc).Enabled {
			m.setFlash(svc+" is already off for "+iso, false)
			return nil
		}
		prompt := fmt.Sprintf("turn %s off for %s? its files and variables are removed there (y/n)", svc, iso)
		if e.services[e.sel].Replaces {
			prompt = fmt.Sprintf("turn %s off for %s? its container is replaced without the shared folders; running agents end (y/n)", svc, iso)
		}
		m.confirm = &confirmation{prompt: prompt,
			onYes: func() tea.Cmd {
				return m.do(svc+" is off for "+iso, true, func(ctx context.Context) error { return be.Unexpose(ctx, iso, svc) })
			}}
	}
	return nil
}

func (m *Model) openExposeForm(svc api.Service) {
	e := m.isoPage.expose
	x := e.exposure(svc.Name)
	var names []string
	for _, env := range e.envs {
		if env.Service == svc.Name {
			names = append(names, env.Name)
		}
	}
	var others []string
	for _, iso := range m.isolations {
		if iso.Name != e.iso {
			others = append(others, iso.Name)
		}
	}
	settings := make([]proxy.Field, 0, len(svc.Settings))
	for _, f := range svc.Settings {
		if f.Isolations {
			f.Choices = others
		}
		settings = append(settings, f)
	}
	var fields []*field
	if e.formEnvs = len(svc.Fields) > 0; e.formEnvs {
		hint := "space ticks; none: the service with no credential (git: public reads only)"
		if svc.Replaces {
			hint = "space ticks; each ticked folder is at /shared/NAME in the isolation"
		}
		envs := checkField("envs", names, x.Envs, hint)
		def := selectField("default", append([]string{optDefault}, names...), "the env plain variables point at; default: the first ticked")
		def.idx = indexOf(def.options, x.Default)
		fields = []*field{envs, def}
	}
	fields = append(fields, genFields(settings, decodeMap(x.Settings), !x.Enabled)...)
	e.form, e.formSvc, e.formSettings = &form{fields: fields}, svc, settings
}

func (m *Model) renderExpose(width int) string {
	e, th, k := m.isoPage.expose, m.th, m.keys
	var b strings.Builder
	b.WriteString(th.title.Render("Services in "+e.iso) + th.dim.Render("   what its proxies serve; the credentials stay on the Mac") + "\n\n")
	if !e.loaded {
		b.WriteString(th.dim.Render("  loading…") + "\n")
	}
	for i, s := range e.services {
		x := e.exposure(s.Name)
		state := th.dim.Render("off")
		detail := ""
		if x.Enabled {
			state = th.ok.Render("on ")
			envs := strings.Join(x.Envs, ",")
			if envs == "" && len(s.Fields) > 0 {
				envs = "no env"
			}
			detail = envs
			if x.Default != "" {
				detail += " (default " + x.Default + ")"
			}
			if st := compact(x.Settings); st != "" {
				detail += "  " + th.dim.Render(st)
			}
			if len(x.Missing) > 0 {
				detail += "  " + th.err.Render("missing: "+strings.Join(x.Missing, ","))
			}
		}
		line := truncate(fmt.Sprintf("%-11s %s  %s", s.Name, state, detail), width-3)
		if i == e.sel && e.form == nil {
			b.WriteString(th.user.Render("› ") + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	b.WriteString("\n")
	if e.form != nil {
		b.WriteString(th.title.Render(e.formSvc.Name+" in "+e.iso) + "\n" + e.form.render(th, width))
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s apply · %s next · space tick · ←/→ change · %s cancel",
			k.Label("submit"), k.Label("focus_next"), k.Label("back"))) + "\n")
		if e.formEnvs {
			b.WriteString(th.dim.Render("  envs are defined on the services page ("+k.Label("services")+")") + "\n")
		}
	} else {
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s turn on / change · %s turn off · %s back",
			k.Label("open"), k.Label("svc_off"), k.Label("back"))) + "\n")
	}
	return b.String()
}

// sharedPaths is where share envs are in the isolation.
func sharedPaths(envs []string) string {
	var out []string
	for _, n := range envs {
		out = append(out, "/shared/"+n)
	}
	return strings.Join(out, ", ")
}
