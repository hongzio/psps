package gitproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/shimproto"
	"github.com/hongzio/gero/internal/store"
)

const (
	GitPort = 3020
	APIPort = 3026
	// GitConfig holds the URL rewrite; the image's /etc/gitconfig includes
	// it, and git ignores the include while it does not exist.
	GitConfig = "/etc/gero/gitconfig"
)

// Env is one git environment: repositories bundled with their credential,
// a token stored in the credentials file under [git <env>] or, failing
// that, what a command such as `gh auth token` prints.
type Env struct {
	Repos        []string  `json:"repos"`
	ReadOnly     bool      `json:"read_only"`
	TokenCommand []string  `json:"token_command,omitempty"`
	APIWrites    *[]string `json:"api_writes,omitempty"`
}

// Settings are the per-isolation switches that have nothing to do with a
// credential.
type Settings struct {
	PublicReads bool `json:"public_reads"`
	API         bool `json:"api"`
}

func defaultSettings() Settings { return Settings{PublicReads: true, API: true} }

type Service struct {
	// GitUpstream and APIUpstream default to GitHub; tests point them at a
	// local server.
	GitUpstream, APIUpstream string
	Transport                http.RoundTripper
}

func (Service) Name() string { return "git" }
func (Service) Ports() []int { return []int{GitPort, APIPort} }
func (Service) Summary() string {
	return "git and the GitHub API; public repositories bare, an allowlist for the rest"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "repos", Kind: proxy.List, Flag: "allow-repo", Clear: "clear-repos", Metavar: "OWNER/REPO", Lookup: true,
			Help: "a repository this env's credential opens (repeat; replaces the list)"},
		{Key: "read_only", Kind: proxy.Bool, Flag: "read-only", Neg: "read-write",
			Help: "refuse push and API writes for these repositories"},
		{Key: "token_command", Kind: proxy.String, Flag: "token-command", Metavar: "BIN",
			Help: "instead of a stored token: a gh-compatible binary whose `auth token` yields the credential, such as gh"},
		{Key: "api_writes", Kind: proxy.List, Flag: "api-write", Clear: "clear-api-writes", Metavar: "FAMILY",
			Choices: familyNames(), Help: "a GitHub API write family to allow (repeat; default comments, pulls, reviews)"},
	}
}

func (Service) Settings() []proxy.Field {
	return []proxy.Field{
		{Key: "public_reads", Kind: proxy.Bool, Flag: "public-reads", Neg: "no-public-reads",
			Help: "forward public repositories with no credential (default on)"},
		{Key: "api", Kind: proxy.Bool, Flag: "api", Neg: "no-api",
			Help: "serve the GitHub API side at GITHUB_API_URL (default on)"},
	}
}

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	repos := proxy.Strings(env, "repos")
	var bad []string
	for _, r := range repos {
		owner, repo, ok := strings.Cut(r, "/")
		if !ok || strings.Contains(repo, "/") || !Segment.MatchString(owner) || !Segment.MatchString(repo) {
			bad = append(bad, r)
		}
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("--allow-repo takes owner/repo: %s", strings.Join(bad, ", "))
	}
	env["repos"] = proxy.SortedSet(repos)
	env["read_only"] = proxy.BoolOr(env, "read_only", false)
	if _, ok := env["api_writes"]; !ok {
		// Explicit on every add and edit, so `gero env ls` shows what the API
		// side will permit rather than a default the reader must know.
		env["api_writes"] = append([]string(nil), DefaultWrites...)
	}
	env["api_writes"] = proxy.SortedSet(proxy.Strings(env, "api_writes"))
	// Pin the credential command to an absolute path now: the daemon may
	// run under launchd's minimal PATH. Without one, the credential is the
	// token stored for the env.
	if tc, ok := env["token_command"].(string); ok {
		cmd, err := pinCommand(tc)
		if err != nil {
			return nil, err
		}
		env["token_command"] = cmd
	}
	var warn []string
	if len(env["repos"].([]string)) == 0 {
		warn = append(warn, fmt.Sprintf("env %q lists no repositories; it routes nothing until `gero env edit git %s --allow-repo owner/repo`", name, name))
	}
	for _, w := range env["api_writes"].([]string) {
		if w == "merge" {
			warn = append(warn, fmt.Sprintf("env %q allows the merge write family: the agent can land pull requests, not just open them", name))
		}
	}
	return warn, nil
}

func pinCommand(bin string) ([]string, error) {
	found, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%q is not on PATH; pass --token-command with a full path", bin)
	}
	return []string{found, "auth", "token"}, nil
}

