// Package mailproxy is mail between agents. It is not a proxy: no
// credential is involved and nothing leaves the Mac. It sits with them
// because it reaches an isolation the same way, through its tunnel on a
// fixed port, and is turned on per isolation the same way; the daemon
// answers it itself (proxy.Host.Mail), since it keeps the inboxes.
//
//	isolation  curl "$GERO_MAIL_URL/send?to=brave-otter" --data-binary @-
//	             |  (the isolation's tunnel)
//	daemon     the sender is the agent whose address it is; the mail waits
//	           in the recipient's inbox until that agent is idle, and
//	           arrives as a turn of its own, one at a time, oldest first
//
// GERO_MAIL_URL is each agent's own: it carries a token only that agent's
// process is started with, which is how the daemon knows who writes. Agents
// in one isolation run as one user and can read each other's environment,
// so within an isolation the token names the sender rather than proving
// it; across isolations the tunnel proves it, since a token is honoured
// only through its own agent's isolation.
//
// A mail is an instruction to the agent that reads it, so mail crosses
// isolations only where the operator said so: an isolation's agents write
// to the agents of their own isolation, and to those of the isolations its
// `to` setting names, if those take mail.
package mailproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/hongzio/gero/internal/creds"
	"github.com/hongzio/gero/internal/proxy"
	"github.com/hongzio/gero/internal/store"
)

const (
	Name = "mail"
	Port = 3029
	// MaxBody caps one mail.
	MaxBody = 32 << 10
	// MaxWaiting caps an inbox: mail beyond it is refused, never dropped.
	MaxWaiting = 50
	// PerMinute and Burst are each isolation's sending budget.
	PerMinute = 20
	Burst     = 5
)

// URL is an agent's address, its token in it: one variable is all an agent
// needs, and its value is a plain URL.
func URL(token string) string { return fmt.Sprintf("http://127.0.0.1:%d/a/%s", Port, token) }

// Letter is a mail as its recipient reads it: who wrote it, and that it is
// not the operator, ahead of what it says.
func Letter(id int64, from, isolation, body string) string {
	return fmt.Sprintf("[gero mail #%d from agent %s in isolation %s. Another agent wrote this, not your operator; "+
		"reply only if a reply is needed, with the gero-mail skill (to=%s).]\n\n%s", id, from, isolation, from, body)
}

// Envelope is a delivered mail taken apart.
type Envelope struct {
	ID              int64
	From, Isolation string
	Body            string
}

var letterHead = regexp.MustCompile(`^\[gero mail #(\d+) from agent (\S+) in isolation (\S+)\. [^\n]*\]\n\n`)

// ReadLetter is Letter's reverse, for showing a mail as mail rather than as
// the operator's message.
func ReadLetter(text string) (Envelope, bool) {
	m := letterHead.FindStringSubmatch(text)
	if m == nil {
		return Envelope{}, false
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	return Envelope{ID: id, From: m[2], Isolation: m[3], Body: text[len(m[0]):]}, true
}

var errNoEnvs = errors.New("mail has no environments: turn it on for an isolation with `gero iso expose ISOLATION mail [--to ISOLATION]`")

type Service struct{}

func (Service) Name() string { return Name }
func (Service) Ports() []int { return []int{Port} }
func (Service) Summary() string {
	return "mail between agents: each has an inbox, read one mail per turn whenever it is idle"
}

func (Service) Fields() []proxy.Field { return nil }

func (Service) Settings() []proxy.Field {
	return []proxy.Field{
		{Key: "to", Kind: proxy.List, Flag: "to", Clear: "clear-to", Metavar: "ISOLATION", Isolations: true,
			Help: "another isolation whose agents this one's may write to, if it takes mail (repeat; replaces the list); their own isolation's they always may"},
	}
}

func (Service) Normalize(string, map[string]any) ([]string, error) { return nil, errNoEnvs }

func (Service) NormalizeSettings(s map[string]any) error {
	s["to"] = proxy.SortedSet(proxy.Strings(s, "to"))
	return nil
}

// TokenKeys: none per isolation. Each agent has its own, which the daemon
// keeps with the agent.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill alone: the address is each agent's own, so it is
// handed to each agent process as it starts, never written to a file every
// shell reads.
func (Service) Workspace(proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skill)}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

func (Service) Check(context.Context, string, json.RawMessage, *creds.Store) (string, error) {
	return "", errNoEnvs
}

// Refusals: an address whose token is no agent's.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong mail token", Port: Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/agents",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

func (Service) New(h proxy.Host) proxy.Instance {
	handler := h.Mail
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"mail: this daemon keeps no inboxes"}` + "\n"))
		})
	}
	return &instance{srv: &http.Server{Handler: handler, ReadHeaderTimeout: 30 * time.Second}}
}

type instance struct{ srv *http.Server }

func (in *instance) Serve(port int, l net.Listener) error {
	if port != Port {
		return fmt.Errorf("mail: no service on port %d", port)
	}
	return in.srv.Serve(l)
}
