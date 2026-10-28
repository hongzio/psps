package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hongzio/gero/internal/api"
	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/awsproxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

// echoService is a proxy that answers each line with who asked and what the
// default env is, so a test can tell the connection reached the right
// isolation's instance with the current configuration.
type echoService struct{}

func (echoService) Name() string                           { return "echo" }
func (echoService) Ports() []int                           { return []int{3020} }
func (echoService) Summary() string                        { return "test" }
func (echoService) Settings() []proxy.Field                { return nil }
func (echoService) NormalizeSettings(map[string]any) error { return nil }
func (echoService) TokenKeys(x store.Exposure) []string    { return x.Envs }
func (echoService) Managed() []string                      { return proxy.SkillDirs("echo") }
func (echoService) Fields() []proxy.Field {
	return []proxy.Field{{Key: "greeting", Kind: proxy.String, Flag: "greeting"}}
}
func (echoService) Normalize(name string, env map[string]any) ([]string, error) {
	env["greeting"] = proxy.StringOr(env, "greeting", "hello")
	return nil, nil
}
func (echoService) Workspace(v proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{
		Env:   []string{"ECHO_URL=http://127.0.0.1:3020", "ECHO_TOKEN=" + v.Tokens[v.Exposure.Default]},
		Files: proxy.SkillFiles("echo", "echo skill"),
	}, nil
}
func (echoService) Check(context.Context, string, json.RawMessage, *creds.Store) (string, error) {
	return "ok", nil
}
func (echoService) New(h proxy.Host) proxy.Instance     { return echoInstance{h} }
func (echoService) Refusals(proxy.View) []proxy.Refusal { return nil }

type echoInstance struct{ h proxy.Host }

func (e echoInstance) Serve(_ int, l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			sc := bufio.NewScanner(c)
			for sc.Scan() {
				v, _ := e.h.View()
				var cfg struct{ Greeting string }
				v.Decode(v.Exposure.Default, &cfg)
				fmt.Fprintf(c, "%s %s from %s\n", cfg.Greeting, sc.Text(), e.h.Name())
			}
		}()
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func ask(t *testing.T, port int, line string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return "dial: " + err.Error()
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintln(c, line)
	reply, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "read: " + err.Error()
	}
	return strings.TrimSpace(reply)
}

