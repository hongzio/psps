package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/isolation"
	"github.com/hongzio/gero/internal/store"
)

// The agent CLIs' own logins. Claude Code and Codex keep their credentials
// in the isolation, where the agent runs; the daemon only runs their login
// commands there with stdio piped, so a login works from any client, a
// phone's browser included. What passes through the daemon is the address
// to open, Codex's one-time code and the code Claude's login page ends on:
// held in memory for the login's length and never logged.

var cliTools = []driver.Kind{driver.Claude, driver.Codex}

// cliLoginTTL is how long a login waits for the browser: Codex's codes
// expire after 15 minutes, and Claude's are no use after that either.
var cliLoginTTL = 15 * time.Minute

// cliLoginKept is how long a finished login stays readable, for the client
// that started it to learn how it ended.
const cliLoginKept = 10 * time.Minute

func cliTool(s string) (driver.Kind, error) {
	switch k := driver.Kind(s); k {
	case driver.Claude, driver.Codex:
		return k, nil
	}
	return "", badRequest(fmt.Errorf("unknown agent CLI %q (claude, codex)", s))
}

// codexLogin logs Codex in under a CODEX_HOME of its own and moves the
// login into place only once it succeeded: `codex login` deletes the login
// it finds as it starts, so run in place, one abandoned halfway would leave
// the isolation logged out. Claude's keeps the old login until the new one
// is made.
const codexLogin = `set -e
rm -rf "$HOME"/.codex-login.*
t=$(mktemp -d "$HOME/.codex-login.XXXXXX")
trap 'rm -rf "$t"' EXIT
trap 'exit 143' TERM INT HUP
mkdir -p "$HOME/.codex"
if [ -f "$HOME/.codex/config.toml" ]; then cp "$HOME/.codex/config.toml" "$t/"; fi
CODEX_HOME="$t" codex login --device-auth
mv -f "$t/auth.json" "$HOME/.codex/auth.json"`

func cliLoginArgv(tool driver.Kind) []string {
	if tool == driver.Codex {
		return []string{"sh", "-c", codexLogin}
	}
	return []string{"claude", "auth", "login"}
}

