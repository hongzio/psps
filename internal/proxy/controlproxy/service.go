// Package controlproxy opens some of what the operator does with the gero
// command to an isolation's agents: list agents and read them, make and
// write to them, stop, archive and fork them, and the like. An env is a
// set of those, ticked one by one; an isolation has one env, and reaches
// its own isolation's agents and those of the isolations its exposure
// names. Like team it is not a proxy: no credential is involved and
// nothing leaves the Mac. It is served on mail's port, at each agent's own
// address, so the daemon knows which agent asks; the daemon answers it
// itself, since it keeps the agents.
//
//	isolation  curl "$GERO_CONTROL_URL/agents"
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the address is an agent's: what its isolation's env opens,
//	           on the agents of the isolations it reaches
//
// What is credentials or rights is never among the actions: accounts,
// envs, services, an isolation's environment and logins, approvals (an
// agent's mode, and approving), a lead's eval (a command run elsewhere),
// making, changing and removing isolations, removing agents, and the Mac's
// own (the fence, the daemon). A team's eval is one only as team.evalset
// has it: run in the isolation of the agent that sets it, never the
// lead's, on a copy of a directory in the lead's.
package controlproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

const Name = "control"

// URL is an agent's control address, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/control" }

// An Action is one thing an env may open, named after the gero command
// that does it.
type Action struct {
	Name string
	Help string
	// Write: it changes something; the rest only read.
	Write bool
}

// Actions are all an env may open, in the order they are shown.
var Actions = []Action{
	{"agent.ls", "list the agents it reaches, and see one", false},
	{"agent.log", "read an agent's conversation", false},
	{"agent.new", "make an agent", true},
	{"agent.send", "write to an agent", true},
	{"agent.interrupt", "stop an agent's turn", true},
	{"agent.archive", "archive an agent", true},
	{"agent.restore", "restore an archived agent", true},
	{"agent.fork", "fork an agent", true},
	{"agent.rename", "rename an agent", true},
	{"agent.team", "put an agent in a team, or take it out", true},
	{"agent.set", "set an agent's model and effort", true},
	{"agent.cron", "see an agent's crons, and set crons on it", true},
	{"agent.inbox", "read an agent's inbox, and clear it", true},
	{"iso.ls", "list the isolations it reaches", false},
	{"iso.services", "see which services an isolation has", false},
	{"team.ls", "list the teams", false},
	{"team.show", "see a team's run", false},
	{"team.new", "make a team", true},
	{"team.rename", "rename a team", true},
	{"team.rm", "remove a team", true},
	{"team.eval", "run a team's eval", true},
	{"team.evalset", "set a team's eval, to run in its own isolation", true},
}

// Reads are what an env opens when it is made without saying.
func Reads() []string {
	var out []string
	for _, a := range Actions {
		if !a.Write {
			out = append(out, a.Name)
		}
	}
	return out
}

func names() []string {
	out := make([]string, len(Actions))
	for i, a := range Actions {
		out[i] = a.Name
	}
	return out
}

const (
	// DefaultMaxLive is how many agents an isolation's agents may have
	// made through the service, live at once, when its env does not say.
	DefaultMaxLive = 3
	maxMaxLive     = 20
)

// Env is an env as stored.
type Env struct {
	Allow   []string `json:"allow"`
	MaxLive int      `json:"max_live"`
}

// Opens is whether the env opens action.
func (e Env) Opens(action string) bool {
	for _, a := range e.Allow {
		if a == action {
			return true
		}
	}
	return false
}

// Settings are an exposure's: the isolations, by ID, whose agents its own
// reach besides their own, and the approvals of an agent they make in one
// of those (ask when left out).
type Settings struct {
	Reach     []string `json:"reach"`
	Approvals string   `json:"approvals,omitempty"`
}

// Elsewhere are the approvals an agent made in another isolation may be
// given: none that approves more than auto's reviewer would.
var Elsewhere = []string{"ask", "auto"}

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }

