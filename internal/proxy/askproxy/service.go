// Package askproxy lets agents leave the operator something to answer: a
// question, a go-ahead to give, or a thing to do by hand. It never waits
// for the operator: the agent posts, goes on with what it can, and hears
// the answer later, as a turn of its own when it is idle. A question it
// cannot go on without is not one for here but its CLI's own (Claude's
// AskUserQuestion). An ask is a thread while it is open: the operator may
// reply without closing it, and the agent add what it found since, until
// the operator answers it or the agent resolves it. Like mail it is not a proxy: no credential is involved
// and nothing leaves the Mac. It is served on mail's port, at each agent's
// own address; the daemon answers it itself, since it keeps the asks.
//
//	isolation  curl "$PSPS_ASK_URL" --json '{"title":"Apply the migration to prod?","kind":"confirm"}'
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the ask is the agent's whose address it is; it waits for
//	           the operator, and their answer reaches the agent when it is
//	           idle, all answers waiting in one message
//	           curl "$PSPS_ASK_URL/42" --json '{"text":"Found the audit logs: ..."}'
//	           adds to it, and it waits on the operator again
//
// An agent sees and withdraws its own asks only. It has at most MaxOpen
// open; one with the title of another still open is that one.
package askproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

const (
	Name = "ask"
	// MaxOpen caps the asks one agent has open.
	MaxOpen = 10
	// MaxBody caps an ask's body; MaxTitle its title.
	MaxBody  = 32 << 10
	MaxTitle = 200
	// MinOptions and MaxOptions bound a question's options, when it has
	// any.
	MinOptions = 2
	MaxOptions = 6
	// MaxText caps what the operator writes back: room for a pasted log
	// or document, which in Korean is 3 bytes a character.
	MaxText = 128 << 10
	// MaxNotes caps what the two add to one ask while it is open.
	MaxNotes = 40
	// MaxTransfer caps the file a transfer moves.
	MaxTransfer = 500 << 20
)

// The kinds of ask.
const (
	Question = "question"
	Confirm  = "confirm"
	Task     = "task"
	// Transfer moves a file from one isolation to another's inbox, along
	// a route the operator allowed, once they approve it (yes or no).
	Transfer = "transfer"
)

// The outcomes the operator gives a confirm and a task.
const (
	Yes  = "yes"
	No   = "no"
	Done = "done"
	Cant = "cant"
)

// inbox is where a transfer lands in the isolation it goes to.
const inbox = "/home/agent/inbox"

// URL is an agent's asks, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/asks" }

var errNoEnvs = errors.New("ask has no environments: turn it on for an isolation with `psps iso expose ISOLATION ask`")

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }
func (Service) Summary() string {
	return "asks agents leave you, never waiting: a question, a go-ahead or a thing to do by hand; your answer reaches them when idle"
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
	return []proxy.Refusal{{What: "a wrong ask token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/asks",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("ask: no service on port %d", port)
}

// Answers is the message that tells an agent what became of asks of its
// the operator closed, or replied to and kept open. The replies new to it
// are the operator's notes not delivered yet.
func Answers(as []store.Ask) string {
	var b strings.Builder
	answered, replied := 0, 0
	for _, a := range as {
		if a.State == store.AskOpen {
			replied++
		} else {
			answered++
		}
	}
	an := func(n int) string {
		if n == 1 {
			return "an ask"
		}
		return fmt.Sprintf("%d asks", n)
	}
	switch {
	case replied == 0 && answered == 1:
		b.WriteString("[psps ask: your operator answered an ask of yours. This is their answer; act on it as you would on their message.]")
	case replied == 0:
		fmt.Fprintf(&b, "[psps ask: your operator answered %d asks of yours. These are their answers; act on them as you would on their message.]", answered)
	case answered == 0:
		fmt.Fprintf(&b, "[psps ask: your operator replied to %s of yours and kept %s open. Act on what they say as you would on their message.]", an(replied), map[bool]string{true: "it", false: "them"}[replied == 1])
	default:
		fmt.Fprintf(&b, "[psps ask: your operator answered %s of yours and replied to %s, kept open. Act on what they say as you would on their message.]", an(answered), an(replied))
	}
	for _, a := range as {
		b.WriteString("\n\n")
		b.WriteString(Answer(a))
	}
	return b.String()
}

// Answer is what became of one ask, as its agent reads it: the operator's
// replies it has not heard yet, and, closed, their answer.
func Answer(a store.Ask) string {
	var b strings.Builder
	v := Verdict(a)
	if a.State == store.AskOpen {
		v = "still open, your turn"
	}
	fmt.Fprintf(&b, "#%d %s (%s): %s.", a.ID, strconv.Quote(a.Title), a.Kind, v)
	for _, n := range a.Notes {
		if n.Author == store.TurnOperator && n.DeliveredAt == nil {
			b.WriteString("\nTheir reply: " + strings.TrimSpace(n.Text))
		}
	}
	if t := a.Transfer; t != nil && t.CopiedAt != nil {
		fmt.Fprintf(&b, "\nCopied: %s/%s, %d bytes, sha256 %s.", inbox, t.Name, t.Size, t.SHA256)
	}
	if opts := a.Current(); a.Pick > 0 && a.Pick <= len(opts) {
		fmt.Fprintf(&b, "\nPicked: %s (option %d)", opts[a.Pick-1].Label, a.Pick)
	}
	if t := strings.TrimSpace(a.Text); t != "" {
		b.WriteString("\nTheir words: " + t)
	}
	if a.State == store.AskOpen {
		fmt.Fprintf(&b, "\nAdd to it with POST \"$PSPS_ASK_URL/%d\" (what you found, or your answer to theirs), or resolve it there if this settles it; do not leave a new ask for it.", a.ID)
	}
	return b.String()
}

// Verdict is what became of an ask, in a few words.
func Verdict(a store.Ask) string {
	switch {
	case a.State == store.AskDismissed:
		return "closed without an answer"
	case a.State == store.AskWithdrawn:
		return "withdrawn"
	case a.State == store.AskResolved:
		return "resolved by the agent"
	case a.State == store.AskOpen:
		return "open"
	case a.Outcome == Yes && a.Kind == Transfer:
		return "approved: the file is copied"
	case a.Outcome == Yes:
		return "approved"
	case a.Outcome == No:
		return "declined"
	case a.Outcome == Done:
		return "done"
	case a.Outcome == Cant:
		return "not done: they could not"
	}
	return "answered"
}

var answersHead = regexp.MustCompile(`^\[psps ask: your operator (?:answered|replied) [^\n]*\]\n\n`)

// IsAnswers is whether text is a message Answers made.
func IsAnswers(text string) bool { return answersHead.MatchString(text) }