// userEnv is the environment the user set on the isolation, which a token
// may still be put in, and which wins over an account.
func userEnv(iso store.Isolation) []string {
	env := make([]string, 0, len(iso.Env))
	for k, v := range iso.Env {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	return env
}

// runInIsolation runs argv as the agent user with env and returns its
// output, whatever it exited with.
func (m *Manager) runInIsolation(ctx context.Context, container string, env []string, argv ...string) (string, error) {
	p, err := m.cli.StartAsAgent(ctx, container, env, argv...)
	if err != nil {
		return "", err
	}
	p.Stdin.Close()
	var out []byte
	err = drain(p, func(b []byte) { out = append(out, b...) })
	return strings.TrimSpace(stripANSI(string(out))), err
}

// outputGrace is how long output is still read once the process exited,
// for whatever it wrote last.
const outputGrace = 2 * time.Second

// drain hands p's output to got until p exits and its output ends. Output
// held open past the exit (by something p left behind) is cut off after
// outputGrace, so a stray process cannot keep a login from ending.
func drain(p isolation.Proc, got func([]byte)) error {
	read := make(chan struct{})
	go func() {
		defer close(read)
		buf := make([]byte, 4096)
		for {
			n, err := p.Output.Read(buf)
			if n > 0 {
				got(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	err := p.Wait()
	select {
	case <-read:
	case <-time.After(outputGrace):
		p.Output.Close()
		<-read
	}
	p.Output.Close()
	return err
}

// CLIs reads each agent CLI's version and login from inside the isolation.
func (m *Manager) CLIs(ctx context.Context, name string) ([]api.CLI, error) {
	iso, err := m.store.Isolation(name)
	if err != nil {
		return nil, err
	}
	out := make([]api.CLI, len(cliTools))
	state, err := m.cli.State(ctx, iso.Container)
	if err == nil && state != "running" {
		err = fmt.Errorf("the isolation is %s", state)
	}
	var wg sync.WaitGroup
	for i, tool := range cliTools {
		out[i] = api.CLI{Tool: tool}
		if err != nil {
			out[i].Error = err.Error()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = m.cliStatus(ctx, iso, tool)
		}()
	}
	wg.Wait()
	used := m.accountsOf(name)
	for i := range out {
		out[i].Account = used[out[i].Tool]
		if l := m.findCLILogin(name, out[i].Tool); l != nil {
			v := l.snapshot()
			out[i].Login = &v
		}
	}
	return out, nil
}

const cliSep = "--gero--"

func (m *Manager) cliStatus(ctx context.Context, iso store.Isolation, tool driver.Kind) api.CLI {
	c := api.CLI{Tool: tool}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	script := "claude --version; echo " + cliSep + "; claude auth status --json"
	if tool == driver.Codex {
		script = "codex --version; echo " + cliSep + "; codex login status"
	}
	// Exit codes say "not logged in" as often as "broken": the output is
	// what is read.
	env := append(m.accountEnv(iso.Name), userEnv(iso)...)
	out, err := m.runInIsolation(ctx, iso.Container, env, "sh", "-c", script+" 2>&1")
	version, status, ok := strings.Cut(out, cliSep)
	if !ok {
		c.Error = clip(orStr(out, fmt.Sprint(err)), 300)
		return c
	}
	version, status = strings.TrimSpace(version), strings.TrimSpace(status)
	if tool == driver.Codex {
		parseCodexStatus(&c, version, status)
	} else {
		parseClaudeStatus(&c, version, status)
	}
	return c
}

func parseClaudeStatus(c *api.CLI, version, status string) {
	// "2.1.311 (Claude Code)"
	if f := strings.Fields(version); len(f) > 0 && strings.Contains(version, "Claude Code") {
		c.Version = f[0]
	}
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIKeySource     string `json:"apiKeySource"`
		Email            string `json:"email"`
		SubscriptionType string `json:"subscriptionType"`
	}
	i := strings.IndexByte(status, '{')
	if i < 0 || json.Unmarshal([]byte(status[i:]), &st) != nil {
		c.Error = clip(firstLine(orStr(status, version)), 300)
		return
	}
	c.LoggedIn, c.Email = st.LoggedIn, st.Email
	switch st.AuthMethod {
	case "", "none":
	case "claude.ai":
		c.Method = "Claude subscription"
		if st.SubscriptionType != "" {
			c.Method += " (" + st.SubscriptionType + ")"
		}
	case "oauth_token":
		c.Method = "CLAUDE_CODE_OAUTH_TOKEN"
	case "api_key":
		c.Method = "API key"
		if st.APIKeySource != "" {
			c.Method += " (" + st.APIKeySource + ")"
		}
	default:
		c.Method = st.AuthMethod
	}
}

func parseCodexStatus(c *api.CLI, version, status string) {
	// "codex-cli 0.164.0"
	if f := strings.Fields(version); len(f) == 2 && f[0] == "codex-cli" {
		c.Version = f[1]
	}
	line := firstLine(status)
	switch {
	case strings.HasPrefix(line, "Logged in using "):
		c.LoggedIn = true
		method, _, _ := strings.Cut(strings.TrimPrefix(line, "Logged in using "), " - ")
		if strings.Contains(method, "API key") {
			method = "API key"
		}
		c.Method = method
	case strings.HasPrefix(line, "Not logged in"):
	default:
		if c.Version == "" {
			line = firstLine(version)
		}
		c.Error = clip(line, 300)
	}
}

// cliLogin is one login in progress, or just ended.
type cliLogin struct {
	mu     sync.Mutex
	view   api.CLILogin
	stdin  io.WriteCloser
	cancel context.CancelFunc
	out    []byte // what the CLI wrote, escape codes and all
	sent   bool
	ended  time.Time
}

func (l *cliLogin) snapshot() api.CLILogin {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.view
}

var (
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)
	urlRE  = regexp.MustCompile(`https://[^\s"'<>]+`)
	// Codex's one-time code: "LK6M-R8WQK".
	codeRE = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4,6}\b`)
)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func orStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// readLocked takes what the CLI said so far: where to go, and for Codex
// the code to type there.
func (l *cliLogin) readLocked() {
	if l.view.State != api.CLILoginStarting {
		return
	}
	text := stripANSI(string(l.out))
	u := urlRE.FindString(text)
	if u == "" {
		return
	}
	if l.view.Tool == driver.Codex {
		_, after, _ := strings.Cut(text, u)
		code := codeRE.FindString(after)
		if code == "" {
			return
		}
		l.view.Code = code
	}
	l.view.URL, l.view.State = u, api.CLILoginOpen
}

// lastWords is the CLI's final line, without the prompt it was printed
// after ("Paste code here if prompted > Login failed: …").
func lastWords(out []byte) string {
	s := lastLine(stripANSI(string(out)))
	if i := strings.LastIndex(s, "> "); i >= 0 && strings.Contains(s[:i], "Paste code") {
		s = s[i+2:]
	}
	return clip(strings.TrimSpace(s), 300)
}

func cliKey(iso string, tool driver.Kind) string { return iso + "/" + string(tool) }

// findCLILogin is the isolation's login for tool, if one is running or ended
// recently.
func (m *Manager) findCLILogin(iso string, tool driver.Kind) *cliLogin {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.cliLogins[cliKey(iso, tool)]
	if l == nil {
		return nil
	}
	l.mu.Lock()
	ended := l.ended
	l.mu.Unlock()
	if !ended.IsZero() && time.Since(ended) > cliLoginKept {
		delete(m.cliLogins, cliKey(iso, tool))
		return nil
	}
	return l
}

// StartCLILogin runs tool's login inside the isolation, replacing one
// already running, and answers once the CLI says where to go.
func (m *Manager) StartCLILogin(ctx context.Context, name, toolName string) (api.CLILogin, error) {
	tool, err := cliTool(toolName)
	if err != nil {
		return api.CLILogin{}, err
	}
	iso, err := m.store.Isolation(name)
	if err != nil {
		return api.CLILogin{}, err
	}
	if _, err := m.ensureIsolation(ctx, name); err != nil {
		return api.CLILogin{}, err
	}
	lctx, cancel := context.WithTimeout(m.ctx, cliLoginTTL)
	// timeout(1) inside as well: should the daemon die without stopping
	// it, the login still ends there. It is also what passes the daemon's
	// SIGTERM on to everything the login started.
	argv := append([]string{"timeout", strconv.Itoa(int(cliLoginTTL/time.Second) + 30)}, cliLoginArgv(tool)...)
	p, err := m.cli.StartAsAgent(lctx, iso.Container, nil, argv...)
	if err != nil {
		cancel()
		return api.CLILogin{}, err
	}
	l := &cliLogin{stdin: p.Stdin, cancel: cancel, view: api.CLILogin{
		Tool: tool, State: api.CLILoginStarting, NeedsCode: tool == driver.Claude, Expires: time.Now().Add(cliLoginTTL),
	}}
	m.mu.Lock()
	if m.cliLogins == nil {
		m.cliLogins = map[string]*cliLogin{}
	}
	old := m.cliLogins[cliKey(name, tool)]
	m.cliLogins[cliKey(name, tool)] = l
	m.mu.Unlock()
	if old != nil {
		old.cancel()
	}
	go m.runCLILogin(lctx, name, l, p)
	return l.await(ctx, 20*time.Second, api.CLILoginStarting), nil
}

// await waits up to d for the login to leave state.
func (l *cliLogin) await(ctx context.Context, d time.Duration, state string) api.CLILogin {
	deadline := time.Now().Add(d)
	for {
		v := l.snapshot()
		if v.State != state || time.Now().After(deadline) || ctx.Err() != nil {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *Manager) runCLILogin(lctx context.Context, iso string, l *cliLogin, p isolation.Proc) {
	err := drain(p, func(b []byte) {
		l.mu.Lock()
		if len(l.out) < 64<<10 {
			l.out = append(l.out, b...)
		}
		l.readLocked()
		l.mu.Unlock()
	})
	l.mu.Lock()
	l.ended = time.Now()
	words := lastWords(l.out)
	tool := l.view.Tool
	switch {
	case err == nil:
		l.view.State, l.view.Message = api.CLILoginDone, orStr(words, "logged in")
	case errors.Is(lctx.Err(), context.DeadlineExceeded):
		l.view.State, l.view.Message = api.CLILoginFailed, "the login expired; start it again"
	case lctx.Err() != nil:
		l.view.State, l.view.Message = api.CLILoginFailed, "cancelled"
	default:
		l.view.State, l.view.Message = api.CLILoginFailed, orStr(words, err.Error())
	}
	l.view.URL, l.view.Code = "", ""
	done := l.view.State == api.CLILoginDone
	l.mu.Unlock()
	l.cancel()
	if done {
		m.log.Printf("isolation %s: %s logged in", iso, tool)
		m.cliChanged(iso, tool)
	}
}

// SendCLICode hands Claude's login the code its page showed, and answers
// with how the login ended (or that it is still going).
func (m *Manager) SendCLICode(ctx context.Context, name, toolName, code string) (api.CLILogin, error) {
	tool, err := cliTool(toolName)
	if err != nil {
		return api.CLILogin{}, err
	}
	l := m.findCLILogin(name, tool)
	if l == nil {
		return api.CLILogin{}, httpError{404, fmt.Errorf("no %s login in progress in %s", tool, name)}
	}
	code = strings.TrimSpace(code)
	l.mu.Lock()
	switch {
	case tool != driver.Claude:
		err = errors.New("codex notices the login by itself; there is no code to paste")
	case l.view.State != api.CLILoginOpen:
		err = fmt.Errorf("the login is %s", l.view.State)
	case l.sent:
		err = errors.New("a code was already handed over; start the login again to try another")
	case code == "" || strings.ContainsAny(code, "\r\n"):
		err = errors.New("paste the code the login page shows")
	}
	if err == nil {
		if _, err = io.WriteString(l.stdin, code+"\n"); err == nil {
			l.sent, l.view.State = true, api.CLILoginVerifying
		}
	}
	l.mu.Unlock()
	if err != nil {
		return api.CLILogin{}, badRequest(err)
	}
	return l.await(ctx, 30*time.Second, api.CLILoginVerifying), nil
}

func (m *Manager) CLILogin(name, toolName string) (api.CLILogin, error) {
	tool, err := cliTool(toolName)
	if err != nil {
		return api.CLILogin{}, err
	}
	l := m.findCLILogin(name, tool)
	if l == nil {
		return api.CLILogin{}, httpError{404, fmt.Errorf("no %s login in %s", tool, name)}
	}
	return l.snapshot(), nil
}

// CancelCLILogin stops the login and forgets it.
func (m *Manager) CancelCLILogin(name, toolName string) error {
	tool, err := cliTool(toolName)
	if err != nil {
		return err
	}
	m.mu.Lock()
	l := m.cliLogins[cliKey(name, tool)]
	delete(m.cliLogins, cliKey(name, tool))
	m.mu.Unlock()
	if l != nil {
		l.cancel()
	}
	return nil
}

// CLILogout removes the CLI's stored login from the isolation. A token in
// the isolation's environment is not touched.
func (m *Manager) CLILogout(ctx context.Context, name, toolName string) error {
	tool, err := cliTool(toolName)
	if err != nil {
		return err
	}
	iso, err := m.store.Isolation(name)
	if err != nil {
		return err
	}
	if _, err := m.ensureIsolation(ctx, name); err != nil {
		return err
	}
	argv := []string{"claude", "auth", "logout"}
	if tool == driver.Codex {
		argv = []string{"codex", "logout"}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := m.runInIsolation(ctx, iso.Container, nil, argv...); err != nil {
		return fmt.Errorf("%s: %s", strings.Join(argv, " "), orStr(out, err.Error()))
	}
	m.log.Printf("isolation %s: %s logged out", name, tool)
	m.cliChanged(name, tool)
	return nil
}

// cliChanged restarts the isolation's agents of that kind at their next
// idle moment: Codex reads its login only when it starts, and neither
// should carry on with one that was removed.
func (m *Manager) cliChanged(iso string, tool driver.Kind) {
	m.mu.Lock()
	var affected []*runtime
	for _, r := range m.runtimes {
		if r.agent.Isolation == iso && r.agent.Kind == tool {
			affected = append(affected, r)
		}
	}
	m.mu.Unlock()
	for _, r := range affected {
		r.envChanged()
	}
}