func (Service) Summary() string {
	return "an isolation's agents do some of what you do with the gero command: the actions its env ticks"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "allow", Kind: proxy.List, Flag: "allow", Clear: "clear-allow", Metavar: "ACTION", Choices: names(),
			Help: "a gero command to open to the agents (repeat; replaces the list; default the reads: " + strings.Join(Reads(), ", ") + ")"},
		{Key: "max_live", Kind: proxy.Int, Flag: "max-live", Metavar: "N",
			Help: fmt.Sprintf("how many agents an isolation's agents may have made through it (agent.new, agent.fork), live at once (default %d)", DefaultMaxLive)},
	}
}

func (Service) Settings() []proxy.Field {
	return []proxy.Field{
		{Key: "reach", Kind: proxy.List, Flag: "reach", Clear: "clear-reach", Metavar: "ISOLATION", Isolations: true,
			Help: "another isolation whose agents this one's reach (repeat; replaces the list); their own they always do"},
		{Key: "approvals", Kind: proxy.String, Flag: "approvals", Metavar: "MODE", Choices: Elsewhere,
			Help: "the approvals of an agent they make in an isolation they reach, at most the maker's own (ask or auto; default ask)"},
	}
}

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	if _, set := env["allow"]; set {
		have := map[string]bool{}
		for _, a := range proxy.Strings(env, "allow") {
			have[a] = true
		}
		allow := []string{}
		for _, a := range names() {
			if have[a] {
				allow = append(allow, a)
			}
		}
		env["allow"] = allow
	} else {
		// Explicit on every add and edit, so `gero env ls` shows what opens.
		env["allow"] = Reads()
	}
	switch n := env["max_live"].(type) {
	case nil:
		env["max_live"] = DefaultMaxLive
	case int:
		if n < 1 || n > maxMaxLive {
			return nil, fmt.Errorf("--max-live is 1 to %d, not %d", maxMaxLive, n)
		}
	case float64:
		if n < 1 || n > maxMaxLive {
			return nil, fmt.Errorf("--max-live is 1 to %d, not %v", maxMaxLive, n)
		}
		env["max_live"] = int(n)
	}
	var writes []string
	for _, a := range env["allow"].([]string) {
		for _, x := range Actions {
			if x.Name == a && x.Write {
				writes = append(writes, a)
			}
		}
	}
	var warn []string
	if len(writes) > 0 {
		warn = append(warn, fmt.Sprintf("env %q lets agents change gero (%s), on the agents they reach", name, strings.Join(writes, ", ")))
	}
	if len(env["allow"].([]string)) == 0 {
		warn = append(warn, fmt.Sprintf("env %q opens nothing", name))
	}
	return warn, nil
}

func (Service) NormalizeSettings(s map[string]any) error {
	s["reach"] = proxy.SortedSet(proxy.Strings(s, "reach"))
	switch a, _ := s["approvals"].(string); a {
	case "":
		delete(s, "approvals")
	case "ask", "auto":
	default:
		return fmt.Errorf("--approvals is %s, not %q", strings.Join(Elsewhere, " or "), a)
	}
	return nil
}

// OneEnv: an isolation has one env, its agents' rights; two would be one,
// their union, under two names.
func (Service) OneEnv() {}

// CheckExposure holds an exposure to one env.
func (Service) CheckExposure(v proxy.View) error {
	if len(v.Exposure.Envs) != 1 {
		return fmt.Errorf("an isolation has one control env, the actions its agents may take; name one (not %d)", len(v.Exposure.Envs))
	}
	return nil
}

// TokenKeys: none. An agent's address is its mail address, which the
// daemon keeps with the agent.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill alone, saying what the env opens: the address is
// each agent's own, handed to its process as it starts.
func (Service) Workspace(v proxy.View) (proxy.Workspace, error) {
	var e Env
	if err := v.Decode(v.Exposure.Default, &e); err != nil {
		return proxy.Workspace{}, fmt.Errorf("control env %s: %w", v.Exposure.Default, err)
	}
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skillFor(e))}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

// Check: there is nothing outside gero to reach; it says what the env
// opens.
func (Service) Check(_ context.Context, _ string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	return fmt.Sprintf("opens %s; it reaches nothing outside gero", orNothing(strings.Join(e.Allow, ", "))), nil
}

// Refusals: an address whose token is no agent's.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong control token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/control",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("control: no service on port %d", port)
}
