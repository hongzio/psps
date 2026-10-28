package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
)

// The accounts page, from the isolations page: each agent CLI in one
// isolation, read as an agent there finds it, and where its credential
// comes from. That is an account, defined once on the Mac and usable by any
// number of isolations (a Claude token, an API key), or a login the daemon
// runs inside the isolation: Claude's ends on a page showing a code to
// paste back here; Codex's asks for a code to be entered in the browser
// and notices by itself.

type clisPage struct {
	iso      string
	clis     []api.CLI
	accounts []api.Account
	loaded   bool
	// sel is a row: the CLIs, then the accounts.
	sel  int
	busy string
	// login is the login on screen, polled until it ends; paste takes
	// Claude's code.
	login *api.CLILogin
	paste *form
	// form is the open form, of formKind, for the CLI or account formFor.
	form     *form
	formKind cliForm
	formFor  string
}

type cliForm int

const (
	formUse     cliForm = iota + 1 // which account a CLI uses
	formAccount                    // a new account
	formSecret                     // an account's new secret
)

const loginInside = "a login inside the isolation"

type (
	clisMsg struct {
		iso      string
		clis     []api.CLI
		accounts []api.Account
		err      error
	}
	cliLoginMsg struct {
		iso string
		l   api.CLILogin
		err error
	}
	cliPollMsg struct {
		iso  string
		tool driver.Kind
	}
)

func (m *Model) openCLIs(iso string) tea.Cmd {
	m.isoPage.clis = &clisPage{iso: iso, busy: "reading the logins in " + iso + "…"}
	return m.fetchCLIs(iso)
}

func (m *Model) fetchCLIs(iso string) tea.Cmd {
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		as, err := be.Accounts(ctx)
		if err != nil {
			return clisMsg{iso: iso, err: err}
		}
		cs, err := be.CLIs(ctx, iso)
		return clisMsg{iso, cs, as, err}
	}
}

func (p *clisPage) rows() int { return len(p.clis) + len(p.accounts) }

func (p *clisPage) currentCLI() (api.CLI, bool) {
	if p.sel < 0 || p.sel >= len(p.clis) {
		return api.CLI{}, false
	}
	return p.clis[p.sel], true
}

func (p *clisPage) currentAccount() (api.Account, bool) {
	i := p.sel - len(p.clis)
	if i < 0 || i >= len(p.accounts) {
		return api.Account{}, false
	}
	return p.accounts[i], true
}

func loginLive(l *api.CLILogin) bool {
	return l != nil && (l.State == api.CLILoginStarting || l.State == api.CLILoginOpen || l.State == api.CLILoginVerifying)
}

// clisPageFor is the open page for iso, or nil once it was left.
func (m *Model) clisPageFor(iso string) *clisPage {
	if m.isoPage == nil || m.isoPage.clis == nil || m.isoPage.clis.iso != iso {
		return nil
	}
	return m.isoPage.clis
}

func (m *Model) onCLIs(msg clisMsg) tea.Cmd {
	p := m.clisPageFor(msg.iso)
	if p == nil {
		return nil
	}
	p.busy = ""
	if msg.err != nil {
		m.setFlash(msg.err.Error(), true)
		return nil
	}
	p.clis, p.accounts, p.loaded = msg.clis, msg.accounts, true
	p.sel = min(p.sel, max(p.rows()-1, 0))
	// A login started elsewhere (the web, the CLI) is picked up here.
	if p.login == nil {
		for _, c := range p.clis {
			if loginLive(c.Login) {
				return m.showCLILogin(p, *c.Login)
			}
		}
	}
	return nil
}

// showCLILogin puts l on screen and polls it while it runs.
func (m *Model) showCLILogin(p *clisPage, l api.CLILogin) tea.Cmd {
	first := p.login == nil || p.login.Tool != l.Tool
	p.login = &l
	if first && l.NeedsCode && l.State == api.CLILoginOpen {
		f := secretField("code", "the code the login page ends on")
		p.paste = &form{fields: []*field{f}}
		f.focus(true)
	}
	iso, tool := p.iso, l.Tool
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return cliPollMsg{iso, tool} })
}

func secretField(label, placeholder string) *field {
	f := textField(label, placeholder, "")
	f.text.EchoMode = textinput.EchoPassword
	return f
}

