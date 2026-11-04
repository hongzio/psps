// Package jobproxy lets an isolation's agents have a job run elsewhere: a
// command of the operator's (a runner, an env of this service) run in the
// runner's isolation, on a copy of a directory of theirs, with options of
// their own, its result coming back to them. psps says who asked in a
// file the command can trust, and what of the result reaches the caller
// is what the command puts there for it. Like team it is not a proxy: no
// credential is involved and nothing leaves the Mac. It is served on
// mail's port, at each agent's own address, so the daemon knows which
// agent asks; the daemon answers it itself, since it runs the jobs.
//
//	isolation  curl "$PSPS_JOB_URL/jobs" --json '{"runner":"sim","dir":"/home/agent/work/x","options":{...}}'
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the address is an agent's: a runner its isolation's exposure
//	           names, in that runner's isolation, within the runner's terms
//
// The same jobs run for psps itself (a team's eval on a runner) and for the
// operator (`psps job run`); the command tells them apart by the caller in
// its meta.json, never by anything in the copy, which is the caller's.
package jobproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/isolation"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

const Name = "job"

// URL is an agent's jobs, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/job" }

// Defaults and bounds of a runner's terms.
const (
	DefaultMinutes  = 60
	MaxMinutes      = 24 * 60
	DefaultMemory   = "4G"
	DefaultPids     = 512
	MinPids         = 16
	MaxPids         = 1 << 16
	DefaultMaxLive  = 2
	MaxMaxLive      = 20
	DefaultQueued   = 20
	MaxQueued       = 200
	DefaultMaxMB    = 500
	MaxMaxMB        = 20 << 10
	DefaultMaxFiles = 50_000
	MaxMaxFiles     = 1_000_000
	// OptionsMax is the most a job's options may be, as JSON.
	OptionsMax = 16 << 10
	// ResultMax is the most a job's result.json may be.
	ResultMax = 1 << 20
)

// DefaultExclude are the names a copy leaves out when the runner does not
// say.
var DefaultExclude = []string{".git"}

// Env is a runner as stored. Isolation holds one isolation, by ID.
type Env struct {
	Isolation []string `json:"isolation"`
	Command   string   `json:"command"`
	Minutes   int      `json:"minutes"`
	Memory    string   `json:"memory"`
	Pids      int      `json:"pids"`
	MaxLive   int      `json:"max_live"`
	MaxQueued int      `json:"max_queued"`
	MaxMB     int      `json:"max_mb"`
	MaxFiles  int      `json:"max_files"`
	Exclude   []string `json:"exclude"`
}

// Iso is the runner's isolation, by ID; "" if it has none.
func (e Env) Iso() string {
	if len(e.Isolation) == 0 {
		return ""
	}
	return e.Isolation[0]
}

// MemoryBytes is the runner's memory limit.
func (e Env) MemoryBytes() int64 {
	b, err := isolation.MemoryBytes(e.Memory)
	if err != nil || b == 0 {
		b, _ = isolation.MemoryBytes(DefaultMemory)
	}
	return int64(b)
}

// Limits are the runner's limits on a copy.
func (e Env) Limits() isolation.CopyLimits {
	return isolation.CopyLimits{MaxBytes: int64(e.MaxMB) << 20, MaxFiles: e.MaxFiles, Exclude: e.Exclude}
}

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }

func (Service) Summary() string {
	return "an isolation's agents have a runner's command run in its isolation, on a copy of a directory of theirs"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "isolation", Kind: proxy.List, Flag: "isolation", Clear: "clear-isolation", Metavar: "ISOLATION", Isolations: true,
			Help: "the isolation the runner's command runs in, as its agent user (one)"},
		{Key: "command", Kind: proxy.String, Flag: "command", Metavar: "CMD",
			Help: "the shell command a job runs, in the copy (PSPS_JOB_WORK), with PSPS_JOB_META, PSPS_JOB_OPTIONS and PSPS_JOB_OUT; it leaves out/result.json"},
		{Key: "minutes", Kind: proxy.Int, Flag: "minutes", Metavar: "N",
			Help: fmt.Sprintf("how long a job may run (default %d, at most %d)", DefaultMinutes, MaxMinutes)},
		{Key: "memory", Kind: proxy.String, Flag: "memory", Metavar: "SIZE",
			Help: fmt.Sprintf("the memory a job's processes may use together, no swap (default %s)", DefaultMemory)},
		{Key: "pids", Kind: proxy.Int, Flag: "pids", Metavar: "N",
			Help: fmt.Sprintf("how many processes a job may have at once (default %d)", DefaultPids)},
		{Key: "max_live", Kind: proxy.Int, Flag: "max-live", Metavar: "N",
			Help: fmt.Sprintf("how many of the runner's jobs run at once; the rest wait (default %d)", DefaultMaxLive)},
		{Key: "max_queued", Kind: proxy.Int, Flag: "max-queued", Metavar: "N",
			Help: fmt.Sprintf("how many may wait; one more is refused (default %d)", DefaultQueued)},
		{Key: "max_mb", Kind: proxy.Int, Flag: "max-mb", Metavar: "N",
			Help: fmt.Sprintf("the most a copy's files may be, in MiB (default %d)", DefaultMaxMB)},
		{Key: "max_files", Kind: proxy.Int, Flag: "max-files", Metavar: "N",
			Help: fmt.Sprintf("the most entries a copy may have (default %d)", DefaultMaxFiles)},
		{Key: "exclude", Kind: proxy.List, Flag: "exclude", Clear: "clear-exclude", Metavar: "NAME",
			Help: "a name a copy leaves out wherever it is in the tree (repeat; replaces the list; default " + strings.Join(DefaultExclude, ", ") + ")"},
	}
}

func (Service) Settings() []proxy.Field { return nil }

// bound fills env[key] with def when unset and holds it to lo..hi.
func bound(env map[string]any, key, flag string, def, lo, hi int) error {
	var n int
	switch v := env[key].(type) {
	case nil:
		env[key] = def
		return nil
	case int:
		n = v
	case float64:
		n = int(v)
		if float64(n) != v {
			return fmt.Errorf("--%s is a whole number, not %v", flag, v)
		}
	default:
		return fmt.Errorf("--%s is a number", flag)
	}
	if n < lo || n > hi {
		return fmt.Errorf("--%s is %d to %d, not %d", flag, lo, hi, n)
	}
	env[key] = n
	return nil
}

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	if n := len(proxy.Strings(env, "isolation")); n != 1 {
		return nil, fmt.Errorf("a runner runs in one isolation; name it with --isolation (not %d)", n)
	}
	env["isolation"] = proxy.Strings(env, "isolation")
	cmd, _ := env["command"].(string)
	if strings.TrimSpace(cmd) == "" {
		return nil, errors.New("a runner has a command; give it with --command")
	}
	mem, _ := env["memory"].(string)
	if mem == "" {
		mem = DefaultMemory
	}
	if _, err := isolation.MemoryBytes(mem); err != nil {
		return nil, fmt.Errorf("--memory: %w", err)
	}
	env["memory"] = mem
	for _, b := range []struct {
		key, flag   string
		def, lo, hi int
	}{
		{"minutes", "minutes", DefaultMinutes, 1, MaxMinutes},
		{"pids", "pids", DefaultPids, MinPids, MaxPids},
		{"max_live", "max-live", DefaultMaxLive, 1, MaxMaxLive},
		{"max_queued", "max-queued", DefaultQueued, 0, MaxQueued},
		{"max_mb", "max-mb", DefaultMaxMB, 1, MaxMaxMB},
		{"max_files", "max-files", DefaultMaxFiles, 1, MaxMaxFiles},
	} {
		if err := bound(env, b.key, b.flag, b.def, b.lo, b.hi); err != nil {
			return nil, err
		}
	}
	if _, set := env["exclude"]; !set {
		env["exclude"] = DefaultExclude
	} else {
		ex := proxy.SortedSet(proxy.Strings(env, "exclude"))
		for _, e := range ex {
			if e == "" || e == "." || e == ".." || strings.Contains(e, "/") {
				return nil, fmt.Errorf("--exclude is a name, not a path: %q", e)
			}
		}
		env["exclude"] = ex
	}
	return nil, nil
}

func (Service) NormalizeSettings(map[string]any) error { return nil }

// TokenKeys: none. An agent's address is its mail address, which the
// daemon keeps with the agent.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill alone: the address is each agent's own, handed
// to its process as it starts, and GET on it lists the runners.
func (Service) Workspace(proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skill)}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

// Check: there is nothing outside psps to reach; it says what the runner
// does.
func (Service) Check(_ context.Context, _ string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	return fmt.Sprintf("runs %q for %dm at most, %s and %d processes; it reaches nothing outside psps itself", e.Command, e.Minutes, e.Memory, e.Pids), nil
}

// Refusals: an address whose token is no agent's.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong job token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/job",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("job: no service on port %d", port)
}