// CredentialElsewhere: an env with a token command needs no stored token.
func (Service) CredentialElsewhere(cfg json.RawMessage) bool {
	var e Env
	return json.Unmarshal(cfg, &e) == nil && len(e.TokenCommand) > 0
}

func (Service) NormalizeSettings(s map[string]any) error {
	s["public_reads"] = proxy.BoolOr(s, "public_reads", true)
	s["api"] = proxy.BoolOr(s, "api", true)
	return nil
}

func (Service) TokenKeys(store.Exposure) []string { return nil }

func decodeEnvs(v proxy.View) (map[string]Env, error) {
	envs := map[string]Env{}
	for _, name := range v.Exposure.Envs {
		var e Env
		if _, ok := v.Envs[name]; !ok {
			continue
		}
		if err := v.Decode(name, &e); err != nil {
			return nil, fmt.Errorf("git env %s: %w", name, err)
		}
		envs[name] = e
	}
	return envs, nil
}

// CheckExposure refuses a repository two exposed envs both list: routing
// would be ambiguous, and the operator should hear it at expose time, not
// the agent at its clone.
func (Service) CheckExposure(v proxy.View) error {
	envs, err := decodeEnvs(v)
	if err != nil {
		return err
	}
	seen := map[string]string{}
	names := make([]string, 0, len(envs))
	for n := range envs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, r := range envs[name].Repos {
			key := strings.ToLower(r)
			if prev, ok := seen[key]; ok && prev != name {
				return fmt.Errorf("repo %s is listed by envs %q and %q; remove it from one before exposing both", r, prev, name)
			}
			seen[key] = name
		}
	}
	return nil
}

func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	s := defaultSettings()
	v.Settings(&s)
	envs, err := decodeEnvs(v)
	if err != nil {
		return proxy.Workspace{}, err
	}
	ws := proxy.Workspace{}
	base := fmt.Sprintf("http://127.0.0.1:%d/", GitPort)
	ws.Files = append(ws.Files, shimproto.SyncFile{Path: GitConfig, Mode: 0o644, Content: "" +
		"# Written by gero. GitHub URLs go through the git proxy, which holds the\n" +
		"# credential; this is a default, not a fence. See the gero-git skill.\n" +
		"[url \"" + base + "\"]\n" +
		"\tinsteadOf = https://github.com/\n" +
		"\tinsteadOf = git@github.com:\n" +
		"\tinsteadOf = ssh://git@github.com/\n"})
	if s.API {
		ws.Env = append(ws.Env, fmt.Sprintf("GITHUB_API_URL=http://127.0.0.1:%d", APIPort))
	}
	ws.Files = append(ws.Files, proxy.SkillFiles("git", skillBody(v, envs, s))...)
	return ws, nil
}

func (Service) Managed() []string { return append([]string{GitConfig}, proxy.SkillDirs("git")...) }