func (m *Model) onCLILogin(msg cliLoginMsg) tea.Cmd {
	p := m.clisPageFor(msg.iso)
	if p == nil {
		return nil
	}
	p.busy = ""
	if msg.err != nil {
		m.setFlash(msg.err.Error(), true)
		if p.login != nil && p.login.State == api.CLILoginVerifying {
			return nil
		}
		p.login, p.paste = nil, nil
		return m.fetchCLIs(p.iso)
	}
	if loginLive(&msg.l) {
		return m.showCLILogin(p, msg.l)
	}
	p.login, p.paste = nil, nil
	if msg.l.State == api.CLILoginDone {
		m.setFlash(fmt.Sprintf("%s is logged in in %s (%s); its idle agents restart with it", msg.l.Tool, p.iso, msg.l.Message), false)
	} else {
		m.setFlash(fmt.Sprintf("%s login: %s", msg.l.Tool, msg.l.Message), true)
	}
	p.busy = "reading the logins…"
	return m.fetchCLIs(p.iso)
}

func (m *Model) pollCLILogin(msg cliPollMsg) tea.Cmd {
	p := m.clisPageFor(msg.iso)
	if p == nil || p.login == nil || p.login.Tool != msg.tool {
		return nil
	}
	be, iso, tool := m.be, msg.iso, string(msg.tool)
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		l, err := be.CLILogin(ctx, iso, tool)
		return cliLoginMsg{iso, l, err}
	}
}

func (m *Model) handleCLIsKey(msg tea.KeyMsg) tea.Cmd {
	p := m.isoPage.clis
	if p.busy != "" {
		return nil
	}
	switch {
	case p.login != nil:
		return m.handleCLILoginKey(msg)
	case p.form != nil:
		return m.handleCLIFormKey(msg)
	}
	k, be, iso := m.keys, m.be, p.iso
	c, onCLI := p.currentCLI()
	a, onAccount := p.currentAccount()
	tool := string(c.Tool)
	switch {
	case k.Is(msg, "back"):
		m.isoPage.clis = nil
	case k.Is(msg, "up"):
		p.sel = max(0, p.sel-1)
	case k.Is(msg, "down"):
		p.sel = min(p.rows()-1, p.sel+1)
	case k.Is(msg, "acct_new"):
		name := textField("name", "lowercase letters, digits, dashes", "")
		var kinds []string
		for _, kd := range api.AccountKinds {
			kinds = append(kinds, kd.Name)
		}
		kind := selectField("kind", kinds, "")
		p.form, p.formKind, p.formFor = &form{fields: []*field{name, kind, secretField("secret", "the token or key; never shown again")}}, formAccount, ""
		name.focus(true)

	case onCLI && c.Account != "" && (k.Is(msg, "cli_login") || k.Is(msg, "open")):
		m.setFlash(fmt.Sprintf("%s here uses account %s; choose %s first (%s)", tool, c.Account, loginInside, k.Label("cli_account")), true)
	case onCLI && (k.Is(msg, "cli_login") || k.Is(msg, "open")):
		p.busy = "starting the " + tool + " login inside " + iso + "…"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			l, err := be.StartCLILogin(ctx, iso, tool)
			return cliLoginMsg{iso, l, err}
		}
	case onCLI && k.Is(msg, "cli_logout"):
		m.confirm = &confirmation{
			prompt: fmt.Sprintf("log %s out in %s? its login inside goes; an account it uses stays (y/n)", tool, iso),
			onYes: func() tea.Cmd {
				p.busy = "logging " + tool + " out…"
				return m.do(tool+" is logged out in "+iso, true, func(ctx context.Context) error {
					return be.CLILogout(ctx, iso, tool)
				})
			},
		}
	case onCLI && k.Is(msg, "cli_account"):
		options := []string{loginInside}
		for _, a := range p.accounts {
			if a.Tool == c.Tool {
				options = append(options, a.Name)
			}
		}
		if len(options) == 1 {
			m.setFlash(fmt.Sprintf("no %s accounts yet; %s adds one", tool, k.Label("acct_new")), true)
			return nil
		}
		f := selectField("account", options, "")
		f.idx = max(indexOf(options, c.Account), 0)
		p.form, p.formKind, p.formFor = &form{fields: []*field{f}}, formUse, tool

	case onAccount && k.Is(msg, "acct_secret"):
		f := secretField("secret", "the new token or key")
		p.form, p.formKind, p.formFor = &form{fields: []*field{f}}, formSecret, a.Name
		f.focus(true)
	case onAccount && k.Is(msg, "acct_delete"):
		name, users := a.Name, a.UsedBy
		prompt := fmt.Sprintf("remove account %s and its secret? (y/n)", name)
		if len(users) > 0 {
			prompt = fmt.Sprintf("remove account %s? %s go back to a login inside (y/n)", name, strings.Join(users, ", "))
		}
		m.confirm = &confirmation{prompt: prompt, onYes: func() tea.Cmd {
			p.busy = "removing " + name + "…"
			return m.do("account "+name+" removed", true, func(ctx context.Context) error {
				return be.DeleteAccount(ctx, name, true)
			})
		}}
	}
	return nil
}