func TestProxiesReachTheIsolationAndFollowItsConfiguration(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	port := freePort(t)
	m.SetProxies(proxy.NewRegistry(echoService{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return port }

	if _, err := m.PutEnv(ctx, "echo", "prod", proxy.Patch{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "echo", api.Expose{Envs: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensureIsolation(ctx, testIsoID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the tunnel", func() bool { return m.tunnelStatus(testIsoID) == "up" })

	// A connection made inside the isolation reaches this isolation's
	// instance, with the env's current definition.
	if got := ask(t, port, "ping"); got != "hello ping from dev" {
		t.Fatalf("reply = %q", got)
	}
	if _, err := m.PutEnv(ctx, "echo", "prod", proxy.Patch{Set: map[string]any{"greeting": "hi"}}, false); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, port, "again"); got != "hi again from dev" {
		t.Fatalf("an edit did not reach the proxy: %q", got)
	}

	// The isolation got the skill and the environment, token included; and
	// an agent process starts with the same environment.
	req, _ := e.fc.lastSync()
	var envFile, skill string
	for _, f := range req.Files {
		switch {
		case f.Path == EnvFile:
			envFile = f.Content
		case strings.HasSuffix(f.Path, "gero-echo/SKILL.md"):
			skill = f.Content
		}
	}
	toks, _ := e.st.Tokens(testIsoID, "echo")
	if !strings.Contains(envFile, "export ECHO_URL='http://127.0.0.1:3020'") || !strings.Contains(envFile, toks["prod"]) || skill != "echo skill" {
		t.Fatalf("sync request: %+v", req)
	}
	if env := strings.Join(m.isolationEnv(testIsoID), "\n"); !strings.Contains(env, "ECHO_TOKEN="+toks["prod"]) {
		t.Fatalf("agent env lacks the proxy's: %s", env)
	}

	// Turning the service off removes what it wrote.
	if err := m.Unexpose(ctx, "dev", "echo"); err != nil {
		t.Fatal(err)
	}
	req, _ = e.fc.lastSync()
	if !containsString(req.Remove, "/home/agent/.claude/skills/gero-echo") {
		t.Fatalf("the skill was not removed: %+v", req.Remove)
	}
	for _, f := range req.Files {
		if f.Path == EnvFile && strings.Contains(f.Content, "ECHO_URL") {
			t.Fatal("the environment still names a service that is off")
		}
	}
	if toks, _ := e.st.Tokens(testIsoID, "echo"); len(toks) != 0 {
		t.Fatalf("tokens outlived the exposure: %v", toks)
	}
}

func TestATunnelComesBackAfterTheContainerRestarts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	port := freePort(t)
	m.SetProxies(proxy.NewRegistry(echoService{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return port }
	m.PutEnv(ctx, "echo", "prod", proxy.Patch{}, true)
	m.Expose(ctx, "dev", "echo", api.Expose{Envs: []string{"prod"}})
	m.ensureIsolation(ctx, testIsoID)
	e.waitFor("the tunnel", func() bool { return m.tunnelStatus(testIsoID) == "up" })
	_, before := e.fc.lastSync()

	// The container goes down and comes back: the tunnel through it dies
	// with it, and the next ensure brings both back.
	e.fc.reboot(testCtr, false)
	e.waitFor("the tunnel to notice", func() bool { return m.tunnelStatus(testIsoID) != "up" })
	if got := ask(t, port, "gone"); !strings.HasPrefix(got, "dial") && !strings.HasPrefix(got, "read") {
		t.Fatalf("a dead tunnel answered: %q", got)
	}
	// As if the health cache had run out, as it does five seconds on.
	st := m.isoLock(testIsoID)
	st.mu.Lock()
	st.healthy = time.Time{}
	st.mu.Unlock()
	if _, err := m.ensureIsolation(ctx, testIsoID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the tunnel again", func() bool { return m.tunnelStatus(testIsoID) == "up" })
	if got := ask(t, port, "back"); got != "hello back from dev" {
		t.Fatalf("reply = %q", got)
	}
	// A restarted container has a fresh root filesystem, so it is synced
	// again even though nothing changed.
	if _, after := e.fc.lastSync(); after <= before {
		t.Fatalf("no sync after the restart (%d -> %d)", before, after)
	}
}

// The AWS broker end to end: a token minted per isolation, handed over in
// env.sh, accepted by the broker on the far side of the tunnel, and the role
// assumed by a fake aws CLI under the isolation's session name.
func TestTheAWSBrokerIssuesThroughTheTunnel(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	port := freePort(t)
	m.SetProxies(proxy.NewRegistry(awsproxy.Service{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return port }

	dir := t.TempDir()
	cli := filepath.Join(dir, "aws")
	os.WriteFile(cli, []byte(`#!/bin/sh
echo "$@" > `+dir+`/argv
printf '{"Credentials": {"AccessKeyId": "ASIAFAKE", "SecretAccessKey": "s", "SessionToken": "t", "Expiration": "2099-01-01T00:00:00Z"}}'
`), 0o755)
	patch := proxy.Patch{Set: map[string]any{"role_arn": "arn:aws:iam::1:role/r", "aws_profile": "dev",
		"aws_cli": cli, "instances": []string{"i-0a66b609cd50bb881=session"}}}
	if _, err := m.PutEnv(ctx, "aws", "dev", patch, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "aws", api.Expose{Envs: []string{"dev"}}); err != nil {
		t.Fatal(err)
	}
	m.ensureIsolation(ctx, testIsoID)
	e.waitFor("the tunnel", func() bool { return m.tunnelStatus(testIsoID) == "up" })

	req, _ := e.fc.lastSync()
	var token string
	for _, f := range req.Files {
		if f.Path == EnvFile {
			for _, line := range strings.Split(f.Content, "\n") {
				if v, ok := strings.CutPrefix(line, "export AWS_CONTAINER_AUTHORIZATION_TOKEN="); ok {
					token = strings.Trim(v, "'")
				}
			}
		}
	}
	if token == "" {
		t.Fatalf("env.sh has no token: %+v", req.Files)
	}
	get := func(auth string) int {
		r, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/creds", port), nil)
		r.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("guess"); code != 401 {
		t.Fatalf("a wrong token got %d", code)
	}
	if code := get(token); code != 200 {
		t.Fatalf("the isolation's token got %d", code)
	}
	if argv, _ := os.ReadFile(filepath.Join(dir, "argv")); !strings.Contains(string(argv), "--role-session-name gero-dev") {
		t.Fatalf("argv = %s", argv)
	}
}

// mcpService is a proxy shaped like Notion's: an MCP server per env, and a
// browser login.
type mcpService struct {
	echoService
	finished *[]string
}

func (mcpService) Name() string { return "mcpx" }
func (mcpService) Workspace(v proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{
		Env: []string{"MCPX_TOKEN=" + v.Tokens[v.Exposure.Default]},
		MCP: []proxy.MCPServer{{Name: "mcpx", URL: "http://127.0.0.1:3020/mcp", TokenEnv: "MCPX_TOKEN"}},
	}, nil
}
func (mcpService) Managed() []string { return nil }
func (mcpService) StartLogin(_ context.Context, env, redirect string) (proxy.Login, error) {
	return proxy.Login{URL: "https://login.example/authorize?state=s1", Redirect: redirect, Expires: time.Now().Add(time.Minute), State: "s1"}, nil
}
func (s mcpService) FinishLogin(_ context.Context, _ *creds.Store, env string, l proxy.Login, pasted string) error {
	if !strings.Contains(pasted, "state="+l.State.(string)) {
		return fmt.Errorf("wrong state")
	}
	*s.finished = append(*s.finished, env)
	return nil
}

func TestMCPServersReachTheLaunchAndAnAgentOwnedConfig(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	m.SetProxies(proxy.NewRegistry(mcpService{finished: new([]string)}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return freePort(t) }
	m.PutEnv(ctx, "mcpx", "main", proxy.Patch{}, true)
	if _, err := m.Expose(ctx, "dev", "mcpx", api.Expose{Envs: []string{"main"}}); err != nil {
		t.Fatal(err)
	}
	m.ensureIsolation(ctx, testIsoID)
	req, _ := e.fc.lastSync()
	toks, _ := e.st.Tokens(testIsoID, "mcpx")
	var cfg *shimproto.SyncFile
	for i, f := range req.Files {
		if f.Path == MCPFile {
			cfg = &req.Files[i]
		}
	}
	if cfg == nil || cfg.Mode != 0o600 || !cfg.Agent || !strings.Contains(cfg.Content, `"Authorization": "Bearer `+toks["main"]+`"`) {
		t.Fatalf("mcp.json: %+v", cfg)
	}
	env, servers := m.launchEnv(testIsoID)
	if !containsString(env, "MCPX_TOKEN="+toks["main"]) || len(servers) != 1 {
		t.Fatalf("%v %v", env, servers)
	}
	claude := launchArgv(driver.Claude, []string{"claude", "-p"}, servers)
	if strings.Join(claude, " ") != "claude -p --mcp-config "+MCPFile {
		t.Fatalf("claude argv %v", claude)
	}
	// Codex: overrides before the subcommand, the token by variable name,
	// never on the command line.
	codex := strings.Join(launchArgv(driver.Codex, []string{"codex", "app-server"}, servers), " ")
	if codex != `codex -c mcp_servers.mcpx.url="http://127.0.0.1:3020/mcp" -c mcp_servers.mcpx.bearer_token_env_var="MCPX_TOKEN" app-server` || strings.Contains(codex, toks["main"]) {
		t.Fatalf("codex argv %s", codex)
	}
	if err := m.Unexpose(ctx, "dev", "mcpx"); err != nil {
		t.Fatal(err)
	}
	if req, _ = e.fc.lastSync(); !containsString(req.Remove, MCPFile) {
		t.Fatalf("mcp.json outlived the service: %+v", req.Remove)
	}
	if _, servers = m.launchEnv(testIsoID); len(servers) != 0 {
		t.Fatalf("%v", servers)
	}
}

func TestABrowserLoginIsHeldByTheDaemonAndSpentOnce(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	finished := new([]string)
	m.SetProxies(proxy.NewRegistry(mcpService{finished: finished}, echoService{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.PutEnv(ctx, "mcpx", "main", proxy.Patch{}, true)
	m.PutEnv(ctx, "echo", "main", proxy.Patch{}, true)

	if _, err := m.StartLogin(ctx, "echo", "main", ""); err == nil {
		t.Fatal("a service with no login started one")
	}
	for _, bad := range []string{"https://evil.example/callback", "http://10.0.0.1/callback", "javascript:x"} {
		if _, err := m.StartLogin(ctx, "mcpx", "main", bad); err == nil {
			t.Errorf("redirect %q accepted", bad)
		}
	}
	l, err := m.StartLogin(ctx, "mcpx", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Redirect, "http://127.0.0.1:") || l.ID == "" || strings.Contains(l.URL, l.ID) {
		t.Fatalf("%+v", l)
	}
	if err := m.FinishLogin(ctx, "mcpx", "other", l.ID, "?state=s1"); err == nil {
		t.Fatal("a login finished for another env")
	}
	// That attempt spent it.
	if err := m.FinishLogin(ctx, "mcpx", "main", l.ID, "?state=s1"); err == nil {
		t.Fatal("a spent login finished")
	}
	l, _ = m.StartLogin(ctx, "mcpx", "main", "http://127.0.0.1:5555/callback")
	if err := m.FinishLogin(ctx, "mcpx", "main", l.ID, "http://127.0.0.1:5555/callback?code=c&state=s1"); err != nil {
		t.Fatal(err)
	}
	if len(*finished) != 1 || (*finished)[0] != "main" {
		t.Fatalf("%v", *finished)
	}
}

// A proxy knows its isolation by ID, and says the name as it is now:
// renamed, the isolation keeps its tunnel and its agents' tokens, and the
// proxies speak of it by its new name.
func TestARenamedIsolationsProxiesServeItUnderItsNewName(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m, stop := e.daemon()
	defer stop()
	port := freePort(t)
	m.SetProxies(proxy.NewRegistry(echoService{}), &creds.Store{Path: filepath.Join(t.TempDir(), "credentials")})
	m.tunnelPort = func(int) int { return port }
	if _, err := m.PutEnv(ctx, "echo", "prod", proxy.Patch{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Expose(ctx, "dev", "echo", api.Expose{Envs: []string{"prod"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ensureIsolation(ctx, testIsoID); err != nil {
		t.Fatal(err)
	}
	e.waitFor("the tunnel", func() bool { return m.tunnelStatus(testIsoID) == "up" })
	toks, _ := e.st.Tokens(testIsoID, "echo")
	e.fc.mu.Lock()
	tunnels := len(e.fc.tunnels[testCtr])
	e.fc.mu.Unlock()

	if _, err := m.UpdateIsolation(ctx, "dev", api.UpdateIsolation{Name: "work"}); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, port, "ping"); got != "hello ping from work" {
		t.Fatalf("reply = %q", got)
	}
	e.fc.mu.Lock()
	again := len(e.fc.tunnels[testCtr])
	e.fc.mu.Unlock()
	if m.tunnelStatus(testIsoID) != "up" || again != tunnels {
		t.Fatalf("the tunnel was reopened (%d dials, then %d) or is %q", tunnels, again, m.tunnelStatus(testIsoID))
	}
	if env := strings.Join(m.isolationEnv(testIsoID), "\n"); !strings.Contains(env, "ECHO_TOKEN="+toks["prod"]) {
		t.Fatalf("the agents' token changed: %s", env)
	}
}
