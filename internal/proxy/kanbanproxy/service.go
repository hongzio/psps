// Package kanbanproxy gives an isolation's agents kanban boards: an env is
// a board, with a color of its own, and an isolation is given the boards
// its agents work on (one or more; the web tells them apart by color).
// A card is in one of three columns, To Do, In Progress and Done; it may
// have an assignee, an agent, and comments. Every change to a card or a
// comment is a line in the board's history, which nothing removes. Like
// mail it is not a proxy: no credential is involved and nothing leaves the
// Mac. It is served on mail's port, at each agent's own address, so the
// daemon knows who changed what; the daemon answers it itself, since it
// keeps the boards.
//
//	isolation  curl "$PSPS_KANBAN_URL"            the boards it sees, and their cards
//	           curl "$PSPS_KANBAN_URL" --json '{"title": "..."}'
//	             |  (the isolation's tunnel, mail's port)
//	daemon     the address is an agent's: what the exposure allows, on the
//	           boards the isolation is given
package kanbanproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/hongzio/psps/internal/creds"
	"github.com/hongzio/psps/internal/proxy"
	"github.com/hongzio/psps/internal/proxy/mailproxy"
	"github.com/hongzio/psps/internal/store"
)

const (
	Name = "kanban"
	// MaxTitle caps a card's title, in characters; MaxBody its body and
	// MaxComment a comment, in bytes.
	MaxTitle   = 200
	MaxBody    = 32 << 10
	MaxComment = 16 << 10
)

// URL is an agent's boards, at its own address on mail's port.
func URL(token string) string { return mailproxy.URL(token) + "/kanban" }

// Colors are what a board may be drawn in: the web's icon colors.
var Colors = []string{"gray", "red", "orange", "yellow", "green", "teal", "blue", "violet", "pink"}

// DefaultColor is a board's when its env does not say.
const DefaultColor = "gray"

// Env is an env as stored: a board.
type Env struct {
	Color string `json:"color"`
}

// The rights an exposure gives its agents, besides reading the boards.
const (
	Create  = "create"
	Edit    = "edit"
	Move    = "move"
	Comment = "comment"
	Delete  = "delete"
)

// An Right is one thing an exposure may let agents do.
type Right struct {
	Name, Help string
}

// Rights are all an exposure may allow, in order.
var Rights = []Right{
	{Create, "make cards"},
	{Edit, "change a card's title, text, assignee and board"},
	{Move, "move a card between To Do, In Progress and Done"},
	{Comment, "write comments, and edit and delete their own"},
	{Delete, "delete cards"},
}

// DefaultAllow is what an exposure allows when it does not say.
var DefaultAllow = []string{Create, Edit, Move, Comment}

func rightNames() []string {
	out := make([]string, len(Rights))
	for i, r := range Rights {
		out[i] = r.Name
	}
	return out
}

// Settings are an exposure's.
type Settings struct {
	Allow []string `json:"allow"`
}

// Allows is whether s lets agents do right.
func (s Settings) Allows(right string) bool {
	for _, a := range s.Allow {
		if a == right {
			return true
		}
	}
	return false
}

// StatusNames are the columns as people name them.
var StatusNames = map[string]string{store.CardTodo: "To Do", store.CardDoing: "In Progress", store.CardDone: "Done"}

// ParseStatus reads a column however it is written: todo, doing, done, or
// its name ("In Progress").
func ParseStatus(s string) (string, error) {
	k := strings.ToLower(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(strings.TrimSpace(s)))
	switch k {
	case "todo":
		return store.CardTodo, nil
	case "doing", "inprogress", "progress", "wip":
		return store.CardDoing, nil
	case "done":
		return store.CardDone, nil
	}
	return "", fmt.Errorf("a card's status is todo, doing or done, not %q", s)
}

// ParseCard reads a card's number: 12, #12 or K-12.
func ParseCard(s string) (int64, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "#"), "K-"), "k-")
	var id int64
	if _, err := fmt.Sscanf(t, "%d", &id); err != nil || id <= 0 || fmt.Sprint(id) != t {
		return 0, fmt.Errorf("%q is not a card's number (K-12, or 12)", s)
	}
	return id, nil
}

