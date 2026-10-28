// Package tui is gero's terminal interface: agents on the left, the selected
// agent's conversation in the middle, the message input at the bottom.
// Everything is driven from the keyboard, with bindings from keys.toml.
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/proxy"
)

// Backend is the daemon API the TUI uses; *client.Client implements it.
type Backend interface {
	Agents(ctx context.Context) ([]api.Agent, error)
	Isolations(ctx context.Context) ([]api.Isolation, error)
	CreateAgent(ctx context.Context, req api.CreateAgent) (api.Agent, error)
	Send(ctx context.Context, id, text string) error
	Approve(ctx context.Context, id, approval string, d driver.Decision) error
	Interrupt(ctx context.Context, id string) error
	SetOptions(ctx context.Context, id string, o driver.Options) error
	Archive(ctx context.Context, id string) error
	Follow(ctx context.Context, id string, after int64, fn func(event.Event) error) error
	CreateIsolation(ctx context.Context, req api.CreateIsolation) (api.Isolation, error)
	UpdateIsolation(ctx context.Context, name string, req api.UpdateIsolation) (api.Isolation, error)
	UpgradeIsolation(ctx context.Context, name string) (api.Isolation, error)
	DeleteIsolation(ctx context.Context, name string, purge bool) error
	SetEnv(ctx context.Context, name string, set api.SetEnv) error
	Verify(ctx context.Context, name string) (api.Verify, error)

	// The agent CLIs' own logins.
	CLIs(ctx context.Context, iso string) ([]api.CLI, error)
	StartCLILogin(ctx context.Context, iso, tool string) (api.CLILogin, error)
	CLILogin(ctx context.Context, iso, tool string) (api.CLILogin, error)
	SendCLICode(ctx context.Context, iso, tool, code string) (api.CLILogin, error)
	CancelCLILogin(ctx context.Context, iso, tool string) error
	CLILogout(ctx context.Context, iso, tool string) error
	Accounts(ctx context.Context) ([]api.Account, error)
	AddAccount(ctx context.Context, name, kind, secret string) (api.Account, error)
	SetAccountSecret(ctx context.Context, name, secret string) (api.Account, error)
	DeleteAccount(ctx context.Context, name string, force bool) error
	UseAccount(ctx context.Context, iso, tool, account string) error

	// The credential proxies.
	Services(ctx context.Context) ([]api.Service, error)
	Envs(ctx context.Context, service string) ([]api.Env, error)
	AddEnv(ctx context.Context, service, name string, p proxy.Patch) (api.Env, error)
	EditEnv(ctx context.Context, service, name string, p proxy.Patch) (api.Env, error)
	DeleteEnv(ctx context.Context, service, name string, force bool) error
	SetSecret(ctx context.Context, service, name string, values map[string]string) error
	CheckEnv(ctx context.Context, service, name string) (api.Check, error)
	Choices(ctx context.Context, service, field string, req api.ChoicesRequest) ([]proxy.Choice, error)
	StartLogin(ctx context.Context, service, name, redirect string) (api.Login, error)
	FinishLogin(ctx context.Context, service, name, id, callback string) error
	Exposures(ctx context.Context, iso string) ([]api.Exposure, error)
	Expose(ctx context.Context, iso, service string, req api.Expose) (api.Exposure, error)
	Unexpose(ctx context.Context, iso, service string) error
}

type focus int

const (
	focusSidebar focus = iota
	focusConvo
	focusInput
	focusMain // a form or the isolations page
)

type view int

const (
	viewAgent view = iota
	viewNewAgent
	viewIsolations
	viewServices
)

const (
	selNew        = "new"
	selArchived   = "archived"
	selIsolations = "isolations"
	selServices   = "services"
)

