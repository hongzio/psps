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
