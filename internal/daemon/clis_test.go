package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/event"
	"github.com/hongzio/gero/internal/isolation"
)

// The agent CLIs as their login commands behave with no terminal, as seen
// in the image: Claude prints where to go and reads the code its page ends
// on; Codex prints a device code and waits until it is entered. Each keeps
// its login in FAKE_HOME. Codex's other commands are the app-server fake.
const fakeClaudeCLI = `#!/bin/sh
case "$1 $2" in
"--version ") echo "2.1.311 (Claude Code)" ;;
"auth status")
  if [ -n "$CLAUDE_CODE_OAUTH_TOKEN" ]; then echo '{"loggedIn": true, "authMethod": "oauth_token"}'
  elif [ -f "$HOME/.claude-login" ]; then echo '{"loggedIn": true, "authMethod": "claude.ai", "email": "me@example.com", "subscriptionType": "max"}'
  else echo '{"loggedIn": false, "authMethod": "none"}'; exit 1; fi ;;
"auth login")
  printf "Opening browser to sign in…\nIf the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&state=abc\nPaste code here if prompted > "
  read code
  if [ "$code" = "good#abc" ]; then touch "$HOME/.claude-login"; echo "Login successful."; exit 0; fi
  echo "Login failed: That code wasn't accepted."; exit 1 ;;
"auth logout") rm -f "$HOME/.claude-login"; echo "Successfully logged out from your Anthropic account." ;;
*) echo "unexpected: $*"; exit 2 ;;
esac
`

// fakeCodexCLI keeps its login in $CODEX_HOME/auth.json and, as the real
// one does, deletes it the moment a login starts.
const fakeCodexCLI = `#!/bin/sh
c="${CODEX_HOME:-$HOME/.codex}"
case "$1 $2" in
"--version ") echo "codex-cli 0.164.0" ;;
"login status") if [ -f "$c/auth.json" ]; then cat "$c/auth.json"; else echo "Not logged in"; exit 1; fi ;;
"login --device-auth")
  rm -f "$c/auth.json"
  printf 'Welcome to Codex [v\033[90m0.164.0\033[0m]\n\n1. Open this link in your browser and sign in to your account\n   \033[94mhttps://auth.openai.com/codex/device\033[0m\n\n2. Enter this one-time code \033[90m(expires in 15 minutes)\033[0m\n   \033[94mLK6M-R8WQK\033[0m\n\n'
  while [ ! -f "$HOME/approved" ]; do sleep 0.05; done
  echo "Logged in using ChatGPT" > "$c/auth.json"; echo "Successfully logged in" ;;
"logout ") rm -f "$c/auth.json"; echo "Successfully logged out" ;;
*) GERO_FAKE_CODEX=1 exec "$GERO_TEST_EXE" "$@" ;;
esac
`

