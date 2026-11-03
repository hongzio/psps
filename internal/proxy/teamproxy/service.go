// Package teamproxy lets a team's lead run its team: make agents of its
// own in the isolation, read how they are doing, write to them, stop and
// archive them, and have the team's eval run. Like cron it is not a proxy:
// no credential is involved and nothing leaves the Mac. It is served on
// mail's port, at each agent's own address; the daemon answers it itself,
// since it keeps the agents.
//
//	isolation  curl "$PSPS_TEAM_URL/agents" --json '{"role":"profiler","kind":"claude","prompt":"..."}'
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the address is the lead's: an agent of its own, in its
//	           team and its isolation, within the operator's terms
//
// Every agent of an isolation that has it gets the address; only a lead's
// answers. A lead reaches the agents it made, and no other.
package teamproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

const Name = "team"

// URL is an agent's team, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/team" }

var errNoEnvs = errors.New("team has no environments: turn it on for an isolation with `psps iso expose ISOLATION team`")

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "a team's lead makes agents of its own here and hears when their turns end"
}

func (Service) Fields() []proxy.Field                              { return nil }
func (Service) Settings() []proxy.Field                            { return nil }
func (Service) Normalize(string, map[string]any) ([]string, error) { return nil, errNoEnvs }
func (Service) NormalizeSettings(map[string]any) error             { return nil }

// TokenKeys: none. An agent's address is its mail address, which the
// daemon keeps with the agent.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill alone: the address is each agent's own, handed
// to its process as it starts.
func (Service) Workspace(proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skill)}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

func (Service) Check(context.Context, string, json.RawMessage, *creds.Store) (string, error) {
	return "", errNoEnvs
}

// Refusals: an address whose token is no agent's.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong team token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/team",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("team: no service on port %d", port)
}