type Model struct {
	be   Backend
	keys Keymap
	th   theme

	width, height int
	agents        []api.Agent
	isolations    []api.Isolation
	sel           string // selNew, selArchived, selIsolations or an agent id
	showArchived  bool
	focus         focus
	view          view
	approving     bool
	help          bool
	confirm       *confirmation
	flash         string
	flashErr      bool
	flashUntil    time.Time
	frame         int
	connErr       string
	lastIsoFetch  time.Time

	convos   map[string]*convo
	drafts   map[string]string
	openID   string
	stopFeed context.CancelFunc
	feedGen  int
	feed     chan tea.Msg
	vp       viewport.Model
	input    textarea.Model
	newForm  *agentForm
	settings *settingsForm
	isoPage  *isoPage
	svcPage  *svcPage
	// clipboard replaces the terminal clipboard (OSC 52) in tests.
	clipboard func(string)
}

// confirmation is a y/n question in the status bar; onYes runs on the UI
// goroutine and returns the work to do.
type confirmation struct {
	prompt string
	onYes  func() tea.Cmd
}

func New(be Backend, keys Keymap, warnings []string) *Model {
	in := textarea.New()
	in.Placeholder = "Message the agent…"
	in.ShowLineNumbers = false
	in.Prompt = ""
	in.CharLimit = 0
	in.SetHeight(1)
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys(keys.keys["newline"]...))
	in.FocusedStyle.CursorLine = lipgloss.NewStyle()
	m := &Model{be: be, keys: keys, th: newTheme(), sel: selNew,
		convos: map[string]*convo{}, drafts: map[string]string{}, feed: make(chan tea.Msg, 512),
		vp: viewport.New(0, 0), input: in}
	if len(warnings) > 0 {
		m.setFlash(strings.Join(warnings, "; "), true)
	}
	return m
}

// Messages.
type (
	agentsMsg struct {
		agents []api.Agent
		err    error
	}
	isolationsMsg struct {
		isos []api.Isolation
		err  error
	}
	feedEventMsg struct {
		gen int
		ev  event.Event
	}
	feedEndMsg struct {
		gen int
		err error
	}
	reopenMsg struct{ gen int }
	tickMsg   time.Time
	doneMsg   struct {
		flash   string
		err     error
		refresh bool
	}
	createdMsg struct {
		agent api.Agent
		err   error
	}
)

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.fetchAgents(), m.fetchIsolations(), m.listen(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) listen() tea.Cmd {
	ch := m.feed
	return func() tea.Msg { return <-ch }
}

func (m *Model) fetchAgents() tea.Cmd {
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		as, err := be.Agents(ctx)
		return agentsMsg{as, err}
	}
}

func (m *Model) fetchIsolations() tea.Cmd {
	m.lastIsoFetch = time.Now()
	be := m.be
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		is, err := be.Isolations(ctx)
		return isolationsMsg{is, err}
	}
}

// do runs a daemon call off the UI goroutine and reports its outcome.
func (m *Model) do(flash string, refresh bool, f func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		return doneMsg{flash: flash, err: f(ctx), refresh: refresh}
	}
}

func (m *Model) setFlash(s string, isErr bool) {
	m.flash, m.flashErr, m.flashUntil = s, isErr, time.Now().Add(5*time.Second)
}

func (m *Model) agent(id string) (api.Agent, bool) {
	for _, a := range m.agents {
		if a.ID == id {
			return a, true
		}
	}
	return api.Agent{}, false
}

func (m *Model) convo(id string) *convo {
	c, ok := m.convos[id]
	if !ok {
		c = newConvo()
		m.convos[id] = c
	}
	return c
}

// items is the sidebar, top to bottom.
func (m *Model) items() []string {
	out := []string{selNew}
	var archived []string
	for _, a := range m.agents {
		if a.ArchivedAt != nil {
			archived = append(archived, a.ID)
		} else {
			out = append(out, a.ID)
		}
	}
	out = append(out, selArchived)
	if m.showArchived {
		out = append(out, archived...)
	}
	return append(out, selIsolations, selServices)
}

func (m *Model) moveSel(delta int) tea.Cmd {
	items := m.items()
	i := 0
	for j, it := range items {
		if it == m.sel {
			i = j
		}
	}
	i = max(0, min(len(items)-1, i+delta))
	return m.selectItem(items[i])
}

