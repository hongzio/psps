// Package cronproxy lets agents set crons of their own: messages sent to
// themselves on a schedule, as the operator's crons are. Like mail it is
// not a proxy: no credential is involved and nothing leaves the Mac. It is
// served on mail's port, at each agent's own address, since that port is
// open in every isolation's tunnel and the address already says which
// agent asks; the daemon answers it itself, since it keeps the crons.
//
//	isolation  curl "$GERO_CRON_URL" --json '{"schedule":"@every 1h","prompt":"check CI"}'
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the cron is the agent's whose address it is, and comes
//	           like the operator's, saying the agent set it
//
// An agent reaches its own crons only: it sees all of them, and changes
// and removes those it set; the operator's stay the operator's. It sets
// at most MaxCrons, none coming more often than MinGap.
package cronproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/proxy/mailproxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Name = "cron"
	// MaxCrons caps the crons one agent sets itself.
	MaxCrons = 20
	// MinGap is the shortest an agent's own cron may leave between runs.
	MinGap = 5 * time.Minute
)

// URL is an agent's crons, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/crons" }

var errNoEnvs = errors.New("cron has no environments: turn it on for an isolation with `gero iso expose ISOLATION cron`")

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "crons agents set themselves: a message to themselves on a schedule, as yours are"
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
	return []proxy.Refusal{{What: "a wrong cron token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/crons",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("cron: no service on port %d", port)
}