func (s Service) New(h proxy.Host) proxy.Instance {
	gitUp, _ := url.Parse(or(s.GitUpstream, "https://github.com"))
	apiUp, _ := url.Parse(or(s.APIUpstream, "https://api.github.com"))
	rt := s.Transport
	if rt == nil {
		rt = &http.Transport{
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	in := &instance{h: h, gitUpstream: gitUp, apiUpstream: apiUp, transport: rt,
		buckets: &proxy.Buckets{PerMinute: 120, Burst: 30}, tokens: map[string]*TokenSource{}}
	in.public = NewPublicRepos(gitUp, rt, func(m string) { in.logf("%s", m) })
	in.gitSrv = &http.Server{Handler: gitHandler{in}, ReadHeaderTimeout: 30 * time.Second}
	in.apiSrv = &http.Server{Handler: apiHandler{in}, ReadHeaderTimeout: 30 * time.Second}
	return in
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type instance struct {
	h                        proxy.Host
	gitUpstream, apiUpstream *url.URL
	transport                http.RoundTripper
	public                   Checker
	buckets                  *proxy.Buckets
	gitSrv, apiSrv           *http.Server

	mu     sync.Mutex
	tokens map[string]*TokenSource
}

func (in *instance) Serve(port int, l net.Listener) error {
	switch port {
	case GitPort:
		return in.gitSrv.Serve(l)
	case APIPort:
		return in.apiSrv.Serve(l)
	}
	return fmt.Errorf("git proxy: no service on port %d", port)
}

func (in *instance) logf(format string, a ...any) { in.h.Logf("git", format, a...) }

type liveConfig struct {
	envs     map[string]Env
	settings Settings
}

func (in *instance) config() (liveConfig, error) {
	v, err := in.h.View()
	if err != nil {
		return liveConfig{}, err
	}
	if !v.Enabled {
		return liveConfig{}, fmt.Errorf("the git proxy is off for isolation %s; the operator turns it on with `gero iso expose %s git <env>`", in.h.Name(), in.h.Name())
	}
	envs, err := decodeEnvs(v)
	if err != nil {
		return liveConfig{}, err
	}
	s := defaultSettings()
	v.Settings(&s)
	return liveConfig{envs: envs, settings: s}, nil
}

// credential is the env's token: the one stored for it, read per request
// like every proxy's secret, or else what its token command prints.
func (in *instance) credential(env string, e Env) (string, error) {
	if token, ok := storedToken(in.h.Creds, env); ok {
		return token, nil
	}
	if len(e.TokenCommand) == 0 {
		return "", proxy.Deny("git env %s has no credential; store a token with `gero env secret git %s`", env, env)
	}
	return in.token(env, e.TokenCommand).Get()
}

func storedToken(c *creds.Store, env string) (string, bool) {
	if c == nil {
		return "", false
	}
	v, err := c.Read("git", env)
	if err != nil || v["token"] == "" {
		return "", false
	}
	return v["token"], true
}

// token is the env's TokenSource, one per env and command, so two GitHub
// accounts never share a cache slot.
func (in *instance) token(env string, cmd []string) *TokenSource {
	key := env + "\x00" + strings.Join(cmd, "\x00")
	in.mu.Lock()
	defer in.mu.Unlock()
	t, ok := in.tokens[key]
	if !ok {
		t = NewTokenSource(cmd, time.Minute)
		in.tokens[key] = t
	}
	return t
}

// Refusals: the API side answers for listed repositories only, so a
// repository no env lists is refused here, before GitHub is asked; with
// the API side off, everything is.
func (Service) Refusals(v proxy.View) []proxy.Refusal {
	s := defaultSettings()
	v.Settings(&s)
	r := proxy.Refusal{Port: APIPort, Method: "GET", Path: "/repos/gero-verify/not-a-listed-repo/pulls"}
	if !s.API {
		r.What, r.Status, r.Want = "the API side, which is off", 503, "gero github api proxy: the GitHub API side is off"
	} else {
		r.What, r.Status, r.Want = "a repository no env lists", 404, "gero github api proxy: gero-verify/not-a-listed-repo is not in any exposed git environment"
	}
	return []proxy.Refusal{r}
}

// Check takes the env's credential and asks GitHub who it is.
func (s Service) Check(ctx context.Context, name string, cfg json.RawMessage, c *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	source := "stored token"
	token, ok := storedToken(c, name)
	if !ok {
		if len(e.TokenCommand) == 0 {
			return "", proxy.Deny("git env %s has no credential; store a token with `gero env secret git %s`", name, name)
		}
		var err error
		if token, err = NewTokenSource(e.TokenCommand, 0).Get(); err != nil {
			return "", err
		}
		source = "token command"
	}
	out := fmt.Sprintf("credential %s (%d repo(s) listed%s)\n", source, len(e.Repos), map[bool]string{true: ", read-only"}[e.ReadOnly])
	login, scopes, err := probeUser(ctx, or(s.APIUpstream, "https://api.github.com"), token)
	if err != nil {
		return out, err
	}
	writes := "none (reads only)"
	if w := WritesFor(e); len(w) > 0 && !e.ReadOnly {
		writes = strings.Join(w, ", ")
	}
	out += fmt.Sprintf("api        authenticates as %s; writes: %s\n", or(login, "?"), writes)
	if scopes != nil {
		if scopes["workflow"] {
			out += "scopes     include workflow: pushes may touch .github/workflows/\n"
		} else {
			out += "scopes     no workflow scope: GitHub refuses pushes touching .github/workflows/\n"
		}
	}
	return out, nil
}

func probeUser(ctx context.Context, base, token string) (string, map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(base, "/")+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "gero-ghapi")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("could not reach %s: %w", base, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("the token was refused by %s (%d): %.200s", base, resp.StatusCode, body)
	}
	var u struct {
		Login string `json:"login"`
	}
	json.Unmarshal(body, &u)
	h, present := resp.Header["X-Oauth-Scopes"]
	var scopes map[string]bool
	if present {
		scopes = ParseScopes(strings.Join(h, ","), true)
	}
	return u.Login, scopes, nil
}
