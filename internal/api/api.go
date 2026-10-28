// Package api holds the request and response types of the daemon's HTTP API,
// shared by the daemon and every client (CLI, TUI, web).
package api

import (
	"time"

	"github.com/hongzio/gero/internal/driver"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const Version = "0.1.0"

type Health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
	// Web is the URL this daemon serves its web UI on, or empty with
	// WebError saying why it does not.
	Web      string `json:"web,omitempty"`
	WebError string `json:"web_error,omitempty"`
}

type Isolation struct {
	store.Isolation
	EnvKeys []string `json:"env_keys,omitempty"`
	State   string   `json:"state"`
	// FenceState is "verified", "off", or why agents cannot start.
	FenceState string `json:"fence_state"`
	// Proxies is "up" while the credential proxies' tunnel is open, or
	// what is keeping it down.
	Proxies string `json:"proxies,omitempty"`
	// Accounts names the account each agent CLI uses, by CLI; a CLI with
	// none uses a login made inside the isolation.
	Accounts map[driver.Kind]string `json:"accounts,omitempty"`
}

type CreateIsolation struct {
	Name   string `json:"name"`
	CPUs   int    `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
	// Fence is "on" (default) or "off".
	Fence string `json:"fence,omitempty"`
}

// SetEnv sets (non-nil) or removes (nil) isolation environment entries.
type SetEnv map[string]*string

type Agent struct {
	store.Agent
	State driver.State `json:"state"`
	// Problem is an infrastructure error (isolation down, shim unreachable)
	// that the agent's own status cannot express.
	Problem string `json:"problem,omitempty"`
}

type CreateAgent struct {
	Isolation string         `json:"isolation"`
	Kind      driver.Kind    `json:"kind"`
	Prompt    string         `json:"prompt"`
	Name      string         `json:"name,omitempty"`
	Repo      string         `json:"repo,omitempty"`
	Options   driver.Options `json:"options"`
}

type Message struct {
	Text string `json:"text"`
}

type Decision struct {
	Decision driver.Decision `json:"decision"`
}

type Error struct {
	Error string `json:"error"`
}

// Service describes one credential proxy, enough for a client to build its
// forms and flags.
type Service struct {
	Name     string        `json:"name"`
	Summary  string        `json:"summary"`
	Ports    []int         `json:"ports"`
	Fields   []proxy.Field `json:"fields"`
	Settings []proxy.Field `json:"settings"`
	// Secrets are the credential fields `gero env secret` asks for; Login
	// means a browser login writes them instead.
	Secrets []string `json:"secrets,omitempty"`
	Login   bool     `json:"login,omitempty"`
}

// Env is an env definition as clients see it: never the secret, only
// whether it is set.
type Env struct {
	store.Env
	// MissingSecrets names the credential fields not yet stored; empty when
	// the env's secret is complete or the service keeps none.
	MissingSecrets []string `json:"missing_secrets,omitempty"`
	SecretError    string   `json:"secret_error,omitempty"`
	ExposedTo      []string `json:"exposed_to,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

// Exposure is one service's state for one isolation.
type Exposure struct {
	Service string `json:"service"`
	Enabled bool   `json:"enabled"`
	store.Exposure
	// Missing names exposed envs the registry no longer has.
	Missing []string `json:"missing,omitempty"`
}

// Expose turns a service on for an isolation, or changes what it serves.
// A nil Envs keeps the current list.
type Expose struct {
	Envs     []string    `json:"envs"`
	Default  string      `json:"default_env,omitempty"`
	Settings proxy.Patch `json:"settings"`
}

type Check struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// Severities of a verify check. An isolation check failing means a
// guarantee is broken; a config check failing means something is set up
// wrong or not at all.
const (
	SevIsolation = "isolation"
	SevConfig    = "config"
)

// VerifyCheck is one measured guarantee.
type VerifyCheck struct {
	Check    string `json:"check"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Expected string `json:"expected,omitempty"`
	Got      string `json:"got"`
	Detail   string `json:"detail,omitempty"`
}

// Verify is `gero iso verify`: an isolation's guarantees, measured now.
type Verify struct {
	Isolation string        `json:"isolation"`
	Checks    []VerifyCheck `json:"checks"`
}

// Exit codes of `gero iso verify`, as isolate's.
const (
	VerifyOK        = 0
	VerifyIsolation = 1
	VerifyConfig    = 2
)

func (v Verify) ExitCode() int {
	code := VerifyOK
	for _, c := range v.Checks {
		switch {
		case c.OK:
		case c.Severity == SevIsolation:
			return VerifyIsolation
		default:
			code = VerifyConfig
		}
	}
	return code
}

// CLI is an agent CLI in an isolation as an agent there finds it: its
// account and the isolation's environment applied.
type CLI struct {
	Tool     driver.Kind `json:"tool"`
	Version  string      `json:"version,omitempty"`
	LoggedIn bool        `json:"logged_in"`
	// Method is how it is logged in: "Claude subscription",
	// "CLAUDE_CODE_OAUTH_TOKEN", "ChatGPT", "API key", …
	Method string `json:"method,omitempty"`
	Email  string `json:"email,omitempty"`
	// Account is the account the isolation uses for it, if any.
	Account string `json:"account,omitempty"`
	// Error is why the state could not be read.
	Error string `json:"error,omitempty"`
	// Login is the login in progress, or the one that just ended.
	Login *CLILogin `json:"login,omitempty"`
}

// CLI login states.
const (
	CLILoginStarting  = "starting"  // the CLI has not said where to go yet
	CLILoginOpen      = "open"      // waiting for the browser
	CLILoginVerifying = "verifying" // Claude's code was handed over
	CLILoginDone      = "done"
	CLILoginFailed    = "failed"
)

// CLILogin is an agent CLI's own login, run by the daemon inside the
// isolation. Claude's ends on a page showing a code to paste back
// (NeedsCode); Codex's asks for Code to be typed at URL and notices by
// itself.
type CLILogin struct {
	Tool      driver.Kind `json:"tool"`
	State     string      `json:"state"`
	URL       string      `json:"url,omitempty"`
	Code      string      `json:"code,omitempty"`
	NeedsCode bool        `json:"needs_code,omitempty"`
	// Message is the CLI's last words once the login ended.
	Message string    `json:"message,omitempty"`
	Expires time.Time `json:"expires"`
}

// CLICode is the code Claude's login page shows.
type CLICode struct {
	Code string `json:"code"`
}

// AccountKind is a kind of agent CLI credential an Account can hold: one
// that stays valid wherever it is used at once, so any number of isolations
// can share it. (A ChatGPT login cannot: its refresh token is single-use,
// so Codex logs in inside each isolation.)
type AccountKind struct {
	Name string      `json:"name"`
	Tool driver.Kind `json:"tool"`
	// Env is the variable agents start with.
	Env     string `json:"env"`
	Summary string `json:"summary"`
	// Prefix is what every value of the kind starts with.
	Prefix string `json:"prefix,omitempty"`
}

var AccountKinds = []AccountKind{
	{Name: "claude-token", Tool: driver.Claude, Env: "CLAUDE_CODE_OAUTH_TOKEN", Prefix: "sk-ant-oat",
		Summary: "a token from `claude setup-token`: your Claude subscription, valid for a year"},
	{Name: "anthropic-api-key", Tool: driver.Claude, Env: "ANTHROPIC_API_KEY", Prefix: "sk-ant-api",
		Summary: "an Anthropic Console API key, billed per use"},
}

func AccountKindNamed(name string) (AccountKind, bool) {
	for _, k := range AccountKinds {
		if k.Name == name {
			return k, true
		}
	}
	return AccountKind{}, false
}

// Account is an account as clients see it: never the secret.
type Account struct {
	store.Account
	Tool driver.Kind `json:"tool"`
	// UsedBy are the isolations that use it.
	UsedBy []string `json:"used_by,omitempty"`
	// SecretError is why its secret cannot be read; empty when it can.
	SecretError string `json:"secret_error,omitempty"`
}

// PutAccount adds an account (Kind and Secret required) or replaces an
// account's secret.
type PutAccount struct {
	Kind   string `json:"kind,omitempty"`
	Secret string `json:"secret"`
}

// UseAccount sets the account an isolation's CLI uses; "" goes back to a
// login inside the isolation.
type UseAccount struct {
	Account string `json:"account"`
}

// LoginStart begins a browser login for a service whose credential is one.
// Redirect is a loopback address the caller may be listening on; empty
// picks one nobody is, and the operator pastes back where the browser
// landed.
type LoginStart struct {
	Redirect string `json:"redirect,omitempty"`
}

type Login struct {
	ID       string    `json:"id"`
	URL      string    `json:"url"`
	Redirect string    `json:"redirect"`
	Expires  time.Time `json:"expires"`
}

// LoginFinish carries the address the browser landed on, or its query.
type LoginFinish struct {
	Callback string `json:"callback"`
}