type Service struct{}

func (Service) Name() string { return Name }

// Ports: none of its own; the daemon serves it on mail's.
func (Service) Ports() []int { return nil }

func (Service) Summary() string {
	return "kanban boards for agents: an env is a board in a color of its own; cards, comments and a history of every change"
}

func (Service) Fields() []proxy.Field {
	return []proxy.Field{
		{Key: "color", Kind: proxy.String, Flag: "color", Metavar: "COLOR", Choices: Colors,
			Help: "the board's color, telling its cards apart where an isolation sees several (default " + DefaultColor + ")"},
	}
}

func (Service) Settings() []proxy.Field {
	return []proxy.Field{
		{Key: "allow", Kind: proxy.List, Flag: "allow", Clear: "clear-allow", Metavar: "RIGHT", Choices: rightNames(),
			Help: "what the agents may do besides reading the boards (repeat; replaces the list; default " + strings.Join(DefaultAllow, ", ") + "; --clear-allow: read only)"},
	}
}

func (Service) Normalize(name string, env map[string]any) ([]string, error) {
	c, _ := env["color"].(string)
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case c == "" || c == "default" || c == "grey":
		c = DefaultColor
	case c == "purple":
		c = "violet"
	}
	ok := false
	for _, x := range Colors {
		ok = ok || x == c
	}
	if !ok {
		return nil, fmt.Errorf("--color is one of %s, not %q", strings.Join(Colors, ", "), c)
	}
	env["color"] = c
	return nil, nil
}

func (Service) NormalizeSettings(s map[string]any) error {
	if _, set := s["allow"]; !set {
		// Explicit on every expose, so `psps iso services` shows it.
		s["allow"] = DefaultAllow
		return nil
	}
	have := map[string]bool{}
	for _, a := range proxy.Strings(s, "allow") {
		have[a] = true
	}
	allow := []string{}
	for _, r := range rightNames() {
		if have[r] {
			allow = append(allow, r)
		}
	}
	s["allow"] = allow
	return nil
}

// CheckExposure: an isolation is given at least one board.
func (Service) CheckExposure(v proxy.View) error {
	if len(v.Exposure.Envs) == 0 {
		return fmt.Errorf("name the boards its agents work on: `psps iso expose %s kanban BOARD,...` (make one with `psps env add kanban NAME --color COLOR`)", v.Isolation)
	}
	return nil
}

// TokenKeys: none. An agent's address is its mail address, which the
// daemon keeps with the agent.
func (Service) TokenKeys(store.Exposure) []string { return nil }

// Workspace is the skill alone: the address is each agent's own, handed
// to its process as it starts, and the boards are read from it.
func (Service) Workspace(proxy.View) (proxy.Workspace, error) {
	return proxy.Workspace{Files: proxy.SkillFiles(Name, skill)}, nil
}

func (Service) Managed() []string { return proxy.SkillDirs(Name) }

// Check: there is nothing outside psps to reach.
func (Service) Check(_ context.Context, name string, cfg json.RawMessage, _ *creds.Store) (string, error) {
	var e Env
	if err := json.Unmarshal(cfg, &e); err != nil {
		return "", err
	}
	return fmt.Sprintf("board %s, in %s; it reaches nothing outside psps", name, e.Color), nil
}

// Refusals: an address whose token is no agent's.
func (Service) Refusals(proxy.View) []proxy.Refusal {
	return []proxy.Refusal{{What: "a wrong kanban token", Port: mailproxy.Port, Method: "GET", Path: "/a/" + proxy.WrongToken + "/kanban",
		Status: http.StatusForbidden, Want: "not the address of an agent"}}
}

// New: nothing to serve; mail's port answers.
func (Service) New(proxy.Host) proxy.Instance { return instance{} }

type instance struct{}

func (instance) Serve(port int, l net.Listener) error {
	l.Close()
	return fmt.Errorf("kanban: no service on port %d", port)
}