// selectItem moves the cursor; an agent under the cursor is shown at once.
func (m *Model) selectItem(sel string) tea.Cmd {
	m.sel = sel
	if _, ok := m.agent(sel); ok {
		m.view = viewAgent
		m.openAgent(sel)
	}
	return nil
}

// openAgent shows an agent's conversation and follows its events from the
// last one already seen, so switching back and forth costs nothing.
func (m *Model) openAgent(id string) {
	if m.openID == id {
		return
	}
	if m.openID != "" {
		m.drafts[m.openID] = m.input.Value()
	}
	if m.stopFeed != nil {
		m.stopFeed()
	}
	m.openID = id
	m.approving = false
	m.settings = nil
	m.input.SetValue(m.drafts[id])
	m.feedGen++
	gen, after, ch, be := m.feedGen, m.convo(id).lastID, m.feed, m.be
	ctx, cancel := context.WithCancel(context.Background())
	m.stopFeed = cancel
	go func() {
		err := be.Follow(ctx, id, after, func(e event.Event) error {
			select {
			case ch <- feedEventMsg{gen, e}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if ctx.Err() == nil {
			ch <- feedEndMsg{gen, err}
		}
	}()
	m.refreshConvo(true)
}

func (m *Model) agentLabel(a api.Agent) string {
	if a.Kind == driver.Codex {
		return "Codex"
	}
	return "Claude"
}

func (m *Model) refreshConvo(forceBottom bool) {
	a, ok := m.agent(m.openID)
	if !ok {
		m.vp.SetContent("")
		return
	}
	atBottom := m.vp.AtBottom() || forceBottom
	m.vp.SetContent(m.convo(a.ID).render(m.vp.Width, m.agentLabel(a), m.th))
	if atBottom {
		m.vp.GotoBottom()
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refreshConvo(false)
		return m, nil

	case tickMsg:
		m.frame++
		var cmds []tea.Cmd
		cmds = append(cmds, tick())
		// Poll the list a little under once a second; the open agent's
		// events arrive on its stream.
		if m.frame%6 == 0 {
			cmds = append(cmds, m.fetchAgents())
		}
		if time.Since(m.lastIsoFetch) > 10*time.Second || (m.view != viewAgent && time.Since(m.lastIsoFetch) > 2*time.Second) {
			cmds = append(cmds, m.fetchIsolations())
		}
		return m, tea.Batch(cmds...)

	case agentsMsg:
		if msg.err != nil {
			m.connErr = msg.err.Error()
			return m, nil
		}
		m.connErr = ""
		m.agents = msg.agents
		if _, ok := m.agent(m.sel); !ok && !isFixed(m.sel) {
			m.sel = selNew
		}
		if m.openID == "" && m.sel == selNew {
			// First load: land on the most recent live agent, if any.
			for i := len(m.agents) - 1; i >= 0; i-- {
				if m.agents[i].ArchivedAt == nil {
					m.selectItem(m.agents[i].ID)
					break
				}
			}
		}
		m.layout()
		return m, nil

	case isolationsMsg:
		if msg.err == nil {
			m.isolations = msg.isos
			if p := m.isoPage; p != nil && p.follow != "" {
				for i, iso := range m.isolations {
					if iso.Name == p.follow {
						p.sel, p.follow = i, ""
					}
				}
			}
			if m.newForm != nil {
				m.newForm.setIsolations(m.isolations)
			}
		}
		return m, nil

	case feedEventMsg:
		if msg.gen == m.feedGen {
			m.convo(m.openID).apply(msg.ev)
			m.layout()
			m.refreshConvo(false)
			if m.approving && len(m.convo(m.openID).pending()) == 0 {
				m.approving = false
			}
		}
		return m, m.listen()

	case feedEndMsg:
		if msg.gen == m.feedGen {
			gen := msg.gen
			return m, tea.Batch(m.listen(), tea.Tick(time.Second, func(time.Time) tea.Msg { return reopenMsg{gen} }))
		}
		return m, m.listen()

	case reopenMsg:
		if msg.gen == m.feedGen && m.openID != "" {
			id := m.openID
			m.openID = ""
			m.openAgent(id)
		}
		return m, nil

	case doneMsg:
		if msg.err != nil {
			m.setFlash(msg.err.Error(), true)
		} else if msg.flash != "" {
			m.setFlash(msg.flash, false)
		}
		if m.isoPage != nil {
			m.isoPage.busy = ""
			if m.isoPage.clis != nil {
				m.isoPage.clis.busy = ""
			}
		}
		if m.svcPage != nil {
			m.svcPage.busy = ""
		}
		if msg.refresh {
			cmds := []tea.Cmd{m.fetchAgents(), m.fetchIsolations()}
			if m.svcPage != nil {
				cmds = append(cmds, m.fetchServices())
			}
			if m.isoPage != nil && m.isoPage.expose != nil {
				cmds = append(cmds, m.fetchExposures(m.isoPage.expose.iso))
			}
			if m.isoPage != nil && m.isoPage.clis != nil {
				cmds = append(cmds, m.fetchCLIs(m.isoPage.clis.iso))
			}
			return m, tea.Batch(cmds...)
		}
		return m, nil

	case servicesMsg:
		if p := m.svcPage; p != nil {
			if msg.err != nil {
				m.setFlash(msg.err.Error(), true)
			} else {
				p.services, p.envs, p.loaded = msg.services, msg.envs, true
				p.sel = min(p.sel, max(len(p.rows())-1, 0))
			}
		}
		return m, nil

	case checkMsg:
		if p := m.svcPage; p != nil {
			p.busy = ""
			p.mode, p.outErr = svcOutput, msg.err != nil
			p.output = []string{msg.title}
			for _, l := range strings.Split(strings.TrimRight(msg.out, "\n"), "\n") {
				if l != "" {
					p.output = append(p.output, l)
				}
			}
			if msg.err != nil {
				p.output = append(p.output, "✗ "+msg.err.Error())
			}
		}
		return m, nil

	case loginStartedMsg:
		p := m.svcPage
		if p == nil {
			if msg.st.stop != nil {
				msg.st.stop()
			}
			return m, nil
		}
		p.busy = ""
		if msg.err != nil {
			m.setFlash(msg.err.Error(), true)
			return m, nil
		}
		paste := textField("landed on", "the address the browser landed on", "only for a browser on another device")
		msg.st.paste = &form{fields: []*field{paste}}
		paste.focus(true)
		p.mode, p.login = svcLogin, msg.st
		return m, waitCaught(msg.st)

	case loginCaughtMsg:
		if p := m.svcPage; p != nil && p.login == msg.st {
			p.mode, p.login = svcList, nil
			return m, m.finishLogin(msg.st, msg.query)
		}
		return m, nil

	case verifyMsg:
		if p := m.isoPage; p != nil {
			p.busy = ""
			if msg.err != nil {
				m.setFlash(msg.err.Error(), true)
			} else {
				p.mode, p.report = isoVerify, &msg.v
			}
		}
		return m, m.fetchIsolations()

	case clisMsg:
		return m, m.onCLIs(msg)
	case cliLoginMsg:
		return m, m.onCLILogin(msg)
	case cliPollMsg:
		return m, m.pollCLILogin(msg)
	case choicesMsg:
		m.onChoices(msg)
		return m, nil

	case exposuresMsg:
		if m.isoPage != nil && m.isoPage.expose != nil && m.isoPage.expose.iso == msg.iso {
			e := m.isoPage.expose
			if msg.err != nil {
				m.setFlash(msg.err.Error(), true)
			} else {
				e.exposures, e.services, e.envs, e.loaded = msg.exposures, msg.services, msg.envs, true
				e.sel = min(e.sel, max(len(e.services)-1, 0))
			}
		}
		return m, nil

	case createdMsg:
		if m.newForm != nil {
			m.newForm.busy = false
		}
		if msg.err != nil {
			if m.newForm != nil {
				m.newForm.err = msg.err.Error()
			}
			return m, nil
		}
		m.newForm = nil
		m.agents = append(m.agents, msg.agent)
		m.selectItem(msg.agent.ID)
		m.setFocus(focusInput)
		m.setFlash("agent "+msg.agent.ID+" started", false)
		return m, m.fetchAgents()

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	if m.focus == focusInput {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
	return m, nil
}

func isFixed(sel string) bool {
	return sel == selNew || sel == selArchived || sel == selIsolations || sel == selServices
}

func (m *Model) setFocus(f focus) {
	m.focus = f
	if f == focusInput {
		m.input.Focus()
	} else {
		m.input.Blur()
	}
	m.layout()
}

func (m *Model) handleKey(msg tea.KeyMsg) tea.Cmd {
	k := m.keys
	if k.Is(msg, "quit") {
		if m.stopFeed != nil {
			m.stopFeed()
		}
		return tea.Quit
	}
	if m.confirm != nil {
		c := m.confirm
		m.confirm = nil
		if msg.String() == "y" {
			return c.onYes()
		}
		m.setFlash("cancelled", false)
		return nil
	}
	if m.help {
		m.help = false
		return nil
	}
	if k.Is(msg, "help") {
		m.help = true
		return nil
	}
	if m.approving {
		return m.handleApprovalKey(msg)
	}

	// Pages that take over the main area handle their own keys first.
	if m.view == viewNewAgent && m.newForm != nil && m.focus == focusMain {
		return m.handleNewAgentKey(msg)
	}
	if m.view == viewIsolations && m.isoPage != nil && m.focus == focusMain {
		return m.handleIsoKey(msg)
	}
	if m.view == viewServices && m.svcPage != nil && m.focus == focusMain {
		return m.handleSvcKey(msg)
	}
	if m.settings != nil && m.focus == focusMain {
		return m.handleSettingsKey(msg)
	}

	switch {
	case k.Is(msg, "new_agent"):
		return m.openNewAgent()
	case k.Is(msg, "isolations"):
		return m.openIsolations()
	case k.Is(msg, "services"):
		return m.openServices()
	case k.Is(msg, "agent_settings"):
		return m.openSettings()
	case k.Is(msg, "approvals"):
		if m.openID != "" && len(m.convo(m.openID).pending()) > 0 {
			m.approving = true
			return nil
		}
		m.setFlash("no approval is pending", false)
		return nil
	case k.Is(msg, "interrupt"):
		if id := m.openID; id != "" {
			be := m.be
			return m.do("interrupt sent", false, func(ctx context.Context) error { return be.Interrupt(ctx, id) })
		}
		return nil
	case k.Is(msg, "focus_next"):
		m.cycleFocus(1)
		return nil
	case k.Is(msg, "focus_prev"):
		m.cycleFocus(-1)
		return nil
	}

	switch m.focus {
	case focusSidebar:
		return m.handleSidebarKey(msg)
	case focusConvo:
		m.handleScrollKey(msg)
		return nil
	case focusInput:
		return m.handleInputKey(msg)
	}
	return nil
}

func (m *Model) cycleFocus(dir int) {
	order := []focus{focusSidebar}
	switch {
	case m.view != viewAgent || m.settings != nil:
		order = append(order, focusMain)
	case m.openID != "":
		order = append(order, focusConvo, focusInput)
	}
	i := 0
	for j, f := range order {
		if f == m.focus {
			i = j
		}
	}
	m.setFocus(order[(i+dir+len(order))%len(order)])
}

func (m *Model) handleSidebarKey(msg tea.KeyMsg) tea.Cmd {
	k := m.keys
	switch {
	case k.Is(msg, "up"):
		return m.moveSel(-1)
	case k.Is(msg, "down"):
		return m.moveSel(1)
	case k.Is(msg, "toggle_archived"):
		m.showArchived = !m.showArchived
		return nil
	case k.Is(msg, "open"):
		switch m.sel {
		case selNew:
			return m.openNewAgent()
		case selIsolations:
			return m.openIsolations()
		case selServices:
			return m.openServices()
		case selArchived:
			m.showArchived = !m.showArchived
			return nil
		}
		if a, ok := m.agent(m.sel); ok && a.ArchivedAt == nil {
			m.setFocus(focusInput)
		}
		return nil
	case k.Is(msg, "archive"):
		a, ok := m.agent(m.sel)
		if !ok || a.ArchivedAt != nil {
			return nil
		}
		be, id := m.be, a.ID
		m.confirm = &confirmation{
			prompt: fmt.Sprintf("archive %q? its history is kept (y/n)", a.Name),
			onYes: func() tea.Cmd {
				return m.do("archived "+id, true, func(ctx context.Context) error { return be.Archive(ctx, id) })
			},
		}
		return nil
	}
	return nil
}

func (m *Model) handleScrollKey(msg tea.KeyMsg) {
	k := m.keys
	switch {
	case k.Is(msg, "scroll_up"):
		m.vp.LineUp(1)
	case k.Is(msg, "scroll_down"):
		m.vp.LineDown(1)
	case k.Is(msg, "page_up"):
		m.vp.HalfViewUp()
	case k.Is(msg, "page_down"):
		m.vp.HalfViewDown()
	case k.Is(msg, "top"):
		m.vp.GotoTop()
	case k.Is(msg, "bottom"):
		m.vp.GotoBottom()
	}
}

func (m *Model) handleInputKey(msg tea.KeyMsg) tea.Cmd {
	k := m.keys
	switch {
	case k.Is(msg, "back"):
		m.setFocus(focusSidebar)
		return nil
	case k.Is(msg, "page_up"), k.Is(msg, "page_down"):
		m.handleScrollKey(msg)
		return nil
	case k.Is(msg, "send"):
		text := strings.TrimSpace(m.input.Value())
		a, ok := m.agent(m.openID)
		if text == "" || !ok {
			return nil
		}
		if a.ArchivedAt != nil {
			m.setFlash("this agent is archived", true)
			return nil
		}
		m.input.Reset()
		m.drafts[a.ID] = ""
		m.layout()
		be, id := m.be, a.ID
		return m.do("", false, func(ctx context.Context) error { return be.Send(ctx, id, text) })
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m.layout()
	return cmd
}

func (m *Model) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	k := m.keys
	pending := m.convo(m.openID).pending()
	if len(pending) == 0 {
		m.approving = false
		return nil
	}
	var d driver.Decision
	switch {
	case k.Is(msg, "accept"):
		d = driver.Accept
	case k.Is(msg, "decline"):
		d = driver.Decline
	case k.Is(msg, "cancel_turn"):
		d = driver.Cancel
	case k.Is(msg, "back"):
		m.approving = false
		return nil
	default:
		return nil
	}
	m.approving = false
	be, id, ap := m.be, m.openID, pending[0].ID
	return m.do("", false, func(ctx context.Context) error { return be.Approve(ctx, id, ap, d) })
}

// layout sizes the conversation to whatever the input and bars leave.
func (m *Model) layout() {
	if m.width == 0 {
		return
	}
	sw := m.sidebarWidth()
	mw := m.width - sw - 1
	lines := min(max(m.input.LineCount(), 1), 6)
	m.input.SetWidth(max(mw-14, 10))
	m.input.SetHeight(lines)
	used := 2 + 1 + lines + 1 // header + rule, rule above input, input, status bar
	if m.openID != "" && len(m.convo(m.openID).pending()) > 0 {
		used += 2
	}
	if m.settings != nil {
		used += 2 + len(m.settings.fields) // title, fields, hint
	}
	m.vp.Width = mw
	m.vp.Height = max(m.height-used, 1)
}

func (m *Model) sidebarWidth() int { return max(22, min(34, m.width/4)) }

var errNoIsolation = errors.New("create an isolation first (ctrl+g)")