func (m *Model) handleCLILoginKey(msg tea.KeyMsg) tea.Cmd {
	p, k, be := m.isoPage.clis, m.keys, m.be
	iso, tool := p.iso, string(p.login.Tool)
	pasted := p.paste != nil && p.paste.fields[0].text.Value() != ""
	switch {
	case k.Is(msg, "back"):
		p.login, p.paste = nil, nil
		return m.do(tool+" login cancelled", false, func(ctx context.Context) error {
			return be.CancelCLILogin(ctx, iso, tool)
		})
	case k.Is(msg, "copy") && !pasted:
		m.copyToClipboard(p.login.URL)
		m.setFlash("the login address is on your clipboard (if the terminal allows OSC 52)", false)
	case p.paste != nil && p.login.State == api.CLILoginOpen && (k.Is(msg, "submit") || msg.String() == "enter"):
		code := p.paste.fields[0].text.Value()
		if strings.TrimSpace(code) == "" {
			return nil
		}
		p.busy = "handing the code to claude…"
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			l, err := be.SendCLICode(ctx, iso, tool, code)
			return cliLoginMsg{iso, l, err}
		}
	case p.paste != nil:
		cmd, _ := p.paste.handle(k, msg)
		return cmd
	}
	return nil
}

func (m *Model) handleCLIFormKey(msg tea.KeyMsg) tea.Cmd {
	p, k, be := m.isoPage.clis, m.keys, m.be
	if k.Is(msg, "back") {
		p.form = nil
		return nil
	}
	last := p.form.cur == len(p.form.fields)-1
	if !(k.Is(msg, "submit") || (msg.String() == "enter" && last)) {
		if msg.String() == "enter" {
			p.form.move(1)
			return nil
		}
		cmd, _ := p.form.handle(k, msg)
		return cmd
	}
	f, iso := p.form.fields, p.iso
	switch p.formKind {
	case formUse:
		tool, account := p.formFor, f[0].value()
		if account == loginInside {
			account = ""
		}
		p.form, p.busy = nil, "saving…"
		flash := fmt.Sprintf("%s in %s uses account %s; its idle agents restart with it", tool, iso, account)
		if account == "" {
			flash = fmt.Sprintf("%s in %s goes back to %s", tool, iso, loginInside)
		}
		return m.do(flash, true, func(ctx context.Context) error {
			return be.UseAccount(ctx, iso, tool, account)
		})
	case formAccount:
		name, kind, secret := f[0].value(), f[1].value(), f[2].value()
		if name == "" || secret == "" {
			m.setFlash("a name and a secret are required", true)
			return nil
		}
		p.form, p.busy = nil, "adding "+name+"…"
		return m.do("account "+name+" added; "+k.Label("cli_account")+" on a CLI uses it", true, func(ctx context.Context) error {
			_, err := be.AddAccount(ctx, name, kind, secret)
			return err
		})
	case formSecret:
		name, secret := p.formFor, f[0].value()
		if secret == "" {
			return nil
		}
		p.form, p.busy = nil, "saving…"
		return m.do("replaced the secret of "+name+"; idle agents using it restart", true, func(ctx context.Context) error {
			_, err := be.SetAccountSecret(ctx, name, secret)
			return err
		})
	}
	return nil
}