// cliEnv is a daemon whose isolation has the fake CLIs, and the directory
// they keep their logins in.
func cliEnv(t *testing.T) (*testEnv, *Manager, string) {
	t.Helper()
	e := newTestEnv(t)
	bin := t.TempDir()
	exe, _ := os.Executable()
	t.Setenv("GERO_TEST_EXE", exe)
	for name, script := range map[string]string{"claude": fakeClaudeCLI, "codex": fakeCodexCLI, "timeout": "#!/bin/sh\nshift\nexec \"$@\"\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.fc.mu.Lock()
	e.fc.bin = bin
	e.fc.mu.Unlock()
	m, stop := e.daemon()
	t.Cleanup(stop)
	return e, m, filepath.Join(e.fc.base, "gero-dev", "home")
}

// loggedInCodex stores a Codex login in the isolation's home, as account.
func loggedInCodex(t *testing.T, home, account string) {
	t.Helper()
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("Logged in using "+account+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliOf(t *testing.T, m *Manager, tool driver.Kind) api.CLI {
	t.Helper()
	cs, err := m.CLIs(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Tool == tool {
			return c
		}
	}
	t.Fatalf("no %s in %+v", tool, cs)
	return api.CLI{}
}

func TestCLIsAreReadAsAnAgentFindsThem(t *testing.T) {
	_, m, _ := cliEnv(t)
	cs, err := m.CLIs(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Tool != driver.Claude || cs[1].Tool != driver.Codex {
		t.Fatalf("%+v", cs)
	}
	for _, c := range cs {
		if c.LoggedIn || c.Error != "" || c.Method != "" {
			t.Errorf("logged in with nothing stored: %+v", c)
		}
	}
	if cs[0].Version != "2.1.311" || cs[1].Version != "0.164.0" {
		t.Errorf("versions: %q %q", cs[0].Version, cs[1].Version)
	}

	// A token in the isolation's environment is what agents there start
	// with, so it counts.
	tok := "sk-ant-oat01-test"
	if err := m.SetEnv("dev", api.SetEnv{"CLAUDE_CODE_OAUTH_TOKEN": &tok}); err != nil {
		t.Fatal(err)
	}
	if c := cliOf(t, m, driver.Claude); !c.LoggedIn || c.Method != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("with a token: %+v", c)
	}

	if _, err := m.CLIs(context.Background(), "nope"); err == nil {
		t.Error("an unknown isolation answered")
	}
}

func TestClaudeLoginTakesTheCodeItsPageShows(t *testing.T) {
	_, m, home := cliEnv(t)
	ctx := context.Background()
	l, err := m.StartCLILogin(ctx, "dev", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != api.CLILoginOpen || !l.NeedsCode || !strings.HasPrefix(l.URL, "https://claude.com/cai/oauth/authorize?") || strings.HasSuffix(l.URL, "Paste") {
		t.Fatalf("%+v", l)
	}
	if c := cliOf(t, m, driver.Claude); c.Login == nil || c.Login.State != api.CLILoginOpen {
		t.Errorf("the login in progress is not shown: %+v", c)
	}

	l, err = m.SendCLICode(ctx, "dev", "claude", "  good#abc \n")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != api.CLILoginDone || l.Message != "Login successful." || l.URL != "" {
		t.Fatalf("%+v", l)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude-login")); err != nil {
		t.Fatal("the login was not stored in the isolation")
	}
	c := cliOf(t, m, driver.Claude)
	if !c.LoggedIn || c.Method != "Claude subscription (max)" || c.Email != "me@example.com" || c.Login == nil || c.Login.State != api.CLILoginDone {
		t.Errorf("%+v", c)
	}

	if err := m.CLILogout(ctx, "dev", "claude"); err != nil {
		t.Fatal(err)
	}
	if c := cliOf(t, m, driver.Claude); c.LoggedIn {
		t.Errorf("still logged in after logging out: %+v", c)
	}
}

func TestClaudeLoginRefusingTheCodeEndsIt(t *testing.T) {
	_, m, _ := cliEnv(t)
	ctx := context.Background()
	if _, err := m.StartCLILogin(ctx, "dev", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SendCLICode(ctx, "dev", "claude", ""); err == nil {
		t.Error("an empty code was handed over")
	}
	l, err := m.SendCLICode(ctx, "dev", "claude", "bad#abc")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != api.CLILoginFailed || l.Message != "Login failed: That code wasn't accepted." {
		t.Fatalf("%+v", l)
	}
	if _, err := m.SendCLICode(ctx, "dev", "claude", "good#abc"); err == nil {
		t.Error("a second code went to a login that ended")
	}
	if c := cliOf(t, m, driver.Claude); c.LoggedIn {
		t.Errorf("%+v", c)
	}
}

func TestCodexLoginShowsTheDeviceCodeAndRestartsItsAgents(t *testing.T) {
	e, m, home := cliEnv(t)
	ctx := context.Background()
	loggedInCodex(t, home, "an API key - sk-old")
	a, err := m.CreateAgent(ctx, api.CreateAgent{Isolation: "dev", Kind: driver.Codex, Prompt: "one"})
	if err != nil {
		t.Fatal(err)
	}
	e.waitFor("first answer", func() bool { return e.hasAssistant(a.ID, "echo: one") && status(m, a.ID) == event.StatusIdle })

	l, err := m.StartCLILogin(ctx, "dev", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if l.State != api.CLILoginOpen || l.URL != "https://auth.openai.com/codex/device" || l.Code != "LK6M-R8WQK" || l.NeedsCode {
		t.Fatalf("%+v", l)
	}
	if _, err := m.SendCLICode(ctx, "dev", "codex", "LK6M-R8WQK"); err == nil {
		t.Error("codex took a pasted code")
	}
	// Codex deletes the login it finds as it starts; the one in place is
	// untouched until the new one is made.
	if c := cliOf(t, m, driver.Codex); !c.LoggedIn || c.Method != "API key" {
		t.Fatalf("the login in place went as the new one started: %+v", c)
	}
	// The operator enters the code in a browser; Codex notices.
	os.WriteFile(filepath.Join(home, "approved"), nil, 0o600)
	e.waitFor("the login to end", func() bool {
		l, _ := m.CLILogin("dev", "codex")
		return l.State == api.CLILoginDone
	})
	if c := cliOf(t, m, driver.Codex); !c.LoggedIn || c.Method != "ChatGPT" {
		t.Errorf("%+v", c)
	}
	// Codex reads its login when it starts: the idle agent is replaced.
	e.waitFor("the agent's process replaced", func() bool {
		ag, _ := m.Agent(a.ID)
		return !ag.State.Alive
	})
}

func TestACLILoginEndsWhenCancelledOrExpired(t *testing.T) {
	_, m, home := cliEnv(t)
	ctx := context.Background()
	loggedInCodex(t, home, "ChatGPT")
	if _, err := m.StartCLILogin(ctx, "dev", "codex"); err != nil {
		t.Fatal(err)
	}
	l := m.findCLILogin("dev", driver.Codex)
	if err := m.CancelCLILogin("dev", "codex"); err != nil {
		t.Fatal(err)
	}
	if v := l.await(ctx, 10*time.Second, api.CLILoginOpen); v.State != api.CLILoginFailed || v.Message != "cancelled" || v.Code != "" {
		t.Fatalf("%+v", v)
	}
	if _, err := m.CLILogin("dev", "codex"); err == nil {
		t.Error("a cancelled login is still listed")
	}
	// What was logged in before still is.
	if c := cliOf(t, m, driver.Codex); !c.LoggedIn || c.Method != "ChatGPT" {
		t.Errorf("a cancelled login logged codex out: %+v", c)
	}

	was := cliLoginTTL
	cliLoginTTL = 300 * time.Millisecond
	t.Cleanup(func() { cliLoginTTL = was })
	if _, err := m.StartCLILogin(ctx, "dev", "codex"); err != nil {
		t.Fatal(err)
	}
	l = m.findCLILogin("dev", driver.Codex)
	if v := l.await(ctx, 10*time.Second, api.CLILoginOpen); v.State != api.CLILoginFailed || v.Message != "the login expired; start it again" {
		t.Fatalf("%+v", v)
	}
	if c := cliOf(t, m, driver.Codex); !c.LoggedIn {
		t.Errorf("an expired login logged codex out: %+v", c)
	}

	if _, err := m.StartCLILogin(ctx, "dev", "gemini"); err == nil {
		t.Error("an unknown CLI started a login")
	}
}

// Something a login leaves behind can hold its output open; the login
// still ends with what was written before the exit.
func TestDrainEndsWithTheProcessNotItsOutput(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & echo last words")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p, err := isolation.StartProc(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	var out []byte
	start := time.Now()
	if err := drain(p, func(b []byte) { out = append(out, b...) }); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > outputGrace+3*time.Second {
		t.Errorf("took %v", took)
	}
	if strings.TrimSpace(string(out)) != "last words" {
		t.Errorf("%q", out)
	}
}