func (m *Model) renderCLIs(width int) string {
	p, th, k := m.isoPage.clis, m.th, m.keys
	var b strings.Builder
	b.WriteString(th.title.Render("Accounts in "+p.iso) + th.dim.Render("   Claude and Codex use an account from the Mac, or a login inside the isolation") + "\n\n")
	if !p.loaded && p.busy == "" {
		b.WriteString(th.dim.Render("  loading…") + "\n")
	}
	cursor := func(i int, line string) {
		if i == p.sel && p.login == nil && p.form == nil {
			b.WriteString(th.user.Render("› ") + line + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	for i, c := range p.clis {
		state := th.warn.Render("not logged in")
		switch {
		case c.Error != "":
			state = th.err.Render("unknown: " + c.Error)
		case c.LoggedIn:
			state = th.ok.Render("✓ logged in") + th.dim.Render(" · "+orDash(c.Method))
			if c.Email != "" {
				state += th.dim.Render(" · " + c.Email)
			}
		}
		from := th.dim.Render("login inside")
		if c.Account != "" {
			from = "account " + th.title.Render(c.Account)
		}
		cursor(i, truncate(fmt.Sprintf("%-7s %-9s %s   %s", c.Tool, orDash(c.Version), from, state), width-4))
	}
	if p.loaded {
		b.WriteString("\n" + th.title.Render("Accounts") + th.dim.Render("   defined once, on the Mac; any number of isolations can use one") + "\n")
		if len(p.accounts) == 0 {
			b.WriteString(th.dim.Render(fmt.Sprintf("  none yet; %s adds one (a token from `claude setup-token`, an API key)", k.Label("acct_new"))) + "\n")
		}
		for i, a := range p.accounts {
			used := "unused"
			if len(a.UsedBy) > 0 {
				used = "used by " + strings.Join(a.UsedBy, ", ")
			}
			line := fmt.Sprintf("%-14s %-18s %s", a.Name, a.Kind, th.dim.Render(used))
			if a.SecretError != "" {
				line += "  " + th.err.Render("no secret stored")
			}
			cursor(len(p.clis)+i, truncate(line, width-4))
		}
	}
	b.WriteString("\n")
	switch {
	case p.login != nil:
		l := p.login
		b.WriteString(th.title.Render("Log "+string(l.Tool)+" in inside "+p.iso) + "\n\n")
		if l.NeedsCode {
			b.WriteString("  1. Open this address in a browser and sign in:\n\n")
			b.WriteString(l.URL + "\n\n")
			b.WriteString("  2. The page ends on a code. Paste it here:\n")
			if p.paste != nil {
				b.WriteString(p.paste.render(th, width))
			}
			b.WriteString(th.dim.Render(fmt.Sprintf("  %s copies the address · %s finish · %s cancel", k.Label("copy"), k.Label("submit"), k.Label("back"))) + "\n")
		} else {
			b.WriteString("  1. Open " + l.URL + " in a browser and sign in\n")
			b.WriteString("  2. Enter this code:  " + th.title.Render(l.Code) + "\n\n")
			b.WriteString(th.working.Render(spinner[m.frame%len(spinner)]+" waiting for the browser") +
				th.dim.Render(" (the code expires at "+l.Expires.Local().Format("15:04")+")") + "\n")
			b.WriteString(th.dim.Render(fmt.Sprintf("  %s copies the address · %s cancel", k.Label("copy"), k.Label("back"))) + "\n")
		}
	case p.form != nil:
		title := map[cliForm]string{formUse: "What " + p.formFor + " in " + p.iso + " uses", formAccount: "New account", formSecret: "New secret for " + p.formFor}[p.formKind]
		b.WriteString(th.title.Render(title) + "\n" + p.form.render(th, width))
		switch p.formKind {
		case formAccount:
			for _, kd := range api.AccountKinds {
				b.WriteString(th.dim.Render(fmt.Sprintf("  %s: %s", kd.Name, kd.Summary)) + "\n")
			}
			b.WriteString(th.dim.Render("  the secret goes to the Mac's credentials file (0600) and is never shown again") + "\n")
		case formUse:
			b.WriteString(th.dim.Render("  ←/→ choose; its idle agents restart with it") + "\n")
		}
		b.WriteString(th.dim.Render(fmt.Sprintf("  %s save · %s cancel", k.Label("submit"), k.Label("back"))) + "\n")
	default:
		hints := []string{k.Label("cli_login") + " log in inside", k.Label("cli_logout") + " log out", k.Label("cli_account") + " use an account",
			k.Label("acct_new") + " new account", k.Label("acct_secret") + " replace its secret", k.Label("acct_delete") + " remove it", k.Label("back") + " back"}
		b.WriteString(th.dim.Render("  "+strings.Join(hints, " · ")) + "\n")
	}
	if p.busy != "" {
		b.WriteString("\n" + th.working.Render(spinner[m.frame%len(spinner)]+" "+p.busy) + "\n")
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
